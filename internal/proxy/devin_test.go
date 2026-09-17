package proxy

import (
	"encoding/json"
	"io"
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

// mockDevinServer emulates GetUserJwt + GetChatMessage with hand-encoded
// protobuf frames: message id, "Hello" text, usage 10/5, stop 0.
func mockDevinServer(t *testing.T) *httptest.Server {
	t.Helper()
	payload := []byte{
		0x0A, 0x06, 'r', 'e', 's', 'p', '-', '1',
		0x1A, 0x05, 'H', 'e', 'l', 'l', 'o',
		0x3A, 0x04, 0x10, 0x0A, 0x18, 0x05,
		0x28, 0x00,
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
}

func testDevinHandler(t *testing.T) (*Handler, *models.Provider) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i + 11)
	}
	ps := provider.NewStore(database, mk)
	p, err := ps.CreateWithOrg("devin", models.ProviderDevin, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	srv := mockDevinServer(t)
	t.Cleanup(srv.Close)
	_, _ = database.Exec(`UPDATE providers SET base_url=? WHERE id=?`, srv.URL, p.ID)
	full, _ := ps.GetByID(p.ID)
	tok := &oauth.Tokens{Access: "sess-123", Refresh: "sess-123", ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli()}
	if err := ps.SetOAuthTokens(full.ID, "devin", tok); err != nil {
		t.Fatalf("set tokens: %v", err)
	}
	full, _ = ps.GetByID(p.ID)
	return New(ps, database), full
}

func TestProxyDevinChatNonStream(t *testing.T) {
	h, p := testDevinHandler(t)
	chatBody := []byte(`{"model":"devin/swe-1-7","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	h.proxyDevin(w, req, chatBody, false, "devin/swe-1-7", "chat.completions", "test", time.Now(), p)
	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	choices, _ := out["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices: %v", out)
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hello" {
		t.Fatalf("content = %v", msg)
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(5) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestProxyDevinChatStream(t *testing.T) {
	h, p := testDevinHandler(t)
	chatBody := []byte(`{"model":"devin/swe-1-7","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	h.proxyDevin(w, req, chatBody, true, "devin/swe-1-7", "chat.completions", "test", time.Now(), p)
	if w.Result().StatusCode != 200 {
		t.Fatalf("status = %d", w.Result().StatusCode)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Hello") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream body missing content: %q", body)
	}
}

func TestProxyDevinMessagesNonStream(t *testing.T) {
	h, p := testDevinHandler(t)
	chatBody := []byte(`{"model":"devin/swe-1-7","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	w := httptest.NewRecorder()
	h.proxyDevin(w, req, chatBody, false, "devin/swe-1-7", "messages", "test", time.Now(), p)
	if w.Result().StatusCode != 200 {
		t.Fatalf("status = %d", w.Result().StatusCode)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["type"] != "message" {
		t.Fatalf("type = %v", out)
	}
}

func TestProxyDevinNotConnected(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	mk := make([]byte, 32)
	ps := provider.NewStore(database, mk)
	p, _ := ps.CreateWithOrg("devin", models.ProviderDevin, "", "", "")
	full, _ := ps.GetByID(p.ID)
	h := New(ps, database)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	h.proxyDevin(w, req, []byte(`{"model":"devin/swe-1-7","messages":[]}`), false, "devin/swe-1-7", "chat.completions", "test", time.Now(), full)
	if w.Result().StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Result().StatusCode)
	}
}

func TestFriendlyDevinTrailerError(t *testing.T) {
	msg := friendlyDevinTrailerError("an internal error occurred (error ID: abc)(trace ID: def)")
	if !strings.Contains(msg, "try another Devin model") {
		t.Fatalf("missing guidance: %q", msg)
	}
	if !strings.Contains(msg, "error ID: abc") || !strings.Contains(msg, "trace ID: def") {
		t.Fatalf("IDs must survive: %q", msg)
	}
	plain := friendlyDevinTrailerError("quota exhausted")
	if plain != "quota exhausted" {
		t.Fatalf("non-internal errors pass through: %q", plain)
	}
}

// swe-2 streams one logical call as head + continuation fragments with no
// id/name. They must merge into one call with valid JSON args, not one
// physical call per fragment (harness "tool not found: tool" errors).
func TestDevinToolAccumFragmentedSwe2(t *testing.T) {
	acc := newDevinToolAccum()
	frags := []string{`{`, `"target_directory": "`, `.`, `"`, `}`}
	var ids []string
	for i, f := range frags {
		id, name := "", ""
		if i == 0 {
			id, name = "call_1", "list_dir"
		}
		fragment, start, eid := acc.Add(id, name, f)
		ids = append(ids, eid)
		if i == 0 && !start {
			t.Fatalf("head must start")
		}
		if i > 0 && start {
			t.Fatalf("continuation %d must not start", i)
		}
		if fragment != f {
			t.Fatalf("fragment %d = %q want %q", i, fragment, f)
		}
		if eid != "call_1" {
			t.Fatalf("effective id %d = %q want call_1", i, eid)
		}
	}
	if got := acc.Name("call_1"); got != "list_dir" {
		t.Fatalf("name = %q want list_dir", got)
	}
	calls := acc.FinalCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d want 1: %+v", len(calls), calls)
	}
	if calls[0].Name != "list_dir" {
		t.Fatalf("call name = %q want list_dir", calls[0].Name)
	}
	raw, _ := json.Marshal(calls[0].Arguments)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("merged args invalid: %s: %v", raw, err)
	}
	if decoded["target_directory"] != "." {
		t.Fatalf("args = %s", raw)
	}
}

func TestDevinDeltasToChunksFragmentedSwe2(t *testing.T) {
	deltas := []devin.Delta{
		{Type: "tool", ID: "call_1", Name: "list_dir", ArgsJSON: `{`},
		{Type: "tool", ArgsJSON: `"target_directory": "`},
		{Type: "tool", ArgsJSON: `.`},
		{Type: "tool", ArgsJSON: `"`},
		{Type: "tool", ArgsJSON: `}`},
	}
	chunks := devinDeltasToChunks(deltas)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d want 1: %+v", len(chunks), chunks)
	}
	if len(chunks[0].ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", chunks[0].ToolCalls)
	}
	tc := chunks[0].ToolCalls[0]
	if tc.Name != "list_dir" {
		t.Fatalf("name = %q want list_dir", tc.Name)
	}
	raw, _ := json.Marshal(tc.Arguments)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("args invalid: %s: %v", raw, err)
	}
	if decoded["target_directory"] != "." {
		t.Fatalf("args = %s", raw)
	}
}

