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
	rows, err := s.db.Query(db.Q(`SELECT id, credential_id, public_key, attestation_type, transports, counter FROM webauthn_credentials WHERE user_id=?`), userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []webauthn.Credential
	for rows.Next() {
		var id, credIDB64 string
		var pubKey []byte
		var attType, transports sql.NullString
		var counter int64
		if err := rows.Scan(&id, &credIDB64, &pubKey, &attType, &transports, &counter); err != nil {
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
			Flags:           webauthn.CredentialFlags{BackupEligible: false, BackupState: false},
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
	_, err := s.db.Exec(db.Q(`INSERT INTO webauthn_credentials(id, user_id, credential_id, public_key, attestation_type, transports, counter, created_at) VALUES(?,?,?,?,?,?,?,?)`),
		id, userID, credIDB64, cred.PublicKey, cred.AttestationType, transports, int64(cred.Authenticator.SignCount), time.Now().UTC())
	return err
}

func (s *Store) UpdateCounter(credID []byte, newSignCount uint32) error {
	b64 := base64.RawURLEncoding.EncodeToString(credID)
	_, err := s.db.Exec(db.Q(`UPDATE webauthn_credentials SET counter=?, last_used_at=? WHERE credential_id=?`), int64(newSignCount), time.Now().UTC(), b64)
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
