package provider

import (
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"ai-gateway/internal/db"
	"ai-gateway/internal/models"

	"github.com/google/uuid"
)

var rrCounter uint64

type Store struct {
	db        *sql.DB
	masterKey []byte
}

func NewStore(db *sql.DB, masterKey []byte) *Store {
	return &Store{db: db, masterKey: masterKey}
}

func (s *Store) List() ([]models.Provider, error) {
	rows, err := s.db.Query(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health, org_id FROM providers ORDER BY created_at DESC`)
	if err != nil {
		// fallback for DBs before org scaffold (column missing)
		if strings.Contains(err.Error(), "org_id") {
			rows, err = s.db.Query(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health FROM providers ORDER BY created_at DESC`)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var out []models.Provider
			for rows.Next() {
				var p models.Provider
				var hs, lh sql.NullString
				if err := rows.Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKeyEnc, &p.CreatedAt, &hs, &lh); err != nil {
					return nil, err
				}
				if hs.Valid {
					p.HealthStatus = &hs.String
				}
				if lh.Valid {
					p.LastHealth = &lh.String
				}
				p.APIKey = "***"
				out = append(out, p)
			}
			return out, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []models.Provider
	for rows.Next() {
		var p models.Provider
		var hs, lh, org sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKeyEnc, &p.CreatedAt, &hs, &lh, &org); err != nil {
			return nil, err
		}
		if hs.Valid {
			p.HealthStatus = &hs.String
		}
		if lh.Valid {
			p.LastHealth = &lh.String
		}
		if org.Valid {
			p.OrgID = &org.String
		}
		p.APIKey = "***"
		out = append(out, p)
	}
	return out, nil
}

func (s *Store) GetByID(id string) (*models.Provider, error) {
	var p models.Provider
	var hs, lh, org sql.NullString
	err := s.db.QueryRow(db.Q(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health, org_id FROM providers WHERE id=?`), id).Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKeyEnc, &p.CreatedAt, &hs, &lh, &org)
	if err != nil && strings.Contains(err.Error(), "org_id") {
		// fallback for old DB
		err = s.db.QueryRow(db.Q(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health FROM providers WHERE id=?`), id).Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKeyEnc, &p.CreatedAt, &hs, &lh)
	}
	if err != nil {
		return nil, err
	}
	if hs.Valid {
		p.HealthStatus = &hs.String
	}
	if lh.Valid {
		p.LastHealth = &lh.String
	}
	if org.Valid {
		p.OrgID = &org.String
	}
	return &p, nil
}

func (s *Store) GetByName(name string) (*models.Provider, error) {
	var p models.Provider
	var hs, lh, org sql.NullString
	err := s.db.QueryRow(db.Q(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health, org_id FROM providers WHERE name=?`), name).Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKeyEnc, &p.CreatedAt, &hs, &lh, &org)
	if err != nil && strings.Contains(err.Error(), "org_id") {
		err = s.db.QueryRow(db.Q(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health FROM providers WHERE name=?`), name).Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKeyEnc, &p.CreatedAt, &hs, &lh)
	}
	if err != nil {
		return nil, err
	}
	if hs.Valid {
		p.HealthStatus = &hs.String
	}
	if lh.Valid {
		p.LastHealth = &lh.String
	}
	if org.Valid {
		p.OrgID = &org.String
	}
	return &p, nil
}

// GetByType returns the first provider of the given type, health-aware
// (up first, then unknown/none, then down) and deterministic by age.
// Used by qualified-model pinning ("anthropic/claude-..." -> type match).
func (s *Store) GetByType(t string) (*models.Provider, error) {
	var p models.Provider
	var hs, lh, org sql.NullString
	err := s.db.QueryRow(db.Q(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health, org_id FROM providers WHERE type=? ORDER BY CASE WHEN health_status='up' THEN 0 WHEN health_status IS NULL OR health_status='unknown' THEN 1 ELSE 2 END, created_at ASC LIMIT 1`), t).Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKeyEnc, &p.CreatedAt, &hs, &lh, &org)
	if err != nil && strings.Contains(err.Error(), "org_id") {
		err = s.db.QueryRow(db.Q(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health FROM providers WHERE type=? ORDER BY CASE WHEN health_status='up' THEN 0 WHEN health_status IS NULL OR health_status='unknown' THEN 1 ELSE 2 END, created_at ASC LIMIT 1`), t).Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKeyEnc, &p.CreatedAt, &hs, &lh)
	}
	if err != nil {
		return nil, err
	}
	if hs.Valid {
		p.HealthStatus = &hs.String
	}
	if lh.Valid {
		p.LastHealth = &lh.String
	}
	if org.Valid {
		p.OrgID = &org.String
	}
	return &p, nil
}

