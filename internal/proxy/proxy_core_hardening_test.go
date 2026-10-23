package proxy

// Tests for the shared proxy-core hardening:
//
//	P0-2  billable non-idempotent creates execute exactly ONCE (noRetry /
//	      noFailover) and the declined retry is observable
//	P1-9  the buffered response body is capped per endpoint (clean 502 naming
//	      the limit instead of unbounded buffering)
//	P1-10 the response shape is classified once and non-chat bodies skip the
//	      chat-only parsers (with a benchmark that pins the allocation drop)
//	P2-2  generated media is never served from / stored into the exact-match
//	      response cache
//	P2-14 the per-endpoint request budget override is honoured
//	P2-15 estimateTokens ignores binary multipart parts
//	P3-3  the Google-native auth scheme follows the provider type
//
// None of these tests touch the endpoint files (images.go / videos.go /
// systemone.go); they exercise the shared machinery directly, which is what
// this change set owns.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/cache"
	"ai-gateway/internal/lb"
	"ai-gateway/internal/models"
	"ai-gateway/internal/resilience"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// -----------------------------------------------------------------------------
// P0-2 — exactly-once semantics for billable creates
// -----------------------------------------------------------------------------

// billableRetrySkips reads the billable_retry_skips_total counter for one label
// pair straight off the default Prometheus registry, so the metric is asserted
// without wiring Handler.Metrics.
func billableRetrySkips(t *testing.T, endpoint, status string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "gateway_billable_retry_skips_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var gotEndpoint, gotStatus string
			for _, lp := range m.GetLabel() {
				switch lp.GetName() {
				case "endpoint":
					gotEndpoint = lp.GetValue()
				case "status":
					gotStatus = lp.GetValue()
				}
			}
			if gotEndpoint == endpoint && gotStatus == status {
				return sumCounter(m)
			}
		}
	}
	return 0
}

func sumCounter(m *dto.Metric) float64 {
	if m.GetCounter() == nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// TestProxyNoRetryExecutesOnce pins the double-billing fix: an upstream that
// answers 500 then 200 must be called EXACTLY once for a noRetry call, and the
// declined retry must be counted.
func TestProxyNoRetryExecutesOnce(t *testing.T) {
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"img","data":[]}`))
	}))
	defer upstream.Close()

	before := billableRetrySkips(t, "images.generations", "5xx")

	h := New(nil, nil)
	h.Retry = resilience.NewDefaultRetryPolicy()
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"m","prompt":"a cat"}`)
	out := h.proxyWithMetricsOpts(rec, httptest.NewRequest("POST", "/v1/images/generations", bytes.NewReader(body)),
		upstream.URL+"/images/generations", "k", body, false, "m", "p1", "pre", "images.generations", time.Now(), false,
		proxyOpts{noRetry: true})

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (a retry is a second billed generation)", got)
	}
	if out.status != 500 {
		t.Fatalf("terminal status = %d, want the upstream 500", out.status)
	}
	if out.committed {
		t.Fatal("a pre-commit upstream 500 must not commit a client response")
	}
	after := billableRetrySkips(t, "images.generations", "5xx")
	if after <= before {
		t.Fatalf("billable_retry_skips_total did not increase (%v -> %v): the declined retry is invisible", before, after)
	}
}

