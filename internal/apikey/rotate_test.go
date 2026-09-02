package apikey

import (
	"strings"
	"testing"
	"time"

	"ai-gateway/internal/db"
)

func TestRotateGeneratesNewSecretSameIdentity(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)

	k, _ := s.CreateWithOrg("web-app", "", "user-1")
	before, _ := s.GetByID(k.ID)

	rotated, newSecret, err := s.Rotate(k.ID)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if !strings.HasPrefix(newSecret, "sk-gw-") {
		t.Fatalf("new secret %q lacks prefix", newSecret)
	}
	if newSecret == k.Key {
		t.Fatal("new secret must differ from the old one")
	}
	// Identity preserved: same id, name, owner.
	if rotated.ID != k.ID || rotated.Name != "web-app" {
		t.Fatalf("identity changed: %+v", rotated)
	}
	if rotated.CreatedBy == nil || *rotated.CreatedBy != "user-1" {
		t.Fatalf("owner lost on rotate: %+v", rotated.CreatedBy)
	}
	// The old secret still authenticates during the grace window —
	// that's the zero-downtime contract (see TestGraceWindow).
	if got, ok := s.Verify(k.Key); !ok || got.ID != k.ID {
		t.Fatal("old secret should still verify during grace window")
	}
	// ...but the NEW secret verifies.
	got, ok := s.Verify(newSecret)
	if !ok || got.ID != k.ID {
		t.Fatalf("new secret fails to verify: ok=%v", ok)
	}
	// Stamped rotation metadata.
	if before != nil && rotated.Prefix == "" {
		t.Fatal("prefix lost")
	}
}

func TestGraceWindowOldSecretStillWorks(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)

	k, _ := s.Create("grace-test")
	_, newSecret, err := s.Rotate(k.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Old secret verifies during the grace window (zero-downtime swap).
	got, ok := s.Verify(k.Key)
	if !ok || got.ID != k.ID {
		t.Fatal("old secret should verify during grace window")
	}
	// New secret also works.
	if got2, ok2 := s.Verify(newSecret); !ok2 || got2.ID != k.ID {
		t.Fatal("new secret must verify")
	}

	// After the grace window lapses, the old secret dies.
	_, err = database.Exec(`UPDATE gateway_keys SET rotated_at = ? WHERE id = ?`, time.Now().Add(-2*RotationGraceWindow), k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(k.Key); ok {
		t.Fatal("old secret must stop working after the grace window")
	}
	if _, ok := s.Verify(newSecret); !ok {
		t.Fatal("new secret must keep working after old grace lapse")
	}
}

func TestRotateWithinWindowKeepsOldestGraceSecret(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)

	k, _ := s.Create("rapid")
	_, s2, _ := s.Rotate(k.ID) // rotation 1: original -> previous, s2 current
	// Rotate again immediately: s2 should NOT push original out of grace;
	// previous_hash stays pointing at the OLDEST still-graceful secret.
	_, s3, _ := s.Rotate(k.ID)
	if s3 == s2 {
		t.Fatal("third secret must differ")
	}
	if _, ok := s.Verify(k.Key); !ok {
		t.Fatal("original secret should still be in grace (oldest kept)")
	}
	// s2 was superseded within the grace window: only the OLDEST secret
	// keeps grace, so s2 (deployed for milliseconds) stops immediately.
	// s3 is the current secret and must verify.
	if _, ok := s.Verify(s3); !ok {
		t.Fatal("newest secret (s3) must verify as current")
	}
	_ = s2
}

func TestRotateNotFoundOrRevoked(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)
	if _, _, err := s.Rotate("no-such-key"); err == nil {
		t.Fatal("expected error for unknown key")
	}
	k, _ := s.Create("revoked-me")
	s.Revoke(k.ID)
	if _, _, err := s.Rotate(k.ID); err == nil {
		t.Fatal("rotating a revoked key must fail")
	}
}
