package proxy

// Tests for the proxy media/endpoint handler hardening fixes:
//   1. Devin auth (GetUserJWT) endpoint fallback when a custom mirror is down.
//   2. Devin buffered collection surfaces upstream read/parse errors as 502
//      instead of a silent empty-200 completion.
//   3. Devin streaming read failures terminate in-band and log a 502 row
//      instead of ending as a "successful" truncated stream.
//   4. Antigravity in-band upstream error frames (data: {"error":...} under a
//      200 status) fail the request instead of being silently dropped.
//   5. Antigravity scanner/read failures log a 502 row and terminate in-band.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/antigravity"
	"ai-gateway/internal/db"
	"ai-gateway/internal/devin"
	"ai-gateway/internal/models"
	"ai-gateway/internal/oauth"
	_ "ai-gateway/internal/oauth/providers"
	"ai-gateway/internal/provider"
)

// ---- unit level ----

func TestCollectDevinDeltasReadErrorSurfaces(t *testing.T) {
	deltas, errStr := collectDevinDeltas(errReader{errors.New("boom")})
	if len(deltas) != 0 {
		t.Fatalf("deltas = %d want 0", len(deltas))
	}
	if errStr == "" {
		t.Fatalf("read error must surface as an error string, got empty")
	}
	if !strings.Contains(errStr, "upstream read failed") {
		t.Fatalf("errStr = %q", errStr)
	}
}

func TestCollectDevinDeltasMalformedFrameSurfaces(t *testing.T) {
	// A frame header declaring an oversized payload is a decode error.
	bad := []byte{0x00, 0xFF, 0xFF, 0xFF, 0xFF}
	deltas, errStr := collectDevinDeltas(strings.NewReader(string(bad)))
	if len(deltas) != 0 {
		t.Fatalf("deltas = %d want 0", len(deltas))
	}
	if errStr == "" || !strings.Contains(errStr, "malformed upstream frame") {
		t.Fatalf("parse error must surface, got %q", errStr)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestFirstAntigravityStreamError(t *testing.T) {
	chunks := []antigravity.ParsedChunk{
		{Texts: []string{"partial"}, HasData: true},
		{Finish: "error:quota exhausted", HasData: true},
	}
	if got := firstAntigravityStreamError(chunks); got != "quota exhausted" {
		t.Fatalf("got %q", got)
	}
	if got := firstAntigravityStreamError([]antigravity.ParsedChunk{{Finish: "STOP"}}); got != "" {
		t.Fatalf("clean finish must not be an error, got %q", got)
	}
}

// ---- devin end-to-end ----

// devinUpstreamAbort serves a valid GetUserJwt, then aborts the chat
// connection mid-body after (optionally) writing payload bytes.
func devinUpstreamAbort(t *testing.T, prefix []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/GetUserJwt"):
			w.Header().Set("Content-Type", "application/proto")
			_, _ = w.Write([]byte{0x0A, 0x0C, 'u', 's', 'e', 'r', '-', 'j', 'w', 't', '-', '1', '2', '3'})
		case strings.HasSuffix(r.URL.Path, "/GetChatMessage"):
			w.Header().Set("Content-Type", "application/connect+proto")
			if len(prefix) > 0 {
				_, _ = w.Write(prefix)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			panic(http.ErrAbortHandler)
		default:
			http.NotFound(w, r)
		}
	}
}

// An upstream that dies mid-body must yield a 502 (not an empty 200) and a
// 502 row in request history.
func TestProxyDevinNonStreamUpstreamAbortIs502(t *testing.T) {
	// Commit a partial frame header first so Do returns 200 and the failure
	// surfaces as a mid-body read error (not a connect error that would fall
	// through to the built-in host).
	partial := devin.FrameConnect([]byte{0x0A, 0x06})[:4]
	h, p := testDevinHandlerWithUpstream(t, devinUpstreamAbort(t, partial))
	chatBody := []byte(`{"model":"devin/swe-1-7","messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()
	h.proxyDevin(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), chatBody, false, "devin/swe-1-7", "chat.completions", "test", time.Now(), p)
	resp := w.Result()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if body := w.Body.String(); !strings.Contains(body, "Devin stream error") {
		t.Fatalf("body = %q", body)
	}
	assertRequestLog502(t, h, p, 1)
}

// A streaming upstream that dies mid-body must terminate in-band (so clients
// do not hang) and log a 502 row.
func TestProxyDevinStreamUpstreamAbortLogs502(t *testing.T) {
	payload := []byte{
		0x0A, 0x06, 'r', 'e', 's', 'p', '-', '1',
		0x1A, 0x05, 'H', 'e', 'l', 'l', 'o',
	}
	h, p := testDevinHandlerWithUpstream(t, devinUpstreamAbort(t, devin.FrameConnect(payload)))
	chatBody := []byte(`{"model":"devin/swe-1-7","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	w := httptest.NewRecorder()
	h.proxyDevin(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), chatBody, true, "devin/swe-1-7", "chat.completions", "test", time.Now(), p)
	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Fatalf("headers already flowed: status = %d, want 200", resp.StatusCode)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Hello") {
		t.Fatalf("pre-abort content must still be delivered: %q", body)
	}
	if !strings.Contains(body, "upstream connection failed") {
		t.Fatalf("in-band terminator missing: %q", body)
	}
	assertRequestLog502(t, h, p, 1)
}

// A dead custom mirror must not fail the whole request: auth and chat fall
// back to the built-in host (DEVIN_HOST here).
func TestProxyDevinAuthFallsBackWhenMirrorDown(t *testing.T) {
	srv := mockDevinServer(t)
	t.Setenv("DEVIN_HOST", srv.URL)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("dead mirror must not be reached for successful fallback")
	}))
	dead.Close()

	h, p := testDevinHandlerWithBaseURL(t, dead.URL)
	chatBody := []byte(`{"model":"devin/swe-1-7","messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()
	h.proxyDevin(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), chatBody, false, "devin/swe-1-7", "chat.completions", "test", time.Now(), p)
	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 via fallback host", resp.StatusCode)
	}
	if !strings.Contains(w.Body.String(), "Hello") {
		t.Fatalf("body = %q", w.Body.String())
	}
}

// ---- antigravity end-to-end ----

func antigravityErrorSSE(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "streamGenerateContent") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}}\n\n"))
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"quota exhausted for this account\"}}\n\n"))
	}
}

// An in-band upstream error under a 200 status must fail a non-stream
// request with 502 instead of returning truncated content as success.
func TestProxyAntigravityInBandErrorNonStream(t *testing.T) {
	h, p, _ := testAntigravityHandler(t, antigravityErrorSSE(t))
	chatBody := []byte(`{"model":"antigravity/gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()
	h.proxyAntigravity(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), chatBody, chatBody, false, "antigravity/gemini-3.8-flash", "chat.completions", "test", time.Now(), p)
	resp := w.Result()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(w.Body.String(), "quota exhausted") {
		t.Fatalf("body = %q", w.Body.String())
	}
	assertRequestLog502(t, h, p, 1)
}