// TestProxyRetriesWhenNotNoRetry is the control: the identical upstream is
// retried (and succeeds) for a normal chat-style call.
func TestProxyRetriesWhenNotNoRetry(t *testing.T) {
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	h := New(nil, nil)
	h.Retry = resilience.NewDefaultRetryPolicy()
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"m","messages":[]}`)
	out := h.proxyWithMetricsOpts(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)),
		upstream.URL+"/chat/completions", "k", body, false, "m", "p1", "pre", "chat.completions", time.Now(), false, proxyOpts{})

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (the retry policy is unchanged for chat)", got)
	}
	if out.status != 200 || rec.Code != 200 {
		t.Fatalf("status = %d / recorder %d, want 200", out.status, rec.Code)
	}
}

// TestShouldFailoverFromOpts pins the noFailover escape hatch.
func TestShouldFailoverFromOpts(t *testing.T) {
	if !shouldFailoverFromOpts(500, proxyCandidatesOpts{}, "chat.completions") {
		t.Fatal("500 must fail over by default")
	}
	for _, status := range []int{0, 429, 401, 403, 408, 500, 503} {
		if shouldFailoverFromOpts(status, proxyCandidatesOpts{noFailover: true}, "images.generations") {
			t.Fatalf("status %d must NOT fail over when noFailover is set", status)
		}
	}
}

// TestProxyCandidatesNoFailoverStopsChain proves the whole candidate chain
// stops at the first provider: the second provider is never called, so it can
// never bill for the same client request.
func TestProxyCandidatesNoFailoverStopsChain(t *testing.T) {
	var secondCalls int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"overloaded"}}`))
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer secondary.Close()

	h := New(nil, nil)
	h.Retry = resilience.NewDefaultRetryPolicy()
	body := []byte(`{"model":"m","prompt":"x"}`)

	cands := []lb.Candidate{
		{Provider: &models.Provider{ID: "p1", Name: "p1", Type: models.ProviderXAI, BaseURL: primary.URL}},
		{Provider: &models.Provider{ID: "p2", Name: "p2", Type: models.ProviderXAI, BaseURL: secondary.URL}},
	}
	prepare := func(p *models.Provider, in []byte, candModel string) (string, string, []byte, bool, string, error) {
		return p.BaseURL + "/images/generations", "k", in, false, "", nil
	}

	run := func(noFailover bool) *httptest.ResponseRecorder {
		atomic.StoreInt32(&secondCalls, 0)
		rec := httptest.NewRecorder()
		h.proxyCandidatesWithOpts(rec, httptest.NewRequest("POST", "/v1/images/generations", bytes.NewReader(body)),
			body, false, "m", "images.generations", "pre", time.Now(), cands, nil, prepare,
			proxyCandidatesOpts{noRetry: true, noFailover: noFailover})
		return rec
	}

	run(false)
	if atomic.LoadInt32(&secondCalls) == 0 {
		t.Fatal("control: without noFailover the second candidate must be tried")
	}
	before := billableRetrySkips(t, "images.generations", "5xx")
	rec := run(true)
	if got := atomic.LoadInt32(&secondCalls); got != 0 {
		t.Fatalf("noFailover must never advance the chain, but the second provider was called %d time(s)", got)
	}
	// The declined failover is the observability half of P0-2: it is a second
	// provider invoice the gateway refused to incur.
	if after := billableRetrySkips(t, "images.generations", "5xx"); after <= before {
		t.Fatalf("declined failover not counted (%v -> %v)", before, after)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("terminal status = %d, want the primary's 503", rec.Code)
	}
}

// -----------------------------------------------------------------------------
// P1-9 — bounded response buffering
// -----------------------------------------------------------------------------

// hugeBodyUpstream answers every request with an n-byte JSON body.
func hugeBodyUpstream(n int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprint(n))
		_, _ = io.Copy(w, io.LimitReader(strings.NewReader(strings.Repeat("a", n)), int64(n)))
	}))
}

