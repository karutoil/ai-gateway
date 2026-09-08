package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
	"ai-gateway/internal/oauth"
	_ "ai-gateway/internal/oauth/providers"
	"ai-gateway/internal/provider"
)

func testAntigravityHandler(t *testing.T, upstream http.HandlerFunc) (*Handler, *models.Provider, string) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i + 7)
	}
	ps := provider.NewStore(database, mk)
	p, err := ps.CreateWithOrg("antigravity", models.ProviderAntigravity, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	full, _ := ps.GetByID(p.ID)
	// Point at the mock upstream.
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	_, _ = database.Exec(`UPDATE providers SET base_url=? WHERE id=?`, srv.URL, full.ID)
	full, _ = ps.GetByID(p.ID)
	// Fake connected OAuth (access long-lived so no refresh HTTP happens).
	tok := &oauth.Tokens{Access: "mock-access", Refresh: "mock-refresh", ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli(), Email: "u@example.com", ProjectID: "proj-1"}
	if err := ps.SetOAuthTokens(full.ID, "antigravity", tok); err != nil {
		t.Fatalf("set tokens: %v", err)
	}
	full, _ = ps.GetByID(p.ID)
	h := New(ps, database)
	return h, full, srv.URL
}

func mockAntigravitySSE(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "streamGenerateContent") {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer mock-access" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["project"] != "proj-1" {
			t.Errorf("project = %v", body["project"])
		}
		if body["model"] == "" {
			t.Error("model missing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":5,\"totalTokenCount\":15}}}\n\n"))
	}
}

func TestProxyAntigravityChatNonStream(t *testing.T) {
	h, p, _ := testAntigravityHandler(t, mockAntigravitySSE(t))
	chatBody := []byte(`{"model":"antigravity/gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	h.proxyAntigravity(w, req, chatBody, chatBody, false, "antigravity/gemini-3.8-flash", "chat.completions", "test", time.Now(), p)
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
}

func TestProxyAntigravityChatStream(t *testing.T) {
	h, p, _ := testAntigravityHandler(t, mockAntigravitySSE(t))
	chatBody := []byte(`{"model":"antigravity/gemini-3.8-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	h.proxyAntigravity(w, req, chatBody, chatBody, true, "antigravity/gemini-3.8-flash", "chat.completions", "test", time.Now(), p)
	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Hello") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream body missing content: %q", body)
	}
}

func TestProxyAntigravityMessagesNonStream(t *testing.T) {
	h, p, _ := testAntigravityHandler(t, mockAntigravitySSE(t))
	orig := []byte(`{"model":"antigravity/claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	chat := []byte(`{"model":"antigravity/claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	w := httptest.NewRecorder()
	h.proxyAntigravity(w, req, orig, chat, false, "antigravity/claude-sonnet-4-6", "messages", "test", time.Now(), p)
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

func TestFriendlyAntigravityValidationRequired(t *testing.T) {
	body := `{"error":{"code":403,"message":"Verify your account to continue.","status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"VALIDATION_REQUIRED","domain":"cloudcode-pa.googleapis.com","metadata":{"validation_error_message":"Verify your account to continue.","validation_url_link_text":"Verify your account","validation_url":"https://accounts.google.com/verify/example-long-url-that-must-not-be-truncated"}}]}}`
	msg := friendlyAntigravityMessage(403, body)
	if !strings.Contains(msg, "verification required") {
		t.Fatalf("should name verification: %q", msg)
	}
	if !strings.Contains(msg, "https://accounts.google.com/verify/example-long-url-that-must-not-be-truncated") {
		t.Fatalf("full validation URL must survive: %q", msg)
	}
	// A long body must not truncate the URL in half.
	longBody := body + strings.Repeat("x", 5000)
	msg2 := friendlyAntigravityMessage(403, longBody)
	if !strings.Contains(msg2, "https://accounts.google.com/verify/example-long-url-that-must-not-be-truncated") {
		t.Fatalf("URL must survive long bodies: %q", msg2)
	}
}