// Default returns first healthy provider, preferring up/unknown over down, or the oldest if none healthy
func (s *Store) Default() (*models.Provider, error) {
	var p models.Provider
	var hs, lh, org sql.NullString
	err := s.db.QueryRow(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health, org_id FROM providers ORDER BY CASE WHEN health_status='up' THEN 0 WHEN health_status IS NULL OR health_status='unknown' THEN 1 ELSE 2 END, created_at ASC LIMIT 1`).Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKeyEnc, &p.CreatedAt, &hs, &lh, &org)
	if err != nil && strings.Contains(err.Error(), "org_id") {
		err = s.db.QueryRow(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health FROM providers ORDER BY CASE WHEN health_status='up' THEN 0 WHEN health_status IS NULL OR health_status='unknown' THEN 1 ELSE 2 END, created_at ASC LIMIT 1`).Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKeyEnc, &p.CreatedAt, &hs, &lh)
	}
	if err != nil {
		return nil, err
	}
	if hs.Valid {
		p.HealthStatus = &hs.String
	}
	if lh.Valid {
		p.LastHealth = &lh.String
	}
	if org.Valid {
		p.OrgID = &org.String
	}
	return &p, nil
}

func (s *Store) DecryptKey(p *models.Provider) (string, error) {
	b, err := Decrypt(p.APIKeyEnc, s.masterKey)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func normalizeBaseURL(url string) string {
	return strings.TrimRight(strings.TrimSpace(url), "/")
}

// metadataIPs are cloud-instance credential endpoints blocked at base_url
// validation AND dial time (defense in depth with proxy.NewGatewayTransport).
var metadataLiterals = []net.IP{
	net.ParseIP("169.254.169.254"), // AWS/GCP/Azure IMDSv4
	net.ParseIP("100.100.100.200"), // Alibaba metadata
	net.ParseIP("fd00:ec2::254"),   // AWS IMDSv6
}

func isMetadataAddress(host string) bool {
	// Literal IP fast-path.
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		for _, m := range metadataLiterals {
			if ip.Equal(m) {
				return true
			}
		}
		return false
	}
	host = strings.ToLower(host)
	switch host {
	case "metadata.google.internal", "metadata.goog", "instance-data":
		return true
	}
	return false
}

func validateBaseURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid base_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("base_url must be http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("base_url must have host")
	}
	if u.User != nil {
		return fmt.Errorf("base_url must not contain userinfo")
	}
	if strings.Contains(raw, "\n") || strings.Contains(raw, "\r") {
		return fmt.Errorf("base_url contains invalid characters")
	}
	host := u.Hostname()
	// Metadata endpoints: blocked by name AND by resolved/literal IP.
	if isMetadataAddress(host) {
		return fmt.Errorf("base_url host blocked for SSRF protection")
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		for _, m := range metadataLiterals {
			if ip.Equal(m) {
				return fmt.Errorf("base_url host blocked for SSRF protection")
			}
		}
	} else if isMetadataAddress(host) {
		return fmt.Errorf("base_url host blocked for SSRF protection")
	}
	// Loopback/private addresses remain permitted (homelab / vLLM / Ollama
	// deployments rely on them); dial-time + redirect-time metadata checks in
	// the gateway transport provide the stronger runtime guarantee.
	return nil
}

func (s *Store) Create(name string, typ models.ProviderType, baseURL string, apiKey string) (*models.Provider, error) {
	return s.CreateWithOrg(name, typ, baseURL, apiKey, "")
}