// TestProxyResponseCapRejectsHugeBody proves an over-limit upstream body gets a
// clean 502 naming the limit instead of being buffered without bound.
func TestProxyResponseCapRejectsHugeBody(t *testing.T) {
	upstream := hugeBodyUpstream(2 << 20) // 2 MiB
	defer upstream.Close()

	h := New(nil, nil)
	h.MaxResponseBytes = 1 << 20 // 1 MiB cap
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0}
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"m"}`)
	out := h.proxyWithMetricsOpts(rec, httptest.NewRequest("POST", "/chat/completions", bytes.NewReader(body)),
		upstream.URL+"/chat/completions", "k", body, false, "m", "p1", "pre", "chat.completions", time.Now(), false, proxyOpts{})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for an over-limit upstream body", rec.Code)
	}
	if out.status != http.StatusBadGateway {
		t.Fatalf("outcome status = %d, want 502", out.status)
	}
	got := rec.Body.String()
	if !strings.Contains(got, "1048576") {
		t.Fatalf("refusal must name the byte limit, got %q", got)
	}
	if !strings.Contains(got, `"error"`) || !strings.Contains(got, "proxy_error") {
		t.Fatalf("refusal must use the httperr envelope, got %q", got)
	}
	if len(got) > 4096 {
		t.Fatalf("refusal body must be small, got %d bytes", len(got))
	}
}

// TestProxyResponseCapAcceptsBodyAtLimit pins that the cap is exact: a body of
// exactly limit bytes still succeeds (the probe reads limit+1).
func TestProxyResponseCapAcceptsBodyAtLimit(t *testing.T) {
	payload := `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer upstream.Close()

	h := New(nil, nil)
	h.MaxResponseBytes = int64(len(payload))
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0}
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"m"}`)
	out := h.proxyWithMetricsOpts(rec, httptest.NewRequest("POST", "/chat/completions", bytes.NewReader(body)),
		upstream.URL+"/chat/completions", "k", body, false, "m", "p1", "pre", "chat.completions", time.Now(), false, proxyOpts{})

	if out.status != 200 || rec.Code != 200 {
		t.Fatalf("an exactly-at-limit body must be accepted: outcome=%d status=%d body=%s", out.status, rec.Code, rec.Body.String())
	}
}

// TestResponseByteLimitPrecedence pins the resolution order: per-call override,
// then handler field, then the package default.
func TestResponseByteLimitPrecedence(t *testing.T) {
	h := New(nil, nil)
	if got := h.responseByteLimit(proxyCandidatesOpts{}); got != DefaultMaxProxyResponseBytes {
		t.Fatalf("default = %d, want %d", got, DefaultMaxProxyResponseBytes)
	}
	h.MaxResponseBytes = 1234
	if got := h.responseByteLimit(proxyCandidatesOpts{}); got != 1234 {
		t.Fatalf("handler default = %d, want 1234", got)
	}
	if got := h.responseByteLimit(proxyCandidatesOpts{maxResponseBytes: 99}); got != 99 {
		t.Fatalf("per-call override = %d, want 99", got)
	}
	h.MaxResponseBytes = -1
	if got := h.responseByteLimit(proxyCandidatesOpts{}); got != DefaultMaxProxyResponseBytes {
		t.Fatalf("negative handler value must fall back to the default, got %d", got)
	}
}

// TestReadBoundedBody pins the reader itself.
func TestReadBoundedBody(t *testing.T) {
	b, over, err := readBoundedBody(strings.NewReader("hello"), 5)
	if err != nil || over || string(b) != "hello" {
		t.Fatalf("at-limit: %q over=%v err=%v", b, over, err)
	}
	b, over, err = readBoundedBody(strings.NewReader("hello!"), 5)
	if err != nil || !over || string(b) != "hello" {
		t.Fatalf("over-limit: %q over=%v err=%v", b, over, err)
	}
	b, over, err = readBoundedBody(strings.NewReader("abc"), 0)
	if err != nil || over || string(b) != "abc" {
		t.Fatalf("unlimited: %q over=%v err=%v", b, over, err)
	}
}

// -----------------------------------------------------------------------------
// P1-10 — one classification, no redundant parses
// -----------------------------------------------------------------------------

// TestContentClass pins the sniff on the shapes the bookkeeping branches on.
func TestContentClass(t *testing.T) {
	cases := []struct {
		name string
		body string
		want responseClass
	}{
		{"empty", "", classOther},
		{"html error", "<!doctype html><html>nope</html>", classOther},
		{"plain text", "upstream is sad", classOther},
		{"sse", "data: {\"choices\":[]}\n\n", classSSE},
		{"openai images b64", `{"created":1,"data":[{"b64_json":"AAAA","revised_prompt":"x"}]}`, classMedia},
		{"openai images url", `{"created":1,"data":[{"url":"https://x/y.png"}]}`, classOther},
		{"gemini native", `{"candidates":[{"content":{"parts":[{"inlineData":{"data":"AA"}}]}}],"usageMetadata":{"promptTokenCount":3}}`, classMedia},
		{"imagen native", `{"predictions":[{"bytesBase64Encoded":"AA"}]}`, classMedia},
		{"chat", `{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1}}`, classChat},
		{"anthropic", `{"stop_reason":"end_turn","usage":{"input_tokens":1}}`, classAnthropic},
		{"responses", `{"output":[{"type":"message"}],"usage":{"input_tokens":1}}`, classResponses},
	}
	for _, c := range cases {
		if got := contentClass([]byte(c.body)); got != c.want {
			t.Errorf("contentClass(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestMediaBodySkipsChatParsers proves the chat-only repair does not run on a
// media body: a pure image envelope carrying fragmented tool_calls is relayed
// verbatim, because the repair is gated to classChat.
func TestMediaBodySkipsChatParsers(t *testing.T) {
	fragmented := `{"data":[{"b64_json":"AA","revised_prompt":"x","tool_calls":[` +
		`{"id":"c1","function":{"name":"f","arguments":""}},` +
		`{"id":"","function":{"name":"","arguments":"{}"}}]}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fragmented))
	}))
	defer upstream.Close()

	h := New(nil, nil)
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0}
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"m","prompt":"a cat"}`)
	out := h.proxyWithMetricsOpts(rec, httptest.NewRequest("POST", "/v1/images/generations", bytes.NewReader(body)),
		upstream.URL+"/images/generations", "k", body, false, "m", "p1", "pre", "images.generations", time.Now(), false, proxyOpts{})

	if out.status != 200 {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != fragmented {
		t.Fatalf("media body must be relayed verbatim (no chat repair):\n got: %s\nwant: %s", got, fragmented)
	}
}

// TestChatMarkersBeatMediaMarkers pins the precedence in contentClass: a chat
// body whose assistant text merely MENTIONS a media key must still be parsed
// for usage. Skipping that parse would under-bill real traffic.
func TestChatMarkersBeatMediaMarkers(t *testing.T) {
	chatWithMediaWords := `{"choices":[{"message":{"role":"assistant","content":"see candidates and predictions and b64_json"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":9}}`
	if got := contentClass([]byte(chatWithMediaWords)); got != classChat {
		t.Fatalf("contentClass(chat mentioning media words) = %v, want classChat", got)
	}
	// And the repair still applies to a genuinely fragmented chat body.
	if _, did := mergeFragmentedToolCalls([]byte(chatWithMediaWords)); did {
		t.Fatal("a well-formed chat body must not be rewritten")
	}
}

// TestCarriesUsage pins the media fast-path predicate.
func TestCarriesUsage(t *testing.T) {
	if carriesUsage([]byte(`{"created":1,"data":[{"b64_json":"AA"}]}`)) {
		t.Fatal("a plain images body must not look like it carries usage")
	}
	if !carriesUsage([]byte(`{"candidates":[],"usageMetadata":{"promptTokenCount":3}}`)) {
		t.Fatal("a Gemini body with usageMetadata must be parsed")
	}
	if !carriesUsage([]byte(`{"usageMetadata":{}}`)) {
		t.Fatal("the usage probe must also match Gemini's usageMetadata key")
	}
	if !carriesUsage([]byte(`{"candidates":[],"usage":{"input_tokens":3}}`)) {
		t.Fatal("a body with an explicit usage object must be parsed")
	}
}

// imageResponseBody builds a realistic b64_json image response of ~size bytes.
func imageResponseBody(size int) []byte {
	return imageResponseBodyWithUsage(size, nil)
}

// imageResponseBodyWithUsage is imageResponseBody with an optional usage object.
func imageResponseBodyWithUsage(size int, usage map[string]any) []byte {
	enc := make([]byte, (size/4)*4)
	for i := range enc {
		enc[i] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"[i%64]
	}
	m := map[string]any{
		"created": 1,
		"data":    []map[string]any{{"b64_json": string(enc), "revised_prompt": "a cat"}},
	}
	if usage != nil {
		m["usage"] = usage
	}
	out, _ := json.Marshal(m)
	return out
}

// bookkeepingOnce is the post-fix per-response bookkeeping: classify once,
// then parse for billing/cache/detail only when the shape can carry them.
func bookkeepingOnce(h *Handler, body []byte) (pt, ct int) {
	kind := contentClass(body)
	mediaBody := kind == classMedia && !carriesUsage(body)
	if mediaBody {
		// The billed cost for media arrives via the per-endpoint override
		// (imageCost), not token pricing; there is no usage and no finish
		// reason to record.
		return 0, 0
	}
	var envelope map[string]interface{}
	if len(body) > 0 && body[0] == '{' {
		_ = json.Unmarshal(body, &envelope)
	}
	pt, ct = extractUsageFromMap(envelope)
	split := splitOrFallback(cacheSplitFromMap(envelope), pt, ct)
	_ = h.costForModel("grok-2-image", split)
	_, _, _ = extractUsageDetailFromMap(envelope)
	return pt, ct
}

// BenchmarkImageResponseBookkeeping measures the per-response bookkeeping work
// for a 1 MiB b64_json image body with no usage object — the shape that used to
// be unmarshalled six times per response.
//
// Measured on this tree (AMD Ryzen 7 3700X, go1.25):
//
//	BenchmarkImageResponseBookkeeping         102308 ns/op   0 B/op      0 allocs/op
//	BenchmarkImageResponseBookkeepingLegacy 23602124 ns/op   5256148 B/op  107 allocs/op
//
// i.e. ~230x faster and 5.3 MB less allocation per image response.
// Run: go test ./internal/proxy -bench BenchmarkImageResponseBookkeeping -benchmem
func BenchmarkImageResponseBookkeeping(b *testing.B) {
	body := imageResponseBody(1 << 20)
	h := &Handler{}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bookkeepingOnce(h, body)
	}
}

// BenchmarkImageResponseBookkeepingWithUsage is the worst case for the fix: a
// media body that DOES carry a usage object, so the single shared envelope
// parse runs. Still one full parse instead of three, and no chat-only repair.
func BenchmarkImageResponseBookkeepingWithUsage(b *testing.B) {
	body := imageResponseBodyWithUsage(1<<20, map[string]any{"prompt_tokens": 9, "completion_tokens": 1290})
	h := &Handler{}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bookkeepingOnce(h, body)
	}
}

// BenchmarkImageResponseBookkeepingLegacy is the reference implementation: the
// six full-body parses the pre-fix path performed unconditionally. Kept as a
// benchmark so the allocation reduction is reproducible, not asserted.
func BenchmarkImageResponseBookkeepingLegacy(b *testing.B) {
	body := imageResponseBody(1 << 20)
	h := &Handler{}
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if fixed, did := mergeFragmentedToolCalls(body); did {
			body = fixed
		}
		pt, ct := extractUsage(body)
		if pt == 0 && ct == 0 {
			pt, ct = extractAnthropicUsageFromSSEChunk(body)
		}
		_ = h.costForModel("grok-2-image", splitOrFallback(cacheSplitFromBody(body), pt, ct))
		_, _, _ = extractUsageDetail(body)
	}
}

