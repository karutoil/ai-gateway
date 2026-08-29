package proxy

// Throughput tests for the gateway's data plane.
//
// These tests wire the exact production hot path from cmd/gateway/main.go:
//
//	RequestID -> Recovery
//	  -> GatewayAuthWithJWTRevocation (sk-gw-* key lookup in SQLite)
//	  -> budget.Middleware (per-key quota checks against spend_counters)
//	  -> GatewayRateLimitWithLimits (in-memory fixed-window limiter)
//	  -> proxy.Handler (alias resolve, provider resolve, mock upstream relay,
//	                    request_logs INSERT, usage recording)
//
// The upstream is an in-process httptest.Server that replies instantly, so the
// measured request rate is the gateway's own per-request overhead (middleware,
// translation, SQLite bookkeeping) rather than provider latency.
//
// Modes (all share the same harness):
//
//	go test ./internal/proxy -run TestGatewayThroughput -v
//	    quick load test: sequential + concurrent + streaming phases
//	    (~1s per phase; overridable via THROUGHPUT_REQUESTS/THROUGHPUT_CLIENTS).
//
//	go test ./internal/proxy -run TestGatewayThroughput -v -short
//	    skips the whole test so the race detector / CI stays fast.
//
//	go test ./internal/proxy -run TestGatewayThroughputSustained -v
//	    ~20s mixed streaming/non-streaming soak, opt-in via
//	    THROUGHPUT_SOAK_SECONDS (auto 2s under -race; 0 skips).
//
//	go test ./internal/proxy -bench BenchmarkGatewayThroughput
//	    standard go benchmarks (sequential, concurrent, streaming).

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/budget"
	"ai-gateway/internal/cache"
	"ai-gateway/internal/catalog"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/otel"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/resilience"

	"github.com/go-chi/chi/v5"
)

// ---------------------------------------------------------------------------
// Harness: production-parity router over an in-memory DB + instant upstream.
// ---------------------------------------------------------------------------

type throughputUpstream struct {
	srv     *httptest.Server
	streams atomic.Int64
	totals  atomic.Int64
}

type throughputHarness struct {
	srv      *httptest.Server
	upstream *throughputUpstream
	key      string // raw sk-gw-... bearer token
	database *sql.DB
}

