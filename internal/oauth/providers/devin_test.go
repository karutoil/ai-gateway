package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/oauth"
)

func TestDevinBuildAuthURL(t *testing.T) {
	def, _, ok := oauth.Get("devin")
	if !ok {
		t.Fatal("devin not registered")
	}
	p := oauth.NewPending(def, "devin", "pid", "http://127.0.0.1:59653/callback")
	u := oauth.AuthURL(def, p)
	for _, want := range []string{
		"/auth/cli/continue?", "redirect_uri=", "state=" + p.State,
		"prompt=select_account", "code_challenge=", "code_challenge_method=S256",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("auth url missing %q: %s", want, u)
		}
	}
	if strings.Contains(u, "client_id") {
		t.Errorf("devin authorize must not send client_id: %s", u)
	}
	oauth.ConsumePending(p.State)
}

func TestDevinExchange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/cli/token" {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["code"] == "" || body["code_verifier"] == "" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"sess-123"}`))
	}))
	defer srv.Close()
	t.Setenv("DEVIN_API_URL", srv.URL)

	def, _, ok := oauth.Get("devin")
	if !ok {
		t.Fatal("devin not registered")
	}
	p := oauth.NewPending(def, "devin", "pid", "http://127.0.0.1:59653/callback")
	tok, err := oauth.ExchangeCode(context.Background(), def, p, "code-abc", nil)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tok.Access != "sess-123" || tok.Refresh != "sess-123" {
		t.Fatalf("tokens: %+v", tok)
	}
	if tok.ExpiresAt <= 0 {
		t.Fatal("expiry missing")
	}
}

func TestDevinExchangeNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	t.Setenv("DEVIN_API_URL", srv.URL)

	def, _, _ := oauth.Get("devin")
	p := oauth.NewPending(def, "devin", "pid", "http://127.0.0.1:59653/callback")
	if _, err := oauth.ExchangeCode(context.Background(), def, p, "code-abc", nil); err == nil {
		t.Fatal("empty token should fail")
	}
}

func TestDevinRefreshNoop(t *testing.T) {
	def, hooks, ok := oauth.Get("devin")
	if !ok || hooks.DoRefresh == nil {
		t.Fatal("devin refresh hook missing")
	}
	tok, err := oauth.Refresh(context.Background(), def, "sess-123", nil)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tok.Access != "sess-123" || tok.Refresh != "sess-123" {
		t.Fatalf("refresh must return same credentials: %+v", tok)
	}
}
