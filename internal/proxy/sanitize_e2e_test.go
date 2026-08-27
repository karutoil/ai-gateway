package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// E2E: the sanitizer must run on the wire path. A strict upstream (the
// hygiene fixture) asserts the shapes that ckff.dev 400s on arrive repaired:
//   - tool message without tool_call_id → synthesized tool_call_id
//   - role "developer" → "system"
//   - assistant content:null (no tool_calls) → key dropped
func TestChatCompletionsSanitizesBeforeForwarding(t *testing.T) {
	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	up := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("upstream got non-JSON body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, chatCompletionOK)
	}
	srv, key := newHygieneEnv(t, up, func(hh *Handler) http.HandlerFunc { return hh.ChatCompletions }, "/v1/chat/completions")
	resp := postHygiene(t, srv.URL+"/v1/chat/completions", key, `{"model":"gpt-4o-mini","messages":[`+
		`{"role":"developer","content":"sys"},`+
		`{"role":"user","content":"hi"},`+
		`{"role":"assistant","content":null},`+
		`{"role":"tool","name":"get_weather","content":"15C"}]}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
	io.Copy(io.Discard, resp.Body)

	if len(got.Messages) != 4 {
		t.Fatalf("want 4 messages, got %d", len(got.Messages))
	}
	if role, _ := got.Messages[0]["role"].(string); role != "system" {
		t.Errorf("developer role not rewritten, got %q", role)
	}
	if _, present := got.Messages[2]["content"]; present {
		t.Errorf("assistant content:null not dropped")
	}
	id, _ := got.Messages[3]["tool_call_id"].(string)
	if id != "get_weather" {
		t.Errorf("tool_call_id = %q, want synthesized from name", id)
	}
}
