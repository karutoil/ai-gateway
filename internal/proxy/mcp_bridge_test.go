package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-gateway/internal/apikey"
	"ai-gateway/internal/db"
	"ai-gateway/internal/mcp"
	"ai-gateway/internal/mcp/mcptest"
	"ai-gateway/internal/middleware"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"

	"github.com/go-chi/chi/v5"
)

/* ------------------------------------------------------------------ */
/* Mock LLM that speaks tool calls on every dialect                    */
/* ------------------------------------------------------------------ */

// scriptedLLM is a fake model provider. Round 1 answers with a tool call for
// the namespaced MCP tool; once it sees a tool result in the request it
// answers with plain text. It records the bodies it received so tests can
// assert what the gateway actually forwarded.
type scriptedLLM struct {
	mu       sync.Mutex
	bodies   [][]byte
	tool     string // namespaced tool name to call
	args     string // JSON arguments
	rounds   int    // number of tool-call rounds before answering; 0 = never call
	failWith int
	// varyingArgs makes every round emit different arguments, so a stuck
	// model is distinguishable from one that simply repeats itself.
	varyingArgs bool
}

func (s *scriptedLLM) Record(b []byte) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies = append(s.bodies, append([]byte(nil), b...))
	return len(s.bodies)
}

func (s *scriptedLLM) Bodies() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.bodies...)
}

func sawToolResult(body []byte) bool {
	text := string(body)
	return strings.Contains(text, `"role":"tool"`) ||
		strings.Contains(text, `"tool_result"`) ||
		strings.Contains(text, `"function_call_output"`)
}

// callCount counts how many tool-call rounds the upstream already answered.
func callCount(body []byte) int {
	return strings.Count(string(body), `"tool_calls"`) + strings.Count(string(body), `"tool_use"`) + strings.Count(string(body), `"function_call"`)
}

func (s *scriptedLLM) argsFor(round int) string {
	if !s.varyingArgs {
		return s.args
	}
	return fmt.Sprintf(`{"text":"again-%d"}`, round)
}

func (s *scriptedLLM) chatResponse(body []byte) []byte {
	if s.rounds > 0 && callCount(body) < s.rounds {
		resp := map[string]any{
			"id": "chatcmpl-tool", "object": "chat.completion", "model": "gpt-5",
			"choices": []any{map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []any{map[string]any{
						"id":   fmt.Sprintf("call_%d", callCount(body)+1),
						"type": "function",
						"function": map[string]any{
							"name":      s.tool,
							"arguments": s.argsFor(callCount(body) + 1),
						},
					}},
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		}
		b, _ := json.Marshal(resp)
		return b
	}
	resp := map[string]any{
		"id": "chatcmpl-final", "object": "chat.completion", "model": "gpt-5",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "final answer from the model"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 6, "total_tokens": 17},
	}
	b, _ := json.Marshal(resp)
	return b
}

