package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/cache"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"

	"github.com/go-chi/chi/v5"
)

func setupTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv, _, key := setupTestServerWithHandler(t)
	return srv, key
}

// setupTestServerWithHandler also returns the proxy Handler so tests can
// flip cache/retry knobs per-case.
func setupTestServerWithHandler(t *testing.T) (*httptest.Server, *Handler, string) {
	t.Helper()
	// in-memory db
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i)
	}
	ps := provider.NewStore(database, master)
	ks := apikey.NewStore(database)

	// mock upstream OpenAI
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "chat/completions") {
			body, _ := io.ReadAll(r.Body)
			var b map[string]interface{}
			json.Unmarshal(body, &b)
			stream, _ := b["stream"].(bool)
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n"))
				w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n"))
				w.Write([]byte("data: [DONE]\n\n"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":      "chatcmpl-test",
				"choices": []map[string]interface{}{{"message": map[string]string{"role": "assistant", "content": "Hello world"}}},
			})
			return
		}
		if strings.Contains(r.URL.Path, "completions") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"cmpl-test","choices":[{"text":"Hello"}]}`))
			return
		}
		if strings.Contains(r.URL.Path, "embeddings") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"object":"list","data":[{"embedding":[0.1,0.2]}]}`))
			return
		}
		if strings.Contains(r.URL.Path, "models") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4o-mini","object":"model","owned_by":"openai"}]}`))
			return
		}
		if strings.Contains(r.URL.Path, "messages") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"msg_test","content":[{"type":"text","text":"Hi from Anthropic"}],"model":"claude-3"}`))
			return
		}
		if strings.Contains(r.URL.Path, "responses") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"resp_test","output":[{"type":"message","content":"Hello Responses"}]}`))
			return
		}
		w.WriteHeader(404)
	}))

	// create provider pointing to mock upstream
	_, err = ps.Create("openai", models.ProviderOpenAI, upstream.URL+"/v1", "sk-test")
	if err != nil {
		t.Fatal(err)
	}
	// also create anthropic provider for message test
	_, err = ps.Create("anthropic", models.ProviderAnthropic, upstream.URL, "sk-ant-test")
	if err != nil {
		t.Fatal(err)
	}

	k, err := ks.Create("test-key")
	if err != nil {
		t.Fatal(err)
	}

	h := newLegacyHandler(ps, database)

	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/chat/completions", h.ChatCompletions)
	r.Post("/v1/completions", h.Completions)
	r.Post("/v1/embeddings", h.Embeddings)
	r.Get("/v1/models", h.Models)
	r.Post("/v1/messages", h.AnthropicMessages)
	r.Post("/v1/responses", h.Responses)

	srv := httptest.NewServer(r)
	return srv, h, k.Key
}

func TestChatCompletionsNonStream(t *testing.T) {
	srv, key := setupTestServer(t)
	defer srv.Close()
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 got %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "Hello world") {
		t.Fatalf("unexpected body %s", string(b))
	}
}

func TestChatCompletionsStream(t *testing.T) {
	srv, key := setupTestServer(t)
	defer srv.Close()
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}],"stream":true}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 got %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "data:") {
		t.Fatalf("expected SSE got %s", string(b))
	}
}

func TestAnthropicMessages(t *testing.T) {
	srv, key := setupTestServer(t)
	defer srv.Close()
	// Use anthropic model so it routes to anthropic provider (native)
	body := `{"model":"claude-3-5-sonnet-20241022","max_tokens":100,"messages":[{"role":"user","content":"Hello"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 got %d body %s", resp.StatusCode, mustRead(resp.Body))
	}
}

func TestModels(t *testing.T) {
	srv, key := setupTestServer(t)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 got %d", resp.StatusCode)
	}
}

func TestUnauthorized(t *testing.T) {
	srv, _ := setupTestServer(t)
	defer srv.Close()
	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-gw-invalid")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401 got %d", resp.StatusCode)
	}
}

func TestResponsesFallback(t *testing.T) {
	srv, key := setupTestServer(t)
	defer srv.Close()
	body := `{"model":"gpt-4o-mini","input":"Hello"}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 got %d %s", resp.StatusCode, mustRead(resp.Body))
	}
}