// Reasoning effort on a base id must route to the suffixed wire id
// ("swe-2" + high -> "swe-2-high"); explicit variants pass through.
func TestProxyDevinEffortRoutesToSuffixedWireID(t *testing.T) {
	var wire []byte
	payload := []byte{
		0x0A, 0x06, 'r', 'e', 's', 'p', '-', '1',
		0x1A, 0x05, 'H', 'e', 'l', 'l', 'o',
		0x28, 0x00,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/GetUserJwt"):
			w.Header().Set("Content-Type", "application/proto")
			_, _ = w.Write([]byte{0x0A, 0x0C, 'u', 's', 'e', 'r', '-', 'j', 'w', 't', '-', '1', '2', '3'})
		case strings.HasSuffix(r.URL.Path, "/GetChatMessage"):
			b, _ := io.ReadAll(r.Body)
			wire = b
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
	for i := range mk {
		mk[i] = byte(i + 11)
	}
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
	full, _ = ps.GetByID(full.ID)
	h := New(ps, database)

	wireModel := func() string {
		frames, _, err := devin.ParseFrames(wire)
		if err != nil {
			t.Fatalf("parse wire frames: %v", err)
		}
		var sb strings.Builder
		for _, fr := range frames {
			sb.Write(fr.Payload)
		}
		return sb.String()
	}

	// Base + effort routes to the suffixed wire id.
	wire = nil
	chatBody := []byte(`{"model":"devin/swe-2","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()
	h.proxyDevin(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), chatBody, false, "devin/swe-2", "chat.completions", "test", time.Now(), full)
	if w.Result().StatusCode != 200 {
		t.Fatalf("status = %d", w.Result().StatusCode)
	}
	if got := wireModel(); !strings.Contains(got, "swe-2-high") {
		t.Fatalf("wire model missing swe-2-high: %q", got)
	}

	// No effort keeps the bare id (never invents a suffix).
	wire = nil
	chatBody = []byte(`{"model":"devin/swe-2","messages":[{"role":"user","content":"hi"}]}`)
	w = httptest.NewRecorder()
	h.proxyDevin(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), chatBody, false, "devin/swe-2", "chat.completions", "test", time.Now(), full)
	if w.Result().StatusCode != 200 {
		t.Fatalf("status = %d", w.Result().StatusCode)
	}
	if got := wireModel(); !strings.Contains(got, "swe-2") || strings.Contains(got, "swe-2-") {
		t.Fatalf("bare request must send bare wire id, got %q", got)
	}

	// Explicit variant passes through verbatim.
	wire = nil
	chatBody = []byte(`{"model":"devin/swe-2-max","messages":[{"role":"user","content":"hi"}]}`)
	w = httptest.NewRecorder()
	h.proxyDevin(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), chatBody, false, "devin/swe-2-max", "chat.completions", "test", time.Now(), full)
	if w.Result().StatusCode != 200 {
		t.Fatalf("status = %d", w.Result().StatusCode)
	}
	if got := wireModel(); !strings.Contains(got, "swe-2-max") {
		t.Fatalf("wire model missing swe-2-max: %q", got)
	}
}

// A backend trailer error mid-stream must still land in request history:
// headers already flowed, so the client gets an in-band SSE error AND the
// requests tab gets a 502 row (previously the failure was invisible).
func TestProxyDevinStreamTrailerErrorIsLogged(t *testing.T) {
	payload := []byte{
		0x0A, 0x06, 'r', 'e', 's', 'p', '-', '1',
		0x1A, 0x05, 'H', 'e', 'l', 'l', 'o',
	}
	trailerJSON := []byte(`{"error":{"message":"an internal error occurred (trace ID: abc)"}}`)
	trailer := append([]byte{0x02, 0, 0, 0, byte(len(trailerJSON))}, trailerJSON...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/GetUserJwt"):
			w.Header().Set("Content-Type", "application/proto")
			_, _ = w.Write([]byte{0x0A, 0x0C, 'u', 's', 'e', 'r', '-', 'j', 'w', 't', '-', '1', '2', '3'})
		case strings.HasSuffix(r.URL.Path, "/GetChatMessage"):
			w.Header().Set("Content-Type", "application/connect+proto")
			_, _ = w.Write(devin.FrameConnect(payload))
			_, _ = w.Write(trailer)
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
	for i := range mk {
		mk[i] = byte(i + 11)
	}
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
	full, _ = ps.GetByID(full.ID)
	h := New(ps, database)

	chatBody := []byte(`{"model":"devin/swe-2","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	w := httptest.NewRecorder()
	h.proxyDevin(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), chatBody, true, "devin/swe-2", "chat.completions", "test", time.Now(), full)
	if w.Result().StatusCode != 200 {
		t.Fatalf("stream status = %d, want 200 with in-band error", w.Result().StatusCode)
	}
	if body := w.Body.String(); !strings.Contains(body, "internal error") {
		t.Fatalf("stream body missing trailer error: %q", body)
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM request_logs WHERE provider_id=? AND status=?`, full.ID, 502).Scan(&n); err != nil {
		t.Fatalf("count logs: %v", err)
	}
	if n != 1 {
		t.Fatalf("request_logs 502 rows = %d, want 1", n)
	}
}

// Per the OMP schema only StopReason MAX_TOKENS (3) means length;
// INCOMPLETE (1) is a clean stop.
func TestDevinStopReasonMapping(t *testing.T) {
	for _, d := range []devin.Delta{{Type: "stop", Stop: 1}} {
		chunks := devinDeltasToChunks([]devin.Delta{d})
		if len(chunks) != 1 || chunks[0].Finish == "MAX_TOKENS" {
			t.Fatalf("stop=1 chunks = %+v, want clean stop", chunks)
		}
	}
	chunks := devinDeltasToChunks([]devin.Delta{{Type: "stop", Stop: 3}})
	if len(chunks) != 1 || chunks[0].Finish != "MAX_TOKENS" {
		t.Fatalf("stop=3 chunks = %+v, want MAX_TOKENS", chunks)
	}
}

// A "tool" placeholder name on continuations must not clobber the head's
// real name.
func TestDevinToolAccumPlaceholderNameKept(t *testing.T) {
	acc := newDevinToolAccum()
	if _, _, _ = acc.Add("call_1", "list_dir", `{"a":`); true {
	}
	if _, start, _ := acc.Add("call_1", "tool", `1}`); start {
		t.Fatalf("continuation must not start")
	}
	if got := acc.Name("call_1"); got != "list_dir" {
		t.Fatalf("name = %q want list_dir", got)
	}
}
