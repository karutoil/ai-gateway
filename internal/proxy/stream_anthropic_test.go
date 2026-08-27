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

func TestAnthropicMessagesStreamViaOpenAI(t *testing.T) {
	// Strict mode: /v1/messages rejects non-anthropic models with 400.
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"chatcmpl-test","choices":[]}`))
	}))
	defer upstream.Close()
	_, err = ps.Create("openai", models.ProviderOpenAI, upstream.URL+"/v1", "sk-test")
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
	r.Post("/v1/messages", h.AnthropicMessages)
	srv := httptest.NewServer(r)
	defer srv.Close()
	body := `{"model":"gpt-4o-mini","max_tokens":100,"messages":[{"role":"user","content":"Hello"}],"stream":true}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+k.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for openai model on /v1/messages, got %d body %s", resp.StatusCode, string(b))
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "not an anthropic model") {
		t.Fatalf("expected anthropic-model error, got %s", string(b))
	}
}

func TestAnthropicMessagesStreamNative(t *testing.T) {
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var b map[string]interface{}
		json.Unmarshal(body, &b)
		if s, _ := b["stream"].(bool); s {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			flusher, _ := w.(http.Flusher)
			chunks := []string{
				`event: message_start
data: {"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant"}}`,
				`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
				`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"output_tokens":5}}`,
				`event: message_stop
data: {"type":"message_stop"}`,
			}
			for _, c := range chunks {
				w.Write([]byte(c + "\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"id": "msg_test", "type": "message", "content": []interface{}{map[string]interface{}{"type": "text", "text": "Hello world"}}, "usage": map[string]interface{}{"input_tokens": 10, "output_tokens": 5}})
	}))
	defer upstream.Close()
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
	r.Post("/v1/messages", h.AnthropicMessages)
	srv := httptest.NewServer(r)
	defer srv.Close()
	body := `{"model":"claude-3-5-sonnet-20241022","max_tokens":100,"messages":[{"role":"user","content":"Hello"}],"stream":true}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+k.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 got %d body %s", resp.StatusCode, string(b))
	}
	b, _ := io.ReadAll(resp.Body)
	s := string(b)
	if !strings.Contains(s, "content_block_delta") {
		t.Fatalf("expected anthropic content_block_delta, got %s", s)
	}
	if !strings.Contains(s, "Hello") {
		t.Fatalf("expected Hello in stream, got %s", s)
	}
}

func TestResponsesStreamViaChat(t *testing.T) {
	// Strict mode: /v1/responses streams straight from upstream via proxyWithMetrics.
	// Use an OpenAI upstream that streams a trivial chunk; gateway should passthrough 200 + data:.
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var b map[string]interface{}
		json.Unmarshal(body, &b)
		if s, _ := b["stream"].(bool); s {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			flusher, _ := w.(http.Flusher)
			chunks := []string{
				`data: {"id":"chatcmpl-test","object":"response","created":123,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
				`data: [DONE]`,
			}
			for _, c := range chunks {
				w.Write([]byte(c + "\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"id": "resp-test", "output": []interface{}{"ok"}})
	}))
	defer upstream.Close()
	_, err = ps.Create("openai", models.ProviderOpenAI, upstream.URL+"/v1", "sk-test")
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
	r.Post("/v1/responses", h.Responses)
	srv := httptest.NewServer(r)
	defer srv.Close()
	body := `{"model":"gpt-4o-mini","input":"Hello","stream":true}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+k.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 got %d body %s", resp.StatusCode, string(b))
	}
	b, _ := io.ReadAll(resp.Body)
	s := string(b)
	if !strings.Contains(s, "data:") {
		t.Fatalf("expected SSE data:, got %s", s)
	}
}