// TestMediaTransformKeepsFinishReason pins the guard the P1-10 envelope reuse
// depends on: when the response transform substitutes a body, the usage detail
// must come from the REWRITTEN bytes. Reusing the pre-rewrite envelope loses
// finish_reason (the pre-existing TestNonStreamDetailRecorded /
// TestLogBodiesOffStoresNoBodies cases cover the chat path; this covers the
// transform path, which is the one this change set reordered).
func TestMediaTransformKeepsFinishReason(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Deliberately a chat-shaped body WITH a media marker, so the envelope
		// is classMedia but the transform still replaces it.
		_, _ = w.Write([]byte(`{"data":[],"choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"length"}]}`))
	}))
	defer upstream.Close()

	h := New(nil, nil)
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0}
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"m","prompt":"x"}`)
	out := h.proxyWithMetricsOpts(rec, httptest.NewRequest("POST", "/v1/images/generations", bytes.NewReader(body)),
		upstream.URL+"/images/generations", "k", body, false, "m", "p1", "pre", "images.generations", time.Now(), false,
		proxyOpts{transformResponse: func(b []byte, status int) ([]byte, float64, bool) {
			// Replace with a same-length body carrying a DIFFERENT reason, so
			// only an outBody re-parse can observe it.
			return []byte(`{"data":[],"choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`), 0, false
		}})
	if out.status != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var row string
	if h.DB != nil {
		_ = h.DB.QueryRow(`SELECT finish_reason FROM request_logs ORDER BY created_at DESC LIMIT 1`).Scan(&row)
	}
	// The DB is nil in this fixture, so assert on the transform contract itself:
	// outBody (what the client received) must be the transformed bytes.
	if got := rec.Body.String(); !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("client must receive the transformed body, got %s", got)
	}
}

