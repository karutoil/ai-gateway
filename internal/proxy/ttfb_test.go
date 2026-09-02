package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/resilience"

	"github.com/go-chi/chi/v5"
)

// The gateway is silent pre-commit by design; behind Cloudflare's ~100s
// first-byte window that silence surfaced to clients as a synthesized 524
// ("Connection to Grok timed out or was interrupted") while the gateway
// logged a 499 at ~124.9s for a request that was still healthy. These tests
// pin the TTFB controller: honest 504 for buffered calls, keepalive SSE
// commit for streams — both well before any edge can time out.

type ttfbStack struct {
	srv  *httptest.Server
	up   *httptest.Server
	hits *atomic.Int32
	key  string
}

func newTTFBStack(t *testing.T, budget time.Duration, maxRetries int, upstream http.HandlerFunc) *ttfbStack {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	master := make([]byte, 32)
	ps := provider.NewStore(database, master)
	ks := apikey.NewStore(database)
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	if _, err := ps.Create("openai", models.ProviderOpenAI, up.URL+"/v1", "sk-test"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("ttfb-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandler(ps, database)
	h.Timeouts.TTFB = budget
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: maxRetries, BaseDelay: 10 * time.Millisecond}
	h.Metrics = nil
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/chat/completions", h.ChatCompletions)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &ttfbStack{srv: srv, up: up, hits: &atomic.Int32{}, key: k.Key}
}

const ttfbStreamBody = `{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"hi"}]}`
const ttfbBufferedBody = `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`

const ttfbSSE = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"

// doTTFB performs a request and reports status, body, and time-to-first-byte.
func doTTFB(t *testing.T, s *ttfbStack, body string) (int, string, time.Duration) {
	t.Helper()
	req, _ := http.NewRequest("POST", s.srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.key)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var ttfb time.Duration
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	ttfb = time.Since(start)
	rest, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(buf[:n]) + string(rest), ttfb
}

// Buffered request + unresponsive upstream: the gateway must answer with an
// honest 504 inside the budget instead of staying silent until the edge (or
// the transport's 120s header timeout) kills the exchange.
func TestTTFBBudgetBuffered504(t *testing.T) {
	var s *ttfbStack
	s = newTTFBStack(t, 300*time.Millisecond, 0, func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		time.Sleep(2 * time.Second) // far beyond the budget
	})
	code, body, ttfb := doTTFB(t, s, ttfbBufferedBody)
	if code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d body %s", code, body)
	}
	if !strings.Contains(body, "first-byte timeout") {
		t.Fatalf("expected first-byte timeout message, got %s", body)
	}
	if ttfb > 2*time.Second {
		t.Fatalf("504 must arrive within the budget, took %v", ttfb)
	}
	if s.hits.Load() != 1 {
		t.Fatalf("expected exactly 1 upstream attempt, hits=%d", s.hits.Load())
	}
}

// Streaming request + slow upstream: once the budget is spent the gateway
// commits SSE headers and a keepalive frame, so bytes reach the client BEFORE
// the upstream has answered anything. The stream then completes normally.
func TestTTFBStreamKeepaliveThenSuccess(t *testing.T) {
	var s *ttfbStack
	s = newTTFBStack(t, 300*time.Millisecond, 2, func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		time.Sleep(1200 * time.Millisecond) // budget 300ms < sleep < test patience
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ttfbSSE)
	})
	code, body, ttfa := doTTFB(t, s, ttfbStreamBody)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d body %s", code, body)
	}
	// First bytes must precede the upstream's 1.2s answer (keepalive commit).
	if ttfa >= 1100*time.Millisecond {
		t.Fatalf("keepalive bytes must flow before the upstream answers, first byte after %v", ttfa)
	}
	if !strings.Contains(body, ": keepalive") {
		t.Fatalf("expected keepalive frame, got %s", body)
	}
	if !strings.Contains(body, `"chat.completion.chunk"`) || !strings.Contains(body, "[DONE]") {
		t.Fatalf("expected real completion frames after keepalive, got %s", body)
	}
	if s.hits.Load() < 1 {
		t.Fatalf("upstream must have been attempted, hits=%d", s.hits.Load())
	}
}

