package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/translate"

	"github.com/go-chi/chi/v5"
)

func newStoreTestHandler(t *testing.T) *Handler {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 32)
	ps := provider.NewStore(database, master)
	return newLegacyHandler(ps, database)
}

func TestResponseStoreRoundTrip(t *testing.T) {
	h := newStoreTestHandler(t)
	msgs := []json.RawMessage{
		json.RawMessage(`{"role":"system","content":"be brief"}`),
		json.RawMessage(`{"role":"user","content":"hi"}`),
		json.RawMessage(`{"role":"assistant","content":"hello"}`),
	}
	h.saveResponseTurn("", "resp_1", "keyA", "prov", "m", msgs)
	got, err := h.loadResponseHistory("resp_1", "keyA")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 3 || got[0].Role != "system" || got[2].Role != "assistant" {
		t.Fatalf("history = %+v", got)
	}
}

func TestResponseStoreKeyIsolation(t *testing.T) {
	h := newStoreTestHandler(t)
	h.saveResponseTurn("", "resp_1", "keyA", "prov", "m",
		[]json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)})
	if _, err := h.loadResponseHistory("resp_1", "keyB"); err == nil {
		t.Fatal("keyB must not read keyA's history")
	}
}

func TestResponseStoreUnknownID(t *testing.T) {
	h := newStoreTestHandler(t)
	if _, err := h.loadResponseHistory("resp_nope", "keyA"); err == nil {
		t.Fatal("unknown id must error")
	}
}

