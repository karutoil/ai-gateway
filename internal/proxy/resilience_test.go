package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/cache"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/resilience"

	"github.com/go-chi/chi/v5"
)

func setupResilienceServer(t *testing.T, upstream http.HandlerFunc) (*httptest.Server, *Handler, string, *httptest.Server) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 32)
	ps := provider.NewStore(database, master)
	ks := apikey.NewStore(database)
	up := httptest.NewServer(upstream)
	if _, err := ps.Create("openai", models.ProviderOpenAI, up.URL+"/v1", "sk-test"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("test-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandler(ps, database)
	h.Cache = cache.NewMemoryCache(32)
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 2, BaseDelay: time.Millisecond}
	h.Breaker = resilience.NewMemoryCircuitBreaker(5, time.Minute, 30*time.Second)
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/chat/completions", h.ChatCompletions)
	r.Get("/v1/models", h.Models)
	srv := httptest.NewServer(r)
	t.Cleanup(func() { srv.Close(); up.Close() })
	return srv, h, k.Key, up
}

func TestRetryOn500ThenSuccess(t *testing.T) {
	var hits atomic.Int32
	srv, _, key, _ := setupResilienceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "chat/completions") {
			w.WriteHeader(404)
			return
		}
		n := hits.Add(1)
		if n < 3 {
			w.WriteHeader(500)
			w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "chatcmpl-retry",
			"choices": []map[string]interface{}{{"message": map[string]string{"role": "assistant", "content": "recovered"}}},
		})
	})
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 after retries got %d %s", resp.StatusCode, string(b))
	}
	if !strings.Contains(string(b), "recovered") {
		t.Fatalf("body %s", string(b))
	}
	if hits.Load() != 3 {
		t.Fatalf("expected 3 upstream hits, got %d", hits.Load())
	}
}

func TestCacheHitOnRepeatPost(t *testing.T) {
	var hits atomic.Int32
	srv, _, key, _ := setupResilienceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "chat/completions") {
			w.WriteHeader(404)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "chatcmpl-cache",
			"choices": []map[string]interface{}{{"message": map[string]string{"role": "assistant", "content": "cached-ok"}}},
		})
	})
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello cache"}]}`
	do := func() *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	r1 := do()
	b1, _ := io.ReadAll(r1.Body)
	r1.Body.Close()
	if r1.StatusCode != 200 {
		t.Fatalf("first %d %s", r1.StatusCode, string(b1))
	}
	if r1.Header.Get("X-Cache") == "HIT" {
		t.Fatal("first response should miss")
	}
	r2 := do()
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.Header.Get("X-Cache") != "HIT" {
		t.Fatalf("expected X-Cache HIT, got %q body %s", r2.Header.Get("X-Cache"), string(b2))
	}
	if !strings.Contains(string(b2), "cached-ok") {
		t.Fatalf("cached body %s", string(b2))
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream should be hit once, got %d", hits.Load())
	}
}

func TestCircuitOpenSkipsUpstream(t *testing.T) {
	var hits atomic.Int32
	srv, h, key, _ := setupResilienceServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(500)
		w.Write([]byte(`{"error":{"message":"down"}}`))
	})
	h.Breaker = resilience.NewMemoryCircuitBreaker(1, time.Minute, 30*time.Second)
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0, BaseDelay: time.Millisecond}

	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}]}`
	do := func() *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	r1 := do()
	io.ReadAll(r1.Body)
	r1.Body.Close()
	// Upstream returns 500; retries are disabled (MaxRetries 0) and there is
	// no second candidate, so the failure surfaces honestly as a passthrough
	// status rather than an implicit success.
	if r1.StatusCode != 500 && r1.StatusCode != http.StatusBadGateway {
		t.Fatalf("first expected 500/502 got %d", r1.StatusCode)
	}
	r2 := do()
	b, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 circuit_open got %d %s", r2.StatusCode, string(b))
	}
	if !strings.Contains(string(b), "circuit_open") {
		t.Fatalf("expected circuit_open body %s", string(b))
	}
	if hits.Load() != 1 {
		t.Fatalf("second request must not hit upstream, hits=%d", hits.Load())
	}
}

func TestModelsCacheHeader(t *testing.T) {
	srv, _, key, _ := setupResilienceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "models") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4o-mini","object":"model","owned_by":"openai"}]}`))
			return
		}
		w.WriteHeader(404)
	})
	do := func() *http.Response {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	r1 := do()
	io.ReadAll(r1.Body)
	r1.Body.Close()
	if r1.StatusCode != 200 {
		t.Fatalf("first models %d", r1.StatusCode)
	}
	r2 := do()
	io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.Header.Get("X-Cache") != "HIT" {
		t.Fatalf("expected models X-Cache HIT, got %q", r2.Header.Get("X-Cache"))
	}
}

