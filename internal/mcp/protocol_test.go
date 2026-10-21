package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

/* ------------------------------------------------------------------ */
/* Tool-call parsing, every dialect, buffered and streamed            */
/* ------------------------------------------------------------------ */

func TestParseToolCallsChatJSON(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[
		{"id":"call_1","type":"function","function":{"name":"github-search_issues","arguments":"{\"q\":\"bug\"}"}}
	]}}]}`)
	calls := ParseToolCalls(ProtocolChat, "application/json", body)
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Name != "github-search_issues" {
		t.Fatalf("unexpected call: %+v", calls[0])
	}
	if calls[0].Input["q"] != "bug" {
		t.Fatalf("arguments not decoded: %+v", calls[0].Input)
	}
}

func TestParseToolCallsChatSSE(t *testing.T) {
	// Arguments arrive fragmented across deltas; the parser must concatenate.
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"github-search_issues","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"mcp\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n")
	calls := ParseToolCalls(ProtocolChat, "text/event-stream", []byte(stream))
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	if calls[0].Input["q"] != "mcp" {
		t.Fatalf("fragmented arguments not reassembled: %s (%+v)", calls[0].Arguments, calls[0].Input)
	}
	if calls[0].ID != "call_a" {
		t.Fatalf("id = %q", calls[0].ID)
	}
}

func TestParseToolCallsChatSSEParallel(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c0","function":{"name":"t-one","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c1","function":{"name":"t-two","arguments":"{}"}}]}}]}`,
	}, "\n\n")
	calls := ParseToolCalls(ProtocolChat, "text/event-stream", []byte(stream))
	if len(calls) != 2 || calls[0].Name != "t-one" || calls[1].Name != "t-two" {
		t.Fatalf("parallel calls: %+v", calls)
	}
}

func TestParseToolCallsAnthropicJSON(t *testing.T) {
	body := []byte(`{"content":[{"type":"text","text":"let me look"},{"type":"tool_use","id":"toolu_1","name":"github-search_issues","input":{"q":"x"}}]}`)
	calls := ParseToolCalls(ProtocolAnthropic, "application/json", body)
	if len(calls) != 1 || calls[0].Name != "github-search_issues" || calls[0].Input["q"] != "x" {
		t.Fatalf("calls: %+v", calls)
	}
}

func TestParseToolCallsAnthropicSSE(t *testing.T) {
	stream := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"github-search_issues","input":{}}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"an"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"thropic\"}"}}`,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
	}, "\n\n")
	calls := ParseToolCalls(ProtocolAnthropic, "text/event-stream", []byte(stream))
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	if calls[0].Name != "github-search_issues" || calls[0].Input["q"] != "anthropic" {
		t.Fatalf("unexpected: %+v", calls[0])
	}
}

func TestParseToolCallsResponsesJSON(t *testing.T) {
	body := []byte(`{"output":[{"type":"function_call","call_id":"call_r","name":"github-search_issues","arguments":"{\"q\":\"y\"}"}]}`)
	calls := ParseToolCalls(ProtocolResponses, "application/json", body)
	if len(calls) != 1 || calls[0].ID != "call_r" || calls[0].Input["q"] != "y" {
		t.Fatalf("calls: %+v", calls)
	}
}

func TestParseToolCallsResponsesSSE(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_s","name":"github-search_issues","arguments":"{\"q\":\"z\"}"}}`,
		`data: {"type":"response.completed","response":{"output":[]}}`,
	}, "\n\n")
	calls := ParseToolCalls(ProtocolResponses, "text/event-stream", []byte(stream))
	if len(calls) != 1 || calls[0].Input["q"] != "z" {
		t.Fatalf("calls: %+v", calls)
	}
}

