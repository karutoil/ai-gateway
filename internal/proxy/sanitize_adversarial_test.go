package proxy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Adversarial inputs must never panic, must always produce valid JSON (or the
// original bytes), and must not corrupt unrelated fields.
func TestSanitizeAdversarialInputs(t *testing.T) {
	inputs := []string{
		`{"messages":[{"role":"user","content":[]}]}`,
		`{"messages":[{"role":"user","content":5}]}`,
		`{"messages":[{"role":"user","content":true}]}`,
		`{"messages":[{"content":[1,2,3]}]}`,
		`{"messages":[5,6,7]}`,
		`{"messages":["hello"]}`,
		`{"messages":{}}`,
		`{"messages":null}`,
		`{"messages":[{"role":"assistant","tool_calls":"not-an-array","content":null}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[null,5,"x"]}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"function":"not-object"}]}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":123}}]}]}`,
		`{"messages":[{"role":"tool","tool_call_id":[1,2],"content":"x"}]}`,
		`{"messages":[{"role":"tool","content":[[1,2],[3,4]]}]}`,
		`{"messages":[{"role":"tool","content":[{"type":123,"text":"x"}]}]}`,
		`{"messages":[{"role":"tool","content":[{"type":"image_url"}]}]}`,
		`{"messages":[{"role":"tool","content":[{"type":"image_url","image_url":5}]}]}`,
		`{"messages":[{"role":"tool","content":[{"type":"image","source":5}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"input_text"}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"input_image","image_url":{}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":null,"text":"x"}]}]}`,
		`{"messages":[{"role":"developer","content":null}]}`,
		`{"messages":[{"role":123,"content":"x"}]}`,
		`{"messages":[{"role":"","content":""}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":123}]}]}`,
		`{"messages":[{"role":"tool","name":123,"content":"x"}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":{"complex":"object"}}}]}]}`,
	}
	for _, in := range inputs {
		out := sanitizeOpenAICompatBody([]byte(in))
		if string(out) != in {
			// changed: must still be valid JSON with a messages array
			var top map[string]json.RawMessage
			if err := json.Unmarshal(out, &top); err != nil {
				t.Errorf("input %s produced invalid JSON: %v\nout: %s", in, err, out)
				continue
			}
		}
	}
}

func TestSanitizeEmptyContentArrayAllRoles(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"user","content":[]}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	content, _ := msgs[0]["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("empty content array should gain a text part, got %v", content)
	}
}

func TestSanitizeToolCallsMissingID(t *testing.T) {
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":null,"tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"}}]},` +
		`{"role":"user","content":"hi"}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	calls, _ := msgs[0]["tool_calls"].([]interface{})
	c := calls[0].(map[string]interface{})
	if id, _ := c["id"].(string); id == "" {
		t.Fatalf("tool_call id should be synthesized, got %v", c["id"])
	}
}

func TestSanitizeSanitizeIdempotent(t *testing.T) {
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"developer","content":"sys"},` +
		`{"role":"user","content":"pic please"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"shot","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":"ok"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBOR"}}]},` +
		`{"role":"function","name":"legacy","content":"res"}]}`)
	once := sanitizeOpenAICompatBody(in)
	twice := sanitizeOpenAICompatBody(once)
	var a, b interface{}
	if err := json.Unmarshal(once, &a); err != nil {
		t.Fatalf("first pass invalid: %v", err)
	}
	if err := json.Unmarshal(twice, &b); err != nil {
		t.Fatalf("second pass invalid: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("sanitizer not idempotent:\nonce : %s\ntwice: %s", once, twice)
	}
}

func TestSanitizeSyntheticMessageFields(t *testing.T) {
	// The synthetic user message must not carry stray fields and the image
	// marker must be a string.
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"tool","tool_call_id":"c1","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBOR"}}]}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	if len(msgs) != 2 {
		t.Fatalf("want tool + synthetic user, got %d", len(msgs))
	}
	syn := msgs[1]
	if role, _ := syn["role"].(string); role != "user" {
		t.Fatalf("synthetic role = %v", syn["role"])
	}
	for k := range syn {
		if k != "role" && k != "content" {
			t.Fatalf("synthetic message has unexpected key %q", k)
		}
	}
}

func TestSanitizeHugePayloadPerf(t *testing.T) {
	// 2MB body with a handful of messages: sanitizer must stay fast and not
	// blow memory. Not a benchmark — just a guard against accidental O(n^2).
	big := strings.Repeat("x", 2_000_000)
	in := []byte(`{"model":"m","messages":[` +
		`{"role":"system","content":"` + big + `"},` +
		`{"role":"user","content":"hi"},` +
		`{"role":"tool","tool_call_id":"c1","content":"res"}]}`)
	out := sanitizeOpenAICompatBody(in)
	msgs := parseMsgs(t, out)
	if len(msgs) != 3 {
		t.Fatalf("message count changed for big payload: %d", len(msgs))
	}
	if s, _ := msgs[0]["content"].(string); len(s) != len(big) {
		t.Fatal("big content corrupted")
	}
}

func FuzzSanitizeOpenAICompatBody(f *testing.F) {
	seeds := []string{
		`{"messages":[{"role":"user","content":"hi"}]}`,
		`{"messages":[{"role":"tool","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBOR"}}]}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"custom","custom":{"name":"f"}}]}]}`,
		`{"messages":[{"role":"function","name":"f","content":"x"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"input_text","text":"a"},{"type":"input_audio","input_audio":{"data":"x"}}]}]}`,
		`{"messages":[]}`,
		`not json`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"\u0000\ufffe"}]}]}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out := sanitizeOpenAICompatBody([]byte(in))
		if string(out) == in {
			return
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(out, &top); err != nil {
			t.Errorf("sanitize produced invalid JSON from %q: %v", in, err)
		}
	})
}
