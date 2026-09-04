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
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/resilience"

	"github.com/go-chi/chi/v5"
)

// multiEnv wires one opencode-go provider (multi-protocol via name) in front
// of a mock upstream, with chat + messages + responses mounted.
type multiEnv struct {
	t        *testing.T
	upstream *httptest.Server
	gateway  *httptest.Server
	key      string

	lastPath string
	lastBody []byte
}

func newMultiEnv(t *testing.T, up http.HandlerFunc) *multiEnv {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	env := &multiEnv{t: t}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		env.lastPath = r.URL.Path
		env.lastBody = append([]byte(nil), b...)
		up.ServeHTTP(w, r)
	})
	env.upstream = httptest.NewServer(inner)
	t.Cleanup(env.upstream.Close)
	if _, err := ps.Create("opencode-go", models.ProviderOpenAICompatible, env.upstream.URL, "sk-go"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("multi-key")
	if err != nil {
		t.Fatal(err)
	}
	env.key = k.Key
	h := newLegacyHandler(ps, database)
	h.Retry = &resilience.DefaultRetryPolicy{MaxRetries: 0}
	h.Timeouts.StreamIdle = 30 * time.Second
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/chat/completions", h.ChatCompletions)
	r.Post("/v1/messages", h.AnthropicMessages)
	r.Post("/v1/responses", h.Responses)
	env.gateway = httptest.NewServer(r)
	t.Cleanup(env.gateway.Close)
	return env
}

func (e *multiEnv) post(path, body string) (int, string) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.gateway.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestMessagesViaMultiProviderNative(t *testing.T) {
	env := newMultiEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"qwen3.8-max","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`)
	})
	code, body := env.post("/v1/messages", `{"model":"qwen3.8-max","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("messages via multi: got %d body %s", code, body)
	}
	if env.lastPath != "/v1/messages" {
		t.Fatalf("expected upstream /v1/messages, got %q", env.lastPath)
	}
}

func TestChatRejectsMessagesModelWithHint(t *testing.T) {
	env := newMultiEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	})
	code, body := env.post("/v1/chat/completions", `{"model":"qwen3.8-max","messages":[{"role":"user","content":"hi"}]}`)
	if code != 400 {
		t.Fatalf("chat for messages-model: got %d body %s, want 400", code, body)
	}
	if !strings.Contains(body, "/v1/messages") {
		t.Fatalf("error should hint /v1/messages, got %s", body)
	}
	if env.lastPath != "" {
		t.Fatalf("rejected request must not reach upstream, hit %q", env.lastPath)
	}
}

func TestMessagesRejectsChatModelWithHint(t *testing.T) {
	env := newMultiEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	})
	code, body := env.post("/v1/messages", `{"model":"glm-5.3-flash","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if code != 400 {
		t.Fatalf("messages for chat-model: got %d body %s, want 400", code, body)
	}
	if !strings.Contains(body, "/v1/chat/completions") {
		t.Fatalf("error should hint /v1/chat/completions, got %s", body)
	}
}

func TestResponsesRoutesToMessagesUpstream(t *testing.T) {
	env := newMultiEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"message":"wrong endpoint"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"qwen3.8-max","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":6,"output_tokens":3}}`)
	})
	code, body := env.post("/v1/responses", `{"model":"qwen3.8-max","input":"hi"}`)
	if code != 200 {
		t.Fatalf("responses for messages-model: got %d body %s", code, body)
	}
	if env.lastPath != "/v1/messages" {
		t.Fatalf("expected upstream /v1/messages, got %q", env.lastPath)
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("gateway responses body not JSON: %v (%s)", err, body)
	}
	if _, ok := m["output"]; !ok {
		t.Fatalf("gateway responses body missing output: %s", body)
	}
}
