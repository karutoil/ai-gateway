package user

import (
	"strings"
	"testing"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/db"
)

func TestPasswordHashUpgradeFromLegacySHA256(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(database)
	if _, err := s.Create("alice", "super-secret-pw", "admin", ""); err != nil {
		t.Fatal(err)
	}
	// Downgrade the stored hash to a legacy unsalted SHA-256 digest.
	if _, err := database.Exec(`UPDATE dashboard_users SET password_hash=? WHERE username='alice'`, auth.HashPasswordLegacy("super-secret-pw")); err != nil {
		t.Fatal(err)
	}
	u, ok := s.VerifyPassword("alice", "super-secret-pw")
	if !ok {
		t.Fatal("legacy password must verify during cutover window")
	}
	fresh, hash, _ := s.GetByUsername("alice")
	_ = fresh
	if auth.NeedsRehash(hash) {
		t.Fatal("hash should have been transparently upgraded to bcrypt at login")
	}
	if !strings.HasPrefix(hash, "$2") && !strings.HasPrefix(hash, "pre:") {
		t.Fatalf("expected bcrypt hash after upgrade, got %q", hash)
	}
	_ = u
}

func TestRecoveryCodeSingleUse(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(database)
	if _, err := s.Create("bob", "another-secret-pw", "admin", ""); err != nil {
		t.Fatal(err)
	}
	bob, _, err := s.GetByUsername("bob")
	if err != nil {
		t.Fatal(err)
	}
	code := GenerateRecoveryCode()
	if err := s.SetRecoveryCode(bob.ID, code); err != nil {
		t.Fatal(err)
	}

	ok1, err := s.VerifyRecoveryCode(bob.ID, code)
	if err != nil || !ok1 {
		t.Fatalf("first recovery use must succeed: %v %v", ok1, err)
	}
	// Consumption happens as part of verification+handler flow; simulate the
	// handler's Consume call by verifying twice directly — the history-row
	// path marks used=1 at first hit.
	ok2, _ := s.VerifyRecoveryCode(bob.ID, code)
	if ok2 {
		t.Fatal("recovery code must be single-use")
	}
}

func TestTokenVersionRevocation(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(database)
	u, err := s.Create("carol", "yet-another-pass-1", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	v0, exists := s.TokenVersionFor(u.Username)
	if !exists || v0 != 0 {
		t.Fatalf("initial version 0 expected, got %d exists=%v", v0, exists)
	}
	if err := s.UpdateRole(u.ID, RoleSupport); err != nil {
		t.Fatal(err)
	}
	v1, _ := s.TokenVersionFor(u.Username)
	if v1 <= v0 {
		t.Fatal("role change must advance token_version")
	}
	if err := s.SetDisabled(u.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, exists := s.TokenVersionFor(u.Username); exists {
		t.Fatal("disabled users must fail session validation")
	}
}
