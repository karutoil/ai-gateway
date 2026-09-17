package devin

import (
	"encoding/json"
	"strings"
	"testing"
)

// One OpenAI assistant turn carrying text + tool_calls must encode as ONE
// wire prompt. Splitting it into N prompts (text turn + one turn per call)
// confuses the backend's turn tracking and breaks follow-up tool calls in
// agentic loops.
func TestFromOpenAICombinesAssistantTextAndToolCalls(t *testing.T) {
	body := `{"model":"devin/swe-2","messages":[
		{"role":"user","content":"list files"},
		{"role":"assistant","content":"I'll list them","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"list_dir","arguments":"{\"target_directory\": \".\"}"}},
			{"id":"call_2","type":"function","function":{"name":"read_file","arguments":"{\"path\": \"a.txt\"}"}}
		]},
		{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`
	in, err := FromOpenAI([]byte(body), "devin/swe-2")
	if err != nil {
		t.Fatalf("from: %v", err)
	}
	if len(in.Messages) != 3 {
		t.Fatalf("messages = %d want 3 (user + combined assistant + tool): %+v", len(in.Messages), in.Messages)
	}
	assistant := in.Messages[1]
	if assistant.Role != WireRoleSystem {
		t.Fatalf("assistant role = %d, want SYSTEM (%d)", assistant.Role, WireRoleSystem)
	}
	if in.Messages[0].Role != WireRoleUser {
		t.Fatalf("user role = %d, want USER (%d)", in.Messages[0].Role, WireRoleUser)
	}
	if !strings.Contains(assistant.Text, "I'll list them") {
		t.Fatalf("assistant text lost: %q", assistant.Text)
	}
	if len(assistant.ToolCalls) != 2 {
		t.Fatalf("assistant tool calls = %d want 2: %+v", len(assistant.ToolCalls), assistant.ToolCalls)
	}
	if assistant.ToolCalls[0].Name != "list_dir" || assistant.ToolCalls[1].Name != "read_file" {
		t.Fatalf("tool names: %+v", assistant.ToolCalls)
	}
}

// Assistant content blocks shaped as output_text (Responses-normalized
// histories) must still contribute their text.
func TestFromOpenAIExtractsOutputTextContent(t *testing.T) {
	body := `{"model":"devin/swe-2","messages":[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"output_text","text":"hello there"}]}]}`
	in, err := FromOpenAI([]byte(body), "devin/swe-2")
	if err != nil {
		t.Fatalf("from: %v", err)
	}
	if len(in.Messages) != 2 {
		t.Fatalf("messages = %+v", in.Messages)
	}
	if in.Messages[1].Text != "hello there" {
		t.Fatalf("assistant text = %q", in.Messages[1].Text)
	}
}

// Upstream tool calls without ids must get a synthesized id so the backend
// can pair results; empty ids echo back as empty and break downstream
// grouping for parallel calls.
func TestFromOpenAISynthesizesMissingToolCallID(t *testing.T) {
	body := `{"model":"devin/swe-2","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"","tool_calls":[{"type":"function","function":{"name":"list_dir","arguments":"{}"}}]}]}`
	in, err := FromOpenAI([]byte(body), "devin/swe-2")
	if err != nil {
		t.Fatalf("from: %v", err)
	}
	if len(in.Messages) != 2 {
		t.Fatalf("messages = %+v", in.Messages)
	}
	if len(in.Messages[1].ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", in.Messages[1].ToolCalls)
	}
	if in.Messages[1].ToolCalls[0].ID == "" {
		t.Fatal("upstream tool call id must be synthesized, got empty")
	}
}

// Tool parameters arriving as a JSON string (loose SDKs) must survive.
func TestFromOpenAIToolParametersAsString(t *testing.T) {
	body := `{"model":"devin/swe-2","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"get_w","description":"w","parameters":"{\"type\":\"object\"}"}}]}`
	in, err := FromOpenAI([]byte(body), "devin/swe-2")
	if err != nil {
		t.Fatalf("from: %v", err)
	}
	if len(in.Tools) != 1 {
		t.Fatalf("tools = %+v", in.Tools)
	}
	var v any
	if err := json.Unmarshal([]byte(in.Tools[0].Parameters), &v); err != nil {
		t.Fatalf("parameters invalid JSON: %q", in.Tools[0].Parameters)
	}
	// A pre-stringified schema must stay a schema object, not be
	// double-encoded into a JSON string literal.
	m, ok := v.(map[string]any)
	if !ok || m["type"] != "object" {
		t.Fatalf("parameters double-encoded: %q", in.Tools[0].Parameters)
	}
}
