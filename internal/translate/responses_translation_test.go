package translate

import (
	"encoding/json"
	"reflect"
	"testing"
)

func translateToMessages(t *testing.T, body string) []OpenAIMessage {
	t.Helper()
	out, _, err := ResponsesToChat([]byte(body))
	if err != nil {
		t.Fatalf("ResponsesToChat: %v", err)
	}
	var req struct {
		Messages []OpenAIMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("translated body invalid: %v", err)
	}
	return req.Messages
}

// function_call items must become assistant tool_calls, not null-content
// user messages (which silently erased the call from the conversation).
func TestResponsesFunctionCallItemBecomesAssistantToolCall(t *testing.T) {
	msgs := translateToMessages(t, `{"model":"m","input":[`+
		`{"role":"user","content":"weather?"},`+
		`{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{\"loc\":\"Paris\"}"}]}`)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d: %+v", len(msgs), msgs)
	}
	a := msgs[1]
	if a.Role != "assistant" {
		t.Fatalf("role = %q, want assistant", a.Role)
	}
	calls, ok := a.ToolCalls.([]interface{})
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %v", a.ToolCalls)
	}
	c := calls[0].(map[string]interface{})
	if c["id"] != "c1" || c["type"] != "function" {
		t.Fatalf("call = %v", c)
	}
	fn := c["function"].(map[string]interface{})
	if fn["name"] != "get_weather" || fn["arguments"] != `{"loc":"Paris"}` {
		t.Fatalf("function = %v", fn)
	}
}

// function_call_output items must become tool messages — the output must
// reach the model, not be dropped.
func TestResponsesFunctionCallOutputBecomesToolMessage(t *testing.T) {
	msgs := translateToMessages(t, `{"model":"m","input":[`+
		`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},`+
		`{"type":"function_call_output","call_id":"c1","output":"15C sunny"}]}`)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	tm := msgs[1]
	if tm.Role != "tool" || tm.ToolCallID != "c1" {
		t.Fatalf("tool msg = %+v", tm)
	}
	if tm.Content != "15C sunny" {
		t.Fatalf("content = %v, want the output text", tm.Content)
	}
}

func TestResponsesFunctionCallOutputPartsJoined(t *testing.T) {
	msgs := translateToMessages(t, `{"model":"m","input":[`+
		`{"type":"function_call_output","call_id":"c1","output":[`+
		`{"type":"output_text","text":"part1"},{"type":"output_text","text":"part2"}]}]}`)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	if msgs[0].Content != "part1\npart2" {
		t.Fatalf("content = %v, want joined parts", msgs[0].Content)
	}
}