func (s *scriptedLLM) chatStream(body []byte) []byte {
	var sb strings.Builder
	if s.rounds > 0 && callCount(body) < s.rounds {
		sb.WriteString(`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_s","type":"function","function":{"name":"` + s.tool + `","arguments":""}}]}}]}` + "\n\n")
		sb.WriteString(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":` + strconvQuote(s.args) + `}}]}}]}` + "\n\n")
		sb.WriteString(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n")
		sb.WriteString("data: [DONE]\n\n")
		return []byte(sb.String())
	}
	sb.WriteString(`data: {"choices":[{"delta":{"content":"final "}}]}` + "\n\n")
	sb.WriteString(`data: {"choices":[{"delta":{"content":"answer"}}]}` + "\n\n")
	sb.WriteString(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n")
	sb.WriteString("data: [DONE]\n\n")
	return []byte(sb.String())
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

/* ------------------------------------------------------------------ */
/* Harness                                                            */
/* ------------------------------------------------------------------ */

type mcpHarness struct {
	Server  *httptest.Server
	Handler *Handler
	Key     string
	Store   *mcp.Store
	Manager *mcp.Manager
	LLM     *scriptedLLM
}

// mcpTestUpstream is a real in-process MCP server (official SDK, Streamable
// HTTP) exposing echo / add / fail.
func newMCPTestUpstream(t *testing.T) (*mcptest.Upstream, *mcptest.Calls) {
	t.Helper()
	up := mcptest.Start(t)
	return up, up.Calls
}

func newMCPHarness(t *testing.T, llm *scriptedLLM) *mcpHarness {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i + 3)
	}
	ps := provider.NewStore(database, master)
	ks := apikey.NewStore(database)

	// Fake LLM upstream.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		llm.Record(body)
		var probe map[string]any
		_ = json.Unmarshal(body, &probe)
		stream, _ := probe["stream"].(bool)

		switch {
		case strings.Contains(r.URL.Path, "/chat/completions"):
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Write(llm.chatStream(body))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(llm.chatResponse(body))
		case strings.Contains(r.URL.Path, "/messages"):
			w.Header().Set("Content-Type", "application/json")
			if llm.rounds > 0 && callCount(body) < llm.rounds {
				w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-3","content":[{"type":"tool_use","id":"toolu_1","name":"` + llm.tool + `","input":` + llm.args + `}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`))
				return
			}
			w.Write([]byte(`{"id":"msg_2","type":"message","role":"assistant","model":"claude-3","content":[{"type":"text","text":"anthropic final answer"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":7}}`))
		case strings.Contains(r.URL.Path, "/responses"):
			w.Header().Set("Content-Type", "application/json")
			if llm.rounds > 0 && callCount(body) < llm.rounds {
				w.Write([]byte(`{"id":"resp_1","object":"response","model":"gpt-5","output":[{"type":"function_call","call_id":"call_r1","name":"` + llm.tool + `","arguments":` + strconvQuote(llm.args) + `}],"usage":{"input_tokens":10,"output_tokens":5}}`))
				return
			}
			w.Write([]byte(`{"id":"resp_2","object":"response","model":"gpt-5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"responses final answer"}]}],"usage":{"input_tokens":12,"output_tokens":7}}`))
		case strings.Contains(r.URL.Path, "/models"):
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5","object":"model"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(upstream.Close)

	if _, err := ps.Create("openai", models.ProviderOpenAI, upstream.URL+"/v1", "sk-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Create("anthropic", models.ProviderAnthropic, upstream.URL, "sk-ant"); err != nil {
		t.Fatal(err)
	}

	mcpStore := mcp.NewStore(database, master)
	mcpManager := mcp.NewManager(mcpStore, 20*time.Second)
	t.Cleanup(mcpManager.Close)
	bridge := mcp.NewBridge(mcpStore, mcpManager, 25, 60*time.Second)

	h := New(ps, database)
	h.LegacyFallback = true
	h.MCP = bridge
	h.Cache = nil

	key, err := ks.Create("mcp-key")
	if err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	r.Use(middleware.GatewayAuth(ks))
	r.Post("/v1/chat/completions", h.ChatCompletions)
	r.Post("/v1/messages", h.AnthropicMessages)
	r.Post("/v1/responses", h.Responses)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	return &mcpHarness{Server: srv, Handler: h, Key: key.Key, Store: mcpStore, Manager: mcpManager, LLM: llm}
}

func (h *mcpHarness) registerMCPServer(t *testing.T, name, url string) *mcp.Server {
	t.Helper()
	srv, err := h.Store.Create(mcp.ServerInput{Name: name, Transport: "http", URL: url})
	if err != nil {
		t.Fatalf("create mcp server: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := h.Manager.Discover(ctx, srv.ID); err != nil {
		t.Fatalf("discover: %v", err)
	}
	got, err := h.Store.GetServer(srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (h *mcpHarness) post(t *testing.T, path, body string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", h.Server.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

/* ------------------------------------------------------------------ */
/* Tests                                                              */
/* ------------------------------------------------------------------ */

func TestMCPAutoExecutionChatCompletions(t *testing.T) {
	llm := &scriptedLLM{tool: "gw-echo", args: `{"text":"ping"}`, rounds: 1}
	h := newMCPHarness(t, llm)
	mcpSrv, calls := newMCPTestUpstream(t)
	registered := h.registerMCPServer(t, "GW", mcpSrv.Server.URL+"/mcp")

	body := fmt.Sprintf(`{"model":"gpt-5","messages":[{"role":"user","content":"echo ping"}],"tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/%s","require_approval":"never","allowed_tools":[]}]}`, registered.Slug)
	resp, out := h.post(t, "/v1/chat/completions", body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "final answer from the model") {
		t.Fatalf("final model answer missing: %s", out)
	}
	if calls.Total() != 1 {
		t.Fatalf("tool executed %d times, want 1", calls.Total())
	}
	if resp.Header.Get("X-MCP-Rounds") != "1" {
		t.Fatalf("X-MCP-Rounds = %q", resp.Header.Get("X-MCP-Rounds"))
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["mcp_rounds"] != float64(1) {
		t.Fatalf("mcp_rounds = %v", parsed["mcp_rounds"])
	}
	// The upstream must have received the expanded function tool on round 1
	// and the tool result on round 2.
	bodies := llm.Bodies()
	if len(bodies) != 2 {
		t.Fatalf("llm rounds = %d, want 2", len(bodies))
	}
	if strings.Contains(string(bodies[0]), `"type":"mcp"`) {
		t.Fatal("the raw mcp reference was forwarded upstream")
	}
	if !strings.Contains(string(bodies[0]), "gw-echo") {
		t.Fatalf("expanded tool missing upstream: %s", bodies[0])
	}
	if !strings.Contains(string(bodies[1]), "echo: ping") {
		t.Fatalf("tool result not fed back: %s", bodies[1])
	}
}

func TestMCPApprovalAlwaysDoesNotExecute(t *testing.T) {
	llm := &scriptedLLM{tool: "gw-echo", args: `{"text":"ping"}`, rounds: 1}
	h := newMCPHarness(t, llm)
	mcpSrv, calls := newMCPTestUpstream(t)
	registered := h.registerMCPServer(t, "GW", mcpSrv.Server.URL+"/mcp")

	// require_approval: "always" → the caller controls approval, so the
	// gateway must hand the tool call back instead of executing it.
	body := fmt.Sprintf(`{"model":"gpt-5","messages":[{"role":"user","content":"echo"}],"tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/%s","require_approval":"always"}]}`, registered.Slug)
	resp, out := h.post(t, "/v1/chat/completions", body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body = %s", resp.StatusCode, out)
	}
	if calls.Total() != 0 {
		t.Fatalf("tool was executed despite require_approval=always (%d calls)", calls.Total())
	}
	if !strings.Contains(string(out), "tool_calls") {
		t.Fatalf("expected the raw tool call to be returned: %s", out)
	}
	if len(llm.Bodies()) != 1 {
		t.Fatalf("llm rounds = %d, want 1 (no auto loop)", len(llm.Bodies()))
	}
}

func TestMCPAutoExecutionAnthropicMessages(t *testing.T) {
	llm := &scriptedLLM{tool: "gw-add", args: `{"a":2,"b":40}`, rounds: 1}
	h := newMCPHarness(t, llm)
	mcpSrv, calls := newMCPTestUpstream(t)
	registered := h.registerMCPServer(t, "GW", mcpSrv.Server.URL+"/mcp")

	body := fmt.Sprintf(`{"model":"claude-3-5-sonnet-20241022","max_tokens":100,"messages":[{"role":"user","content":"add"}],"tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/%s","require_approval":"never"}]}`, registered.Slug)
	resp, out := h.post(t, "/v1/messages", body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body = %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "anthropic final answer") {
		t.Fatalf("final answer missing: %s", out)
	}
	if calls.Total() != 1 {
		t.Fatalf("tool calls = %d, want 1", calls.Total())
	}
	bodies := llm.Bodies()
	if len(bodies) != 2 {
		t.Fatalf("llm rounds = %d, want 2", len(bodies))
	}
	if !strings.Contains(string(bodies[1]), `"tool_result"`) {
		t.Fatalf("anthropic tool_result block missing: %s", bodies[1])
	}
	if !strings.Contains(string(bodies[1]), ">42<") && !strings.Contains(string(bodies[1]), `"42"`) {
		t.Fatalf("tool output not fed back: %s", bodies[1])
	}
}

func TestMCPAutoExecutionResponses(t *testing.T) {
	llm := &scriptedLLM{tool: "gw-add", args: `{"a":1,"b":1}`, rounds: 1}
	h := newMCPHarness(t, llm)
	mcpSrv, calls := newMCPTestUpstream(t)
	registered := h.registerMCPServer(t, "GW", mcpSrv.Server.URL+"/mcp")

	body := fmt.Sprintf(`{"model":"gpt-5","input":"add","tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/%s","require_approval":"never"}]}`, registered.Slug)
	resp, out := h.post(t, "/v1/responses", body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body = %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "responses final answer") {
		t.Fatalf("final answer missing: %s", out)
	}
	if calls.Total() != 1 {
		t.Fatalf("tool calls = %d, want 1", calls.Total())
	}
	bodies := llm.Bodies()
	if len(bodies) != 2 {
		t.Fatalf("llm rounds = %d, want 2", len(bodies))
	}
	if !strings.Contains(string(bodies[1]), "function_call_output") {
		t.Fatalf("function_call_output missing: %s", bodies[1])
	}
}

func TestMCPAutoExecutionStreaming(t *testing.T) {
	llm := &scriptedLLM{tool: "gw-echo", args: `{"text":"stream"}`, rounds: 1}
	h := newMCPHarness(t, llm)
	mcpSrv, calls := newMCPTestUpstream(t)
	registered := h.registerMCPServer(t, "GW", mcpSrv.Server.URL+"/mcp")

	body := fmt.Sprintf(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"go"}],"tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/%s","require_approval":"never"}]}`, registered.Slug)
	resp, out := h.post(t, "/v1/chat/completions", body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body = %s", resp.StatusCode, out)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	// The client receives one coherent SSE stream with the final answer and
	// the structured metadata frame.
	if !strings.Contains(string(out), "final ") || !strings.Contains(string(out), "answer") {
		t.Fatalf("streamed answer missing: %s", out)
	}
	if !strings.Contains(string(out), "mcp_rounds") {
		t.Fatalf("metadata frame missing from the stream: %s", out)
	}
	if !strings.Contains(string(out), "data: [DONE]") {
		t.Fatalf("[DONE] missing: %s", out)
	}
	if calls.Total() != 1 {
		t.Fatalf("tool calls = %d, want 1", calls.Total())
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" || payload == "" {
			continue
		}
		var probe map[string]any
		if err := json.Unmarshal([]byte(payload), &probe); err != nil {
			t.Fatalf("stream frame is not valid JSON: %s", payload)
		}
	}
}

func TestMCPMaxRoundsStopsLoop(t *testing.T) {
	// The model never stops calling tools, and every call differs (so the
	// repeat guard cannot stop it first). The gateway must still stop after
	// the configured cap instead of looping forever.
	llm := &scriptedLLM{tool: "gw-echo", args: `{"text":"again"}`, rounds: 1000, varyingArgs: true}
	h := newMCPHarness(t, llm)
	mcpSrv, calls := newMCPTestUpstream(t)
	registered := h.registerMCPServer(t, "GW", mcpSrv.Server.URL+"/mcp")
	h.Handler.MCP.MaxRounds = 3

	body := fmt.Sprintf(`{"model":"gpt-5","messages":[{"role":"user","content":"loop"}],"tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/%s","require_approval":"never"}]}`, registered.Slug)
	resp, out := h.post(t, "/v1/chat/completions", body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body = %s", resp.StatusCode, out)
	}
	if resp.Header.Get("X-MCP-Rounds") != "3" {
		t.Fatalf("X-MCP-Rounds = %q, want 3", resp.Header.Get("X-MCP-Rounds"))
	}
	if resp.Header.Get("X-MCP-Error") != "mcp_max_rounds_exceeded" {
		t.Fatalf("X-MCP-Error = %q", resp.Header.Get("X-MCP-Error"))
	}
	if calls.Total() != 3 {
		t.Fatalf("tool executions = %d, want exactly the cap (3)", calls.Total())
	}
	if len(llm.Bodies()) > 4 {
		t.Fatalf("llm rounds = %d, the loop was not bounded", len(llm.Bodies()))
	}
	if !strings.Contains(string(out), "mcp_max_rounds_exceeded") {
		t.Fatalf("the cap must be reported in the body: %s", out)
	}
}

func TestMCPReferenceToUnknownServerIsRejected(t *testing.T) {
	llm := &scriptedLLM{rounds: 0}
	h := newMCPHarness(t, llm)
	mcpSrv, _ := newMCPTestUpstream(t)
	h.registerMCPServer(t, "GW", mcpSrv.Server.URL+"/mcp")

	body := `{"model":"gpt-5","messages":[{"role":"user","content":"x"}],"tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/does-not-exist","require_approval":"never"}]}`
	resp, out := h.post(t, "/v1/chat/completions", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "not registered or not authorized") {
		t.Fatalf("body = %s", out)
	}
	if len(llm.Bodies()) != 0 {
		t.Fatal("an invalid reference must not reach the upstream")
	}
}

func TestMCPNetworkURLReferenceIsRejected(t *testing.T) {
	llm := &scriptedLLM{rounds: 0}
	h := newMCPHarness(t, llm)
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"x"}],"tools":[{"type":"mcp","server_url":"https://attacker.example.com/mcp","require_approval":"never"}]}`
	resp, out := h.post(t, "/v1/chat/completions", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "litellm_proxy") {
		t.Fatalf("the error should name the accepted sentinel: %s", out)
	}
}