func TestParseToolCallsRejectsMalformedArguments(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"tool_calls":[{"id":"c","function":{"name":"t","arguments":"not json"}}]}}]}`)
	calls := ParseToolCalls(ProtocolChat, "application/json", body)
	if len(calls) != 1 {
		t.Fatal("a call with unparsable arguments must still be surfaced")
	}
	if _, ok := calls[0].Input["_raw"]; !ok {
		t.Fatalf("raw arguments should be preserved: %+v", calls[0].Input)
	}
}

/* ------------------------------------------------------------------ */
/* Appending tool results                                             */
/* ------------------------------------------------------------------ */

func callAndResults() ([]ToolCall, []ToolResult) {
	calls := []ToolCall{
		{ID: "call_1", Name: "github-search_issues", Arguments: `{"q":"bug"}`, Input: map[string]any{"q": "bug"}},
		{ID: "call_2", Name: "github-create_issue", Arguments: `{}`, Input: map[string]any{}},
	}
	results := []ToolResult{
		{ServerSlug: "github", Tool: "search_issues", Namespaced: "github-search_issues", Text: "found 2"},
		{ServerSlug: "github", Tool: "create_issue", Namespaced: "github-create_issue", IsError: true, Text: "denied"},
	}
	return calls, results
}

func TestAppendToolResultsChat(t *testing.T) {
	calls, results := callAndResults()
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"go"}]}`)
	out, err := AppendToolResults(ProtocolChat, body, calls, results, "")
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Messages) != 4 { // user + assistant(tool_calls) + 2 tool results
		t.Fatalf("messages = %d, want 4", len(root.Messages))
	}
	assistant := root.Messages[1]
	if assistant["role"] != "assistant" {
		t.Fatalf("second message role = %v", assistant["role"])
	}
	rawCalls, _ := assistant["tool_calls"].([]any)
	if len(rawCalls) != 2 {
		t.Fatalf("assistant tool_calls = %d", len(rawCalls))
	}
	if _, ok := assistant["content"]; !ok {
		t.Fatal("assistant message must carry a content field (null is valid)")
	}
	for i, want := range []string{"found 2", "denied"} {
		msg := root.Messages[2+i]
		if msg["role"] != "tool" {
			t.Fatalf("message %d role = %v", 2+i, msg["role"])
		}
		if msg["content"] != want {
			t.Fatalf("tool content = %v, want %v", msg["content"], want)
		}
		if msg["tool_call_id"] != calls[i].ID {
			t.Fatalf("tool_call_id = %v, want %v", msg["tool_call_id"], calls[i].ID)
		}
	}
}

func TestAppendToolResultsAnthropic(t *testing.T) {
	calls, results := callAndResults()
	body := []byte(`{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":"go"}]}`)
	out, err := AppendToolResults(ProtocolAnthropic, body, calls, results, "")
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Messages []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	// The pre-existing user turn keeps its string content; unwrap it so the
	// typed decode below only sees the appended blocks.
	var rawRoot struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &rawRoot); err != nil {
		t.Fatal(err)
	}
	rawRoot.Messages[0]["content"] = []any{}
	patched, _ := json.Marshal(rawRoot)
	if err := json.Unmarshal(patched, &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Messages) != 3 {
		t.Fatalf("messages = %d", len(root.Messages))
	}
	if root.Messages[1].Role != "assistant" || root.Messages[2].Role != "user" {
		t.Fatalf("roles: %+v", root.Messages)
	}
	uses := 0
	for _, block := range root.Messages[1].Content {
		if block["type"] == "tool_use" {
			uses++
			if block["id"] == nil || block["name"] == nil {
				t.Fatalf("tool_use block incomplete: %v", block)
			}
		}
	}
	if uses != 2 {
		t.Fatalf("tool_use blocks = %d", uses)
	}
	resultsBlocks := 0
	for _, block := range root.Messages[2].Content {
		if block["type"] != "tool_result" {
			continue
		}
		resultsBlocks++
		if block["tool_use_id"] == nil {
			t.Fatal("tool_result missing tool_use_id")
		}
	}
	if resultsBlocks != 2 {
		t.Fatalf("tool_result blocks = %d", resultsBlocks)
	}
}

