package passkey

import (
	"database/sql"
	"encoding/base64"
	"time"

	"ai-gateway/internal/db"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
)

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) ListCredentials(userID string) ([]webauthn.Credential, error) {
	// backup_eligible / backed_up MUST round-trip: go-webauthn rejects the
	// login assertion when the stored BackupEligible flag disagrees with the
	// fresh one ("Backup Eligible flag inconsistency detected during login
	// validation"). Synced passkeys (iCloud Keychain etc.) have BE=1, so
	// hardcoding false broke every login with them.
	rows, err := s.db.Query(db.Q(`SELECT id, credential_id, public_key, attestation_type, transports, counter, backup_eligible, backed_up FROM webauthn_credentials WHERE user_id=?`), userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []webauthn.Credential
	for rows.Next() {
		var id, credIDB64 string
		var pubKey []byte
		var attType, transports sql.NullString
		var counter, backupEligible, backedUp int64
		if err := rows.Scan(&id, &credIDB64, &pubKey, &attType, &transports, &counter, &backupEligible, &backedUp); err != nil {
			return nil, err
		}
		credID, err := base64.RawURLEncoding.DecodeString(credIDB64)
		if err != nil {
			continue
		}
		cred := webauthn.Credential{
			ID:              credID,
			PublicKey:       pubKey,
			AttestationType: attType.String,
			Transport:       parseTransports(transports.String),
			Flags:           webauthn.CredentialFlags{BackupEligible: backupEligible != 0, BackupState: backedUp != 0},
			Authenticator:   webauthn.Authenticator{SignCount: uint32(counter), CloneWarning: false},
		}
		out = append(out, cred)
	}
	return out, nil
}

func parseTransports(s string) []protocol.AuthenticatorTransport {
	if s == "" {
		return nil
	}
	// stored as comma-separated
	var out []protocol.AuthenticatorTransport
	for _, p := range splitComma(s) {
		if p != "" {
			out = append(out, protocol.AuthenticatorTransport(p))
		}
	}
	return out
}
func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, ch := range s {
		if ch == ',' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(ch)
		}
	}
	out = append(out, cur)
	return out
}

func (s *Store) SaveCredential(userID string, cred webauthn.Credential) error {
	id := uuid.NewString()
	credIDB64 := base64.RawURLEncoding.EncodeToString(cred.ID)
	transports := ""
	for i, t := range cred.Transport {
		if i > 0 {
			transports += ","
		}
		transports += string(t)
	}
	_, err := s.db.Exec(db.Q(`INSERT INTO webauthn_credentials(id, user_id, credential_id, public_key, attestation_type, transports, backup_eligible, backed_up, counter, created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`),
		id, userID, credIDB64, cred.PublicKey, cred.AttestationType, transports,
		db.BoolInt(cred.Flags.BackupEligible), db.BoolInt(cred.Flags.BackupState),
		int64(cred.Authenticator.SignCount), time.Now().UTC())
	return err
}

// UpdateCounter refreshes the post-login credential state. backupState is
// BackupState (BS) from the fresh assertion — the library updates the
// returned credential's flags from the response, and BS is legitimately
// mutable (the authenticator's backup state changes over time). BE is
// immutable per spec and validated by the library, so it is never rewritten
// here.
func (s *Store) UpdateCounter(credID []byte, newSignCount uint32, backupState bool) error {
	b64 := base64.RawURLEncoding.EncodeToString(credID)
	_, err := s.db.Exec(db.Q(`UPDATE webauthn_credentials SET counter=?, backed_up=?, last_used_at=? WHERE credential_id=?`), int64(newSignCount), db.BoolInt(backupState), time.Now().UTC(), b64)
	return err
}

func (s *Store) DeleteCredential(userID, credentialIDB64 string) error {
	_, err := s.db.Exec(db.Q(`DELETE FROM webauthn_credentials WHERE user_id=? AND credential_id=?`), userID, credentialIDB64)
	return err
}

func (s *Store) DeleteAllForUser(userID string) error {
	_, err := s.db.Exec(db.Q(`DELETE FROM webauthn_credentials WHERE user_id=?`), userID)
	return err
}