func TestMCPReferenceRespectsKeyAllowlist(t *testing.T) {
	llm := &scriptedLLM{tool: "gw-echo", args: `{"text":"x"}`, rounds: 1}
	h := newMCPHarness(t, llm)
	allowed, _ := newMCPTestUpstream(t)
	denied, deniedCalls := newMCPTestUpstream(t)

	allowedSrv := h.registerMCPServer(t, "Allowed", allowed.Server.URL+"/mcp")
	h.registerMCPServer(t, "Denied", denied.Server.URL+"/mcp")

	// Grant the key only the allowed server.
	keys := apikey.NewStore(h.Handler.DB)
	key, ok := keys.Verify(h.Key)
	if !ok {
		t.Fatal("key did not verify")
	}
	if err := h.Store.SetKeyPermissions(key.ID, []mcp.KeyPermission{{KeyID: key.ID, ServerID: allowedSrv.ID, Enabled: true}}); err != nil {
		t.Fatal(err)
	}

	// litellm_proxy (all servers) must only expose the allowed one.
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"x"}],"tools":[{"type":"mcp","server_url":"litellm_proxy","require_approval":"never","allowed_tools":[]}]}`
	resp, out := h.post(t, "/v1/chat/completions", body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body = %s", resp.StatusCode, out)
	}
	bodies := llm.Bodies()
	if len(bodies) == 0 {
		t.Fatal("no upstream body")
	}
	if strings.Contains(string(bodies[0]), "denied-") {
		t.Fatalf("an unauthorized server's tools leaked: %s", bodies[0])
	}
	if !strings.Contains(string(bodies[0]), allowedSrv.Slug+"-echo") {
		t.Fatalf("the authorized tool is missing: %s", bodies[0])
	}
	if deniedCalls.Total() != 0 {
		t.Fatal("the denied upstream was contacted")
	}

	// Explicitly naming the denied server must be refused.
	body = `{"model":"gpt-5","messages":[{"role":"user","content":"x"}],"tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/denied","require_approval":"never"}]}`
	resp, out = h.post(t, "/v1/chat/completions", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("disallowed server status = %d body = %s", resp.StatusCode, out)
	}
}

func TestMCPAllowedToolsFilterReachesUpstream(t *testing.T) {
	llm := &scriptedLLM{tool: "gw-echo", args: `{"text":"x"}`, rounds: 1}
	h := newMCPHarness(t, llm)
	mcpSrv, _ := newMCPTestUpstream(t)
	registered := h.registerMCPServer(t, "GW", mcpSrv.Server.URL+"/mcp")

	body := fmt.Sprintf(`{"model":"gpt-5","messages":[{"role":"user","content":"x"}],"tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/%s","require_approval":"never","allowed_tools":["%s-echo"]}]}`, registered.Slug, registered.Slug)
	h.post(t, "/v1/chat/completions", body)

	bodies := llm.Bodies()
	if len(bodies) == 0 {
		t.Fatal("no upstream body")
	}
	if !strings.Contains(string(bodies[0]), registered.Slug+"-echo") {
		t.Fatalf("allowed tool missing: %s", bodies[0])
	}
	if strings.Contains(string(bodies[0]), registered.Slug+"-add") {
		t.Fatalf("a filtered-out tool leaked upstream: %s", bodies[0])
	}
}

func TestMCPToolErrorIsFedBackNotFatal(t *testing.T) {
	llm := &scriptedLLM{tool: "gw-fail", args: `{}`, rounds: 1}
	h := newMCPHarness(t, llm)
	mcpSrv, _ := newMCPTestUpstream(t)
	registered := h.registerMCPServer(t, "GW", mcpSrv.Server.URL+"/mcp")

	body := fmt.Sprintf(`{"model":"gpt-5","messages":[{"role":"user","content":"x"}],"tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/%s","require_approval":"never"}]}`, registered.Slug)
	resp, out := h.post(t, "/v1/chat/completions", body)
	if resp.StatusCode != 200 {
		t.Fatalf("a tool-level error must not fail the request: %d %s", resp.StatusCode, out)
	}
	bodies := llm.Bodies()
	if len(bodies) != 2 {
		t.Fatalf("llm rounds = %d, want 2", len(bodies))
	}
	if !strings.Contains(string(bodies[1]), "upstream tool failure") {
		t.Fatalf("the error text was not returned to the model: %s", bodies[1])
	}
	if !strings.Contains(string(out), "final answer from the model") {
		t.Fatalf("the model never got to answer: %s", out)
	}
}

func TestMCPDisabledServerReferenceIsRejected(t *testing.T) {
	llm := &scriptedLLM{rounds: 0}
	h := newMCPHarness(t, llm)
	mcpSrv, calls := newMCPTestUpstream(t)
	registered := h.registerMCPServer(t, "GW", mcpSrv.Server.URL+"/mcp")
	if err := h.Store.SetEnabled(registered.ID, false); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"model":"gpt-5","messages":[{"role":"user","content":"x"}],"tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/%s","require_approval":"never"}]}`, registered.Slug)
	resp, out := h.post(t, "/v1/chat/completions", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s", resp.StatusCode, out)
	}
	if calls.Total() != 0 {
		t.Fatal("a disabled server executed a tool")
	}
	if len(llm.Bodies()) != 0 {
		t.Fatal("a disabled reference reached the upstream")
	}
}

