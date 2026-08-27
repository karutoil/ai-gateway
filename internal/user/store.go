package user

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"ai-gateway/internal/auth"
	"ai-gateway/internal/db"

	"github.com/google/uuid"
)

type Role string

const (
	RoleAdmin    Role = "admin"
	RoleSupport  Role = "support"
	RoleMember   Role = "member"
	RoleReadonly Role = "readonly"
)

var ValidRoles = map[Role]bool{
	RoleAdmin: true, RoleSupport: true, RoleMember: true, RoleReadonly: true,
}

// MinPasswordLen is the enforced floor for any newly-set password or recovery
// change. Existing users are unaffected until they next change passwords.
const MinPasswordLen = 8

func IsValidRole(r string) bool { _, ok := ValidRoles[Role(strings.ToLower(r))]; return ok }
func NormalizeRole(r string) Role {
	r = strings.ToLower(strings.TrimSpace(r))
	// Fail closed: unknown/empty roles normalize to least privilege.
	if r == "" {
		return RoleReadonly
	}
	if IsValidRole(r) {
		return Role(r)
	}
	return RoleMember
}

type DashboardUser struct {
	ID              string     `json:"id"`
	Username        string     `json:"username"`
	Role            Role       `json:"role"`
	DisplayName     *string    `json:"display_name,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	LastLoginAt     *time.Time `json:"last_login_at,omitempty"`
	LoginCount      int        `json:"login_count"`
	PasskeyEnabled  bool       `json:"passkey_enabled"`
	HasRecoveryCode bool       `json:"has_recovery_code"`
	Disabled        bool       `json:"disabled"`
	PasskeyCount    int        `json:"passkey_count"`
}

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) Count() (int, error) {
	var c int
	err := s.db.QueryRow("SELECT COUNT(*) FROM dashboard_users WHERE disabled=0").Scan(&c)
	return c, err
}

func (s *Store) List() ([]DashboardUser, error) {
	rows, err := s.db.Query(`SELECT id, username, role, display_name, created_at, updated_at, last_login_at, login_count, passkey_enabled, recovery_code_hash, disabled FROM dashboard_users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DashboardUser
	for rows.Next() {
		var u DashboardUser
		var dn sql.NullString
		var rh sql.NullString
		var pe, dis int
		var lla sql.NullTime
		var lc sql.NullInt64
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &dn, &u.CreatedAt, &u.UpdatedAt, &lla, &lc, &pe, &rh, &dis); err != nil {
			return nil, err
		}
		if lla.Valid {
			u.LastLoginAt = &lla.Time
		}
		if lc.Valid {
			u.LoginCount = int(lc.Int64)
		}
		if dn.Valid {
			u.DisplayName = &dn.String
		}
		u.PasskeyEnabled = pe != 0
		u.HasRecoveryCode = rh.Valid && rh.String != ""
		u.Disabled = dis != 0
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Fetch passkey counts separately to avoid holding rows open with single sqlite conn
	for i := range out {
		s.db.QueryRow(db.Q("SELECT COUNT(*) FROM webauthn_credentials WHERE user_id=?"), out[i].ID).Scan(&out[i].PasskeyCount)
	}
	return out, nil
}

func (s *Store) GetByID(id string) (*DashboardUser, error) {
	var u DashboardUser
	var dn, rh sql.NullString
	var pe, dis int
	var lla sql.NullTime
	var lc sql.NullInt64
	err := s.db.QueryRow(db.Q(`SELECT id, username, role, display_name, created_at, updated_at, last_login_at, login_count, passkey_enabled, recovery_code_hash, disabled FROM dashboard_users WHERE id=?`), id).Scan(&u.ID, &u.Username, &u.Role, &dn, &u.CreatedAt, &u.UpdatedAt, &lla, &lc, &pe, &rh, &dis)
	if err != nil {
		return nil, err
	}
	if lla.Valid {
		u.LastLoginAt = &lla.Time
	}
	if lc.Valid {
		u.LoginCount = int(lc.Int64)
	}
	if dn.Valid {
		u.DisplayName = &dn.String
	}
	u.PasskeyEnabled = pe != 0
	u.HasRecoveryCode = rh.Valid && rh.String != ""
	u.Disabled = dis != 0
	s.db.QueryRow(db.Q("SELECT COUNT(*) FROM webauthn_credentials WHERE user_id=?"), u.ID).Scan(&u.PasskeyCount)
	return &u, nil
}