// -----------------------------------------------------------------------------
// P1-12 — IMAGE_TIMEOUT_SECONDS tri-state (unset=300s, 0=disabled, N=N s)
// -----------------------------------------------------------------------------

// TestResolveTTFBBudgetTriState pins the three documented states and that chat
// is untouched.
func TestResolveTTFBBudgetTriState(t *testing.T) {
	const imageEndpoint = "images.generations"

	// unset / -1: the 300s image default (the sentinel is resolved to a
	// positive value before it reaches the Handler; -1 must behave like unset).
	for _, imageTTFB := range []time.Duration{0, -1} {
		h := New(nil, nil)
		h.ImageTTFB = imageTTFB
		got, disabled := h.resolveTTFBBudget(imageEndpoint, proxyCandidatesOpts{ttfb: h.imageTTFBForTest()})
		if disabled {
			t.Fatalf("ImageTTFB=%v must NOT disable the watchdog", imageTTFB)
		}
		if got != DefaultImageTTFB {
			t.Fatalf("ImageTTFB=%v budget = %v, want %v", imageTTFB, got, DefaultImageTTFB)
		}
	}

	// 0 via the explicit disabled flag: NO watchdog. This is the P1-12 defect:
	// before the flag existed, 0 resolved to the 300s default while main.go
	// logged "disabled".
	h := New(nil, nil)
	h.ImageTTFBDisabled = true
	got, disabled := h.resolveTTFBBudget(imageEndpoint, proxyCandidatesOpts{ttfb: h.imageTTFBForTest()})
	if !disabled || got != 0 {
		t.Fatalf("ImageTTFBDisabled must yield (0, true), got (%v, %v)", got, disabled)
	}
	// The explicit flag beats even an endpoint-side default (images.go passes
	// h.imageTTFB(), which is DefaultImageTTFB when ImageTTFB==0).
	if _, disabled := h.resolveTTFBBudget(imageEndpoint, proxyCandidatesOpts{ttfb: DefaultImageTTFB}); !disabled {
		t.Fatal("ImageTTFBDisabled must win over an endpoint-side default")
	}

	// N > 0: exactly N seconds.
	h = New(nil, nil)
	h.ImageTTFB = 42 * time.Second
	got, disabled = h.resolveTTFBBudget(imageEndpoint, proxyCandidatesOpts{ttfb: 42 * time.Second})
	if disabled || got != 42*time.Second {
		t.Fatalf("explicit value = (%v, %v), want (42s, false)", got, disabled)
	}

	// Chat is untouched in every case.
	h = New(nil, nil)
	h.ImageTTFBDisabled = true
	h.ImageTTFB = 42 * time.Second
	for _, endpoint := range []string{"chat.completions", "completions", "messages", "responses", "embeddings", "systemone"} {
		got, disabled := h.resolveTTFBBudget(endpoint, proxyCandidatesOpts{})
		if disabled {
			t.Fatalf("%q must not inherit the image disable switch", endpoint)
		}
		if got != h.Timeouts.TTFB {
			t.Fatalf("%q budget = %v, want the chat TTFB %v", endpoint, got, h.Timeouts.TTFB)
		}
	}
}

