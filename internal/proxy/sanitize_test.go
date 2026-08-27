package proxy

import (
	"encoding/json"
	"reflect"
	"testing"
)

func parseMsgs(t *testing.T, body []byte) []map[string]interface{} {
	t.Helper()
	var top struct {
		Messages []map[string]interface{} `json:"messages"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("sanitized body is not valid JSON: %v", err)
	}
	return top.Messages
}

// The exact user-visible failure: tool message carries only `name`, upstream
// (ckff.dev) answers 400 "***.***.content: Invalid input".
func TestSanitizeToolMessageWithoutToolCallID(t *testing.T) {
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"tool","name":"get_weather","content":"15C"}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	got, _ := msgs[1]["tool_call_id"].(string)
	if got != "get_weather" {
		t.Fatalf("tool_call_id = %q, want name fallback %q", got, "get_weather")
	}
}

func TestSanitizeToolMessageWithoutAnyID(t *testing.T) {
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"tool","content":"15C"}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	got, _ := msgs[1]["tool_call_id"].(string)
	if got == "" {
		t.Fatal("tool_call_id not synthesized for id-less tool message")
	}
	if got != "call_unnamed_1" {
		t.Fatalf("tool_call_id = %q, want deterministic placeholder", got)
	}
}

func TestSanitizeKeepsProperToolMessageUntouched(t *testing.T) {
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"tool","tool_call_id":"call_123","content":"15C"}]}`)
	out := sanitizeOpenAICompatBody(in)
	var orig, got map[string]interface{}
	_ = json.Unmarshal(in, &orig)
	_ = json.Unmarshal(out, &got)
	if !reflect.DeepEqual(orig, got) {
		t.Fatalf("valid body mutated:\n orig=%v\n got =%v", orig, got)
	}
}

func TestSanitizeDeveloperRoleToSystem(t *testing.T) {
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"developer","content":"You are helpful."},` +
		`{"role":"user","content":"hi"}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	if role, _ := msgs[0]["role"].(string); role != "system" {
		t.Fatalf("role = %q, want system", role)
	}
	if content, _ := msgs[0]["content"].(string); content != "You are helpful." {
		t.Fatalf("content = %q, want preserved", content)
	}
}

func TestSanitizeDropsAssistantNullContentWithoutToolCalls(t *testing.T) {
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":null},` +
		`{"role":"user","content":"ok"}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	if _, present := msgs[1]["content"]; present {
		t.Fatal("content key should be dropped for assistant null content")
	}
}

func TestSanitizeKeepsAssistantNullContentWithToolCalls(t *testing.T) {
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
		`{"role":"user","content":"hi"}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	if _, present := msgs[0]["content"]; !present {
		t.Fatal("content:null must be preserved on assistant message with tool_calls")
	}
	if _, present := msgs[0]["tool_calls"]; !present {
		t.Fatal("tool_calls must be preserved")
	}
}

