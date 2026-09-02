package pat

import (
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/db"
	"ai-gateway/internal/rbac"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	// PATs FK-reference dashboard_users; seed a user row for the tests.
	if _, err := database.Exec(
		`INSERT INTO dashboard_users (id, username, password_hash, role, created_at, updated_at) VALUES
		 ('user-1', 'testuser', 'x', 'member', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	return NewStore(database)
}

func TestTokenLifecycle(t *testing.T) {
	s := testStore(t)

	raw := prefix + "deadbeef" // any raw shape works for Create
	tok, secret, err := s.Create("user-1", "ci-token", nil, "logs:read,analytics:read")
	if err != nil {
		t.Fatal(err)
	}
	_ = raw
	if !strings.HasPrefix(secret, "gwp_") {
		t.Fatalf("secret %q lacks gwp_ prefix", secret)
	}
	if tok.Prefix != PrefixOf(secret) {
		t.Fatalf("prefix mismatch: %q vs %q", tok.Prefix, PrefixOf(secret))
	}

	// Authenticate works and returns scopes
	uid, scopes, ok := s.Authenticate(secret)
	if !ok || uid != "user-1" {
		t.Fatalf("authenticate failed: ok=%v uid=%q", ok, uid)
	}
	if scopes != "logs:read,analytics:read" {
		t.Fatalf("scopes = %q", scopes)
	}

	// Wrong token fails
	if _, _, ok := s.Authenticate("gwp_notarealtoken"); ok {
		t.Fatal("garbage token authenticated")
	}
	// Gateway keys must not authenticate as PATs
	if _, _, ok := s.Authenticate("sk-gw-not-a-pat"); ok {
		t.Fatal("gateway key shape authenticated as PAT")
	}

	// Revoke kills it
	if err := s.Revoke(tok.ID, "user-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Authenticate(secret); ok {
		t.Fatal("revoked token still authenticates")
	}
}

func TestTokenExpiry(t *testing.T) {
	s := testStore(t)
	expired := time.Now().Add(-time.Hour)
	_, secret, err := s.Create("user-1", "expired", &expired, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Authenticate(secret); ok {
		t.Fatal("expired token authenticated")
	}

	future := time.Now().Add(time.Hour)
	_, live, err := s.Create("user-1", "live", &future, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Authenticate(live); !ok {
		t.Fatal("unexpired token rejected")
	}
}

func TestScopeNarrowing(t *testing.T) {
	effective := map[string]bool{
		rbac.PermLogsRead: true,
		rbac.PermKeysRead: true,
	}

	// Empty scopes inherit everything
	got := CheckScopes(effective, "")
	if len(got) != len(effective) {
		t.Fatalf("empty scopes should inherit: got %v", got)
	}

	// Scopes narrow to the intersection
	got = CheckScopes(effective, "logs:read, users:write")
	if !got[rbac.PermLogsRead] {
		t.Error("granted scope within user rights should pass")
	}
	if got["users:write"] {
		t.Error("scope exceeding user rights must be dropped")
	}
	if got[rbac.PermKeysRead] {
		t.Error("unscoped perms must not leak through narrowing")
	}

	// Unknown permissions never pass
	got = CheckScopes(effective, "bogus:perm")
	if got["bogus:perm"] {
		t.Error("unknown permission passed through scopes")
	}
}
