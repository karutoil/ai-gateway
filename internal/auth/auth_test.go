package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMakeAndVerifyToken(t *testing.T) {
	secret := []byte("test-jwt-secret-which-is-long-enough")
	tok, err := MakeTokenWithOrg(secret, "alice", "org1", "member")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := VerifyToken(secret, tok)
	if err != nil {
		t.Fatal(err)
	}
	if claims["sub"] != "alice" || claims["role"] != "member" || claims["org_id"] != "org1" {
		t.Fatalf("claims %#v", claims)
	}
}

func TestPasswordEqual(t *testing.T) {
	if !PasswordEqual("abc", "abc") {
		t.Fatal("equal")
	}
	if PasswordEqual("abc", "abd") {
		t.Fatal("not equal")
	}
}

func TestAdminMiddleware(t *testing.T) {
	secret := []byte("test-jwt-secret-which-is-long-enough")
	tok, err := MakeTokenWithOrg(secret, "bob", "", "readonly")
	if err != nil {
		t.Fatal(err)
	}
	h := AdminMiddleware(secret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if GetSubject(r) != "bob" || GetRole(r) != "readonly" {
			t.Fatalf("ctx %s %s", GetSubject(r), GetRole(r))
		}
		w.WriteHeader(200)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestAdminMiddlewareRejectsBadToken(t *testing.T) {
	secret := []byte("test-jwt-secret-which-is-long-enough")
	h := AdminMiddleware(secret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	req.Header.Set("Authorization", "Bearer not-a-jwt")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("status %d", rec.Code)
	}
}

// SECURITY: the PAT authentication path must fail closed when the wired
// identity resolver returns a role outside the allowlist (or an empty
// username) — previously any string flowed into the role context.
func TestAdminMiddlewarePATRejectsInvalidRole(t *testing.T) {
	SetPATAuthenticator(
		func(raw string) (string, string, bool) { return "user-1", "", true },
		func(userID string) (string, string, error) { return "legacyuser", "superuser", nil },
	)
	t.Cleanup(func() { SetPATAuthenticator(nil, nil) })

	h := AdminMiddleware([]byte("test-jwt-secret-which-is-long-enough"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/anything", nil)
	req.Header.Set("Authorization", "Bearer gwp_sometokenvalue")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("PAT with non-allowlisted role must be rejected, got %d", rec.Code)
	}
}

func TestAdminMiddlewarePATValidRolePasses(t *testing.T) {
	SetPATAuthenticator(
		func(raw string) (string, string, bool) { return "user-1", "logs:read", true },
		func(userID string) (string, string, error) { return "alice", "member", nil },
	)
	t.Cleanup(func() { SetPATAuthenticator(nil, nil) })

	var gotRole, gotSubject, gotScopes string
	h := AdminMiddleware([]byte("test-jwt-secret-which-is-long-enough"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRole, gotSubject, gotScopes = GetRole(r), GetSubject(r), PATScopes(r)
		w.WriteHeader(200)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/anything", nil)
	req.Header.Set("Authorization", "Bearer gwp_anothertokenvalue")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("valid PAT must pass, got %d", rec.Code)
	}
	if gotRole != "member" || gotSubject != "alice" || gotScopes != "logs:read" {
		t.Fatalf("context wrong: role=%q subject=%q scopes=%q", gotRole, gotSubject, gotScopes)
	}
}