func TestAppendToolResultsResponses(t *testing.T) {
	calls, results := callAndResults()
	body := []byte(`{"model":"m","input":"go"}`)
	out, err := AppendToolResults(ProtocolResponses, body, calls, results, "")
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Input) != 5 { // user + 2 calls + 2 outputs
		t.Fatalf("input items = %d, want 5 (%s)", len(root.Input), out)
	}
	if root.Input[0]["role"] != "user" {
		t.Fatalf("the original input was lost: %v", root.Input[0])
	}
	// Items are emitted as call/output pairs, in call order.
	want := []string{"function_call", "function_call_output", "function_call", "function_call_output"}
	for i, typ := range want {
		if root.Input[i+1]["type"] != typ {
			t.Fatalf("item %d type = %v, want %v (%v)", i+1, root.Input[i+1]["type"], typ, root.Input)
		}
	}
	if root.Input[1]["call_id"] != "call_1" || root.Input[2]["call_id"] != "call_1" {
		t.Fatalf("call_id pairing broken: %v", root.Input)
	}
	if root.Input[3]["call_id"] != "call_2" || root.Input[4]["call_id"] != "call_2" {
		t.Fatalf("call_id pairing broken: %v", root.Input)
	}
}

func TestAppendToolResultsPreservesInputArray(t *testing.T) {
	calls, results := callAndResults()
	body := []byte(`{"model":"m","input":[{"role":"user","content":"go"}]}`)
	out, err := AppendToolResults(ProtocolResponses, body, calls, results, "")
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Input) != 5 {
		t.Fatalf("input items = %d, want 5", len(root.Input))
	}
}

/* ------------------------------------------------------------------ */
/* Metadata injection                                                 */
/* ------------------------------------------------------------------ */

func TestInjectMetadataJSON(t *testing.T) {
	body := []byte(`{"id":"x","choices":[]}`)
	summaries := []CallSummary{{Server: "github", Tool: "search_issues", Name: "github-search_issues", Round: 1}}
	out := InjectMetadata(ProtocolChat, "application/json", body, 2, summaries, "")
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if root["mcp_rounds"] != float64(2) {
		t.Fatalf("mcp_rounds = %v", root["mcp_rounds"])
	}
	calls, _ := root["mcp_tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("mcp_tool_calls = %v", root["mcp_tool_calls"])
	}
	if root["id"] != "x" {
		t.Fatal("the original body must be preserved")
	}
}

func TestInjectMetadataSSEChat(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n\n")
	out := InjectMetadata(ProtocolChat, "text/event-stream", []byte(stream), 1, nil, "")
	if !strings.Contains(string(out), `"mcp_rounds":1`) {
		t.Fatalf("metadata not injected: %s", out)
	}
	if !strings.Contains(string(out), "data: [DONE]") {
		t.Fatal("the DONE sentinel must survive")
	}
	// Every data line must still be valid JSON (or [DONE]).
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue
		}
		var probe map[string]any
		if err := json.Unmarshal([]byte(payload), &probe); err != nil {
			t.Fatalf("frame is no longer valid JSON: %s", payload)
		}
	}
}

func TestInjectMetadataSSEAnthropic(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m"}}`,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
	}, "\n\n")
	out := InjectMetadata(ProtocolAnthropic, "text/event-stream", []byte(stream), 3, nil, "")
	if !strings.Contains(string(out), `"mcp_rounds":3`) {
		t.Fatalf("metadata not injected: %s", out)
	}
	if !strings.Contains(string(out), "event: message_delta") {
		t.Fatal("event names must be preserved")
	}
}

func TestInjectMetadataSSEResponses(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_item.done","item":{"type":"message"}}`,
		`data: {"type":"response.completed","response":{"id":"r"}}`,
	}, "\n\n")
	out := InjectMetadata(ProtocolResponses, "text/event-stream", []byte(stream), 2, nil, "")
	if !strings.Contains(string(out), `"mcp_rounds":2`) {
		t.Fatalf("metadata not injected: %s", out)
	}
	if !strings.Contains(string(out), `"type":"response.completed"`) {
		t.Fatal("the completed frame must survive")
	}
}

func TestInjectMetadataLoopError(t *testing.T) {
	out := InjectMetadata(ProtocolChat, "application/json", []byte(`{"choices":[]}`), 25, nil, "mcp_max_rounds_exceeded")
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if root["mcp_error"] != "mcp_max_rounds_exceeded" {
		t.Fatalf("mcp_error = %v", root["mcp_error"])
	}
}