// Images in tool-result content arrays 400 on strict upstreams, but the SAME
// image reaches the model from a user message right after the tool message
// (verified live). The sanitizer must relocate, not drop.
func TestSanitizeRelocatesToolResultImageToUserMessage(t *testing.T) {
	const png = "data:image/png;base64,iVBORw0KGgo="
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":"screenshot?"},` +
		`{"role":"tool","tool_call_id":"c1","content":[` +
		`{"type":"text","text":"captured"},` +
		`{"type":"image_url","image_url":{"url":"` + png + `"}}]}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages (tool + synthetic user), got %d", len(msgs))
	}
	tool := msgs[1]
	if role, _ := tool["role"].(string); role != "tool" {
		t.Fatalf("msgs[1] role = %v, want tool", tool["role"])
	}
	content, _ := tool["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("tool content should keep only the text part, got %v", content)
	}
	syn := msgs[2]
	if role, _ := syn["role"].(string); role != "user" {
		t.Fatalf("synthetic message role = %v, want user", syn["role"])
	}
	synContent, _ := syn["content"].([]interface{})
	if len(synContent) != 2 {
		t.Fatalf("synthetic user content should be [text, image], got %d parts", len(synContent))
	}
	img, _ := synContent[1].(map[string]interface{})
	if img["type"] != "image_url" {
		t.Fatalf("synthetic image part type = %v", img["type"])
	}
	iu, _ := img["image_url"].(map[string]interface{})
	if iu["url"] != png {
		t.Fatalf("image url not preserved: %v", iu["url"])
	}
}

// Anthropic-style base64 image leaks in tool results must be converted to the
// OpenAI image_url shape in the synthetic user message.
func TestSanitizeRelocatesAnthropicStyleToolImage(t *testing.T) {
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"tool","tool_call_id":"c1","content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}]}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	if len(msgs) != 2 {
		t.Fatalf("want tool + synthetic user, got %d messages", len(msgs))
	}
	// Empty-after-strip tool content keeps a breadcrumb instead of [].
	toolContent, _ := msgs[0]["content"].([]interface{})
	if len(toolContent) != 1 {
		t.Fatalf("tool content should hold one breadcrumb text part, got %v", toolContent)
	}
	synContent, _ := msgs[1]["content"].([]interface{})
	if len(synContent) != 2 {
		t.Fatalf("synthetic content should be [text, image], got %d", len(synContent))
	}
	img, _ := synContent[1].(map[string]interface{})
	iu, _ := img["image_url"].(map[string]interface{})
	want := "data:image/png;base64,iVBOR"
	if iu["url"] != want {
		t.Fatalf("converted url = %v, want %v", iu["url"], want)
	}
}

// Unknown non-image blocks in tool content become text placeholders on the
// tool message itself (no synthetic user message needed).
func TestSanitizePlaceholderForUnknownToolBlocks(t *testing.T) {
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"tool","tool_call_id":"c1","content":[` +
		`{"type":"text","text":"a"},` +
		`{"type":"tool_result","content":[{"type":"text","text":"nested"}]}]}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	if len(msgs) != 1 {
		t.Fatalf("no synthetic message expected, got %d total", len(msgs))
	}
	content, _ := msgs[0]["content"].([]interface{})
	if len(content) != 2 {
		t.Fatalf("tool content = %v, want [text, placeholder-text]", content)
	}
}

// --- probe battery round 2: legacy/unknown roles, tool_calls repair, part fixes ---

func TestSanitizeLegacyFunctionRoleToTool(t *testing.T) {
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":"weather?"},` +
		`{"role":"function","name":"get_weather","content":"15C"}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	if role, _ := msgs[1]["role"].(string); role != "tool" {
		t.Fatalf("role = %q, want tool", role)
	}
	if id, _ := msgs[1]["tool_call_id"].(string); id != "get_weather" {
		t.Fatalf("tool_call_id = %q, want synthesized from name", id)
	}
}

func TestSanitizeUnknownRoleToUser(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"agent","content":"hi"}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	if role, _ := msgs[0]["role"].(string); role != "user" {
		t.Fatalf("role = %q, want user", role)
	}
	if content, _ := msgs[0]["content"].(string); content != "hi" {
		t.Fatalf("content = %q, want preserved", content)
	}
}

func TestSanitizeCanonicalRolesUntouched(t *testing.T) {
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"system","content":"s"},{"role":"user","content":"u"},` +
		`{"role":"assistant","content":"a"},{"role":"tool","tool_call_id":"c","content":"t"}]}`)
	out := sanitizeOpenAICompatBody(in)
	var orig, got map[string]interface{}
	_ = json.Unmarshal(in, &orig)
	_ = json.Unmarshal(out, &got)
	if !reflect.DeepEqual(orig, got) {
		t.Fatalf("canonical body mutated:\n orig=%v\n got =%v", orig, got)
	}
}

func TestSanitizeDropsToolCallWithoutFunction(t *testing.T) {
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function"},{"id":"c2","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	calls, _ := msgs[1]["tool_calls"].([]interface{})
	if len(calls) != 1 {
		t.Fatalf("want 1 surviving tool_call, got %d", len(calls))
	}
	c := calls[0].(map[string]interface{})
	fn := c["function"].(map[string]interface{})
	if fn["name"] != "f" {
		t.Fatalf("wrong survivor: %v", c)
	}
}

func TestSanitizeCustomToolCallToFunction(t *testing.T) {
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"custom","custom":{"name":"browser"}}]},` +
		`{"role":"user","content":"hi"}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	calls, _ := msgs[0]["tool_calls"].([]interface{})
	if len(calls) != 1 {
		t.Fatalf("want 1 tool_call, got %d", len(calls))
	}
	c := calls[0].(map[string]interface{})
	if c["type"] != "function" {
		t.Fatalf("type = %v, want function", c["type"])
	}
	fn := c["function"].(map[string]interface{})
	if fn["name"] != "browser" {
		t.Fatalf("name = %v, want browser", fn["name"])
	}
	if fn["arguments"] != "{}" {
		t.Fatalf("arguments = %v, want {}", fn["arguments"])
	}
}