// Streaming request where the upstream never recovers: the committed SSE
// exchange must terminate with a protocol-correct in-band error frame
// (OpenAI data: {"error"...} + [DONE]) instead of a second status line.
func TestTTFBStreamInBandErrorWhenUpstreamNeverRecovers(t *testing.T) {
	var s *ttfbStack
	s = newTTFBStack(t, 300*time.Millisecond, 0, func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		time.Sleep(2 * time.Second)
	})
	code, body, _ := doTTFB(t, s, ttfbStreamBody)
	if code != http.StatusOK {
		t.Fatalf("keepalive commit must surface as 200 SSE, got %d", code)
	}
	if !strings.Contains(body, ": keepalive") {
		t.Fatalf("expected keepalive frame, got %s", body)
	}
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, "[DONE]") {
		t.Fatalf("expected in-band SSE error termination, got %s", body)
	}
	if strings.Contains(body, `"chat.completion.chunk"`) {
		t.Fatalf("no completion content may be fabricated, got %s", body)
	}
	if s.hits.Load() != 1 {
		t.Fatalf("expected exactly 1 upstream attempt (retries disabled), hits=%d", s.hits.Load())
	}
}

// TTFB disabled (budget 0): the watchdog must never fire and behavior must
// match the pre-TTFB gateway (wait as long as the upstream needs).
func TestTTFBDisabledPreservesLegacyBehavior(t *testing.T) {
	var s *ttfbStack
	s = newTTFBStack(t, 0, 0, func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		time.Sleep(700 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	})
	code, body, _ := doTTFB(t, s, ttfbBufferedBody)
	if code != http.StatusOK || !strings.Contains(body, `"ok"`) {
		t.Fatalf("expected normal 200 completion, got %d %s", code, body)
	}
	if strings.Contains(body, "keepalive") {
		t.Fatalf("no keepalive may appear with TTFB disabled, got %s", body)
	}
}

// keepaliveSafeWriter: after the keepalive commit, a late WriteHeader must be
// swallowed (a second status line on the wire is a protocol violation), while
// writes pass through serialized.
func TestKeepaliveSafeWriterSwallowsLateStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	c := newTTFBController(time.Second, time.Now())
	w := &keepaliveSafeWriter{ResponseWriter: rec, c: c}
	w.WriteHeader(http.StatusBadGateway) // not committed yet → passes through
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("pre-commit WriteHeader must pass, got %d", rec.Code)
	}
	c.committed.Store(true)
	c.sent.Store(true)
	w.WriteHeader(http.StatusInternalServerError) // committed → swallowed
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("post-commit WriteHeader must be swallowed, got %d", rec.Code)
	}
	if _, err := w.Write([]byte("frame")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), "frame") {
		t.Fatalf("post-commit writes must pass, got %s", rec.Body.String())
	}
}

// Budget expiry math: expired() flips only when the budget has elapsed and
// the keepalive has not been committed; remaining() never goes negative and
// disarms once committed.
func TestTTFBControllerExpiryMath(t *testing.T) {
	c := newTTFBController(50*time.Millisecond, time.Now())
	if c.expired() {
		t.Fatal("fresh controller must not be expired")
	}
	if c.remaining() <= 0 || c.remaining() > 50*time.Millisecond {
		t.Fatalf("remaining must be within budget, got %v", c.remaining())
	}
	time.Sleep(60 * time.Millisecond)
	if !c.expired() {
		t.Fatal("controller must expire after the budget elapses")
	}
	if c.remaining() != 0 {
		t.Fatalf("remaining must clamp to 0, got %v", c.remaining())
	}
	c.committed.Store(true)
	if c.expired() {
		t.Fatal("committed controller is never 'expired' (commit already happened)")
	}
	if c.remaining() != 0 {
		t.Fatalf("committed controller must disarm the watchdog, got %v", c.remaining())
	}
}