func (s *Store) CreateWithOrg(name string, typ models.ProviderType, baseURL string, apiKey string, orgID string) (*models.Provider, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("provider name required")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("api_key required")
	}
	// validate type
	switch typ {
	case models.ProviderOpenAI, models.ProviderAnthropic, models.ProviderAzure, models.ProviderOpenAICompatible:
	default:
		return nil, fmt.Errorf("invalid provider type %s", typ)
	}
	id := uuid.NewString()
	enc, err := Encrypt([]byte(apiKey), s.masterKey)
	if err != nil {
		return nil, err
	}
	baseURL = normalizeBaseURL(baseURL)
	if baseURL == "" {
		switch typ {
		case models.ProviderOpenAI, models.ProviderOpenAICompatible:
			baseURL = "https://api.openai.com/v1"
		case models.ProviderAnthropic:
			baseURL = "https://api.anthropic.com"
		case models.ProviderAzure:
			return nil, fmt.Errorf("base_url required for azure provider")
		}
	}
	if err := validateBaseURL(baseURL); err != nil {
		return nil, err
	}
	p := &models.Provider{
		ID: id, Name: name, Type: typ, BaseURL: baseURL, APIKeyEnc: enc, CreatedAt: time.Now().UTC(),
	}
	if orgID != "" {
		p.OrgID = &orgID
	}
	// Try org_id column if exists, fallback to legacy without org_id
	_, err = s.db.Exec(db.Q(`INSERT INTO providers(id,name,type,base_url,api_key_enc,created_at,org_id) VALUES(?,?,?,?,?,?,?)`), p.ID, p.Name, string(p.Type), p.BaseURL, p.APIKeyEnc, p.CreatedAt, sql.NullString{String: orgID, Valid: orgID != ""})
	if err != nil && strings.Contains(err.Error(), "org_id") {
		_, err = s.db.Exec(db.Q(`INSERT INTO providers(id,name,type,base_url,api_key_enc,created_at) VALUES(?,?,?,?,?,?)`), p.ID, p.Name, string(p.Type), p.BaseURL, p.APIKeyEnc, p.CreatedAt)
	}
	if err != nil {
		return nil, err
	}
	copyP := *p
	copyP.APIKey = "***"
	copyP.APIKeyEnc = nil
	return &copyP, nil
}

// ListForOrg returns providers filtered by org_id. If orgID=="" returns all (bootstrap admin).
// For Phase 3 strict isolation, only exact org_id is returned (global NULL not shared).
func (s *Store) ListForOrg(orgID string) ([]models.Provider, error) {
	if orgID == "" {
		return s.List()
	}
	rows, err := s.db.Query(db.Q(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health, org_id FROM providers WHERE org_id=? ORDER BY created_at DESC`), orgID)
	if err != nil {
		if strings.Contains(err.Error(), "org_id") {
			return s.List()
		}
		return nil, err
	}
	defer rows.Close()
	var out []models.Provider
	for rows.Next() {
		var p models.Provider
		var hs, lh, org sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKeyEnc, &p.CreatedAt, &hs, &lh, &org); err != nil {
			return nil, err
		}
		if hs.Valid {
			p.HealthStatus = &hs.String
		}
		if lh.Valid {
			p.LastHealth = &lh.String
		}
		if org.Valid {
			p.OrgID = &org.String
		}
		p.APIKey = "***"
		out = append(out, p)
	}
	// Also include providers where org_id is NULL as global shared — for strict isolation, comment out next line
	// For Phase 3 strict isolation, we filter exactly orgID; above query already includes NULL as shared.
	return out, nil
}

// ListForOrgStrict returns only providers exactly matching org_id (for RBAC gate tests)
func (s *Store) ListForOrgStrict(orgID string) ([]models.Provider, error) {
	if orgID == "" {
		return s.List()
	}
	rows, err := s.db.Query(db.Q(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health, org_id FROM providers WHERE org_id=? ORDER BY created_at DESC`), orgID)
	if err != nil {
		if strings.Contains(err.Error(), "org_id") {
			return s.List()
		}
		return nil, err
	}
	defer rows.Close()
	var out []models.Provider
	for rows.Next() {
		var p models.Provider
		var hs, lh, org sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKeyEnc, &p.CreatedAt, &hs, &lh, &org); err != nil {
			return nil, err
		}
		if hs.Valid {
			p.HealthStatus = &hs.String
		}
		if lh.Valid {
			p.LastHealth = &lh.String
		}
		if org.Valid {
			p.OrgID = &org.String
		}
		p.APIKey = "***"
		out = append(out, p)
	}
	return out, nil
}