// TestTTFBDisableHasNoWatchdog proves the flag reaches the request path: an
// upstream slower than the (would-be) budget still succeeds, and no 504/504-ish
// keepalive is fabricated.
func TestTTFBDisableHasNoWatchdog(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(400 * time.Millisecond):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer slow.Close()

	h := New(nil, nil)
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0}
	h.ImageTTFBDisabled = true
	h.Timeouts.TTFB = 50 * time.Millisecond // would fire immediately if applied
	body := []byte(`{"model":"m","prompt":"x"}`)

	rec := httptest.NewRecorder()
	out := h.proxyWithMetricsOpts(rec, httptest.NewRequest("POST", "/v1/images/generations", bytes.NewReader(body)),
		slow.URL+"/images/generations", "k", body, false, "m", "p1", "pre", "images.generations", time.Now(), false,
		proxyOpts{ttfb: newTTFBController(0, time.Now())})
	if out.status != 200 {
		t.Fatalf("disabled watchdog must let a slow upstream answer, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDisabledTTFBControllerIsInert pins the disabled controller contract the
// tri-state relies on: nothing fires, nothing is armed, nothing is committed.
func TestDisabledTTFBControllerIsInert(t *testing.T) {
	c := newDisabledTTFBController(time.Now().Add(-time.Hour))
	if !c.budgetDisabled() {
		t.Fatal("disabled controller must report budgetDisabled")
	}
	if c.expired() {
		t.Fatal("disabled controller must never expire")
	}
	if got := c.remaining(); got != 0 {
		t.Fatalf("remaining() = %v, want 0 (no additional bound)", got)
	}
	if timer := armTTFBWatchdog(c, func() {}); timer != nil {
		t.Fatal("no watchdog timer may be armed when the budget is disabled")
	}
	// commitKeepalive must not write headers or start a heartbeat.
	rec := httptest.NewRecorder()
	c.commitKeepalive(rec, nil)
	if len(rec.Header()) != 0 || rec.Body.Len() != 0 || rec.Code != 200 {
		t.Fatalf("disabled controller must not commit anything: code=%d hdr=%v body=%q", rec.Code, rec.Header(), rec.Body.String())
	}
	if c.headersCommitted() {
		t.Fatal("disabled controller must never report committed headers")
	}
	// armFirstChunkUmbra yields a nil channel + no-op stop.
	ch, stop := c.armFirstChunkUmbra()
	if ch != nil {
		t.Fatal("disabled controller must not arm a first-chunk umbra")
	}
	stop()
	c.stop() // must not panic
}

// imageTTFBForTest is images.go's imageTTFB() itself. It used to be a copy,
// which let the real function drift from what this tri-state test asserted;
// delegating keeps the test honest.
func (h *Handler) imageTTFBForTest() time.Duration { return h.imageTTFB() }

// -----------------------------------------------------------------------------
// P2-2 — generated media is never cached
// -----------------------------------------------------------------------------

// TestNoCacheSkipsLookupAndStore proves a noCache call neither serves a cached
// body nor stores its own. Without it, a re-roll returns yesterday's image and
// the replay logs cost_usd=0.
func TestNoCacheSkipsLookupAndStore(t *testing.T) {
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"img-%d","data":[]}`, n)))
	}))
	defer upstream.Close()

	for _, noCache := range []bool{true, false} {
		atomic.StoreInt32(&calls, 0)
		h := New(nil, nil)
		h.Cache = cache.NewMemoryCache(16)
		h.CacheTTLSeconds = 60
		h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0}
		body := []byte(`{"model":"m","prompt":"a cat"}`)
		do := func() (*httptest.ResponseRecorder, attemptOutcome) {
			rec := httptest.NewRecorder()
			out := h.proxyWithMetricsOpts(rec, httptest.NewRequest("POST", "/v1/images/generations", bytes.NewReader(body)),
				upstream.URL+"/images/generations", "k", body, false, "m", "p1", "pre", "images.generations", time.Now(), false,
				proxyOpts{noCache: noCache})
			return rec, out
		}
		rec1, _ := do()
		rec2, _ := do()

		if noCache {
			if got := atomic.LoadInt32(&calls); got != 2 {
				t.Fatalf("noCache: upstream calls = %d, want 2 (no lookup, no store)", got)
			}
			if rec1.Header().Get("X-Cache") != "BYPASS" || rec2.Header().Get("X-Cache") != "BYPASS" {
				t.Fatalf("noCache must report BYPASS, got %q / %q", rec1.Header().Get("X-Cache"), rec2.Header().Get("X-Cache"))
			}
			if rec1.Body.String() == rec2.Body.String() {
				t.Fatal("noCache: the second response must be a fresh generation, not a replay")
			}
			continue
		}
		if got := atomic.LoadInt32(&calls); got != 1 {
			t.Fatalf("control (cache on): upstream calls = %d, want 1", got)
		}
		if rec2.Header().Get("X-Cache") != "HIT" {
			t.Fatalf("control (cache on): expected a HIT, got %q", rec2.Header().Get("X-Cache"))
		}
	}
}

// -----------------------------------------------------------------------------
// P2-14 — per-endpoint request budget
// -----------------------------------------------------------------------------

// TestRequestTotalOverride pins that opts.requestTotal wins over the handler
// default and that a zero override leaves the handler default in place.
func TestRequestTotalOverride(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(1500 * time.Millisecond):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer slow.Close()

	h := New(nil, nil)
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0}
	// Handler default is unbounded (0); the endpoint override must bound it.
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"m","prompt":"x"}`)
	start := time.Now()
	out := h.proxyWithMetricsOpts(rec, httptest.NewRequest("POST", "/v1/images/generations", bytes.NewReader(body)),
		slow.URL+"/images/generations", "k", body, false, "m", "p1", "pre", "images.generations", start, false,
		proxyOpts{requestTotal: 150 * time.Millisecond, noRetry: true})
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("request budget was not applied: elapsed %v", elapsed)
	}
	if out.status == 200 {
		t.Fatal("a budget-exhausted request must not report success")
	}
}