func (s *Store) GetByUsername(username string) (*DashboardUser, string, error) {
	// returns user, password_hash, error
	var u DashboardUser
	var dn, rh sql.NullString
	var pe, dis int
	var lla sql.NullTime
	var lc sql.NullInt64
	var pwHash string
	err := s.db.QueryRow(db.Q(`SELECT id, username, password_hash, role, display_name, created_at, updated_at, last_login_at, login_count, passkey_enabled, recovery_code_hash, disabled FROM dashboard_users WHERE username=?`), strings.ToLower(username)).Scan(&u.ID, &u.Username, &pwHash, &u.Role, &dn, &u.CreatedAt, &u.UpdatedAt, &lla, &lc, &pe, &rh, &dis)
	if err != nil {
		return nil, "", err
	}
	if lla.Valid {
		u.LastLoginAt = &lla.Time
	}
	if lc.Valid {
		u.LoginCount = int(lc.Int64)
	}
	if dn.Valid {
		u.DisplayName = &dn.String
	}
	u.PasskeyEnabled = pe != 0
	u.HasRecoveryCode = rh.Valid && rh.String != ""
	u.Disabled = dis != 0
	s.db.QueryRow(db.Q("SELECT COUNT(*) FROM webauthn_credentials WHERE user_id=?"), u.ID).Scan(&u.PasskeyCount)
	return &u, pwHash, nil
}

func (s *Store) Create(username, password, role, displayName string) (*DashboardUser, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return nil, fmt.Errorf("username required")
	}
	if len(username) < 2 || len(username) > 64 {
		return nil, fmt.Errorf("username 2-64 chars")
	}
	if password == "" {
		return nil, fmt.Errorf("password required")
	}
	if len(password) < MinPasswordLen {
		return nil, fmt.Errorf("password too short (minimum %d characters)", MinPasswordLen)
	}
	r := NormalizeRole(role)
	id := uuid.NewString()
	now := time.Now().UTC()
	hash := auth.HashPassword(password)
	var dn sql.NullString
	if displayName != "" {
		dn = sql.NullString{String: displayName, Valid: true}
	}
	_, err := s.db.Exec(db.Q(`INSERT INTO dashboard_users(id, username, password_hash, role, display_name, created_at, updated_at) VALUES(?,?,?,?,?,?,?)`), id, username, hash, string(r), dn, now, now)
	if err != nil {
		return nil, err
	}
	return s.GetByID(id)
}

func (s *Store) UpdatePassword(id, newPassword string) error {
	if newPassword == "" {
		return fmt.Errorf("password required")
	}
	if len(newPassword) < MinPasswordLen {
		return fmt.Errorf("password too short (minimum %d characters)", MinPasswordLen)
	}
	hash := auth.HashPassword(newPassword)
	// Bump token_version in the same statement: outstanding sessions are revoked
	// on any credential change.
	_, err := s.db.Exec(db.Q(`UPDATE dashboard_users SET password_hash=?, token_version=token_version+1, updated_at=? WHERE id=?`), hash, time.Now().UTC(), id)
	return err
}

func (s *Store) UpdateRole(id string, role Role) error {
	r := NormalizeRole(string(role))
	// Role changes re-mint authority → revoke outstanding tokens.
	_, err := s.db.Exec(db.Q(`UPDATE dashboard_users SET role=?, token_version=token_version+1, updated_at=? WHERE id=?`), string(r), time.Now().UTC(), id)
	return err
}