func TestStreamRetriesPreCommitThenFails(t *testing.T) {
	var hits atomic.Int32
	srv, _, key, _ := setupResilienceServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(500)
		w.Write([]byte(`{"error":{"message":"stream boom"}}`))
	})
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}],"stream":true}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// POST-hardening contract: streams retry while NOTHING has been committed
	// (identical to buffered calls). Default policy retries 2× → 3 hits, then
	// the failure surfaces as a clean error envelope — never partial SSE that
	// pretends success.
	if resp.StatusCode != http.StatusInternalServerError && resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 500/502 got %d body %s", resp.StatusCode, string(respBody))
	}
	if strings.Contains(string(respBody), "data:") {
		t.Fatalf("no SSE frames may reach the client on total failure, got %s", string(respBody))
	}
	if hits.Load() != 3 {
		t.Fatalf("expected 1 initial + 2 pre-commit stream retries, hits=%d", hits.Load())
	}
}

func TestStreamNoFailoverToHealthyCandidate(t *testing.T) {
	// PRODUCT CONTRACT: cross-provider failover is disabled. When the selected
	// (pinned) provider fails, the stream request surfaces the upstream error
	// and the healthy sibling is never contacted.
	var primaryHits atomic.Int32
	var standbyHits atomic.Int32
	standby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		standbyHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"from-standby\"}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer standby.Close()

	srv, h, key, up := setupResilienceServer(t, func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		w.WriteHeader(503)
		w.Write([]byte(`{"error":{"message":"primary down"}}`))
	})
	if _, err := h.ProviderStore.Create("standby-openai", models.ProviderOpenAI, standby.URL+"/v1", "sk-test"); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []string{"standby-openai", "openai"} {
		var provID string
		if err := h.DB.QueryRow(`SELECT id FROM providers WHERE name=?`, pid).Scan(&provID); err != nil {
			t.Fatal(err)
		}
		if _, err := h.DB.Exec(`INSERT OR IGNORE INTO provider_models(id, provider_id, model_id, display_name, owned_by, source, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?)`,
			pid+"-gpt-4o-mini", provID, "gpt-4o-mini", "GPT-4o mini", "openai", "manual", time.Now().UTC(), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0, BaseDelay: time.Millisecond}
	_ = up

	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}],"stream":true}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Provider", "openai") // deterministic pin to the failing primary
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode < 500 {
		t.Fatalf("expected honest failure from pinned primary, got %d %s", resp.StatusCode, string(respBody))
	}
	if strings.Contains(string(respBody), "from-standby") {
		t.Fatalf("failover is banned; standby content leaked: %s", string(respBody))
	}
	if n := standbyHits.Load(); n != 0 {
		t.Fatalf("failover is banned: standby hit %d times", n)
	}
}

func TestFallbackProviderOn503(t *testing.T) {
	// PRODUCT CONTRACT (was: failover on 503): providers are pinned and the
	// gateway NEVER switches on failure. prov-a 503s; prov-b (healthy, same
	// model) must receive zero traffic; client gets the honest error.
	var hitsA, hitsB atomic.Int32
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA.Add(1)
		w.WriteHeader(503)
		w.Write([]byte(`{"error":{"message":"down"}}`))
	}))
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "chatcmpl-b",
			"choices": []map[string]interface{}{{"message": map[string]string{"role": "assistant", "content": "from-b"}}},
		})
	}))
	t.Cleanup(func() { upA.Close(); upB.Close() })
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 32)
	ps := provider.NewStore(database, master)
	ks := apikey.NewStore(database)
	pa, err := ps.Create("prov-a", models.ProviderOpenAI, upA.URL+"/v1", "sk-a")
	if err != nil {
		t.Fatal(err)
	}
	pb, err := ps.Create("prov-b", models.ProviderOpenAI, upB.URL+"/v1", "sk-b")
	if err != nil {
		t.Fatal(err)
	}
	now := "2026-01-01T00:00:00Z"
	for _, id := range []string{pa.ID, pb.ID} {
		if _, err := database.Exec(`INSERT INTO provider_models(id, provider_id, model_id, display_name, source, created_at, updated_at) VALUES(?,?,?,?,?,?,?)`, id+"-m", id, "gpt-4o-mini", "gpt-4o-mini", "manual", now, now); err != nil {
			t.Fatal(err)
		}
	}
	k, err := ks.Create("test-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandler(ps, database)
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0, BaseDelay: time.Millisecond}
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/chat/completions", h.ChatCompletions)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+k.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Provider", "prov-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 500 || strings.Contains(string(b), "from-b") {
		t.Fatalf("expected honest failure without switching, got %d %s", resp.StatusCode, string(b))
	}
	if n := hitsB.Load(); n != 0 {
		t.Fatalf("fallback banned: prov-b contacted %d times", n)
	}
	if hitsA.Load() == 0 {
		t.Fatal("primary should have been attempted once")
	}
}

func TestModelsQualifiedID(t *testing.T) {
	srv, key := setupTestServer(t)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %s", resp.StatusCode, string(b))
	}
	if !strings.Contains(string(b), "openai/gpt-4o-mini") {
		t.Fatalf("expected provider/model id, got %s", string(b))
	}
}
