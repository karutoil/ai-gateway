package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"ai-gateway/internal/db"
	"ai-gateway/internal/models"

	"github.com/google/uuid"
)

const prefix = "sk-gw-"

func Generate() string {
	b := make([]byte, 32)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func Hash(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func Prefix(key string) string {
	if len(key) >= len(prefix)+8 {
		return key[len(prefix) : len(prefix)+8]
	}
	if len(key) >= 8 {
		if len(key) > 8 {
			return key[len(key)-8:]
		}
		return key[:8]
	}
	return key
}

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) Create(name string) (*models.GatewayKey, error) {
	return s.CreateWithOrg(name, "")
}

// CreateWithOrg creates a key. createdBy optionally records the dashboard
// user that created it (the keys:read_own ownership anchor); empty means
// unowned (visible only to keys:read holders and admins).
func (s *Store) CreateWithOrg(name, orgID string, createdBy ...string) (*models.GatewayKey, error) {
	raw := Generate()
	hash := Hash(raw)
	pfx := Prefix(raw)
	id := uuid.NewString()
	now := time.Now().UTC()
	var owner string
	if len(createdBy) > 0 {
		owner = createdBy[0]
	}
	_, err := s.db.Exec(db.Q(`INSERT INTO gateway_keys(id,name,prefix,hash,created_at,rate_limit_rpm,org_id,created_by) VALUES(?,?,?,?,?,?,?,?)`), id, name, pfx, hash, now, 60, sql.NullString{String: orgID, Valid: orgID != ""}, sql.NullString{String: owner, Valid: owner != ""})
	if err != nil && (strings.Contains(err.Error(), "created_by") || strings.Contains(err.Error(), "org_id")) {
		_, err = s.db.Exec(db.Q(`INSERT INTO gateway_keys(id,name,prefix,hash,created_at,rate_limit_rpm,org_id) VALUES(?,?,?,?,?,?,?)`), id, name, pfx, hash, now, 60, sql.NullString{String: orgID, Valid: orgID != ""})
	}
	if err != nil && strings.Contains(err.Error(), "org_id") {
		_, err = s.db.Exec(db.Q(`INSERT INTO gateway_keys(id,name,prefix,hash,created_at,rate_limit_rpm) VALUES(?,?,?,?,?,60)`), id, name, pfx, hash, now)
	}
	if err != nil {
		return nil, err
	}
	k := &models.GatewayKey{
		ID: id, Name: name, Prefix: pfx, Hash: hash, Key: raw, CreatedAt: now, RateLimitRPM: 60,
	}
	if orgID != "" {
		k.OrgID = &orgID
	}
	if owner != "" {
		k.CreatedBy = &owner
	}
	return k, nil
}

func parseAllowedModels(raw sql.NullString) []string {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
		// fallback: comma-separated
		parts := strings.Split(raw.String, ",")
		var cleaned []string
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				cleaned = append(cleaned, p)
			}
		}
		return cleaned
	}
	return out
}

func scanGatewayKey(k *models.GatewayKey, rpm, rph, rpd, tpm sql.NullInt64, allowed sql.NullString) {
	if rpm.Valid && rpm.Int64 > 0 {
		k.RateLimitRPM = int(rpm.Int64)
	} else {
		k.RateLimitRPM = 60
	}
	if rph.Valid {
		k.RateLimitRPH = int(rph.Int64)
	}
	if rpd.Valid {
		k.RateLimitRPD = int(rpd.Int64)
	}
	if tpm.Valid {
		k.RateLimitTPM = int(tpm.Int64)
	}
	if allowed.Valid {
		k.AllowedModelsRaw = allowed.String
		k.AllowedModels = parseAllowedModels(allowed)
	}
}