func newThroughputHarness(tb testing.TB) *throughputHarness {
	tb.Helper()

	// File-backed (like production) so durability/pragma effects are
	// measurable: :memory: has no disk journal and hides fsync costs.
	database, err := db.Open(filepath.Join(tb.TempDir(), "throughput.db"))
	if err != nil {
		tb.Fatalf("open temp-file db: %v", err)
	}

	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i)
	}
	ps := provider.NewStore(database, master)
	ks := apikey.NewStore(database)

	up := &throughputUpstream{}
	up.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.totals.Add(1)
		if !strings.Contains(r.URL.Path, "chat/completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"chatcmpl-bench","object":"chat.completion","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"Hello throughput"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`)
			return
		}
		up.streams.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-bench\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-bench\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"}}]}\n\n")
		// Final usage-only chunk (what metered OpenAI-compatible upstreams emit).
		io.WriteString(w, "data: {\"id\":\"chatcmpl-bench\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":2,\"total_tokens\":14}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))

	if _, err := ps.Create("openai", models.ProviderOpenAI, up.srv.URL+"/v1", "sk-bench"); err != nil {
		tb.Fatalf("create provider: %v", err)
	}

	// Key limits: a fresh key defaults to 60 RPM (apikey.Verify fallback), so
	// load testing needs an operator-style raised limit — the same thing a
	// real deployment does for high-throughput keys. RPH/RPD stay unset.
	k, err := ks.Create("throughput-key")
	if err != nil {
		tb.Fatalf("create key: %v", err)
	}
	rpm := 100000
	if err := ks.UpdateLimits(k.ID, &rpm, nil, nil, nil, nil); err != nil {
		tb.Fatalf("raise key rpm limit: %v", err)
	}

	// --- Production wiring (mirrors cmd/gateway/main.go) ---
	h := newLegacyHandlerWithCatalog(ps, catalog.NewStore(database), database)
	h.Timeouts = DefaultTimeouts()
	h.CacheTTLSeconds = 10
	h.StreamUsageInject = true
	h.Cache = cache.NewMemoryCache(512)
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 2, BaseDelay: 200 * time.Millisecond}
	h.Breaker = resilience.NewMemoryCircuitBreakerFull(5, 60*time.Second, 30*time.Second, 2)
	h.Metrics = otel.NewMetrics()
	limiter := budget.NewDBLimiter(database)
	h.Usage = &budget.UsageSink{Limiter: limiter}
	h.RateLimiter = middleware.NewRateLimiter()

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recovery)
	r.Use(middleware.GatewayAuthWithJWTRevocation(ks, nil, nil))
	r.Use(budget.Middleware(limiter))
	rl := middleware.NewRateLimiter()
	r.Use(middleware.GatewayRateLimitWithLimits(rl, func(req *http.Request) middleware.RateLimits {
		if k, ok := middleware.GatewayKeyFromContext(req.Context()); ok && k != nil {
			rpm := k.RateLimitRPM
			if rpm == 0 {
				rpm = 60
			}
			return middleware.RateLimits{RPM: rpm, RPH: k.RateLimitRPH, RPD: k.RateLimitRPD}
		}
		return middleware.RateLimits{RPM: 60}
	}))
	r.Post("/v1/chat/completions", h.ChatCompletions)

	srv := httptest.NewServer(r)

	return &throughputHarness{srv: srv, upstream: up, key: k.Key, database: database}
}

func (th *throughputHarness) close() {
	th.srv.Close()
	th.upstream.srv.Close()
	th.database.Close()
}

// ---------------------------------------------------------------------------
// Load generation.
// ---------------------------------------------------------------------------

type throughputResult struct {
	requests  int
	errors    []string
	elapsed   time.Duration
	latencies []time.Duration
}

func (r *throughputResult) rps() float64 {
	if r.elapsed <= 0 {
		return 0
	}
	return float64(r.requests) / r.elapsed.Seconds()
}

// chatBody builds a unique chat payload. Uniqueness matters: the gateway
// caches non-streaming responses (10s TTL here), and repeated bodies would
// measure the cache instead of the proxy path. seq is a process-wide counter.
func chatBody(seq int64, stream bool) string {
	return fmt.Sprintf(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"throughput probe %d"}],"stream":%t}`, seq, stream)
}

// bodySeq hands out globally unique body indices so no two requests in a
// process (across phases, benchmarks iterations, or soak loops) collide.
var bodySeq atomic.Int64

func nextBody(stream bool) string {
	return chatBody(bodySeq.Add(1), stream)
}

