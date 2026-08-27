package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/cache"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/resilience"

	"github.com/go-chi/chi/v5"
)

// Regression B15: cached completions must be scoped per gateway key (and org).
// Previously the cache key was endpoint+model+body, so two keys asking for the
// identical payload received each other's completions (cross-tenant HIT,
// skewed cost attribution). Key A primes; key B must MISS.
func TestCompletionCacheScopedPerKey(t *testing.T) {
	var upstreamHits atomic.Int32
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 32)
	ps := provider.NewStore(database, master)
	ks := apikey.NewStore(database)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"cmpl-x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"resp"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()
	if _, err := ps.Create("openai", models.ProviderOpenAI, up.URL+"/v1", "sk-test"); err != nil {
		t.Fatal(err)
	}
	keyA, _ := ks.Create("key-a")
	keyB, _ := ks.Create("key-b")

	h := New(ps, database)
	h.Cache = cache.NewMemoryCache(16)
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0}
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/chat/completions", h.ChatCompletions)
	srv := httptest.NewServer(r)
	defer srv.Close()

	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"identical"}]}`
	do := func(key string) (int, string, http.Header) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 8192)
		n, _ := resp.Body.Read(buf)
		resp.Body.Close()
		return resp.StatusCode, string(buf[:n]), resp.Header
	}

	codeA1, bodyA1, hdrA1 := do(keyA.Key)
	if codeA1 != 200 || !strings.Contains(bodyA1, "resp") {
		t.Fatalf("priming request failed: %d %s", codeA1, bodyA1)
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("upstream hits after prime = %d", upstreamHits.Load())
	}
	if hdrA1.Get("X-Cache") == "HIT" {
		t.Fatalf("prime request must MISS")
	}
	codeA2, bodyA2, hdrA2 := do(keyA.Key)
	if codeA2 != 200 || !strings.Contains(bodyA2, "resp") || hdrA2.Get("X-Cache") != "HIT" {
		t.Fatalf("same key should get cache HIT: %d %s %q", codeA2, bodyA2, hdrA2.Get("X-Cache"))
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("upstream should still be 1 hit, got %d", upstreamHits.Load())
	}
	codeB1, bodyB1, _ := do(keyB.Key)
	if upstreamHits.Load() != 2 {
		t.Fatalf("different key MUST bypass cache (got hit count %d)", upstreamHits.Load())
	}
	if codeB1 != 200 || !strings.Contains(bodyB1, "resp") {
		t.Fatalf("cross-key response malformed: %d %s", codeB1, bodyB1)
	}
}