func (s *Store) ListForOrg(orgID string) ([]models.GatewayKey, error) {
	if orgID == "" {
		return s.List()
	}
	rows, err := s.db.Query(`SELECT id,name,prefix,hash,last_used_at,created_at,revoked_at,rate_limit_rpm,rate_limit_rph,rate_limit_rpd,rate_limit_tpm,allowed_models,org_id,COALESCE(created_by,''),expires_at,rotated_at,COALESCE(ip_allowlist,''),COALESCE(monthly_budget_usd,0) FROM gateway_keys ORDER BY created_at DESC`)
	if err != nil {
		if strings.Contains(err.Error(), "allowed_models") || strings.Contains(err.Error(), "rate_limit_rph") || strings.Contains(err.Error(), "rate_limit_rpd") || strings.Contains(err.Error(), "rate_limit_tpm") {
			return s.List()
		}
		if strings.Contains(err.Error(), "org_id") {
			return s.List()
		}
		return nil, err
	}
	defer rows.Close()
	var out []models.GatewayKey
	for rows.Next() {
		var k models.GatewayKey
		var rpm, rph, rpd, tpm sql.NullInt64
		var org, allowed, createdBy sql.NullString
		var expiresAt, rotatedAt sql.NullTime
		var ipAllow sql.NullString
		var monthlyBudget sql.NullFloat64
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.LastUsedAt, &k.CreatedAt, &k.RevokedAt, &rpm, &rph, &rpd, &tpm, &allowed, &org, &createdBy, &expiresAt, &rotatedAt, &ipAllow, &monthlyBudget); err != nil {
			return nil, err
		}
		scanGatewayKey(&k, rpm, rph, rpd, tpm, allowed)
		if expiresAt.Valid {
			k.ExpiresAt = &expiresAt.Time
		}
		if rotatedAt.Valid {
			k.RotatedAt = &rotatedAt.Time
		}
		if ipAllow.Valid {
			k.IPAllowlist = ipAllow.String
		}
		if monthlyBudget.Valid {
			k.MonthlyBudgetUSD = monthlyBudget.Float64
		}
		if org.Valid {
			k.OrgID = &org.String
		}
		if createdBy.Valid && createdBy.String != "" {
			k.CreatedBy = &createdBy.String
		}
		if k.OrgID != nil && *k.OrgID != orgID {
			continue
		}
		out = append(out, k)
	}
	return out, nil
}