// reasoning / item_reference items have no chat counterpart and must be
// skipped, not forwarded as null-content user messages.
func TestResponsesReasoningItemsSkipped(t *testing.T) {
	msgs := translateToMessages(t, `{"model":"m","input":[`+
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}]},`+
		`{"type":"item_reference","id":"x"},`+
		`{"role":"user","content":"hi"}]}`)
	if len(msgs) != 1 {
		t.Fatalf("want only the user message, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" {
		t.Fatalf("role = %q", msgs[0].Role)
	}
}

// Responses tools are flat ({type,name,parameters}); chat needs them nested.
func TestResponsesToolsConvertedToChatShape(t *testing.T) {
	out, _, err := ResponsesToChat([]byte(`{"model":"m","input":"hi","tools":[` +
		`{"type":"function","name":"get_weather","description":"d","parameters":{"type":"object","properties":{"loc":{"type":"string"}}}},` +
		`{"type":"web_search"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("want 1 tool (web_search dropped), got %d", len(req.Tools))
	}
	tl := req.Tools[0]
	if tl.Type != "function" || tl.Function.Name != "get_weather" || tl.Function.Description != "d" {
		t.Fatalf("tool = %+v", tl)
	}
	if len(tl.Function.Parameters) == 0 {
		t.Fatal("parameters lost in conversion")
	}
}

func TestResponsesChatShapedToolsPassThrough(t *testing.T) {
	out, _, err := ResponsesToChat([]byte(`{"model":"m","input":"hi","tools":[` +
		`{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{}}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(out), `"name":"f"`) {
		t.Fatalf("chat-shaped tool lost: %s", out)
	}
}

func TestResponsesToolChoiceNamedConverted(t *testing.T) {
	out, _, err := ResponsesToChat([]byte(`{"model":"m","input":"hi",` +
		`"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{}}}],` +
		`"tool_choice":{"type":"function","name":"f"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		ToolChoice interface{} `json:"tool_choice"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	m, ok := req.ToolChoice.(map[string]interface{})
	if !ok || m["type"] != "function" {
		t.Fatalf("tool_choice = %v", req.ToolChoice)
	}
	fn := m["function"].(map[string]interface{})
	if fn["name"] != "f" {
		t.Fatalf("tool_choice.function = %v", fn)
	}
}

func TestResponsesMaxTokensFallback(t *testing.T) {
	out, _, err := ResponsesToChat([]byte(`{"model":"m","input":"hi","max_tokens":33}`))
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		MaxTokens *int `json:"max_tokens"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 33 {
		t.Fatalf("max_tokens = %v, want 33", req.MaxTokens)
	}
}

func TestResponsesInstructionsBecomeSystem(t *testing.T) {
	msgs := translateToMessages(t, `{"model":"m","input":"hi","instructions":"You are terse."}`)
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[0].Content != "You are terse." {
		t.Fatalf("messages = %+v", msgs)
	}
}

// Full agent loop: user → call → output must survive as a coherent chat
// conversation, in order.
func TestResponsesAgentLoopEndToEnd(t *testing.T) {
	msgs := translateToMessages(t, `{"model":"m","input":[`+
		`{"role":"user","content":"weather in Paris?"},`+
		`{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{}"},`+
		`{"type":"function_call_output","call_id":"c1","output":"15C"},`+
		`{"type":"reasoning","summary":[]},`+
		`{"role":"user","content":"and in Rome?"}],`+
		`"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object","properties":{"loc":{"type":"string"}}}}]}`)
	want := []struct{ role string }{{"user"}, {"assistant"}, {"tool"}, {"user"}}
	if len(msgs) != len(want) {
		t.Fatalf("want %d messages, got %d: %+v", len(want), len(msgs), msgs)
	}
	for i, w := range want {
		if msgs[i].Role != w.role {
			t.Fatalf("msg[%d] role = %q, want %q", i, msgs[i].Role, w.role)
		}
	}
	if msgs[2].Content != "15C" {
		t.Fatalf("tool output lost: %+v", msgs[2])
	}
}

func TestFunctionOutputToText(t *testing.T) {
	cases := []struct {
		in   interface{}
		want string
	}{
		{nil, ""},
		{"plain", "plain"},
		{[]interface{}{map[string]interface{}{"type": "output_text", "text": "a"}}, "a"},
		{[]interface{}{map[string]interface{}{"type": "output_text", "text": "a"}, map[string]interface{}{"type": "output_text", "text": "b"}}, "a\nb"},
		{map[string]interface{}{"weird": 1}, `{"weird":1}`},
	}
	for _, c := range cases {
		if got := functionOutputToText(c.in); got != c.want {
			t.Errorf("functionOutputToText(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResponsesToolPassthroughUnchanged(t *testing.T) {
	in := []byte(`{"model":"m","input":"hi","tools":[{"type":"function","function":{"name":"f"}}]}`)
	out, _, err := ResponsesToChat(in)
	if err != nil {
		t.Fatal(err)
	}
	var orig, got struct {
		Tools []json.RawMessage `json:"tools"`
	}
	_ = json.Unmarshal(in, &orig)
	_ = json.Unmarshal(out, &got)
	if !reflect.DeepEqual(orig, got) {
		t.Fatalf("chat-shaped tools mutated:\n orig=%v\n got =%v", orig, got)
	}
}
