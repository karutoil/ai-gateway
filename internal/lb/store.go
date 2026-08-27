// Package lb implements the operator-curated load-balancer routing rules:
// per-model ordered provider groups. Rotation happens ACROSS requests
// (each request selects one member by a monotonically increasing offset);
// there is deliberately NO within-request failover between members — that is
// a product decision, not an omission.
package lb

import (
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"ai-gateway/internal/db"
	"ai-gateway/internal/models"

	"github.com/google/uuid"
)

var rrOffset uint64

// NextStart returns the rotating start index for a group of size n.
func NextStart(n int) int {
	if n <= 0 {
		return 0
	}
	return int(atomic.AddUint64(&rrOffset, 1)-1) % n
}

// Store manages lb_rules rows.
type Store struct{ DB *sql.DB }

func NewStore(database *sql.DB) *Store { return &Store{DB: database} }

// Member is one rule entry joined with live provider info for display.
type Member struct {
	ProviderID   string  `json:"provider_id"`
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	Position     int     `json:"-"`
	HealthStatus *string `json:"health_status,omitempty"`
}

// Rule is the UI-facing shape: model plus its ordered member list.
type Rule struct {
	Model    string   `json:"model"`
	Members  []Member `json:"providers"`
	PinnedOK bool     `json:"-"` // informational: qualified IDs still bypass (always true)
}

// normalizeModel canonicalizes rule keys: trimmed, lowercase, no slash-form
// duplication ("OpenAI/gpt-4o" normalizes to "openai/gpt-4o"; bare names stay
// bare). Rules are keyed on the POST-alias-resolution model string, matching
// what candidate selection sees.
func normalizeModel(m string) string {
	return strings.ToLower(strings.TrimSpace(m))
}

// ReplaceRule atomically swaps the member set for a model. Empty ids deletes.
func (s *Store) ReplaceRule(model string, providerIDs []string) error {
	if s.DB == nil {
		return fmt.Errorf("lb store unavailable")
	}
	model = normalizeModel(model)
	if model == "" {
		return fmt.Errorf("model required")
	}
	if len(model) > 256 {
		return fmt.Errorf("model name too long")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(db.Q(`DELETE FROM lb_rules WHERE model=?`), model); err != nil {
		return err
	}
	now := time.Now().UTC()
	for pos, pid := range providerIDs {
		for _, prev := range providerIDs[:pos] {
			if prev == pid {
				return fmt.Errorf("duplicate provider in rule")
			}
		}
		var exists int
		if err := tx.QueryRow(db.Q(`SELECT COUNT(*) FROM providers WHERE id=?`), pid).Scan(&exists); err != nil || exists == 0 {
			return fmt.Errorf("unknown provider %q", pid)
		}
		if _, err := tx.Exec(db.Q(`INSERT INTO lb_rules(id,model,provider_id,position,created_at) VALUES(?,?,?,?,?)`),
			uuid.NewString(), model, pid, pos, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteRule removes the rule for a model entirely.
func (s *Store) DeleteRule(model string) error {
	_, err := s.DB.Exec(db.Q(`DELETE FROM lb_rules WHERE model=?`), normalizeModel(model))
	return err
}

// RuleForModel returns the ordered rule for a model, or nil when none exists.
// Members marked health_status=down are skipped so rotation never lands on a
// known-bad upstream; if every member is down the full set returns and lets
// normal breaker/error handling report honestly.
func (s *Store) RuleForModel(model string) *Rule {
	model = normalizeModel(model)
	rows, err := s.DB.Query(db.Q(`SELECT lr.provider_id, p.name, p.type, lr.position, p.health_status FROM lb_rules lr JOIN providers p ON p.id = lr.provider_id WHERE lr.model=? ORDER BY lr.position ASC`), model)
	if err != nil {
		return nil
	}
	defer rows.Close()
	rule := &Rule{Model: model}
	for rows.Next() {
		var m Member
		var hs sql.NullString
		if err := rows.Scan(&m.ProviderID, &m.Name, &m.Type, &m.Position, &hs); err != nil {
			continue
		}
		if hs.Valid {
			m.HealthStatus = &hs.String
		}
		rule.Members = append(rule.Members, m)
	}
	if err := rows.Err(); err != nil || len(rule.Members) == 0 {
		return nil
	}
	return rule
}

// AllRules lists every configured rule with joined provider metadata,
// ordered by model then position.
func (s *Store) AllRules() ([]Rule, error) {
	rows, err := s.DB.Query(db.Q(`SELECT lr.model, lr.provider_id, p.name, p.type, lr.position, p.health_status FROM lb_rules lr JOIN providers p ON p.id = lr.provider_id ORDER BY lr.model ASC, lr.position ASC`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byModel := map[string]*Rule{}
	var order []string
	for rows.Next() {
		var model, pid, pname, ptype string
		var pos int
		var hs sql.NullString
		if err := rows.Scan(&model, &pid, &pname, &ptype, &pos, &hs); err != nil {
			continue
		}
		m := Member{ProviderID: pid, Name: pname, Type: ptype, Position: pos}
		if hs.Valid {
			m.HealthStatus = &hs.String
		}
		r, ok := byModel[model]
		if !ok {
			r = &Rule{Model: model}
			byModel[model] = r
			order = append(order, model)
		}
		r.Members = append(r.Members, m)
	}
	out := make([]Rule, 0, len(order))
	for _, k := range order {
		out = append(out, *byModel[k])
	}
	return out, nil
}

// ProvidersFromMembers materializes rule members into provider records filtered
// to healthy-or-unknown, rotated so each call starts at the next position —
// this is the round-robin across requests. Returns nil when no healthy member
// exists.
func (s *Store) RotateProviders(rule *Rule) []*models.Provider {
	var eligible []Member
	for _, m := range rule.Members {
		if m.HealthStatus != nil && *m.HealthStatus == "down" {
			continue
		}
		eligible = append(eligible, m)
	}
	if len(eligible) == 0 {
		eligible = rule.Members // let honest errors surface instead of silence
	}
	start := NextStart(len(eligible))
	var out []*models.Provider
	for i := 0; i < len(eligible); i++ {
		m := eligible[(start+i)%len(eligible)]
		p, err := (&providerFetcher{s}).byID(m.ProviderID)
		if err != nil || p == nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

// providerFetcher isolates the single-row provider fetch used above.
type providerFetcher struct{ s *Store }

func (f *providerFetcher) byID(id string) (*models.Provider, error) {
	var p models.Provider
	var hs, lh, org sql.NullString
	err := f.s.DB.QueryRow(db.Q(`SELECT id, name, type, base_url, api_key_enc, created_at, health_status, last_health, org_id FROM providers WHERE id=?`), id).
		Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.APIKeyEnc, &p.CreatedAt, &hs, &lh, &org)
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
	p.APIKey = "***"
	return &p, nil
}
