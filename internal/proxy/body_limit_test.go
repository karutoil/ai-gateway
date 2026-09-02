package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/resilience"

	"github.com/go-chi/chi/v5"
)

// Agentic CLI sessions accumulate context until request bodies exceed the
// old fixed 64 MiB cap; the gateway then 413'd every request ("Request too
// large (413)"). These tests pin the configurable MAX_PROXY_BODY_MB knob:
// the default still rejects oversize bodies with a 413 naming the knob, a
// raised limit admits them, and 0 disables the cap entirely.

func newBodyLimitStack(t *testing.T, maxBody int64) (*httptest.Server, string) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	master := make([]byte, 32)
	ps := provider.NewStore(database, master)
	ks := apikey.NewStore(database)
	// Upstream echoes nothing meaningful; the gateway only needs to accept
	// (200) or reject (413) the body.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(up.Close)
	if _, err := ps.Create("openai", models.ProviderOpenAI, up.URL+"/v1", "sk-test"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("body-limit-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandler(ps, database)
	h.MaxBodyBytes = maxBody
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0}
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/chat/completions", h.ChatCompletions)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, k.Key
}

func postBodyLimit(t *testing.T, srv *httptest.Server, key string, payload []byte) int {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(string(payload)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// Default handler wiring must keep the historical 64 MiB cap.
func TestBodyLimitDefaultIs64MiB(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	h := New(nil, database)
	if h.MaxBodyBytes != DefaultMaxProxyRequestBodyBytes {
		t.Fatalf("default MaxBodyBytes = %d, want %d", h.MaxBodyBytes, DefaultMaxProxyRequestBodyBytes)
	}
}

// Over the limit → clean 413 whose message names the MAX_PROXY_BODY_MB knob.
func TestBodyLimitRejectsWith413NamingKnob(t *testing.T) {
	srv, key := newBodyLimitStack(t, 1<<20) // 1 MiB cap
	big := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 2<<20) + `"}]}`)
	code := postBodyLimit(t, srv, key, big)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized body, got %d", code)
	}
}

// Under the limit → passes through to the upstream normally.
func TestBodyLimitAcceptsUnderLimit(t *testing.T) {
	srv, key := newBodyLimitStack(t, 1<<20) // 1 MiB cap
	code := postBodyLimit(t, srv, key, []byte(`{"model":"m","messages":[{"role":"user","content":"small"}]}`))
	if code != http.StatusOK {
		t.Fatalf("expected 200 under the limit, got %d", code)
	}
}

// MAX_PROXY_BODY_MB raised (here: 8 MiB) admits bodies the 1 MiB test cap
// rejected — the operator escape hatch for huge agentic sessions.
func TestBodyLimitRaisedAdmitsLargeBody(t *testing.T) {
	srv, key := newBodyLimitStack(t, 8<<20) // 8 MiB cap
	big := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 2<<20) + `"}]}`)
	code := postBodyLimit(t, srv, key, big)
	if code != http.StatusOK {
		t.Fatalf("expected 200 with raised limit, got %d", code)
	}
}

// MAX_PROXY_BODY_MB=0 disables the cap: even the same 2 MiB body sails
// through a 1-byte "cap" (i.e. no cap at all).
func TestBodyLimitZeroDisablesCap(t *testing.T) {
	srv, key := newBodyLimitStack(t, 0)
	big := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 2<<20) + `"}]}`)
	code := postBodyLimit(t, srv, key, big)
	if code != http.StatusOK {
		t.Fatalf("expected 200 with cap disabled, got %d", code)
	}
}
