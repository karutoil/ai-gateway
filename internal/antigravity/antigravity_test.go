package antigravity

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResolveRuntime(t *testing.T) {
	cases := []struct{ public, effort, want string }{
		{"gemini-3.8-flash", "off", "gemini-3.8-flash-low"},
		{"gemini-3.8-flash", "medium", "gemini-3.8-flash-medium"},
		{"gemini-3.8-flash", "high", "gemini-3.8-flash-high"},
		{"gemini-3.5-flash", "high", "gemini-3-flash-agent"},
		{"gemini-3.1-pro", "high", "gemini-pro-agent"},
		{"claude-sonnet-4-6", "high", "claude-sonnet-4-6"},
		{"gpt-oss-120b", "medium", "gpt-oss-120b-medium"},
		{"unknown-model", "high", "unknown-model"},
	}
	for _, c := range cases {
		if got := ResolveRuntime(c.public, c.effort); got != c.want {
			t.Errorf("ResolveRuntime(%q,%q)=%q want %q", c.public, c.effort, got, c.want)
		}
	}
}

func TestThinkingBudget(t *testing.T) {
	if inc, b := ThinkingBudget("gemini-3.8-flash-low", "off"); inc || b != 0 {
		t.Error("off should disable thoughts")
	}
	if _, b := ThinkingBudget("gemini-3.8-flash-low", "high"); b != -1 {
		t.Errorf("gemini high should be -1, got %d", b)
	}
	if _, b := ThinkingBudget("claude-sonnet-4-6", "high"); b != 1024 {
		t.Errorf("claude should be 1024, got %d", b)
	}
	if _, b := ThinkingBudget("gpt-oss-120b-medium", "medium"); b != 8192 {
		t.Errorf("gpt-oss should be 8192, got %d", b)
	}
}

func TestBuildRequestShape(t *testing.T) {
	openAI := `{"model":"antigravity/gemini-3.8-flash","messages":[{"role":"system","content":"Be brief"},{"role":"user","content":"Hello"}],"temperature":0.2,"tools":[{"type":"function","function":{"name":"get_w","description":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`
	body, err := BuildRequest([]byte(openAI), "proj-123", "gemini-3.8-flash-low", "off")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if out["project"] != "proj-123" || out["model"] != "gemini-3.8-flash-low" {
		t.Errorf("project/model mismatch: %v", out)
	}
	req, _ := out["request"].(map[string]any)
	if req == nil {
		t.Fatal("missing request")
	}
	contents, _ := req["contents"].([]any)
	if len(contents) == 0 {
		t.Fatal("empty contents")
	}
	sys, _ := req["systemInstruction"].(map[string]any)
	parts, _ := sys["parts"].([]any)
	if len(parts) == 0 {
		t.Error("system instruction missing")
	}
	gen, _ := req["generationConfig"].(map[string]any)
	if gen == nil {
		t.Fatal("missing generationConfig")
	}
	tc, _ := gen["thinkingConfig"].(map[string]any)
	if tc == nil {
		t.Fatal("missing thinkingConfig")
	}
	tools, _ := req["tools"].([]any)
	if len(tools) == 0 {
		t.Error("tools missing")
	}
	if out["requestType"] != "Agent" || out["userAgent"] != "antigravity" {
		t.Errorf("envelope mismatch: %v", out)
	}
}

func TestParseChunkTextAndUsage(t *testing.T) {
	data := `{"response":{"candidates":[{"content":{"parts":[{"text":"Hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":2,"cachedContentTokenCount":1,"totalTokenCount":17}}}`
	c := ParseChunk(data)
	if !c.HasData || len(c.Texts) != 1 || c.Texts[0] != "Hello" {
		t.Fatalf("text parse: %+v", c)
	}
	if !c.Usage.HasUsage || c.Usage.Input != 9 || c.Usage.Output != 7 || c.Usage.Total != 17 {
		t.Fatalf("usage parse: %+v", c.Usage)
	}
}

func TestParseChunkToolCall(t *testing.T) {
	data := `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_w","args":{"city":"Paris"},"id":"call_1"}}]}}]}}`
	c := ParseChunk(data)
	if len(c.ToolCalls) != 1 || c.ToolCalls[0].Name != "get_w" {
		t.Fatalf("tool parse: %+v", c)
	}
}

func TestNonStreamOpenAI(t *testing.T) {
	chunks := []ParsedChunk{{Texts: []string{"Hi"}, HasData: true, Usage: Usage{Input: 3, Output: 2, Total: 5, HasUsage: true}, Finish: "STOP"}}
	body, text, prompt, completion, _, finish := NonStreamOpenAI("gemini-3.8-flash", chunks)
	if text != "Hi" || prompt != 3 || completion != 2 || finish != "stop" {
		t.Fatalf("nonstream: %q %d %d %q", text, prompt, completion, finish)
	}
	if !strings.Contains(string(body), "chat.completion") {
		t.Fatalf("body shape: %s", body)
	}
}

func TestBuildRequestDropsUnsignedToolHistoryForGemini(t *testing.T) {
	openAI := `{"model":"antigravity/gemini-3.8-flash","messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_w","arguments":"{\"city\":\"Paris\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"},
		{"role":"user","content":"thanks"}]}`
	body, err := BuildRequest([]byte(openAI), "proj-1", "gemini-3.8-flash-low", "off")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	s := string(body)
	if strings.Contains(s, "functionCall") || strings.Contains(s, "functionResponse") {
		t.Fatalf("unsigned tool history must become observations, got: %s", s)
	}
	if !strings.Contains(s, "[Observation from") || !strings.Contains(s, "sunny") {
		t.Fatalf("observation text missing: %s", s)
	}
}

func TestBuildRequestKeepsToolHistoryForClaude(t *testing.T) {
	openAI := `{"model":"antigravity/claude-sonnet-4-6","messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_w","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"}]}`
	body, err := BuildRequest([]byte(openAI), "proj-1", "claude-sonnet-4-6", "high")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, "functionCall") || !strings.Contains(s, "functionResponse") {
		t.Fatalf("claude history must keep tool parts, got: %s", s)
	}
}
