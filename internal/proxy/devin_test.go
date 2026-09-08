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