// ListCreatedBy returns keys created by one dashboard user (the
// keys:read_own scoped view). Legacy keys have created_by NULL and are
// intentionally excluded — they belong to no one.
func (s *Store) ListCreatedBy(userID string) ([]models.GatewayKey, error) {
	rows, err := s.db.Query(`SELECT id,name,prefix,hash,last_used_at,created_at,revoked_at,rate_limit_rpm,rate_limit_rph,rate_limit_rpd,rate_limit_tpm,allowed_models,org_id,COALESCE(created_by,''),expires_at,rotated_at,COALESCE(ip_allowlist,''),COALESCE(monthly_budget_usd,0) FROM gateway_keys WHERE created_by = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		// Legacy DB without created_by: an owner-scoped view is empty rather
		// than leaking everything (fail closed).
		return []models.GatewayKey{}, nil
	}
	defer rows.Close()
	var out []models.GatewayKey
	for rows.Next() {
		var k models.GatewayKey
		var rpm, rph, rpd, tpm sql.NullInt64
		var org, allowed, createdBy sql.NullString
		var expiresAt, rotatedAt sql.NullTime
		var ipAllow sql.NullString
		var monthlyBudget sql.NullFloat64
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.LastUsedAt, &k.CreatedAt, &k.RevokedAt, &rpm, &rph, &rpd, &tpm, &allowed, &org, &createdBy, &expiresAt, &rotatedAt, &ipAllow, &monthlyBudget); err != nil {
			return nil, err
		}
		scanGatewayKey(&k, rpm, rph, rpd, tpm, allowed)
		if expiresAt.Valid {
			k.ExpiresAt = &expiresAt.Time
		}
		if rotatedAt.Valid {
			k.RotatedAt = &rotatedAt.Time
		}
		if ipAllow.Valid {
			k.IPAllowlist = ipAllow.String
		}
		if monthlyBudget.Valid {
			k.MonthlyBudgetUSD = monthlyBudget.Float64
		}
		if org.Valid {
			k.OrgID = &org.String
		}
		if createdBy.Valid && createdBy.String != "" {
			k.CreatedBy = &createdBy.String
		}
		out = append(out, k)
	}
	return out, nil
}

func (s *Store) List() ([]models.GatewayKey, error) {
	rows, err := s.db.Query(`SELECT id,name,prefix,hash,last_used_at,created_at,revoked_at,rate_limit_rpm,rate_limit_rph,rate_limit_rpd,rate_limit_tpm,allowed_models,org_id,COALESCE(created_by,''),expires_at,rotated_at,COALESCE(ip_allowlist,''),COALESCE(monthly_budget_usd,0) FROM gateway_keys ORDER BY created_at DESC`)
	fallback := ""
	if err != nil && (strings.Contains(err.Error(), "allowed_models") || strings.Contains(err.Error(), "rate_limit_rph") || strings.Contains(err.Error(), "rate_limit_rpd") || strings.Contains(err.Error(), "rate_limit_tpm") || strings.Contains(err.Error(), "org_id") || strings.Contains(err.Error(), "created_by")) {
		rows, err = s.db.Query(`SELECT id,name,prefix,hash,last_used_at,created_at,revoked_at,rate_limit_rpm FROM gateway_keys ORDER BY created_at DESC`)
		fallback = "minimal"
		if err != nil {
			return nil, err
		}
	} else if err != nil && strings.Contains(err.Error(), "org_id") {
		rows, err = s.db.Query(`SELECT id,name,prefix,hash,last_used_at,created_at,revoked_at,rate_limit_rpm,allowed_models FROM gateway_keys ORDER BY created_at DESC`)
		fallback = "no_org"
		if err != nil && strings.Contains(err.Error(), "allowed_models") {
			rows, err = s.db.Query(`SELECT id,name,prefix,hash,last_used_at,created_at,revoked_at,rate_limit_rpm FROM gateway_keys ORDER BY created_at DESC`)
			fallback = "minimal"
		}
		if err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.GatewayKey
	if fallback == "minimal" {
		for rows.Next() {
			var k models.GatewayKey
			var rpm sql.NullInt64
			if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.LastUsedAt, &k.CreatedAt, &k.RevokedAt, &rpm); err != nil {
				return nil, err
			}
			if rpm.Valid && rpm.Int64 > 0 {
				k.RateLimitRPM = int(rpm.Int64)
			} else {
				k.RateLimitRPM = 60
			}
			out = append(out, k)
		}
		return out, nil
	}
	if fallback == "no_org" {
		for rows.Next() {
			var k models.GatewayKey
			var rpm sql.NullInt64
			var allowed sql.NullString
			var expiresAt sql.NullTime
			var rotatedAt sql.NullTime
			var ipAllow sql.NullString
			var monthlyBudget sql.NullFloat64
			_ = expiresAt
			_ = rotatedAt
			_ = ipAllow
			_ = monthlyBudget
			if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.LastUsedAt, &k.CreatedAt, &k.RevokedAt, &rpm, &allowed); err != nil {
				return nil, err
			}
			scanGatewayKey(&k, rpm, sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{}, allowed)
			out = append(out, k)
		}
		return out, nil
	}
	for rows.Next() {
		var k models.GatewayKey
		var rpm, rph, rpd, tpm sql.NullInt64
		var org, createdBy sql.NullString
		var allowed sql.NullString
		var expiresAt sql.NullTime
		var rotatedAt sql.NullTime
		var ipAllow sql.NullString
		var monthlyBudget sql.NullFloat64
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.LastUsedAt, &k.CreatedAt, &k.RevokedAt, &rpm, &rph, &rpd, &tpm, &allowed, &org, &createdBy, &expiresAt, &rotatedAt, &ipAllow, &monthlyBudget); err != nil {
			return nil, err
		}
		scanGatewayKey(&k, rpm, rph, rpd, tpm, allowed)
		if expiresAt.Valid {
			k.ExpiresAt = &expiresAt.Time
		}
		if rotatedAt.Valid {
			k.RotatedAt = &rotatedAt.Time
		}
		if ipAllow.Valid {
			k.IPAllowlist = ipAllow.String
		}
		if monthlyBudget.Valid {
			k.MonthlyBudgetUSD = monthlyBudget.Float64
		}
		if org.Valid {
			k.OrgID = &org.String
		}
		if createdBy.Valid && createdBy.String != "" {
			k.CreatedBy = &createdBy.String
		}
		out = append(out, k)
	}
	return out, nil
}

func (s *Store) Verify(raw string) (*models.GatewayKey, bool) {
	hash := Hash(raw)
	var k models.GatewayKey
	var rpm, rph, rpd, tpm sql.NullInt64
	var org sql.NullString
	var allowed sql.NullString
	var expiresAt sql.NullTime
	var rotatedAt sql.NullTime
	var ipAllow sql.NullString
	var monthlyBudget sql.NullFloat64
	_ = expiresAt
	_ = rotatedAt
	_ = ipAllow
	_ = monthlyBudget
	err := s.db.QueryRow(db.Q(`SELECT id,name,prefix,hash,last_used_at,created_at,revoked_at,rate_limit_rpm,rate_limit_rph,rate_limit_rpd,rate_limit_tpm,allowed_models,org_id,expires_at,rotated_at,COALESCE(ip_allowlist,''),COALESCE(monthly_budget_usd,0) FROM gateway_keys WHERE hash=? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`), hash, time.Now().UTC()).Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.LastUsedAt, &k.CreatedAt, &k.RevokedAt, &rpm, &rph, &rpd, &tpm, &allowed, &org, &expiresAt, &rotatedAt, &ipAllow, &monthlyBudget)
	if err != nil && (strings.Contains(err.Error(), "allowed_models") || strings.Contains(err.Error(), "rate_limit_rph") || strings.Contains(err.Error(), "rate_limit_tpm")) {
		err = s.db.QueryRow(db.Q(`SELECT id,name,prefix,hash,last_used_at,created_at,revoked_at,rate_limit_rpm,org_id FROM gateway_keys WHERE hash=? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`), hash, time.Now().UTC()).Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.LastUsedAt, &k.CreatedAt, &k.RevokedAt, &rpm, &org)
		if err != nil && strings.Contains(err.Error(), "org_id") {
			err = s.db.QueryRow(db.Q(`SELECT id,name,prefix,hash,last_used_at,created_at,revoked_at,rate_limit_rpm FROM gateway_keys WHERE hash=? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`), hash, time.Now().UTC()).Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.LastUsedAt, &k.CreatedAt, &k.RevokedAt, &rpm)
		}
	}
	if err != nil && strings.Contains(err.Error(), "org_id") {
		err = s.db.QueryRow(db.Q(`SELECT id,name,prefix,hash,last_used_at,created_at,revoked_at,rate_limit_rpm FROM gateway_keys WHERE hash=? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`), hash, time.Now().UTC()).Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.LastUsedAt, &k.CreatedAt, &k.RevokedAt, &rpm)
	}
	// Grace window: if the primary hash miss was actually a recent
	// rotation's previous secret, honor it (zero-downtime rotation).
	if err != nil {
		k2, ok2 := s.verifyPreviousHash(raw)
		if ok2 {
			return k2, true
		}
		return nil, false
	}
	if allowed.Valid || rph.Valid || rpd.Valid || tpm.Valid {
		scanGatewayKey(&k, rpm, rph, rpd, tpm, allowed)
		if expiresAt.Valid {
			k.ExpiresAt = &expiresAt.Time
		}
		if rotatedAt.Valid {
			k.RotatedAt = &rotatedAt.Time
		}
		if ipAllow.Valid {
			k.IPAllowlist = ipAllow.String
		}
		if monthlyBudget.Valid {
			k.MonthlyBudgetUSD = monthlyBudget.Float64
		}
	} else {
		if rpm.Valid && rpm.Int64 > 0 {
			k.RateLimitRPM = int(rpm.Int64)
		} else {
			k.RateLimitRPM = 60
		}
	}
	if org.Valid {
		k.OrgID = &org.String
	}
	s.db.Exec(db.Q(`UPDATE gateway_keys SET last_used_at=? WHERE id=?`), time.Now().UTC(), k.ID)
	return &k, true
}

// verifyPreviousHash authenticates a secret against previous_hash during
// the post-rotation grace window. Used by Verify when the current hash
// misses, giving apps a zero-downtime window to swap in the new secret.
func (s *Store) verifyPreviousHash(raw string) (*models.GatewayKey, bool) {
	hash := Hash(raw)
	var id string
	var rotatedAt sql.NullTime
	err := s.db.QueryRow(db.Q(`SELECT id, rotated_at FROM gateway_keys WHERE previous_hash = ? AND revoked_at IS NULL`), hash).Scan(&id, &rotatedAt)
	if err != nil {
		return nil, false
	}
	if !rotatedAt.Valid || time.Since(rotatedAt.Time) > RotationGraceWindow {
		return nil, false
	}
	k, err := s.GetByID(id)
	if err != nil {
		return nil, false
	}
	s.db.Exec(db.Q(`UPDATE gateway_keys SET last_used_at = ? WHERE id = ?`), time.Now().UTC(), id)
	return k, true
}

// RotationGraceWindow is how long the previous secret keeps authenticating
// after a rotation — long enough to update an app's env vars without an
// outage, short enough to limit a compromised secret's exposure.
const RotationGraceWindow = 24 * time.Hour

// Rotate regenerates a key's secret in place: same id, name, owner, org,
// limits, and analytics history. The current hash moves to previous_hash
// (still authenticating during the grace window); the fresh secret is
// returned raw, exactly once — only hashes are ever stored.
func (s *Store) Rotate(id string) (*models.GatewayKey, string, error) {
	raw := Generate()
	newHash := Hash(raw)
	now := time.Now().UTC()

	// The outgoing hash only becomes previous_hash if any prior grace window
	// has lapsed; repeated rotations inside the window keep the OLDEST
	// still-graceful secret so an app mid-update never breaks.
	res, err := s.db.Exec(
		db.Q(`UPDATE gateway_keys SET
			previous_hash = CASE WHEN rotated_at IS NULL OR rotated_at < ? THEN hash ELSE previous_hash END,
			hash = ?,
			rotated_at = ?
			WHERE id = ? AND revoked_at IS NULL`),
		now.Add(-RotationGraceWindow), newHash, now, id,
	)
	if err != nil && (strings.Contains(err.Error(), "no such column") || strings.Contains(err.Error(), "previous_hash") || strings.Contains(err.Error(), "rotated_at")) {
		// Legacy DB without rotation columns: rotate without grace.
		if _, err2 := s.db.Exec(db.Q(`UPDATE gateway_keys SET hash = ? WHERE id = ? AND revoked_at IS NULL`), newHash, id); err2 != nil {
			return nil, "", err2
		}
		k, gerr := s.GetByID(id)
		return k, raw, gerr
	}
	if err != nil {
		return nil, "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, "", fmt.Errorf("key not found or revoked")
	}
	k, gerr := s.GetByID(id)
	if gerr != nil {
		return nil, "", gerr
	}
	return k, raw, gerr
}

func (s *Store) Revoke(id string) error {
	_, err := s.db.Exec(db.Q(`UPDATE gateway_keys SET revoked_at=? WHERE id=?`), time.Now().UTC(), id)
	return err
}

func (s *Store) UpdateName(id, name string) error {
	if name == "" {
		return fmt.Errorf("name required")
	}
	_, err := s.db.Exec(db.Q(`UPDATE gateway_keys SET name=? WHERE id=?`), name, id)
	return err
}

func (s *Store) Delete(id string) error {
	_, err := s.db.Exec(db.Q(`DELETE FROM gateway_keys WHERE id=?`), id)
	return err
}

func (s *Store) GetByID(id string) (*models.GatewayKey, error) {
	var k models.GatewayKey
	var rpm, rph, rpd, tpm sql.NullInt64
	var org, createdBy sql.NullString
	var allowed sql.NullString
	var expiresAt sql.NullTime
	var rotatedAt sql.NullTime
	var ipAllow sql.NullString
	var monthlyBudget sql.NullFloat64
	err := s.db.QueryRow(db.Q(`SELECT id,name,prefix,hash,last_used_at,created_at,revoked_at,rate_limit_rpm,rate_limit_rph,rate_limit_rpd,rate_limit_tpm,allowed_models,org_id,COALESCE(created_by,''),expires_at,rotated_at,COALESCE(ip_allowlist,''),COALESCE(monthly_budget_usd,0) FROM gateway_keys WHERE id=?`), id).Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.LastUsedAt, &k.CreatedAt, &k.RevokedAt, &rpm, &rph, &rpd, &tpm, &allowed, &org, &createdBy, &expiresAt, &rotatedAt, &ipAllow, &monthlyBudget)
	if err != nil && (strings.Contains(err.Error(), "allowed_models") || strings.Contains(err.Error(), "rate_limit_rph") || strings.Contains(err.Error(), "created_by")) {
		err = s.db.QueryRow(db.Q(`SELECT id,name,prefix,hash,last_used_at,created_at,revoked_at,rate_limit_rpm,org_id FROM gateway_keys WHERE id=?`), id).Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.LastUsedAt, &k.CreatedAt, &k.RevokedAt, &rpm, &org)
		if err != nil && strings.Contains(err.Error(), "org_id") {
			err = s.db.QueryRow(db.Q(`SELECT id,name,prefix,hash,last_used_at,created_at,revoked_at,rate_limit_rpm FROM gateway_keys WHERE id=?`), id).Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.LastUsedAt, &k.CreatedAt, &k.RevokedAt, &rpm)
		}
	}
	if err != nil {
		return nil, err
	}
	scanGatewayKey(&k, rpm, rph, rpd, tpm, allowed)
	if expiresAt.Valid {
		k.ExpiresAt = &expiresAt.Time
	}
	if rotatedAt.Valid {
		k.RotatedAt = &rotatedAt.Time
	}
	if ipAllow.Valid {
		k.IPAllowlist = ipAllow.String
	}
	if monthlyBudget.Valid {
		k.MonthlyBudgetUSD = monthlyBudget.Float64
	}
	if org.Valid {
		k.OrgID = &org.String
	}
	if createdBy.Valid && createdBy.String != "" {
		k.CreatedBy = &createdBy.String
	}
	return &k, nil
}

// UpdateLimits updates rate limits and allowed_models for a key. AllowedModels nil means unchanged, empty slice clears.
func (s *Store) UpdateLimits(id string, rpm *int, rph *int, rpd *int, tpm *int, allowedModels *[]string) error {
	if rpm == nil && rph == nil && rpd == nil && tpm == nil && allowedModels == nil {
		return fmt.Errorf("no fields to update")
	}
	sets := []string{}
	args := []interface{}{}
	if rpm != nil {
		if *rpm < 0 || *rpm > 100000 {
			return fmt.Errorf("rpm out of range")
		}
		sets = append(sets, "rate_limit_rpm=?")
		args = append(args, *rpm)
	}
	if rph != nil {
		if *rph < 0 || *rph > 1000000 {
			return fmt.Errorf("rph out of range")
		}
		sets = append(sets, "rate_limit_rph=?")
		args = append(args, *rph)
	}
	if rpd != nil {
		if *rpd < 0 || *rpd > 10000000 {
			return fmt.Errorf("rpd out of range")
		}
		sets = append(sets, "rate_limit_rpd=?")
		args = append(args, *rpd)
	}
	if tpm != nil {
		if *tpm < 0 || *tpm > 10000000 {
			return fmt.Errorf("tpm out of range")
		}
		sets = append(sets, "rate_limit_tpm=?")
		args = append(args, *tpm)
	}
	if allowedModels != nil {
		// Validate each model: non-empty, max 256 chars
		for _, m := range *allowedModels {
			if m == "" {
				return fmt.Errorf("allowed_models contains empty entry")
			}
			if len(m) > 256 {
				return fmt.Errorf("allowed_models entry too long: %q", m)
			}
		}
		b, _ := json.Marshal(*allowedModels)
		val := string(b)
		if len(*allowedModels) == 0 {
			val = ""
		}
		sets = append(sets, "allowed_models=?")
		args = append(args, val)
	}
	query := "UPDATE gateway_keys SET " + strings.Join(sets, ", ") + " WHERE id=?"
	args = append(args, id)
	_, err := s.db.Exec(db.Q(query), args...)
	if err != nil && (strings.Contains(err.Error(), "allowed_models") || strings.Contains(err.Error(), "rate_limit_rph") || strings.Contains(err.Error(), "rate_limit_rpd") || strings.Contains(err.Error(), "rate_limit_tpm")) {
		// Retry without new columns if they don't exist yet
		sets2 := []string{}
		args2 := []interface{}{}
		if rpm != nil {
			sets2 = append(sets2, "rate_limit_rpm=?")
			args2 = append(args2, *rpm)
		}
		if allowedModels != nil || rph != nil || rpd != nil || tpm != nil {
			return fmt.Errorf("new rate limit columns not yet migrated; run migrations")
		}
		if len(sets2) == 0 {
			return err
		}
		query2 := "UPDATE gateway_keys SET " + strings.Join(sets2, ", ") + " WHERE id=?"
		args2 = append(args2, id)
		_, err = s.db.Exec(db.Q(query2), args2...)
	}
	return err
}

// IsModelAllowed checks if model is permitted for the key (empty allowlist = all allowed).
// Supports exact, wildcard prefix (e.g. "gpt-4*"), and "provider/*" style is via wildcard.
// Matching is case-sensitive on the resolved model id.
// Also matches provider/model vs bare: "openai/gpt-4o" allows "gpt-4o" and vice versa (suffix after last "/").
func IsModelAllowed(allowed []string, model string) bool {
	if len(allowed) == 0 {
		return true
	}
	modelBase := model
	if idx := strings.LastIndex(model, "/"); idx != -1 {
		modelBase = model[idx+1:]
	}
	for _, pat := range allowed {
		if pat == "*" {
			return true
		}
		if pat == model {
			return true
		}
		// bare vs provider/model: allow suffix match in both directions
		if pat == modelBase {
			return true
		}
		if strings.Contains(pat, "/") {
			patBase := pat[strings.LastIndex(pat, "/")+1:]
			// handle wildcard inside patBase (e.g. "openai/gpt-4*")
			if strings.HasSuffix(patBase, "*") {
				prefix := strings.TrimSuffix(patBase, "*")
				if prefix != "" && (strings.HasPrefix(model, prefix) || strings.HasPrefix(modelBase, prefix)) {
					return true
				}
			} else if patBase == model || patBase == modelBase {
				return true
			}
		}
		if strings.HasSuffix(pat, "*") {
			prefix := strings.TrimSuffix(pat, "*")
			if strings.HasPrefix(model, prefix) || strings.HasPrefix(modelBase, prefix) {
				return true
			}
			continue
		}
	}
	return false
}

// SetKeyOwner reassigns a key's created_by. Empty userID clears ownership
// (the key becomes unowned — visible only to keys:read holders).
func (s *Store) SetKeyOwner(keyID, userID string) error {
	_, err := s.db.Exec(db.Q(`UPDATE gateway_keys SET created_by=? WHERE id=?`), sql.NullString{String: userID, Valid: userID != ""}, keyID)
	return err
}

// CountByOwner returns how many (non-revoked) keys each user id created —
// feeds the Users page "keys" column. Legacy DBs without the column yield
// an empty map rather than an error.
func (s *Store) CountByOwner() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT created_by, COUNT(*) FROM gateway_keys WHERE created_by IS NOT NULL AND created_by != '' AND revoked_at IS NULL GROUP BY created_by`)
	if err != nil {
		return map[string]int{}, nil
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var uid string
		var n int
		if err := rows.Scan(&uid, &n); err == nil {
			out[uid] = n
		}
	}
	return out, nil
}

