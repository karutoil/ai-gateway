package oauth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestExpiredWithSkew(t *testing.T) {
	now := time.Now()
	if (&Tokens{Access: ""}).Expired(now) != true {
		t.Fatal("empty access should be expired")
	}
	if (&Tokens{Access: "x", ExpiresAt: now.UnixMilli() + 10*60*1000}).Expired(now) {
		t.Fatal("future token should not be expired")
	}
	if !(&Tokens{Access: "x", ExpiresAt: now.UnixMilli() + 60*1000}).Expired(now) {
		t.Fatal("token within 5m skew should count as expired")
	}
}

func TestAuthURLPKCE(t *testing.T) {
	def := Definition{
		ID: "test", AuthURL: "https://example.com/auth",
		TokenURL: "https://example.com/token",
		Scopes:   []string{"a", "b"},
		ClientID: "cid", UsePKCE: true,
		ExtraAuthParams: map[string]string{"access_type": "offline"},
	}
	p := NewPending(def, "prov", "pid", "http://localhost:51121/oauth-callback")
	u := AuthURL(def, p)
	for _, want := range []string{"client_id=cid", "code_challenge=", "code_challenge_method=S256", "state=" + p.State, "access_type=offline", "redirect_uri="} {
		found := false
		for _, part := range []string{u} {
			if len(part) > 0 && contains(part, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("auth url missing %q: %s", want, u)
		}
	}
	if _, ok := ConsumePending(p.State); !ok {
		t.Fatal("pending should be consumable once")
	}
	if _, ok := ConsumePending(p.State); ok {
		t.Fatal("pending must be single-use")
	}
}

func TestParseCallbackURL(t *testing.T) {
	code, err := ParseCallbackURL("http://localhost:51121/oauth-callback?state=abc&code=xyz", "abc")
	if err != nil || code != "xyz" {
		t.Fatalf("parse full url: %v %q", err, code)
	}
	code, err = ParseCallbackURL("state=abc&code=xyz", "abc")
	if err != nil || code != "xyz" {
		t.Fatalf("parse bare query: %v %q", err, code)
	}
	if _, err := ParseCallbackURL("http://localhost:51121/oauth-callback?state=abc&code=xyz", "other"); err == nil {
		t.Fatal("state mismatch should fail")
	}
	if _, err := ParseCallbackURL("http://localhost:51121/oauth-callback?state=abc", "abc"); err == nil {
		t.Fatal("missing code should fail")
	}
}

func TestRegistry(t *testing.T) {
	Register(Definition{ID: "dummy-test", Name: "Dummy"}, Hooks{})
	def, _, ok := Get("dummy-test")
	if !ok || def.Name != "Dummy" {
		t.Fatal("registry get failed")
	}
	found := false
	for _, d := range List() {
		if d.ID == "dummy-test" {
			found = true
			if d.Name != "Dummy" {
				t.Fatal("list name mismatch")
			}
		}
	}
	if !found {
		t.Fatal("registry list missing dummy")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// doTokenRequest must store the RAW expiry: the 5-minute refresh skew lives
// in Tokens.Expired (read side). Subtracting it at write time as well made
// every token refresh 10 minutes early — doubling the refresh cadence and
// the invalid_grant exposure for short-lived grants.
func TestDoTokenRequestStoresRawExpiry(t *testing.T) {
	srv := newTokenServer(t, `{"access_token":"a1","refresh_token":"r1","expires_in":600}`)
	defer srv.close()
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", "r1")
	tok, err := doTokenRequest(context.Background(), srv.url, form, srv.client())
	if err != nil {
		t.Fatal(err)
	}
	want := srv.now.UnixMilli() + 600*1000
	if d := tok.ExpiresAt - want; d < -2000 || d > 2000 {
		t.Fatalf("ExpiresAt = %d, want ~%d (raw expiry, no write-side skew): delta %d", tok.ExpiresAt, want, d)
	}
	// Read-side skew still applies: the token counts as expired 5m early.
	if !tok.Expired(time.UnixMilli(want - 4*60*1000)) {
		t.Fatal("token should be expired 4 minutes before raw expiry (5m read skew)")
	}
	if tok.Expired(time.UnixMilli(want - 6*60*1000)) {
		t.Fatal("token must not be expired 6 minutes before raw expiry")
	}
}

type tokenServer struct {
	url    string
	now    time.Time
	closed func()
}

func (s *tokenServer) close() { s.closed() }

func (s *tokenServer) client() *http.Client { return http.DefaultClient }

func newTokenServer(t *testing.T, body string) *tokenServer {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	return &tokenServer{url: ts.URL, now: time.Now(), closed: ts.Close}
}
