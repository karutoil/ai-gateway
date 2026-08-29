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

// Non-stream: a chat tool_call must become a Responses function_call output
// item. The pre-fix converter emitted an empty assistant message and the
// model's tool call vanished — /v1/responses agents could never loop.
func TestResponsesToolCallSurvivesNonStream(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/responses") {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_9","type":"function","function":{"name":"get_weather","arguments":"{\"loc\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`)
	}
	srv, key := newHygieneEnv(t, up, func(hh *Handler) http.HandlerFunc { return hh.Responses }, "/v1/responses")
	resp := postHygiene(t, srv.URL+"/v1/responses", key, `{"model":"gpt-4o-mini","input":"weather?","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object","properties":{"loc":{"type":"string"}}}}]}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var out struct {
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Status    string `json:"status"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, body)
	}
	var fc *struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Status    string `json:"status"`
	}
	for i := range out.Output {
		if out.Output[i].Type == "function_call" {
			fc = &out.Output[i]
			break
		}
	}
	if fc == nil {
		t.Fatalf("no function_call output item in %s", body)
	}
	if fc.CallID != "call_9" || fc.Name != "get_weather" || fc.Arguments != `{"loc":"Paris"}` || fc.Status != "completed" {
		t.Fatalf("function_call = %+v", fc)
	}
}

// Streaming: chat tool_call deltas must be re-emitted as Responses
// function_call events with the full arguments in response.completed.
func TestResponsesToolCallSurvivesStream(t *testing.T) {
	up := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/responses") {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_w\",\"arguments\":\"{\\\"lo\"}}]}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"c\\\":\\\"Paris\\\"}\"}}]}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}
	srv, key := newHygieneEnv(t, up, func(hh *Handler) http.HandlerFunc { return hh.Responses }, "/v1/responses")
	resp := postHygiene(t, srv.URL+"/v1/responses", key, `{"model":"gpt-4o-mini","input":"weather?","stream":true,"tools":[{"type":"function","name":"get_w","parameters":{"type":"object","properties":{}}}]}`)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	sse := string(raw)
	for _, want := range []string{
		`"type":"function_call"`,
		`response.output_item.added`,
		`response.function_call_arguments.delta`,
		`response.function_call_arguments.done`,
		`"call_id":"call_1"`,
		`"name":"get_w"`,
		`{\"loc\":\"Paris\"}`,
		`response.completed`,
	} {
		if !strings.Contains(sse, want) {
			t.Fatalf("stream missing %s\n%s", want, sse)
		}
	}
	// The completed frame must carry the function_call in its output array.
	idx := strings.Index(sse, "response.completed")
	if idx < 0 {
		t.Fatal("no response.completed frame")
	}
	if !strings.Contains(sse[idx:], `"function_call"`) {
		t.Fatalf("response.completed output lacks function_call item\n%s", sse[idx:])
	}
}

// Streaming via an anthropic dialect upstream: tool_use content blocks with
// input_json_delta fragments must become function_call events too.
func TestResponsesToolCallSurvivesAnthropicStream(t *testing.T) {
	anthCalled := false
	up := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/responses") {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/messages") {
			anthCalled = true
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":2}}}\n\n")
			io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"get_w\"}}\n\n")
			io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"lo\"}}\n\n")
			io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"c\\\":\\\"Paris\\\"}\"}}\n\n")
			io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n")
			io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		t.Errorf("unexpected upstream path %s", r.URL.Path)
	}
	// Same harness but the provider is registered as anthropic-type so the
	// translated path targets /v1/messages with x-api-key semantics.
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ps := provider.NewStore(database, make([]byte, 32))
	ks := apikey.NewStore(database)
	us := httptest.NewServer(http.HandlerFunc(up))
	t.Cleanup(us.Close)
	if _, err := ps.Create("hyg-anth", models.ProviderAnthropic, us.URL+"/v1", "sk-h"); err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("hyg-key-anth")
	if err != nil {
		t.Fatal(err)
	}
	hh := newLegacyHandler(ps, database)
	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/responses", hh.Responses)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp := postHygiene(t, srv.URL+"/v1/responses", k.Key, `{"model":"gpt-4o-mini","input":"weather?","stream":true,"tools":[{"type":"function","name":"get_w","parameters":{"type":"object","properties":{}}}]}`)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	sse := string(raw)
	if !anthCalled {
		t.Fatalf("anthropic upstream never called; stream:\n%s", sse)
	}
	for _, want := range []string{
		`response.function_call_arguments.delta`,
		`response.function_call_arguments.done`,
		`"call_id":"toolu_1"`,
		`"name":"get_w"`,
		`{\"loc\":\"Paris\"}`,
		`response.completed`,
	} {
		if !strings.Contains(sse, want) {
			t.Fatalf("anthropic stream missing %s\n%s", want, sse)
		}
	}
}