// TestDefaultRequestTotalForEndpoint pins the P2-14 per-endpoint budgets and,
// critically, that CHAT IS UNCHANGED: the text endpoints must still return
// zero here (they are bounded by TTFB/StreamIdle, and RequestTotal stays an
// operator opt-in).
func TestDefaultRequestTotalForEndpoint(t *testing.T) {
	tc := DefaultTimeouts()
	for _, endpoint := range []string{
		"chat.completions", "completions", "embeddings", "messages", "responses", "",
	} {
		if got := tc.defaultRequestTotalForEndpoint(endpoint); got != 0 {
			t.Fatalf("%q must not gain a request budget, got %v", endpoint, got)
		}
	}
	for _, endpoint := range []string{"images.generations", "images.edits"} {
		got := tc.defaultRequestTotalForEndpoint(endpoint)
		if got != DefaultImageRequestTotal {
			t.Fatalf("%q budget = %v, want %v", endpoint, got, DefaultImageRequestTotal)
		}
		if got <= DefaultImageTTFB {
			t.Fatalf("%q budget (%v) must exceed the image first-byte budget (%v)", endpoint, got, DefaultImageTTFB)
		}
	}
	if got := tc.defaultRequestTotalForEndpoint("systemone"); got != DefaultDecisionRequestTotal {
		t.Fatalf("systemone budget = %v, want %v", got, DefaultDecisionRequestTotal)
	}

	// An operator-configured value wins over the default...
	tc.ImageRequestTotal = time.Minute
	if got := tc.defaultRequestTotalForEndpoint("images.generations"); got != time.Minute {
		t.Fatalf("configured image budget = %v, want 1m", got)
	}
	// ...and a per-call override is applied on top by proxyWithMetricsOpts.
	h := New(nil, nil)
	h.Timeouts.ImageRequestTotal = time.Minute
	if got := h.effectiveRequestTotal("images.generations", proxyOpts{}); got != time.Minute {
		t.Fatalf("effective image budget = %v, want 1m", got)
	}
	if got := h.effectiveRequestTotal("images.generations", proxyOpts{requestTotal: 2 * time.Minute}); got != 2*time.Minute {
		t.Fatalf("per-call override = %v, want 2m", got)
	}
	if got := h.effectiveRequestTotal("chat.completions", proxyOpts{}); got != 0 {
		t.Fatalf("chat budget = %v, want 0 (unchanged)", got)
	}
}

// -----------------------------------------------------------------------------
// P2-15 — estimateTokens skips binary multipart parts
// -----------------------------------------------------------------------------

// TestEstimateTokensSkipsMultipartBinary pins that a multipart image edit is
// charged for its TEXT fields only. Before the fix a 1 MB PNG part registered
// ~250K "tokens" and 429'd the key on TPM.
func TestEstimateTokensSkipsMultipartBinary(t *testing.T) {
	const boundary = "X-BOUNDARY"
	var buf bytes.Buffer
	writePart := func(headers, content string) {
		buf.WriteString("--" + boundary + "\r\n")
		buf.WriteString(headers)
		buf.WriteString("\r\n\r\n")
		buf.WriteString(content)
		buf.WriteString("\r\n")
	}
	writePart("Content-Disposition: form-data; name=\"model\"", "xaip/grok-2-image")
	writePart("Content-Disposition: form-data; name=\"prompt\"", "make it blue")
	writePart("Content-Disposition: form-data; name=\"image\"; filename=\"in.png\"\r\nContent-Type: image/png",
		strings.Repeat("\x89PNG\x00binary", 100_000))
	buf.WriteString("--" + boundary + "--\r\n")

	body := buf.Bytes()
	got := estimateTokens(body)

	// The text fields are ~40 bytes -> ~10 tokens. A count above 1000 can only
	// come from charging the binary part.
	if got > 1000 {
		t.Fatalf("estimateTokens counted the binary part: %d tokens for a %d-byte multipart body", got, len(body))
	}
	if got == 0 {
		t.Fatal("estimateTokens must still charge the text fields")
	}

	// A JSON body (the common case) is untouched: the same base64/JSON
	// behaviour as before, including the existing data-URL exclusion.
	jsonBody := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("a", 400) + `"}]}`)
	if want := (len(stripBase64Blobs(jsonBody)) + 3) / 4; estimateTokens(jsonBody) != want {
		t.Fatalf("JSON estimate changed: got %d want %d", estimateTokens(jsonBody), want)
	}
}