// measureLoad drives n requests against the harness with c concurrent
// clients. Only status-200 responses count as success; every non-200 is
// recorded. elapsed is the wall-clock time of the whole phase.
func (th *throughputHarness) measureLoad(n, c int, streamRatio float64) *throughputResult {
	if c < 1 {
		c = 1
	}
	res := &throughputResult{requests: n}
	var mu sync.Mutex
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 60 * time.Second}

	start := time.Now()
	wg.Add(c)
	for w := 0; w < c; w++ {
		go func(worker int) {
			defer wg.Done()
			for i := worker; i < n; i += c {
				stream := streamRatio > 0 && int64(i%100) < int64(streamRatio*100)
				reqStart := time.Now()
				req, err := http.NewRequest(http.MethodPost, th.srv.URL+"/v1/chat/completions",
					strings.NewReader(nextBody(stream)))
				if err != nil {
					mu.Lock()
					res.errors = append(res.errors, fmt.Sprintf("build request: %v", err))
					mu.Unlock()
					continue
				}
				req.Header.Set("Authorization", "Bearer "+th.key)
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				if err != nil {
					mu.Lock()
					res.errors = append(res.errors, fmt.Sprintf("request: %v", err))
					mu.Unlock()
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				lat := time.Since(reqStart)
				mu.Lock()
				res.latencies = append(res.latencies, lat)
				if resp.StatusCode != http.StatusOK {
					res.errors = append(res.errors, fmt.Sprintf("status %d", resp.StatusCode))
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	res.elapsed = time.Since(start)
	return res
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

func summarize(tb testing.TB, label string, res *throughputResult) {
	tb.Helper()
	sort.Slice(res.latencies, func(i, j int) bool { return res.latencies[i] < res.latencies[j] })
	tb.Logf("%s: %d req in %v => %.0f req/s (p50=%v p95=%v p99=%v, errors=%d)",
		label, res.requests, res.elapsed.Truncate(time.Millisecond), res.rps(),
		percentile(res.latencies, 0.50).Truncate(time.Microsecond),
		percentile(res.latencies, 0.95).Truncate(time.Microsecond),
		percentile(res.latencies, 0.99).Truncate(time.Microsecond),
		len(res.errors))
}

func firstErrors(res *throughputResult) string {
	if len(res.errors) == 0 {
		return ""
	}
	return strings.Join(res.errors[:min(len(res.errors), 5)], "; ")
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

// TestGatewayThroughput exercises the production hot path sequentially and
// concurrently, asserting correctness (all 200s, upstream actually hit,
// usage metered, logs written) and logging measured req/s and percentiles.
func TestGatewayThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput load test skipped in -short mode")
	}
	h := newThroughputHarness(t)
	defer h.close()

	// Race instrumentation slows the hot loop several-fold and distorts
	// latency percentiles; halve the load when -race is on.
	n := envInt("THROUGHPUT_REQUESTS", 400)
	c := envInt("THROUGHPUT_CLIENTS", 8)
	if raceDetector {
		n, c = n/2, c/2
	}

	// Phase 1: sequential — isolates single-request overhead.
	res := h.measureLoad(n, 1, 0)
	if err := firstErrors(res); err != "" {
		t.Fatalf("sequential phase errors: %s", err)
	}
	summarize(t, "sequential", res)

	// Phase 2: concurrent — the headline throughput number.
	res2 := h.measureLoad(n*c, c, 0)
	if err := firstErrors(res2); err != "" {
		t.Fatalf("concurrent phase errors: %s", err)
	}
	summarize(t, fmt.Sprintf("concurrent x%d", c), res2)

	// Phase 3: streaming — SSE relay path (no response caching involved).
	res3 := h.measureLoad(n, c, 1.0)
	if err := firstErrors(res3); err != "" {
		t.Fatalf("streaming phase errors: %s", err)
	}
	summarize(t, fmt.Sprintf("streaming x%d", c), res3)

	// Correctness: every request reached the real upstream (cache must not
	// serve unique bodies), usage was metered, and every proxied request was
	// logged by the time the load settled (per-request bookkeeping is
	// synchronous up to the stream pump's final INSERT).
	if got := h.upstream.totals.Load(); got < int64(n) {
		t.Fatalf("upstream saw %d requests, want >= %d (cache must not serve unique bodies)", got, n)
	}
	verifyBookkeeping(t, h.database, h.upstream.totals.Load())
}

// TestGatewayThroughputSustained is an opt-in soak: a mixed streaming and
// non-streaming load held for THROUGHPUT_SOAK_SECONDS seconds (unset = skip;
// under -race it defaults to 2s for CI). Reports throughput plus
// p50/p95/p99 latencies and fails on any error.
//
//	THROUGHPUT_SOAK_SECONDS=60 go test ./internal/proxy -run TestGatewayThroughputSustained -v
func TestGatewayThroughputSustained(t *testing.T) {
	if testing.Short() {
		t.Skip("sustained soak skipped in -short mode")
	}
	// Opt-in: a fixed 20s soak would slow every plain `go test ./...` run.
	// CI (race mode) gets a short soak for leak/race detection; timing runs
	// are intentional invocations with the env var set.
	soak := 0
	if raceDetector {
		soak = 2
	}
	if v := os.Getenv("THROUGHPUT_SOAK_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			soak = n
		}
	}
	if soak <= 0 {
		t.Skip("set THROUGHPUT_SOAK_SECONDS to run the sustained soak")
	}
	duration := time.Duration(soak) * time.Second

	h := newThroughputHarness(t)
	defer h.close()

	const clients = 8
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var count int
	var latencies []time.Duration
	var firstErr string
	client := &http.Client{Timeout: 60 * time.Second}

	worker := func(w int) {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			stream := i%10 < 3 // ~30% streaming mix, like production traffic
			start := time.Now()
			req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/chat/completions",
				strings.NewReader(nextBody(stream)))
			if err != nil {
				mu.Lock()
				if firstErr == "" {
					firstErr = err.Error()
				}
				mu.Unlock()
				continue
			}
			req.Header.Set("Authorization", "Bearer "+h.key)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				mu.Lock()
				if firstErr == "" {
					firstErr = fmt.Sprintf("request: %v", err)
				}
				mu.Unlock()
				continue
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			lat := time.Since(start)
			mu.Lock()
			if resp.StatusCode != http.StatusOK && firstErr == "" {
				firstErr = fmt.Sprintf("status %d", resp.StatusCode)
			}
			count++
			latencies = append(latencies, lat)
			mu.Unlock()
		}
	}

	wg.Add(clients)
	for w := 0; w < clients; w++ {
		go worker(w)
	}
	time.Sleep(duration)
	close(stop)
	wg.Wait()

	if firstErr != "" {
		t.Fatalf("sustained load hit an error: %s", firstErr)
	}
	if count == 0 {
		t.Fatal("no requests completed during soak window")
	}
	rps := float64(count) / duration.Seconds()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	t.Logf("sustained %.0fs @ %d clients: %d requests => %.0f req/s (p50=%v p95=%v p99=%v)",
		duration.Seconds(), clients, count, rps,
		percentile(latencies, 0.50).Truncate(time.Microsecond),
		percentile(latencies, 0.95).Truncate(time.Microsecond),
		percentile(latencies, 0.99).Truncate(time.Microsecond))
	verifyBookkeeping(t, h.database, h.upstream.totals.Load())
}

// verifyBookkeeping asserts the per-request bookkeeping keeps up with the
// load: after the load returns, request_logs must have one row per proxied
// request and spend_counters must be populated. Streaming requests relay
// [DONE] to the client marginally before the pump goroutine reads upstream
// EOF and issues its log INSERT, so give the tail a short settle window.
func verifyBookkeeping(t *testing.T, database *sql.DB, upstreamHits int64) {
	t.Helper()
	var logs, spends int
	scan := func() error {
		if err := database.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&logs); err != nil {
			return err
		}
		return database.QueryRow(`SELECT COUNT(*) FROM spend_counters`).Scan(&spends)
	}
	if err := scan(); err != nil {
		t.Fatalf("count bookkeeping tables: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for int64(logs) < upstreamHits && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		if err := scan(); err != nil {
			t.Fatalf("count bookkeeping tables: %v", err)
		}
	}
	if int64(logs) != upstreamHits {
		t.Errorf("request_logs rows = %d, upstream hits = %d (every proxied request must be logged)", logs, upstreamHits)
	}
	if spends == 0 {
		t.Error("spend_counters is empty (usage recording never ran)")
	}
}

// ---------------------------------------------------------------------------
// Benchmarks.
// ---------------------------------------------------------------------------

// BenchmarkGatewayThroughput measures req/s of the full hot path. Run with:
//
//	go test ./internal/proxy -bench BenchmarkGatewayThroughput -benchtime=3s
//
// -benchmem shows allocations per request; req/s is reported directly.
func BenchmarkGatewayThroughput(b *testing.B) {
	h := newThroughputHarness(b)
	defer h.close()

	cases := []struct {
		name        string
		parallel    bool
		streamRatio float64
	}{
		{"sequential", false, 0},
		{"concurrent", true, 0},
		{"streaming", true, 1.0},
	}

	client := &http.Client{Timeout: 60 * time.Second}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()

			one := func() {
				stream := tc.streamRatio > 0
				req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/chat/completions",
					strings.NewReader(nextBody(stream)))
				if err != nil {
					b.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+h.key)
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				if err != nil {
					b.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					b.Fatalf("status %d", resp.StatusCode)
				}
			}

			if tc.parallel {
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						one()
					}
				})
			} else {
				for i := 0; i < b.N; i++ {
					one()
				}
			}
			b.StopTimer()
			// ns/op inverts to req/s; report it directly for readability.
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "req/s")
		})
	}
}