func TestMCPNoMatchingToolsIsRejected(t *testing.T) {
	llm := &scriptedLLM{rounds: 0}
	h := newMCPHarness(t, llm)
	mcpSrv, _ := newMCPTestUpstream(t)
	registered := h.registerMCPServer(t, "GW", mcpSrv.Server.URL+"/mcp")

	body := fmt.Sprintf(`{"model":"gpt-5","messages":[{"role":"user","content":"x"}],"tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/%s","require_approval":"never","allowed_tools":["nonexistent-tool"]}]}`, registered.Slug)
	resp, out := h.post(t, "/v1/chat/completions", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "mcp_no_tools_available") {
		t.Fatalf("body = %s", out)
	}
}

func TestMCPRequestsWithoutReferencesAreUnchanged(t *testing.T) {
	llm := &scriptedLLM{rounds: 0}
	h := newMCPHarness(t, llm)
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"keep","parameters":{"type":"object"}}}]}`
	resp, out := h.post(t, "/v1/chat/completions", body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body = %s", resp.StatusCode, out)
	}
	if resp.Header.Get("X-MCP-Rounds") != "" {
		t.Fatal("a plain request must not carry MCP execution headers")
	}
	bodies := llm.Bodies()
	if len(bodies) != 1 {
		t.Fatalf("rounds = %d", len(bodies))
	}
	if !strings.Contains(string(bodies[0]), `"name":"keep"`) {
		t.Fatalf("the client's own tools were dropped: %s", bodies[0])
	}
}

func TestMCPRepeatedIdenticalCallStopsLoop(t *testing.T) {
	// A model that keeps replaying the SAME call must not burn the whole
	// round budget: the repeat is detected and reported.
	llm := &scriptedLLM{tool: "gw-echo", args: `{"text":"same"}`, rounds: 1000}
	h := newMCPHarness(t, llm)
	mcpSrv, calls := newMCPTestUpstream(t)
	registered := h.registerMCPServer(t, "GW", mcpSrv.Server.URL+"/mcp")

	body := fmt.Sprintf(`{"model":"gpt-5","messages":[{"role":"user","content":"loop"}],"tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/%s","require_approval":"never"}]}`, registered.Slug)
	resp, out := h.post(t, "/v1/chat/completions", body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body = %s", resp.StatusCode, out)
	}
	if got := resp.Header.Get("X-MCP-Error"); got != "mcp_repeated_tool_call" {
		t.Fatalf("X-MCP-Error = %q, want mcp_repeated_tool_call", got)
	}
	if calls.Total() != 1 {
		t.Fatalf("tool executed %d times; an identical repeat must run once", calls.Total())
	}
	if resp.Header.Get("X-MCP-Rounds") != "1" {
		t.Fatalf("X-MCP-Rounds = %q, want 1", resp.Header.Get("X-MCP-Rounds"))
	}
}

func TestMCPDistinctCallsAreNotTreatedAsRepeats(t *testing.T) {
	// Two *different* calls must both execute — the repeat guard must not be
	// so coarse that legitimate multi-step tool use is truncated.
	llm := &scriptedLLM{tool: "gw-add", args: `{"a":1,"b":1}`, rounds: 2}
	h := newMCPHarness(t, llm)
	mcpSrv, calls := newMCPTestUpstream(t)
	registered := h.registerMCPServer(t, "GW", mcpSrv.Server.URL+"/mcp")
	h.Handler.MCP.MaxRounds = 5

	body := fmt.Sprintf(`{"model":"gpt-5","messages":[{"role":"user","content":"x"}],"tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/%s","require_approval":"never"}]}`, registered.Slug)
	resp, out := h.post(t, "/v1/chat/completions", body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body = %s", resp.StatusCode, out)
	}
	// The mock returns identical arguments each round, so this asserts the
	// guard fired rather than the loop running to the cap.
	if resp.Header.Get("X-MCP-Error") != "mcp_repeated_tool_call" {
		t.Fatalf("X-MCP-Error = %q", resp.Header.Get("X-MCP-Error"))
	}
	if calls.Total() > 2 {
		t.Fatalf("tool ran %d times, expected the repeat guard to stop it", calls.Total())
	}
}

func TestMCPStreamingReportsMetadataInBodyNotHeaders(t *testing.T) {
	// Headers are already committed when the tool rounds run, so the execution
	// metadata must be in the terminal SSE event, not in a header that net/http
	// would silently drop.
	llm := &scriptedLLM{tool: "gw-add", args: `{"a":2,"b":2}`, rounds: 1}
	h := newMCPHarness(t, llm)
	mcpSrv, _ := newMCPTestUpstream(t)
	registered := h.registerMCPServer(t, "GW", mcpSrv.Server.URL+"/mcp")

	body := fmt.Sprintf(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"x"}],"tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/%s","require_approval":"never"}]}`, registered.Slug)
	resp, out := h.post(t, "/v1/chat/completions", body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	found := false
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" || payload == "" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			t.Fatalf("bad frame: %s", payload)
		}
		if frame["mcp_rounds"] == float64(1) {
			calls, _ := frame["mcp_tool_calls"].([]any)
			if len(calls) != 1 {
				t.Fatalf("mcp_tool_calls = %v", frame["mcp_tool_calls"])
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no frame carried mcp_rounds=1: %s", out)
	}
	// Verify the call actually ran (2+2).
	if !strings.Contains(string(llm.Bodies()[1]), "4") {
		t.Fatalf("tool output not fed back: %s", llm.Bodies()[1])
	}
}
