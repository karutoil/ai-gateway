package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"

	"github.com/go-chi/chi/v5"
)

func setupTestServer(t *testing.T) (*httptest.Server, string) {
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

	h := New(ps, database)

	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/chat/completions", h.ChatCompletions)
	r.Post("/v1/completions", h.Completions)
	r.Post("/v1/embeddings", h.Embeddings)
	r.Get("/v1/models", h.Models)
	r.Post("/v1/messages", h.AnthropicMessages)
	r.Post("/v1/responses", h.Responses)

	srv := httptest.NewServer(r)
	return srv, k.Key
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
