package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"ai-gateway/internal/translate"
)

// Audit remediation: synthesized tool-call IDs must be unique per call
// (PROTO-008). Parallel calls to the same function with missing IDs must
// not share one deterministic ID.
func TestAuditSynthToolIDsUnique(t *testing.T) {
	// Anthropic -> OpenAI direction via public API.
	aReq := `{"model":"claude-3","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":[{"type":"tool_use","name":"search","input":{"q":"a"}},{"type":"tool_use","name":"search","input":{"q":"b"}}]}]}`
	out, _, err := translate.AnthropicToOpenAI([]byte(aReq))
	if err != nil {
		t.Fatal(err)
	}
	var oReq map[string]any
	if err := json.Unmarshal(out, &oReq); err != nil {
		t.Fatal(err)
	}
	msgs, _ := oReq["messages"].([]any)
	ids := map[string]bool{}
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		tcs, _ := mm["tool_calls"].([]any)
		for _, tc := range tcs {
			tcm, _ := tc.(map[string]any)
			id, _ := tcm["id"].(string)
			if id == "" {
				continue
			}
			if ids[id] {
				t.Fatalf("duplicate synthesized tool_call id %q", id)
			}
			ids[id] = true
		}
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 unique tool_call ids, got %v", ids)
	}

	// OpenAI -> Anthropic direction via public API.
	oChat := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"search","arguments":"{}"}},{"type":"function","function":{"name":"search","arguments":"{}"}}]}]}`
	aOut, _, err := translate.OpenAIToAnthropic([]byte(oChat))
	if err != nil {
		t.Fatal(err)
	}
	var aResp map[string]any
	if err := json.Unmarshal(aOut, &aResp); err != nil {
		t.Fatal(err)
	}
	amsgs, _ := aResp["messages"].([]any)
	aid := map[string]bool{}
	for _, m := range amsgs {
		mm, _ := m.(map[string]any)
		content, _ := mm["content"].([]any)
		for _, item := range content {
			bm, _ := item.(map[string]any)
			if bm["type"] == "tool_use" {
				id, _ := bm["id"].(string)
				if aid[id] {
					t.Fatalf("duplicate tool_use id %q", id)
				}
				aid[id] = true
			}
		}
	}
	if len(aid) != 2 {
		t.Fatalf("expected 2 unique tool_use ids, got %v", aid)
	}
}

// Audit remediation: toInt must clamp negative usage to zero (ACCT-005)
// so a malicious upstream cannot decrement quotas.
func TestAuditToIntClampsNegative(t *testing.T) {
	for _, v := range []any{float64(-5), int(-3), int64(-9)} {
		if got := toInt(v); got != 0 {
			t.Fatalf("toInt(%v) = %d, want 0", v, got)
		}
	}
	if got := toInt(float64(12.7)); got != 12 {
		t.Fatalf("toInt(12.7) = %d, want 12", got)
	}
	var _ = strings.Contains
}