// SetDisabled stores the disabled flag and revokes outstanding sessions when
// disabling. Returns error if the user does not exist.
func (s *Store) SetDisabled(id string, disabled bool) error {
	v := 0
	if disabled {
		v = 1
	}
	res, err := s.db.Exec(db.Q(`UPDATE dashboard_users SET disabled=?, token_version=token_version+1, updated_at=? WHERE id=?`), v, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) Delete(id string) error {
	_, err := s.db.Exec(db.Q(`DELETE FROM dashboard_users WHERE id=?`), id)
	return err
}

func (s *Store) SetPasskeyEnabled(id string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.Exec(db.Q(`UPDATE dashboard_users SET passkey_enabled=?, updated_at=? WHERE id=?`), v, time.Now().UTC(), id)
	return err
}

// Recovery codes
func GenerateRecoveryCode() string {
	b := make([]byte, 12)
	rand.Read(b)
	hexStr := hex.EncodeToString(b) // 24 chars
	// Format as XXXX-XXXX-XXXX-XXXX (24 hex -> 4 groups of 6)
	return fmt.Sprintf("%s-%s-%s-%s", hexStr[0:6], hexStr[6:12], hexStr[12:18], hexStr[18:24])
}

func (s *Store) SetRecoveryCode(userID, code string) error {
	hash := auth.HashPassword(code)
	now := time.Now().UTC()
	// Single transaction: history swap + primary hash update must be atomic.
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(db.Q(`UPDATE dashboard_users SET recovery_code_hash=?, recovery_generated_at=?, updated_at=? WHERE id=?`), hash, now, now, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(db.Q(`DELETE FROM recovery_codes WHERE user_id=?`), userID); err != nil {
		return err
	}
	id := uuid.NewString()
	if _, err := tx.Exec(db.Q(`INSERT INTO recovery_codes(id, user_id, code_hash, created_at) VALUES(?,?,?,?)`), id, userID, hash, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) VerifyRecoveryCode(userID, code string) (bool, error) {
	var hash sql.NullString
	err := s.db.QueryRow(db.Q(`SELECT recovery_code_hash FROM dashboard_users WHERE id=?`), userID).Scan(&hash)
	if err != nil {
		return false, err
	}
	consumed := false
	if hash.Valid && hash.String != "" && auth.VerifyPasswordHash(hash.String, code) {
		// Single-use semantics enforced at verification time, independent of
		// caller discipline: burn the code wherever it is found.
		s.db.Exec(db.Q(`UPDATE dashboard_users SET recovery_code_hash=NULL WHERE id=?`), userID)
		s.db.Exec(db.Q(`UPDATE recovery_codes SET used=1 WHERE user_id=?`), userID)
		consumed = true
	}
	// also check recovery_codes table (single-use history rows)
	rows, _ := s.db.Query(db.Q(`SELECT id, code_hash FROM recovery_codes WHERE user_id=? AND used=0`), userID)
	if rows == nil {
		if consumed {
			return true, nil
		}
		return false, nil
	}
	defer rows.Close()
	for rows.Next() {
		var id, ch string
		if err := rows.Scan(&id, &ch); err != nil {
			continue
		}
		if auth.VerifyPasswordHash(ch, code) {
			// Consume immediately.
			if _, err := s.db.Exec(db.Q(`UPDATE recovery_codes SET used=1 WHERE id=?`), id); err == nil && !consumed {
				s.db.Exec(db.Q(`UPDATE dashboard_users SET recovery_code_hash=NULL WHERE id=?`), userID)
				return true, nil
			}
			break
		}
	}
	return consumed, nil
}

func (s *Store) ConsumeRecoveryCode(userID string) error {
	// Mark all codes used and clear main hash to force regeneration.
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(db.Q(`UPDATE recovery_codes SET used=1 WHERE user_id=?`), userID); err != nil {
		return err
	}
	_, err = tx.Exec(db.Q(`UPDATE dashboard_users SET recovery_code_hash=NULL, recovery_generated_at=NULL, updated_at=? WHERE id=?`), time.Now().UTC(), userID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// TokenVersionFor implements auth.SessionChecker. A missing user reports
// exists=false so all outstanding tokens for deleted users fail closed.
func (s *Store) TokenVersionFor(subject string) (int64, bool) {
	u, _, err := s.GetByUsername(subject)
	if err != nil || u == nil || u.Disabled {
		return 0, false
	}
	var v int64
	if err := s.db.QueryRow(db.Q(`SELECT COALESCE(token_version,0) FROM dashboard_users WHERE username=?`), strings.ToLower(subject)).Scan(&v); err != nil {
		return 0, false
	}
	return v, true
}

func (s *Store) VerifyPassword(username, password string) (*DashboardUser, bool) {
	u, hash, err := s.GetByUsername(username)
	if err != nil || u.Disabled {
		return nil, false
	}
	if auth.VerifyPasswordHash(hash, password) {
		// Transparent upgrade from legacy unsalted SHA-256 to bcrypt.
		if auth.NeedsRehash(hash) {
			_ = s.UpgradePasswordHash(u.ID, password)
		}
		return u, true
	}
	return nil, false
}

// UpgradePasswordHash rewrites a legacy hash in place WITHOUT bumping
// token_version (the just-presented credential stays valid).
func (s *Store) UpgradePasswordHash(id, password string) error {
	_, err := s.db.Exec(db.Q(`UPDATE dashboard_users SET password_hash=? WHERE id=?`), auth.HashPassword(password), id)
	return err
}

func (s *Store) UpdateDisplayName(id, displayName string) error {
	_, err := s.db.Exec(db.Q(`UPDATE dashboard_users SET display_name=?, updated_at=? WHERE id=?`), displayName, time.Now().UTC(), id)
	return err
}

func (s *Store) UpdateLastLogin(id string) error {
	_, err := s.db.Exec(db.Q(`UPDATE dashboard_users SET last_login_at=?, login_count=login_count+1, updated_at=? WHERE id=?`), time.Now().UTC(), time.Now().UTC(), id)
	return err
}

func (s *Store) ChangePasswordWithOld(id, oldPassword, newPassword string) error {
	var hash string
	err := s.db.QueryRow(db.Q(`SELECT password_hash FROM dashboard_users WHERE id=?`), id).Scan(&hash)
	if err != nil {
		return err
	}
	if !auth.VerifyPasswordHash(hash, oldPassword) {
		return fmt.Errorf("old password incorrect")
	}
	return s.UpdatePassword(id, newPassword)
}

func (s *Store) DisablePasskey(userID string) error {
	_, err := s.db.Exec(db.Q(`DELETE FROM webauthn_credentials WHERE user_id=?`), userID)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(db.Q(`UPDATE dashboard_users SET passkey_enabled=0, updated_at=? WHERE id=?`), time.Now().UTC(), userID)
	return err
}