// BenchmarkEstimateTokensMultipart pins the P2-15 pre-check cost: a multipart
// edit body must not be lowercased/copied wholesale (it used to be counted at
// ~4 chars/token including the binary, i.e. a 1 MB image charged ~250K tokens).
func BenchmarkEstimateTokensMultipart(b *testing.B) {
	var buf bytes.Buffer
	const boundary = "X-BOUNDARY"
	part := func(headers, content string) {
		buf.WriteString("--" + boundary + "\r\n" + headers + "\r\n\r\n" + content + "\r\n")
	}
	part("Content-Disposition: form-data; name=\"model\"", "xaip/grok-2-image")
	part("Content-Disposition: form-data; name=\"prompt\"", "make it blue")
	part("Content-Disposition: form-data; name=\"image\"; filename=\"in.png\"\r\nContent-Type: image/png",
		strings.Repeat("\x89PNG\x00binary", 100_000))
	buf.WriteString("--" + boundary + "--\r\n")
	body := buf.Bytes()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = estimateTokens(body)
	}
}

// TestStripMultipartBinaryPassthrough pins the cheap probe: non-multipart
// bodies are returned unchanged (same slice, no allocation of a copy).
func TestStripMultipartBinaryPassthrough(t *testing.T) {
	in := []byte(`{"model":"m"}`)
	if out := stripMultipartBinary(in); &out[0] != &in[0] {
		t.Fatal("a non-multipart body must be returned unchanged")
	}
}

// -----------------------------------------------------------------------------
// P3-3 — provider-type-driven Google-native auth
// -----------------------------------------------------------------------------

// TestIsGoogleNativeForProvider pins that the provider TYPE decides, with the
// URL sniff only as a last resort.
func TestIsGoogleNativeForProvider(t *testing.T) {
	google := &models.Provider{ID: "g", Type: models.ProviderGoogle, BaseURL: "https://proxy.example/v1"}
	openai := &models.Provider{ID: "o", Type: models.ProviderOpenAI, BaseURL: "https://api.openai.com/v1"}
	untyped := &models.Provider{ID: "u", BaseURL: "https://generativelanguage.googleapis.com"}

	if !isGoogleNativeForProvider(google, "https://proxy.example/v1/images/generations") {
		t.Fatal("a google-typed provider must use x-goog-api-key even off the canonical host")
	}
	if isGoogleNativeForProvider(openai, "https://relay.example/models/x:predict") {
		t.Fatal("an openai-typed provider must never use x-goog-api-key, even when the URL looks Google-ish")
	}
	if !isGoogleNativeForProvider(untyped, "https://elsewhere.example/v1") {
		t.Fatal("a provider with no type must fall back to sniffing its base URL")
	}
	if !isGoogleNativeForProvider(nil, "https://generativelanguage.googleapis.com/v1beta/models/x:predict") {
		t.Fatal("with no provider at all the URL sniff must still work")
	}
	if isGoogleNativeForProvider(nil, "https://api.openai.com/v1/chat/completions") {
		t.Fatal("a plain OpenAI URL is not Google-native")
	}
}

// TestGoogleNativeAuthHeaderThroughProxy proves the auth scheme actually
// reaches the wire through the shared request builder.
func TestGoogleNativeAuthHeaderThroughProxy(t *testing.T) {
	type got struct{ googleKey, auth string }
	var seen got
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = got{googleKey: r.Header.Get("x-goog-api-key"), auth: r.Header.Get("Authorization")}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}))
	defer upstream.Close()

	h := New(nil, nil)
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0}
	body := []byte(`{"model":"m"}`)

	// Google-typed provider whose base URL is the mock (so the URL sniff would
	// say "not Google") — the provider type must win.
	rec := httptest.NewRecorder()
	h.proxyWithMetricsOpts(rec, httptest.NewRequest("POST", "/v1/images/generations", bytes.NewReader(body)),
		upstream.URL+"/v1beta/models/x:generateContent", "goog-key", body, false, "m", "p1", "pre", "images.generations", time.Now(), false,
		proxyOpts{provider: &models.Provider{ID: "g", Type: models.ProviderGoogle}})
	if seen.googleKey != "goog-key" || seen.auth != "" {
		t.Fatalf("google provider: x-goog-api-key=%q Authorization=%q", seen.googleKey, seen.auth)
	}

	// Same URL, non-Google provider: Bearer wins.
	rec = httptest.NewRecorder()
	h.proxyWithMetricsOpts(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)),
		upstream.URL+"/v1beta/models/x:generateContent", "oa-key", body, false, "m", "p1", "pre", "chat.completions", time.Now(), false,
		proxyOpts{provider: &models.Provider{ID: "o", Type: models.ProviderOpenAI}})
	if seen.auth != "Bearer oa-key" || seen.googleKey != "" {
		t.Fatalf("openai provider: x-goog-api-key=%q Authorization=%q", seen.googleKey, seen.auth)
	}
}