func TestSanitizeAddsMissingToolCallArguments(t *testing.T) {
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f"}}]}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	calls, _ := msgs[0]["tool_calls"].([]interface{})
	fn := calls[0].(map[string]interface{})["function"].(map[string]interface{})
	if fn["arguments"] != "{}" {
		t.Fatalf("arguments = %v, want synthesized {}", fn["arguments"])
	}
}

func TestSanitizeDropsAllOrphanToolCalls(t *testing.T) {
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function"}]},` +
		`{"role":"user","content":"hi"}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	if _, present := msgs[0]["tool_calls"]; present {
		t.Fatal("all-invalid tool_calls should be removed entirely")
	}
}

// input_text / input_image (Responses-style) parts are silently DROPPED by
// the upstream — content loss, or "Prompt must contain at least one message"
// when nothing remains. The sanitizer must re-type them.
func TestSanitizeReTypesResponsesStyleParts(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"input_text","text":"see this"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,iVBOR"}]}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	content, _ := msgs[0]["content"].([]interface{})
	if len(content) != 2 {
		t.Fatalf("want 2 parts, got %d", len(content))
	}
	p0 := content[0].(map[string]interface{})
	if p0["type"] != "text" || p0["text"] != "see this" {
		t.Fatalf("part0 = %v, want re-typed text", p0)
	}
	p1 := content[1].(map[string]interface{})
	if p1["type"] != "image_url" {
		t.Fatalf("part1 type = %v, want image_url", p1["type"])
	}
	iu := p1["image_url"].(map[string]interface{})
	if iu["url"] != "data:image/png;base64,iVBOR" {
		t.Fatalf("url = %v", iu["url"])
	}
}

func TestSanitizeUnTypedTextPart(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"user","content":[{"text":"hi"}]}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	content, _ := msgs[0]["content"].([]interface{})
	p := content[0].(map[string]interface{})
	if p["type"] != "text" || p["text"] != "hi" {
		t.Fatalf("part = %v, want {type:text,text:hi}", p)
	}
}

// audio/file parts are hard-rejected (400 Invalid input) — must become text
// placeholders, not forwarded.
func TestSanitizeAudioAndFilePartsOmitted(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"listen"},` +
		`{"type":"input_audio","input_audio":{"data":"c29tZQ==","format":"wav"}},` +
		`{"type":"file","file":{"filename":"a.txt","file_data":"data:text/plain;base64,SGk="}}]}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	content, _ := msgs[0]["content"].([]interface{})
	if len(content) != 3 {
		t.Fatalf("want 3 parts, got %d", len(content))
	}
	for i, want := range []string{"listen", "[audio attachment omitted", "[file attachment omitted"} {
		p := content[i].(map[string]interface{})
		if p["type"] != "text" {
			t.Fatalf("part%d type = %v, want text", i, p["type"])
		}
		txt, _ := p["text"].(string)
		if len(txt) < len(want) || txt[:len(want)] != want {
			t.Fatalf("part%d text = %q, want prefix %q", i, txt, want)
		}
	}
}

func TestChatMessagesPresent(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, true},
		{`{"model":"m","messages":[]}`, false},
		{`{"model":"m"}`, false},
		{`{"messages":"weird"}`, false},
		{`not json`, true}, // opaque bodies are not this check's business
	}
	for _, c := range cases {
		if got := chatMessagesPresent([]byte(c.body)); got != c.want {
			t.Errorf("chatMessagesPresent(%s) = %v, want %v", c.body, got, c.want)
		}
	}
}

func TestSanitizePassthroughOpaqueBody(t *testing.T) {
	// Not an object with a messages array — relay byte-for-byte.
	for _, in := range []string{
		`{"prompt":"hi","model":"m"}`,
		`not json at all`,
		`{"messages":"not-an-array"}`,
		`[]`,
	} {
		if out := sanitizeOpenAICompatBody([]byte(in)); string(out) != in {
			t.Fatalf("opaque body %q changed to %q", in, out)
		}
	}
}

func TestSanitizePreservesOtherTopLevelFields(t *testing.T) {
	in := []byte(`{"model":"m","temperature":0.7,"stream":true,"messages":[` +
		`{"role":"developer","content":"sys"},{"role":"user","content":"hi"}]}`)
	out := sanitizeOpenAICompatBody(in)
	var top map[string]interface{}
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	if top["temperature"] != 0.7 || top["stream"] != true || top["model"] != "m" {
		t.Fatalf("top-level fields lost: %v", top)
	}
}