func TestChatCompletionsRejectsAnthropicModel(t *testing.T) {
	srv, key := setupTestServer(t)
	defer srv.Close()
	body := `{"model":"muse-spark-1.2-contributor","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	bb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 for anthropic model on /v1/chat/completions, got %d %s", resp.StatusCode, string(bb))
	}
	if !strings.Contains(string(bb), "anthropic model") {
		t.Fatalf("expected anthropic-model error, got %s", string(bb))
	}
}
func mustRead(r io.Reader) string { b, _ := io.ReadAll(r); return string(b) }

func TestStreamResponseCache(t *testing.T) {
	srv, h, key := setupTestServerWithHandler(t)
	defer srv.Close()
	h.CacheTTLSeconds = 10
	h.CacheStreams = true
	h.Cache = cache.NewMemoryCache(16)

	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}],"stream":true}`
	do := func() (*http.Response, string) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	resp1, body1 := do()
	if resp1.Header.Get("X-Cache") != "MISS" {
		t.Fatalf("first stream should MISS, got %q", resp1.Header.Get("X-Cache"))
	}
	resp2, body2 := do()
	if resp2.Header.Get("X-Cache") != "HIT" {
		t.Fatalf("identical stream should replay from cache (HIT), got %q", resp2.Header.Get("X-Cache"))
	}
	if body1 != body2 {
		t.Fatalf("replayed stream must be byte-identical:\nfirst:  %q\nsecond: %q", body1, body2)
	}
	if !strings.Contains(body2, "data:") || !strings.Contains(body2, "[DONE]") {
		t.Fatalf("replayed body must be the original SSE frames, got %q", body2)
	}
	if ct := resp2.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("replayed stream must keep the SSE content type, got %q", ct)
	}

	// A non-streaming request with the same body must NOT receive cached SSE
	// bytes — the stream cache key is salted by dialect.
	nsBody := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(nsBody))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), "data:") {
		t.Fatalf("non-stream client must never receive cached SSE bytes, got %q", string(b))
	}

	// Cache rows: the replay must be logged with cache_hit=1, is_stream=1,
	// and every request carries its tri-state cache disposition.
	var hits int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM request_logs WHERE cache_hit=1 AND is_stream=1 AND cache_status='hit'`).Scan(&hits); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("expected exactly 1 cached stream row, got %d", hits)
	}
	var missStatus string
	if err := h.DB.QueryRow(`SELECT cache_status FROM request_logs WHERE is_stream=1 AND cache_hit=0`).Scan(&missStatus); err != nil {
		t.Fatal(err)
	}
	if missStatus != "miss" {
		t.Fatalf("eligible stream miss must be recorded as cache_status=miss, got %q", missStatus)
	}
}

// Disabled by default: without CACHE_STREAMS, identical streams always MISS.
func TestStreamResponseCacheDisabledByDefault(t *testing.T) {
	srv, h, key := setupTestServerWithHandler(t)
	defer srv.Close()
	h.CacheTTLSeconds = 10
	h.CacheStreams = false
	h.Cache = cache.NewMemoryCache(16)

	body := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}],"stream":true}`
	do := func() string {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		return resp.Header.Get("X-Cache")
	}
	if first := do(); first != "MISS" {
		t.Fatalf("first: %q", first)
	}
	if second := do(); second != "MISS" {
		t.Fatalf("stream caching must be opt-in; identical stream should still MISS, got %q", second)
	}
	var bypassStatus string
	if err := h.DB.QueryRow(`SELECT cache_status FROM request_logs WHERE is_stream=1 ORDER BY created_at DESC LIMIT 1`).Scan(&bypassStatus); err != nil {
		t.Fatal(err)
	}
	if bypassStatus != "bypass" {
		t.Fatalf("streams with the stream cache off must be cache_status=bypass, got %q", bypassStatus)
	}
}

// Every request carries a cache disposition, and the models-list endpoint —
// previously invisible in reporting — logs hit and miss rows too.
func TestModelsListCacheReporting(t *testing.T) {
	srv, h, key := setupTestServerWithHandler(t)
	defer srv.Close()
	h.Cache = cache.NewMemoryCache(16)

	get := func() string {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		return resp.Header.Get("X-Cache")
	}
	if first := get(); first != "MISS" {
		t.Fatalf("first models list should MISS, got %q", first)
	}
	if second := get(); second != "HIT" {
		t.Fatalf("second models list should HIT, got %q", second)
	}
	var missN, hitN int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FILTER (WHERE cache_status='miss'), COUNT(*) FILTER (WHERE cache_status='hit') FROM request_logs WHERE endpoint='models'`).Scan(&missN, &hitN); err != nil {
		t.Fatal(err)
	}
	if missN != 1 || hitN != 1 {
		t.Fatalf("models rows: want 1 miss + 1 hit, got %d/%d", missN, hitN)
	}
}

// Streams must report usage detail (cache tokens) even with LOG_BODIES off —
// the harvest used to live inside the body-logging capture, silently zeroing
// cache_read_tokens for all streaming traffic on privacy-conscious deploys.
func TestStreamUsageDetailWithoutBodyLogging(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 32)
	ps := provider.NewStore(database, master)
	ks := apikey.NewStore(database)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5,\"total_tokens\":105,\"prompt_tokens_details\":{\"cached_tokens\":80}}}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()
	if _, err := ps.Create("cachedprov", models.ProviderOpenAI, upstream.URL+"/v1", "sk-x"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("cache-detail-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandler(ps, database)
	h.LogBodies = false // the regression condition

	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/chat/completions", h.ChatCompletions)
	srv := httptest.NewServer(r)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"cachedprov/m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+k.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "data:") {
		t.Fatalf("expected SSE, got %q", string(b))
	}

	// The client can drain the SSE body a beat before the handler goroutine
	// finishes finishClean and inserts the row — poll briefly instead of
	// racing the insert.
	var promptTok, cacheRead int
	var finish string
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := h.DB.QueryRow(`SELECT prompt_tokens, cache_read_tokens, COALESCE(finish_reason,'') FROM request_logs ORDER BY created_at DESC LIMIT 1`).Scan(&promptTok, &cacheRead, &finish)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request_logs row never appeared:", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if promptTok != 100 {
		t.Fatalf("prompt tokens: want 100, got %d", promptTok)
	}
	if cacheRead != 80 {
		t.Fatalf("cache_read_tokens must be harvested with LOG_BODIES off: want 80, got %d", cacheRead)
	}
	if finish != "stop" {
		t.Fatalf("finish_reason: want stop, got %q", finish)
	}
}
