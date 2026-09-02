// Package pat implements personal access tokens (PATs) for the dashboard
// API: long-lived bearer tokens for automation/CI that authenticate AS a
// dashboard user with that user's effective permissions (optionally
// narrowed by scopes). Tokens are hashed at rest — the raw value is shown
// exactly once at creation, like gateway keys.
package pat

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"time"

	"ai-gateway/internal/db"
	"ai-gateway/internal/rbac"

	"github.com/google/uuid"
)

const prefix = "gwp_"

type Token struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	Scopes    string     `json:"scopes,omitempty"`
	LastUsed  *time.Time `json:"last_used_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func Hash(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

func PrefixOf(raw string) string {
	if len(raw) > len(prefix)+8 {
		return raw[len(prefix) : len(prefix)+8]
	}
	return raw[:min(len(raw), 8)]
}

func Generate() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// Create mints a token for the user. expiresAt nil = no expiry. scopes: ""
// inherits the user's effective permissions at request time; otherwise a
// comma-separated allowlist of permission names (intersected with the
// user's effective set — a scope can never exceed the user's rights).
func (s *Store) Create(userID, name string, expiresAt *time.Time, scopes string) (*Token, string, error) {
	raw := Generate()
	id := uuid.NewString()
	now := time.Now().UTC()
	var exp any
	if expiresAt != nil {
		exp = expiresAt.UTC()
	}
	_, err := s.db.Exec(db.Q(`INSERT INTO personal_access_tokens
		(id, user_id, name, hash, prefix, scopes, expires_at, created_at)
		VALUES (?,?,?,?,?,?,?,?)`),
		id, userID, name, Hash(raw), PrefixOf(raw), strings.TrimSpace(scopes), exp, now)
	if err != nil {
		return nil, "", err
	}
	t := &Token{ID: id, UserID: userID, Name: name, Prefix: PrefixOf(raw), Scopes: scopes, CreatedAt: now, ExpiresAt: expiresAt}
	return t, raw, nil
}

// Authenticate validates a raw token and returns (userID, scopes, ok).
// Records last_used_at on success. Expired or revoked tokens fail closed.
func (s *Store) Authenticate(raw string) (userID string, scopes string, ok bool) {
	if !strings.HasPrefix(raw, prefix) {
		return "", "", false
	}
	var id string
	var exp sql.NullTime
	var revoked sql.NullTime
	var sc string
	err := s.db.QueryRow(db.Q(
		`SELECT user_id, scopes, expires_at, revoked_at FROM personal_access_tokens WHERE hash = ?`),
		Hash(raw)).Scan(&id, &sc, &exp, &revoked)
	if err != nil {
		return "", "", false
	}
	if revoked.Valid || (exp.Valid && time.Now().After(exp.Time)) {
		return "", "", false
	}
	_, _ = s.db.Exec(db.Q(`UPDATE personal_access_tokens SET last_used_at = ? WHERE id = ?`), time.Now().UTC(), id)
	return id, sc, true
}

// UsernameFor resolves the dashboard username owning a token (the auth
// middleware authenticates PATs by mapping them onto a dashboard user).
func (s *Store) UserIDValid(userID string) bool {
	var n int
	if err := s.db.QueryRow(db.Q(`SELECT COUNT(*) FROM dashboard_users WHERE id=? AND (COALESCE(disabled,0)=0)`), userID).Scan(&n); err != nil {
		return false
	}
	return n == 1
}

func (s *Store) List(userID string) ([]Token, error) {
	rows, err := s.db.Query(db.Q(`
		SELECT id, user_id, name, prefix, COALESCE(scopes,''), last_used_at, expires_at, created_at, revoked_at
		FROM personal_access_tokens WHERE user_id = ? ORDER BY created_at DESC`), userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var scopes string
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &scopes, &t.LastUsed, &t.ExpiresAt, &t.CreatedAt, &t.RevokedAt); err != nil {
			continue
		}
		t.Scopes = scopes
		out = append(out, t)
	}
	return out, nil
}

func (s *Store) Revoke(id, userID string) error {
	_, err := s.db.Exec(db.Q(`UPDATE personal_access_tokens SET revoked_at=? WHERE id=? AND user_id=?`), time.Now().UTC(), id, userID)
	return err
}

// CheckScopes: PAT scopes narrow the user's effective set. Empty scopes =
// inherit everything the user has. Returns the narrowed permission map.
func CheckScopes(effective map[string]bool, scopes string) map[string]bool {
	scopes = strings.TrimSpace(scopes)
	if scopes == "" {
		return effective
	}
	narrowed := map[string]bool{}
	for _, p := range strings.Split(scopes, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// A scope can never exceed the user's own effective rights.
		if rbac.Valid(p) && effective[p] {
			narrowed[p] = true
		}
	}
	return narrowed
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
