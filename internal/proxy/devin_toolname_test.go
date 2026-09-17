package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/db"
	"ai-gateway/internal/devin"
	"ai-gateway/internal/models"
	"ai-gateway/internal/oauth"
	_ "ai-gateway/internal/oauth/providers"
	"ai-gateway/internal/provider"
)

// A head frame with no tool name must never surface the literal placeholder
// "tool" to harnesses — no such tool exists and every harness rejects the
// turn with "unknown tool: tool", poisoning the agent loop into retries.
func TestDevinToolAccumNeverEmitsPlaceholderName(t *testing.T) {
	acc := newDevinToolAccum()
	if _, _, _ = acc.Add("call_1", "", `{"a":`); true {
	}
	if _, _, _ = acc.Add("", "", `1}`); true {
	}
	if got := acc.Name("call_1"); got == "tool" {
		t.Fatalf("Name must not fall back to %q", got)
	}
	// A completely unnamed call must be dropped from the final payload,
	// not emitted with a fake name and (likely invalid) args.
	if calls := acc.FinalCalls(); len(calls) != 0 {
		t.Fatalf("unnamed calls must be dropped, got %+v", calls)
	}
}

// A late-arriving real name (after an unnamed head) must be adopted.
func TestDevinToolAccumAdoptsLateName(t *testing.T) {
	acc := newDevinToolAccum()
	if _, _, _ = acc.Add("call_1", "", `{"a":`); true {
	}
	if _, _, _ = acc.Add("call_1", "get_w", `1}`); true {
	}
	if got := acc.Name("call_1"); got != "get_w" {
		t.Fatalf("name = %q want get_w", got)
	}
	calls := acc.FinalCalls()
	if len(calls) != 1 || calls[0].Name != "get_w" {
		t.Fatalf("calls = %+v", calls)
	}
	raw, _ := json.Marshal(calls[0].Arguments)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded["a"] != float64(1) {
		t.Fatalf("args = %s", raw)
	}
}

// Cumulative payloads (each ArgsJSON is the full object so far) must replace,
// not append: appending two individually-valid objects corrupts the args into
// invalid JSON, which FinalCalls then degrades to {} and harnesses reject
// with "missing required property" errors.
func TestDevinToolAccumCumulativePayloadsReplace(t *testing.T) {
	acc := newDevinToolAccum()
	if _, _, _ = acc.Add("call_1", "get_w", `{"a":1}`); true {
	}
	if _, _, _ = acc.Add("call_1", "get_w", `{"a":1,"b":2}`); true {
	}
	calls := acc.FinalCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	raw, _ := json.Marshal(calls[0].Arguments)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("merged args invalid: %s", raw)
	}
	if decoded["a"] != float64(1) || decoded["b"] != float64(2) {
		t.Fatalf("args = %s", raw)
	}
}

// End to end: fragmented swe-2 tool frames (head + nameless continuations)
// through proxyDevin must yield one tool call with the real name and merged
// args — never the placeholder "tool", never split fragments.
func TestProxyDevinFragmentedToolCallEndToEnd(t *testing.T) {
	toolMsg := func(id, name, args string) []byte {
		var inner []byte
		if id != "" {
			inner = append(inner, 0x0A, byte(len(id)))
			inner = append(inner, []byte(id)...)
		}
		if name != "" {
			inner = append(inner, 0x12, byte(len(name)))
			inner = append(inner, []byte(name)...)
		}
		inner = append(inner, 0x1A, byte(len(args)))
		inner = append(inner, []byte(args)...)
		out := []byte{0x32, byte(len(inner))}
		return append(out, inner...)
	}
	payload := []byte{0x0A, 0x06, 'r', 'e', 's', 'p', '-', '1'}
	payload = append(payload, toolMsg("call_1", "list_dir", `{`)...)
	payload = append(payload, toolMsg("", "", `"target_directory": "."}`)...)
	payload = append(payload, 0x28, 0x00) // stop 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/GetUserJwt"):
			w.Header().Set("Content-Type", "application/proto")
			_, _ = w.Write([]byte{0x0A, 0x0C, 'u', 's', 'e', 'r', '-', 'j', 'w', 't', '-', '1', '2', '3'})
		case strings.HasSuffix(r.URL.Path, "/GetChatMessage"):
			w.Header().Set("Content-Type", "application/connect+proto")
			_, _ = w.Write(devin.FrameConnect(payload))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	mk := make([]byte, 32)
	ps := provider.NewStore(database, mk)
	p, err := ps.CreateWithOrg("devin", models.ProviderDevin, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, _ = database.Exec(`UPDATE providers SET base_url=? WHERE id=?`, srv.URL, p.ID)
	full, _ := ps.GetByID(p.ID)
	tok := &oauth.Tokens{Access: "sess-123", Refresh: "sess-123", ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli()}
	if err := ps.SetOAuthTokens(full.ID, "devin", tok); err != nil {
		t.Fatalf("set tokens: %v", err)
	}
	full, _ = ps.GetByID(p.ID)
	h := New(ps, database)

	chatBody := []byte(`{"model":"devin/swe-2","messages":[{"role":"user","content":"list files"}],
		"tools":[{"type":"function","function":{"name":"list_dir","description":"list","parameters":{"type":"object","properties":{"target_directory":{"type":"string"}}}}}]}`)

	for _, stream := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		w := httptest.NewRecorder()
		h.proxyDevin(w, req, chatBody, stream, "devin/swe-2", "chat.completions", "test", time.Now(), full)
		if w.Code != 200 {
			t.Fatalf("stream=%v status = %d", stream, w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, `"name":"list_dir"`) {
			t.Fatalf("stream=%v missing real tool name:\n%s", stream, body)
		}
		if !strings.Contains(body, `target_directory`) {
			t.Fatalf("stream=%v missing merged args:\n%s", stream, body)
		}
		if strings.Contains(body, `"name":"tool"`) {
			t.Fatalf("stream=%v leaked placeholder tool name:\n%s", stream, body)
		}
	}
}
func TestDevinDeltasToChunksDropsUnnamedTools(t *testing.T) {
	deltas := []devin.Delta{
		{Type: "tool", ID: "call_1", ArgsJSON: `{`},
		{Type: "tool", ArgsJSON: `"a":1}`},
	}
	chunks := devinDeltasToChunks(deltas)
	for _, c := range chunks {
		for _, tc := range c.ToolCalls {
			if tc.Name == "tool" || tc.Name == "" {
				t.Fatalf("placeholder tool call leaked: %+v", tc)
			}
		}
		if len(c.ToolCalls) > 0 {
			t.Fatalf("unnamed deltas must not produce tool calls: %+v", c.ToolCalls)
		}
	}
}
