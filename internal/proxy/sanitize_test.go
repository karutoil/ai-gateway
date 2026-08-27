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
