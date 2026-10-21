package passkey

import (
	"testing"

	"ai-gateway/internal/db"
	"ai-gateway/internal/user"

	"github.com/go-webauthn/webauthn/webauthn"
)

// Regression test for the "Backup Eligible flag inconsistency detected during
// login validation" 401s: the store used to hardcode BackupEligible=false on
// load, so any synced passkey (BE=1 — iCloud Keychain, 1Password, ...) failed
// every login. The flags must round-trip through the schema (migration 033).
func TestCredentialBackupFlagRoundTrip(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	users := user.NewStore(database)
	if _, err := users.Create("flagsuser", "super-secret-pw", "admin", ""); err != nil {
		t.Fatal(err)
	}
	u, _, err := users.GetByUsername("flagsuser")
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(database)

	cred := webauthn.Credential{
		ID:        []byte{0x01, 0x02, 0x03},
		PublicKey: []byte("pk"),
		Flags:     webauthn.CredentialFlags{BackupEligible: true, BackupState: true},
		Authenticator: webauthn.Authenticator{
			SignCount: 7,
		},
	}
	if err := s.SaveCredential(u.ID, cred); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListCredentials(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 credential, got %d", len(got))
	}
	if !got[0].Flags.BackupEligible {
		t.Fatal("BackupEligible must round-trip: a synced passkey (BE=1) stored as BE=0 fails every login with 'Backup Eligible flag inconsistency'")
	}
	if !got[0].Flags.BackupState {
		t.Fatal("BackupState must round-trip")
	}
	if got[0].Authenticator.SignCount != 7 {
		t.Fatalf("counter must round-trip, got %d", got[0].Authenticator.SignCount)
	}

	// BackupState is mutable: the login path refreshes it alongside the counter.
	if err := s.UpdateCounter(cred.ID, 8, false); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListCredentials(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Flags.BackupState {
		t.Fatal("UpdateCounter must refresh BackupState from the fresh assertion")
	}
	if got[0].Authenticator.SignCount != 8 {
		t.Fatalf("UpdateCounter must refresh the signature counter, got %d", got[0].Authenticator.SignCount)
	}
}