// SetExpiresAt sets (or clears, with nil/zero time) the key's expiry.
func (s *Store) SetExpiresAt(id string, t *time.Time) error {
	var v any
	if t != nil && !t.IsZero() {
		v = t.UTC()
	}
	_, err := s.db.Exec(db.Q(`UPDATE gateway_keys SET expires_at = ? WHERE id = ?`), v, id)
	return err
}

// SetIPAllowlist replaces the key's IP allowlist (empty = any IP).
func (s *Store) SetIPAllowlist(id, allowlist string) error {
	_, err := s.db.Exec(db.Q(`UPDATE gateway_keys SET ip_allowlist = ? WHERE id = ?`), strings.TrimSpace(allowlist), id)
	return err
}

// SetMonthlyBudget sets the calendar-month spend cap in USD (0 = unlimited).
func (s *Store) SetMonthlyBudget(id string, usd float64) error {
	if usd < 0 {
		usd = 0
	}
	_, err := s.db.Exec(db.Q(`UPDATE gateway_keys SET monthly_budget_usd = ? WHERE id = ?`), usd, id)
	return err
}

// MonthSpendUSD returns the summed cost for a key within the current UTC
// calendar month. Used for monthly budget enforcement at authentication time.
func (s *Store) MonthSpendUSD(keyID string) float64 {
	var sum sql.NullFloat64
	monthStart := time.Now().UTC().Format("2006-01") + "-01 00:00:00"
	s.db.QueryRow(db.Q(
		`SELECT SUM(cost_usd) FROM request_logs WHERE key_id = ? AND created_at >= ?`),
		keyID, monthStart).Scan(&sum)
	if sum.Valid {
		return sum.Float64
	}
	return 0
}