// Streaming: headers already flowed, so the error must arrive in-band plus a
// 502 row in request history.
func TestProxyAntigravityInBandErrorStream(t *testing.T) {
	h, p, _ := testAntigravityHandler(t, antigravityErrorSSE(t))
	chatBody := []byte(`{"model":"antigravity/gemini-3.8-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	w := httptest.NewRecorder()
	h.proxyAntigravity(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), chatBody, chatBody, true, "antigravity/gemini-3.8-flash", "chat.completions", "test", time.Now(), p)
	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Fatalf("headers already flowed: status = %d, want 200", resp.StatusCode)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Antigravity stream error") || !strings.Contains(body, "quota exhausted") {
		t.Fatalf("in-band error missing: %q", body)
	}
	assertRequestLog502(t, h, p, 1)
}

// The messages (Anthropic) dialect must also fail instead of truncating.
func TestProxyAntigravityInBandErrorMessages(t *testing.T) {
	h, p, _ := testAntigravityHandler(t, antigravityErrorSSE(t))
	chatBody := []byte(`{"model":"antigravity/claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()
	h.proxyAntigravity(w, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), chatBody, chatBody, false, "antigravity/claude-sonnet-4-6", "messages", "test", time.Now(), p)
	resp := w.Result()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	assertRequestLog502(t, h, p, 1)
}

// An upstream that dies mid-body on a streaming request must terminate
// in-band and log a 502 row.
func TestProxyAntigravityStreamAbortLogs502(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "streamGenerateContent") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hel\"}]}}]}}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}
	h, p, _ := testAntigravityHandler(t, upstream)
	chatBody := []byte(`{"model":"antigravity/gemini-3.8-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	w := httptest.NewRecorder()
	h.proxyAntigravity(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), chatBody, chatBody, true, "antigravity/gemini-3.8-flash", "chat.completions", "test", time.Now(), p)
	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Fatalf("headers already flowed: status = %d, want 200", resp.StatusCode)
	}
	if body := w.Body.String(); !strings.Contains(body, "upstream connection failed") {
		t.Fatalf("in-band terminator missing: %q", body)
	}
	assertRequestLog502(t, h, p, 1)
}

// Non-stream read failures must be logged in request history (previously a
// bare 502 with no request_logs row).
func TestProxyAntigravityNonStreamReadFailureIsLogged(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "streamGenerateContent") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hel\"}]}]}}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}
	h, p, _ := testAntigravityHandler(t, upstream)
	chatBody := []byte(`{"model":"antigravity/gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()
	h.proxyAntigravity(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), chatBody, chatBody, false, "antigravity/gemini-3.8-flash", "chat.completions", "test", time.Now(), p)
	if w.Result().StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Result().StatusCode)
	}
	assertRequestLog502(t, h, p, 1)
}

// ---- helpers ----

func assertRequestLog502(t *testing.T, h *Handler, p *models.Provider, want int) {
	t.Helper()
	if h.DB == nil {
		t.Fatalf("handler has no DB")
	}
	var n int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM request_logs WHERE provider_id=? AND status=?`, p.ID, 502).Scan(&n); err != nil {
		t.Fatalf("count logs: %v", err)
	}
	if n != want {
		t.Fatalf("request_logs 502 rows = %d, want %d", n, want)
	}
}

// testDevinHandlerWithUpstream builds a Devin provider wired to a custom
// upstream handler.
func testDevinHandlerWithUpstream(t *testing.T, upstream http.HandlerFunc) (*Handler, *models.Provider) {
	t.Helper()
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	return testDevinHandlerWithBaseURL(t, srv.URL)
}

func testDevinHandlerWithBaseURL(t *testing.T, baseURL string) (*Handler, *models.Provider) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i + 11)
	}
	ps := provider.NewStore(database, mk)
	p, err := ps.CreateWithOrg("devin", models.ProviderDevin, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, _ = database.Exec(`UPDATE providers SET base_url=? WHERE id=?`, baseURL, p.ID)
	full, _ := ps.GetByID(p.ID)
	tok := &oauth.Tokens{Access: "sess-123", Refresh: "sess-123", ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli()}
	if err := ps.SetOAuthTokens(full.ID, "devin", tok); err != nil {
		t.Fatalf("set tokens: %v", err)
	}
	full, _ = ps.GetByID(p.ID)
	return New(ps, database), full
}