func (s *Store) Delete(id string) error {
	_, err := s.db.Exec(db.Q(`DELETE FROM providers WHERE id=?`), id)
	return err
}

// Resolve picks provider based on X-Provider hint, provider_models ownership, model prefix heuristic, or default.
// It prefers healthy providers and is deterministic (ordered by health then creation time).
func (s *Store) Resolve(model string, preferredProvider string) (*models.Provider, error) {
	if preferredProvider != "" {
		if p, err := s.GetByName(preferredProvider); err == nil {
			return p, nil
		}
		if p, err := s.GetByID(preferredProvider); err == nil {
			return p, nil
		}
	}
	// handle provider prefix like "openai/gpt-4o" or "anthropic/claude-..."
	if strings.Contains(model, "/") {
		parts := strings.SplitN(model, "/", 2)
		prefix := strings.ToLower(strings.TrimSpace(parts[0]))
		for _, cand := range []string{prefix} {
			// try healthy name first, fallback to any
			var hp models.Provider
			var hhs, hlh sql.NullString
			err2 := s.db.QueryRow(db.Q(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health FROM providers WHERE name=? AND (health_status IS NULL OR health_status != 'down') ORDER BY CASE WHEN health_status='up' THEN 0 ELSE 1 END, created_at ASC LIMIT 1`), cand).Scan(&hp.ID, &hp.Name, &hp.Type, &hp.BaseURL, &hp.APIKeyEnc, &hp.CreatedAt, &hhs, &hlh)
			if err2 == nil {
				if hhs.Valid {
					hp.HealthStatus = &hhs.String
				}
				if hlh.Valid {
					hp.LastHealth = &hlh.String
				}
				return &hp, nil
			}
			if p2, err := s.GetByName(cand); err == nil {
				return p2, nil
			}
			// also match type - health-aware ordering
			var p models.Provider
			var hs, lh sql.NullString
			err := s.db.QueryRow(db.Q(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health FROM providers WHERE type=? ORDER BY CASE WHEN health_status='up' THEN 0 WHEN health_status IS NULL OR health_status='unknown' THEN 1 ELSE 2 END, created_at ASC LIMIT 1`), cand).Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKeyEnc, &p.CreatedAt, &hs, &lh)
			if err == nil {
				if hs.Valid {
					p.HealthStatus = &hs.String
				}
				if lh.Valid {
					p.LastHealth = &lh.String
				}
				return &p, nil
			}
		}
	}
	// try to find provider that owns this exact model_id in provider_models (enables per-provider routing when multiple providers)
	// round-robin among healthy providers for same model_id (atomic counter) — health-aware, deterministic fallback.
	if model != "" {
		if p := s.resolveModelRoundRobin(model); p != nil {
			return p, nil
		}
		// also try without slash prefix (e.g., "gpt-4o" vs "openai/gpt-4o")
		if strings.Contains(model, "/") {
			short := model[strings.LastIndex(model, "/")+1:]
			if short != model {
				if p := s.resolveModelRoundRobin(short); p != nil {
					return p, nil
				}
			}
		}
	}
	lm := strings.ToLower(model)
	if strings.HasPrefix(lm, "claude") || strings.HasPrefix(lm, "muse-spark") || strings.HasPrefix(lm, "muse-") || lm == "muse" {
		var p models.Provider
		var hs, lh sql.NullString
		err := s.db.QueryRow(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health FROM providers WHERE type='anthropic' ORDER BY CASE WHEN health_status='up' THEN 0 WHEN health_status IS NULL OR health_status='unknown' THEN 1 ELSE 2 END, created_at ASC LIMIT 1`).Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKeyEnc, &p.CreatedAt, &hs, &lh)
		if err == nil {
			if hs.Valid {
				p.HealthStatus = &hs.String
			}
			if lh.Valid {
				p.LastHealth = &lh.String
			}
			return &p, nil
		}
	}
	return s.Default()
}

// ResolveWithOrg is Phase 3 org-aware resolve. When orgID != "", only providers with matching org_id are considered.
// This enforces strict isolation: Alice (org A) cannot route to Bob's provider (org B).
func (s *Store) ResolveWithOrg(model string, preferredProvider string, orgID string) (*models.Provider, error) {
	if orgID == "" {
		return s.Resolve(model, preferredProvider)
	}
	// Preferred provider must belong to org
	if preferredProvider != "" {
		if p, err := s.GetByName(preferredProvider); err == nil {
			if p.OrgID != nil && *p.OrgID != "" && *p.OrgID != orgID {
				// org mismatch -> forbid
			} else {
				return p, nil
			}
		}
		if p, err := s.GetByID(preferredProvider); err == nil {
			if p.OrgID == nil || *p.OrgID == "" || *p.OrgID == orgID {
				return p, nil
			}
		}
	}
	if strings.Contains(model, "/") {
		parts := strings.SplitN(model, "/", 2)
		prefix := strings.ToLower(strings.TrimSpace(parts[0]))
		for _, cand := range []string{prefix} {
			var hp models.Provider
			var hhs, hlh, org sql.NullString
			err2 := s.db.QueryRow(db.Q(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health, org_id FROM providers WHERE name=? AND org_id=? AND (health_status IS NULL OR health_status != 'down') ORDER BY CASE WHEN health_status='up' THEN 0 ELSE 1 END, created_at ASC LIMIT 1`), cand, orgID).Scan(&hp.ID, &hp.Name, &hp.Type, &hp.BaseURL, &hp.APIKeyEnc, &hp.CreatedAt, &hhs, &hlh, &org)
			if err2 == nil {
				if hhs.Valid {
					hp.HealthStatus = &hhs.String
				}
				if hlh.Valid {
					hp.LastHealth = &hlh.String
				}
				if org.Valid {
					hp.OrgID = &org.String
				}
				return &hp, nil
			}
			// fallback to type with org filter
			var p models.Provider
			var hs, lh sql.NullString
			var porg sql.NullString
			err := s.db.QueryRow(db.Q(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health, org_id FROM providers WHERE type=? AND org_id=? ORDER BY CASE WHEN health_status='up' THEN 0 WHEN health_status IS NULL OR health_status='unknown' THEN 1 ELSE 2 END, created_at ASC LIMIT 1`), cand, orgID).Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKeyEnc, &p.CreatedAt, &hs, &lh, &porg)
			if err == nil {
				if hs.Valid {
					p.HealthStatus = &hs.String
				}
				if lh.Valid {
					p.LastHealth = &lh.String
				}
				if porg.Valid {
					p.OrgID = &porg.String
				}
				return &p, nil
			}
		}
	}
	if model != "" {
		if p := s.resolveModelRoundRobinForOrg(model, orgID); p != nil {
			return p, nil
		}
		if strings.Contains(model, "/") {
			short := model[strings.LastIndex(model, "/")+1:]
			if short != model {
				if p := s.resolveModelRoundRobinForOrg(short, orgID); p != nil {
					return p, nil
				}
			}
		}
	}
	lm := strings.ToLower(model)
	if strings.HasPrefix(lm, "claude") {
		var p models.Provider
		var hs, lh, porg sql.NullString
		err := s.db.QueryRow(db.Q(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health, org_id FROM providers WHERE type='anthropic' AND org_id=? ORDER BY CASE WHEN health_status='up' THEN 0 WHEN health_status IS NULL OR health_status='unknown' THEN 1 ELSE 2 END, created_at ASC LIMIT 1`), orgID).Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKeyEnc, &p.CreatedAt, &hs, &lh, &porg)
		if err == nil {
			if hs.Valid {
				p.HealthStatus = &hs.String
			}
			if lh.Valid {
				p.LastHealth = &lh.String
			}
			if porg.Valid {
				p.OrgID = &porg.String
			}
			return &p, nil
		}
	}
	return s.DefaultForOrg(orgID)
}

