package oauth

import (
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
