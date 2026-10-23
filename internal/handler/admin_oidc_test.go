package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/config"
	"ai-gateway/internal/db"
	"ai-gateway/internal/user"

	"github.com/go-chi/chi/v5"
)

// Regression: the /api/auth/oidc endpoint once minted admin JWTs from
// client-supplied claims when OIDC_ISSUER was unset. It must refuse — always.
func TestOIDCLoginRefusesWithoutIssuer(t *testing.T) {
	t.Setenv("OIDC_ISSUER", "")
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	us := user.NewStore(database)
	cfg := &config.Config{AdminPassword: "unused", JWTSecret: []byte("test-jwt-secret-which-is-long-enough")}
	h := &AdminHandler{Config: cfg, DB: database, UserStore: us}
	r := chi.NewRouter()
	r.Post("/api/auth/oidc", h.OIDCLogin)
	srv := httptest.NewServer(r)
	defer srv.Close()

	for _, body := range []string{
		`{"org_id":"x","role":"admin"}`,
		`{"role":"admin"}`,
		`{"subject":"evil","role":"admin","org_id":""}`,
		`{"id_token":"","role":"admin"}`,
	} {
		resp, err := http.Post(srv.URL+"/api/auth/oidc", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]interface{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if resp.StatusCode == 200 || out["token"] != nil {
			t.Fatalf("oidc login must refuse without issuer, got %d %v", resp.StatusCode, out)
		}
	}
}

// Regression: admin JWTs issued via login must fail validation after the
// user's token_version advances (password change/role change/disable/delete).
func TestSessionRevocationOnCredentialChange(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	us := user.NewStore(database)
	if _, err := us.Create("admin", "adminpass-long-enough", "admin", "Admin"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AdminPassword: "unused", JWTSecret: []byte("test-jwt-secret-which-is-long-enough")}
	h := &AdminHandler{Config: cfg, DB: database, UserStore: us}
	r := chi.NewRouter()
	r.Post("/api/auth/login", h.Login)
	r.Group(func(r chi.Router) {
		r.Use(auth.AdminMiddlewareWithRevocation(cfg.JWTSecret, us))
		r.Get("/me-probe", func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(200) })
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	b, _ := json.Marshal(map[string]string{"username": "admin", "password": "adminpass-long-enough"})
	resp, err := http.Post(srv.URL+"/api/auth/login", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body.Token == "" {
		t.Fatal("no token returned")
	}

	probe := func() int {
		req, _ := http.NewRequest("GET", srv.URL+"/me-probe", nil)
		req.Header.Set("Authorization", "Bearer "+body.Token)
		pr, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		pr.Body.Close()
		return pr.StatusCode
	}
	if code := probe(); code != 200 {
		t.Fatalf("fresh session should pass, got %d", code)
	}
	if err := us.UpdatePassword(adminID(t, us), "brand-new-password-1"); err != nil {
		t.Fatal(err)
	}
	if code := probe(); code != 401 {
		t.Fatalf("session must be revoked after password change, got %d", code)
	}
}

// adminID fetches the admin user's id via the store.
func adminID(t *testing.T, us *user.Store) string {
	t.Helper()
	u, _, err := us.GetByUsername("admin")
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// TestOIDCProvisioningRequiresAllowlist pins the admission gate.
//
// Verification proves the token came from the configured issuer. It says
// nothing about whether that subject may have a gateway account — and a
// self-provisioned account is a `member`, which carries keys:create/rotate/
// delete, providers:write, mcp:write and logs:read_bodies. Without this gate,
// pointing OIDC_ISSUER at a corporate or self-service IdP turned "has an account
// somewhere" into "can read every stored prompt and mint gateway keys".
func TestOIDCProvisioningRequiresAllowlist(t *testing.T) {
	h := &AdminHandler{}
	claims := map[string]any{"email": "someone@example.com", "email_verified": true}

	// Fail closed: no allowlist configured at all.
	t.Setenv("OIDC_ALLOWED_SUBJECTS", "")
	t.Setenv("OIDC_ALLOWED_DOMAINS", "")
	if h.oidcSubjectAllowed("someone", claims) {
		t.Error("with no allowlist configured, auto-provisioning must be refused")
	}

	// Exact subject allowlist.
	t.Setenv("OIDC_ALLOWED_SUBJECTS", "alice, bob")
	if !h.oidcSubjectAllowed("bob", claims) {
		t.Error("a listed subject must be admitted")
	}
	if h.oidcSubjectAllowed("mallory", claims) {
		t.Error("an unlisted subject must be refused")
	}

	// Domain allowlist, with email_verified enforced.
	t.Setenv("OIDC_ALLOWED_SUBJECTS", "")
	t.Setenv("OIDC_ALLOWED_DOMAINS", "@example.com")
	if !h.oidcSubjectAllowed("anyone", claims) {
		t.Error("a verified email in an allowed domain must be admitted")
	}
	if h.oidcSubjectAllowed("anyone", map[string]any{"email": "x@evil.com", "email_verified": true}) {
		t.Error("a different domain must be refused")
	}
	if h.oidcSubjectAllowed("anyone", map[string]any{"email": "x@example.com", "email_verified": false}) {
		t.Error("an UNVERIFIED email must not satisfy a domain gate")
	}
	if h.oidcSubjectAllowed("anyone", map[string]any{}) {
		t.Error("a token with no email must be refused when only domains are allowed")
	}
}