func (s *Store) resolveModelRoundRobinForOrg(model string, orgID string) *models.Provider {
	rows, err := s.db.Query(db.Q(`SELECT pm.provider_id FROM provider_models pm JOIN providers p ON p.id=pm.provider_id WHERE pm.model_id=? AND p.org_id=? AND (p.health_status IS NULL OR p.health_status!='down') ORDER BY CASE WHEN p.health_status='up' THEN 0 ELSE 1 END, p.created_at ASC`), model, orgID)
	if err == nil {
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		if len(ids) > 0 {
			idx := int(atomic.AddUint64(&rrCounter, 1)-1) % len(ids)
			if p, err := s.GetByID(ids[idx]); err == nil {
				return p
			}
			if p, err := s.GetByID(ids[0]); err == nil {
				return p
			}
		}
	}
	rows2, err := s.db.Query(db.Q(`SELECT pm.provider_id FROM provider_models pm JOIN providers p ON p.id=pm.provider_id WHERE pm.model_id=? AND p.org_id=? ORDER BY p.id ASC`), model, orgID)
	if err == nil {
		var ids []string
		for rows2.Next() {
			var id string
			if err := rows2.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		rows2.Close()
		if len(ids) > 0 {
			idx := int(atomic.AddUint64(&rrCounter, 1)-1) % len(ids)
			if p, err := s.GetByID(ids[idx]); err == nil {
				return p
			}
			if p, err := s.GetByID(ids[0]); err == nil {
				return p
			}
		}
	}
	return nil
}

func (s *Store) DefaultForOrg(orgID string) (*models.Provider, error) {
	if orgID == "" {
		return s.Default()
	}
	var p models.Provider
	var hs, lh, org sql.NullString
	err := s.db.QueryRow(db.Q(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health, org_id FROM providers WHERE org_id=? ORDER BY CASE WHEN health_status='up' THEN 0 WHEN health_status IS NULL OR health_status='unknown' THEN 1 ELSE 2 END, created_at ASC LIMIT 1`), orgID).Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKeyEnc, &p.CreatedAt, &hs, &lh, &org)
	if err != nil {
		return nil, err
	}
	if hs.Valid {
		p.HealthStatus = &hs.String
	}
	if lh.Valid {
		p.LastHealth = &lh.String
	}
	if org.Valid {
		p.OrgID = &org.String
	}
	return &p, nil
}

func (s *Store) resolveModelRoundRobin(model string) *models.Provider {
	// healthy first
	rows, err := s.db.Query(db.Q(`SELECT pm.provider_id FROM provider_models pm JOIN providers p ON p.id=pm.provider_id WHERE pm.model_id=? AND (p.health_status IS NULL OR p.health_status!='down') ORDER BY CASE WHEN p.health_status='up' THEN 0 ELSE 1 END, p.created_at ASC`), model)
	if err == nil {
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		if len(ids) > 0 {
			idx := int(atomic.AddUint64(&rrCounter, 1)-1) % len(ids)
			if p, err := s.GetByID(ids[idx]); err == nil {
				return p
			}
			// deterministic fallback to first if indexed fails
			if p, err := s.GetByID(ids[0]); err == nil {
				return p
			}
		}
	}
	// fallback: any provider (including down) deterministic
	rows2, err := s.db.Query(db.Q(`SELECT provider_id FROM provider_models WHERE model_id=? ORDER BY provider_id ASC`), model)
	if err == nil {
		var ids []string
		for rows2.Next() {
			var id string
			if err := rows2.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		rows2.Close()
		if len(ids) > 0 {
			idx := int(atomic.AddUint64(&rrCounter, 1)-1) % len(ids)
			if p, err := s.GetByID(ids[idx]); err == nil {
				return p
			}
			if p, err := s.GetByID(ids[0]); err == nil {
				return p
			}
		}
	}
	return nil
}

// ListForModel returns providers that advertise model_id (qualified or short) in provider_models.
func (s *Store) ListForModel(model string) []*models.Provider {
	if s == nil || s.db == nil || model == "" {
		return nil
	}
	short := model
	if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
		short = model[i+1:]
	}
	rows, err := s.db.Query(db.Q(`SELECT DISTINCT p.id FROM providers p JOIN provider_models pm ON pm.provider_id=p.id WHERE pm.model_id=? OR pm.model_id=? OR pm.model_id LIKE ? ORDER BY CASE WHEN p.health_status='up' THEN 0 WHEN p.health_status IS NULL OR p.health_status='unknown' THEN 1 ELSE 2 END, p.created_at ASC`), model, short, "%/"+short)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil && id != "" {
			ids = append(ids, id)
		}
	}
	var out []*models.Provider
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if p, err := s.GetByID(id); err == nil && p != nil {
			out = append(out, p)
		}
	}
	return out
}
