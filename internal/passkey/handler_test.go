package passkey

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/config"
	"ai-gateway/internal/db"
	"ai-gateway/internal/user"
)

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	userStore := user.NewStore(database)
	h, err := NewHandler(database, userStore, &config.Config{JWTSecret: []byte("test-jwt-secret-which-is-long-enough")})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func reqWithRole(method, target, subject, role string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	ctx := auth.WithRole(r.Context(), role)
	return r.WithContext(auth.WithSubject(ctx, subject))
}

// SECURITY: GenerateRecovery/DisablePasskey must fail closed when the
// caller's subject cannot be resolved to a user. The previous guard only
// rejected mismatches it could SEE (lookup success); a lookup failure let
// any unresolved non-admin caller act on an arbitrary user_id — regenerating
// the victim's recovery code, i.e. full account takeover via VerifyRecovery.
func TestGenerateRecoveryFailsClosedForUnresolvedSubject(t *testing.T) {
	h := newTestHandler(t)
	victim, err := h.UserStore.Create("victim", "password-123", "member", "")
	if err != nil {
		t.Fatal(err)
	}

	// Unresolved non-admin subject targeting another user → 403.
	r := reqWithRole(http.MethodPost, "/api/auth/passkey/recovery/generate?user_id="+victim.ID, "ghost", "member")
	rec := httptest.NewRecorder()
	h.GenerateRecovery(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unresolved subject must be forbidden, got %d body %s", rec.Code, rec.Body.String())
	}

	// Same for DisablePasskey.
	r = reqWithRole(http.MethodPost, "/api/auth/passkey/disable?user_id="+victim.ID, "ghost", "member")
	rec = httptest.NewRecorder()
	h.DisablePasskey(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("DisablePasskey: unresolved subject must be forbidden, got %d", rec.Code)
	}

	// Self is allowed (subject resolves to the target).
	r = reqWithRole(http.MethodPost, "/api/auth/passkey/recovery/generate?user_id="+victim.ID, "victim", "member")
	rec = httptest.NewRecorder()
	h.GenerateRecovery(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("self recovery generation must succeed, got %d body %s", rec.Code, rec.Body.String())
	}

	// Admin may target any user.
	r = reqWithRole(http.MethodPost, "/api/auth/passkey/recovery/generate?user_id="+victim.ID, "someadmin", "admin")
	rec = httptest.NewRecorder()
	h.GenerateRecovery(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin recovery generation must succeed, got %d body %s", rec.Code, rec.Body.String())
	}

	// A resolved non-admin member targeting a DIFFERENT user is still forbidden.
	other, err := h.UserStore.Create("other", "password-456", "member", "")
	if err != nil {
		t.Fatal(err)
	}
	r = reqWithRole(http.MethodPost, "/api/auth/passkey/recovery/generate?user_id="+other.ID, "victim", "member")
	rec = httptest.NewRecorder()
	h.GenerateRecovery(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-user recovery generation must be forbidden, got %d", rec.Code)
	}
}

// beginLoginFor drives BeginLogin and returns the decoded response map.
func beginLoginFor(t *testing.T, h *Handler, username string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/passkey/login/begin", strings.NewReader(`{"username":`+jsonQuote(username)+`}`))
	rec := httptest.NewRecorder()
	h.BeginLogin(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("begin login for %q: status %d body %s", username, rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v (%s)", err, rec.Body.String())
	}
	return resp
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// allowCredentialCount digs allowCredentials out of the encoded options.
func allowCredentialCount(t *testing.T, resp map[string]any) int {
	t.Helper()
	opts, ok := resp["options"].(map[string]any)
	if !ok {
		t.Fatalf("options missing/malformed: %#v", resp["options"])
	}
	pk, ok := opts["publicKey"].(map[string]any)
	if !ok {
		t.Fatalf("publicKey missing/malformed: %#v", opts)
	}
	allow, ok := pk["allowCredentials"].([]any)
	if !ok {
		return 0
	}
	return len(allow)
}

// SECURITY: BeginLogin must not let attackers enumerate usernames. Real
// begins used to carry allowCredentials while unknown users got a
// discoverable ceremony (no allowCredentials) and cred-less users got a
// 500 — both direct existence oracles. All username paths must now return
// a shape-identical response (same keys, same allowCredentials cardinality,
// same session-id format).
func TestBeginLoginAntiEnumeration(t *testing.T) {
	h := newTestHandler(t)

	victim, err := h.UserStore.Create("victim", "password-123", "member", "")
	if err != nil {
		t.Fatal(err)
	}

	// Unknown username: 200 + full options shape (allowCredentials present).
	unknown := beginLoginFor(t, h, "no-such-user-xyz")
	if n := allowCredentialCount(t, unknown); n != 1 {
		t.Fatalf("unknown username must look like a user with one credential, allowCredentials=%d", n)
	}
	unkSession, _ := unknown["session"].(string)
	if !strings.Contains(unkSession, ":") || strings.HasPrefix(unkSession, "decoy:") || strings.HasPrefix(unkSession, "discoverable:") {
		t.Fatalf("decoy session id shape must match real sessions, got %q", unkSession)
	}

	// Known user WITHOUT credentials: previously a 500 oracle, now identical.
	noCreds := beginLoginFor(t, h, "victim")
	if n := allowCredentialCount(t, noCreds); n != 1 {
		t.Fatalf("cred-less user must look like a user with one credential, allowCredentials=%d", n)
	}

	// A challenge must be present in every response.
	for name, resp := range map[string]map[string]any{"unknown": unknown, "no-creds": noCreds} {
		opts := resp["options"].(map[string]any)
		pk := opts["publicKey"].(map[string]any)
		if ch, _ := pk["challenge"].(string); ch == "" {
			t.Fatalf("%s: missing challenge", name)
		}
	}

	// Known user WITH one credential: real begin, same visible cardinality.
	credID := make([]byte, 32)
	rand.Read(credID)
	pub := make([]byte, 65)
	rand.Read(pub)
	if _, err := h.DB.Exec(
		`INSERT INTO webauthn_credentials (id, user_id, credential_id, public_key, attestation_type, transports, counter, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		"cred-1", victim.ID, base64.RawURLEncoding.EncodeToString(credID), pub, "none", "", 0, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	real := beginLoginFor(t, h, "victim")
	if n := allowCredentialCount(t, real); n != 1 {
		t.Fatalf("real user with one credential: allowCredentials=%d", n)
	}
	realSession, _ := real["session"].(string)
	if !strings.HasPrefix(realSession, victim.ID+":") {
		t.Fatalf("real session should be bound to the user id, got %q", realSession)
	}
}

// SECURITY: a decoy session (unknown username) must finish with the same
// 401 "auth failed" as a bad credential — not a 404 "user not found" that
// distinguishes decoys from real sessions.
func TestFinishLoginDecoyFailsClosed(t *testing.T) {
	h := newTestHandler(t)
	resp := beginLoginFor(t, h, "no-such-user-xyz")
	sessionID, _ := resp["session"].(string)
	if sessionID == "" {
		t.Fatal("no session id in decoy response")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/passkey/login/finish", strings.NewReader(`{"id":"x","response":{}}`))
	req.Header.Set("X-WebAuthn-Session", sessionID)
	rec := httptest.NewRecorder()
	h.FinishLogin(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("decoy session finish must 401, got %d body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "user not found") {
		t.Fatalf("decoy finish must not disclose user resolution state: %s", rec.Body.String())
	}
}