func TestResponseStoreExpiry(t *testing.T) {
	h := newStoreTestHandler(t)
	old := time.Now().Add(-25 * time.Hour).Unix()
	_, err := h.DB.Exec(db.Q(`INSERT INTO response_turns(id, key_prefix, provider_id, model, prev_id, messages, created_at_unix) VALUES(?,?,?,?,?,?,?)`),
		"resp_old", "keyA", "prov", "m", "", `[{"role":"user"}]`, old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.loadResponseHistory("resp_old", "keyA"); err == nil {
		t.Fatal("expired history must miss")
	}
}

func TestMergeResponseHistorySystemDedup(t *testing.T) {
	history := mustOpenAIMessages(t, `[{"role":"system","content":"old"},{"role":"user","content":"hi"}]`)
	fresh := mustOpenAIMessages(t, `[{"role":"system","content":"new"},{"role":"user","content":"again"}]`)
	merged := mergeResponseHistory(history, fresh)
	if len(merged) != 3 {
		t.Fatalf("merged = %+v", merged)
	}
	// The explicit per-turn system prompt wins: stored "old" is dropped.
	if merged[0].Role != "user" || merged[1].Role != "system" || merged[2].Role != "user" {
		t.Fatalf("order wrong: %+v", merged)
	}
	// Without fresh instructions the stored system prompt survives.
	merged2 := mergeResponseHistory(
		mustOpenAIMessages(t, `[{"role":"system","content":"old"},{"role":"user","content":"hi"}]`),
		mustOpenAIMessages(t, `[{"role":"user","content":"again"}]`),
	)
	if len(merged2) != 3 || merged2[0].Role != "system" {
		t.Fatalf("system prompt lost: %+v", merged2)
	}
}

func mustOpenAIMessages(t *testing.T, s string) []translate.OpenAIMessage {
	t.Helper()
	var out []translate.OpenAIMessage
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAssistantHistoryMessageFoldsTools(t *testing.T) {
	out := []byte(`{"id":"resp_1","output":[
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checking"}]},
		{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"}]}`)
	raw := assistantHistoryMessage(out)
	if raw == nil {
		t.Fatal("nil history message")
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["role"] != "assistant" || m["content"] != "checking" {
		t.Fatalf("msg = %v", m)
	}
	tcs, ok := m["tool_calls"].([]interface{})
	if !ok || len(tcs) != 1 {
		t.Fatalf("tool_calls = %v", m["tool_calls"])
	}
	tc := tcs[0].(map[string]interface{})
	if tc["id"] != "c1" {
		t.Fatalf("call id = %v", tc)
	}
}

func TestAssistantHistoryMessageEmpty(t *testing.T) {
	if assistantHistoryMessage([]byte(`{"id":"r","output":[]}`)) != nil {
		t.Fatal("empty output must yield nil")
	}
	if assistantHistoryMessage([]byte(`not json`)) != nil {
		t.Fatal("invalid body must yield nil")
	}
}

func TestStoreTranslatedTurnRoundTrip(t *testing.T) {
	h := newStoreTestHandler(t)
	chatBody := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	respBody := []byte(`{"id":"resp_9","output":[
		{"type":"message","content":[{"type":"output_text","text":"yo"}]},
		{"type":"function_call","call_id":"c2","name":"f","arguments":"{}"}]}`)
	h.storeTranslatedTurn("", "keyA", "prov", "m", chatBody, respBody)
	got, err := h.loadResponseHistory("resp_9", "keyA")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want input + assistant reply, got %+v", got)
	}
	if got[1].Role != "assistant" {
		t.Fatalf("reply role = %q", got[1].Role)
	}
}

func TestStoreStreamedTurnRoundTrip(t *testing.T) {
	h := newStoreTestHandler(t)
	chatBody := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	h.storeStreamedTurn("", "keyA", "prov", "m", chatBody, responsesPumpResult{
		respID:    "resp_s1",
		text:      "streamed",
		toolCalls: []streamedHistoryCall{{ID: "c3", Name: "f", Args: `{"a":1}`}},
	})
	got, err := h.loadResponseHistory("resp_s1", "keyA")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 || got[1].Role != "assistant" {
		t.Fatalf("history = %+v", got)
	}
}

// End-to-end: two chained /v1/responses turns against a phant-like upstream
// (native /responses 404s, chat serves) must merge history server-side.
func TestResponsesChainedTurnsMergeHistory(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 32)
	ps := provider.NewStore(database, master)
	ks := apikey.NewStore(database)

	var mu sync.Mutex
	var chatBodies []string
	var n int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			chatBodies = append(chatBodies, string(body))
			mu.Unlock()
			id := atomic.AddInt64(&n, 1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":      "chatcmpl-chain",
				"choices": []map[string]interface{}{{"message": map[string]string{"role": "assistant", "content": "Hello world"}}},
				"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12},
			})
			_ = id
			return
		}
		// No native Responses API — like the phant LiteLLM proxy.
		w.WriteHeader(404)
	}))
	defer upstream.Close()

	if _, err := ps.Create("phantx", models.ProviderOpenAI, upstream.URL+"/v1", "sk-test"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("chain-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandler(ps, database)
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/responses", h.Responses)
	srv := httptest.NewServer(r)
	defer srv.Close()

	post := func(body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/v1/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+k.Key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Turn 1: fresh conversation.
	code, b1 := post(`{"model":"phantx/glm-prox/swe-2-max","input":"first question"}`)
	if code != 200 {
		t.Fatalf("turn1 status=%d body=%s", code, b1)
	}
	var r1 struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(b1), &r1); err != nil || r1.ID == "" {
		t.Fatalf("turn1 no response id: %s", b1)
	}

	// Turn 2: chained via previous_response_id.
	code, b2 := post(`{"model":"phantx/glm-prox/swe-2-max","input":"follow-up","previous_response_id":"` + r1.ID + `"}`)
	if code != 200 {
		t.Fatalf("turn2 status=%d body=%s", code, b2)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(chatBodies) != 2 {
		t.Fatalf("want 2 upstream chat calls, got %d", len(chatBodies))
	}
	var turn2 struct {
		Model    string `json:"model"`
		Messages []struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(chatBodies[1]), &turn2); err != nil {
		t.Fatalf("turn2 chat body invalid: %v", err)
	}
	// The slash-bearing upstream model id must survive prefix stripping.
	if turn2.Model != "glm-prox/swe-2-max" {
		t.Fatalf("upstream model = %q, want glm-prox/swe-2-max", turn2.Model)
	}
	if len(turn2.Messages) != 3 {
		t.Fatalf("want history+follow-up (3 msgs), got %+v", turn2.Messages)
	}
	if turn2.Messages[0].Content != "first question" || turn2.Messages[1].Content != "Hello world" || turn2.Messages[2].Content != "follow-up" {
		t.Fatalf("history wrong: %+v", turn2.Messages)
	}

	// Unknown previous_response_id must refuse loudly, never answer blind.
	code, b3 := post(`{"model":"phantx/glm-prox/swe-2-max","input":"x","previous_response_id":"resp_missing"}`)
	if code != 400 {
		t.Fatalf("unknown prev id status=%d, want 400 body=%s", code, b3)
	}
}

// Full-history multi-turn input (assistant output_text + server tool items)
// must reach the chat upstream as clean text, with no null-content turns.
func TestResponsesFullHistorySanitizedUpstream(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 32)
	ps := provider.NewStore(database, master)
	ks := apikey.NewStore(database)

	var mu sync.Mutex
	var chatBodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			chatBodies = append(chatBodies, string(body))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"chatcmpl-x","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer upstream.Close()

	if _, err := ps.Create("phantx", models.ProviderOpenAI, upstream.URL+"/v1", "sk-test"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("hist-key")
	if err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandler(ps, database)
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/responses", h.Responses)
	srv := httptest.NewServer(r)
	defer srv.Close()

	body := `{"model":"phantx/glm-prox/swe-2-max","input":[
		{"role":"user","content":"q"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a","annotations":[]}]},
		{"type":"web_search_call","id":"ws1"},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"r"},
		{"role":"user","content":"q2"}]}`
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
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(chatBodies) != 1 {
		t.Fatalf("want 1 upstream call, got %d", len(chatBodies))
	}
	up := chatBodies[0]
	if strings.Contains(up, "output_text") {
		t.Fatalf("output_text leaked upstream: %s", up)
	}
	if strings.Contains(up, "web_search_call") {
		t.Fatalf("server tool item leaked upstream: %s", up)
	}
	if strings.Contains(up, `"content":null`) {
		t.Fatalf("null-content turn leaked upstream: %s", up)
	}
	var parsed struct {
		Messages []map[string]interface{} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(up), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Messages) != 5 {
		t.Fatalf("want 5 clean messages, got %d: %s", len(parsed.Messages), up)
	}
}
