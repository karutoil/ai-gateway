// Package lb implements the operator-curated load-balancer routing rules:
// per-model ordered provider groups with a selectable strategy per rule
// (round-robin, random, weighted, failover). A member is a specific
// (provider, model) option — the same provider may appear multiple times
// with different model overrides, so a rule can offer as many specific
// provider+model options as the operator wants. For every strategy except
// failover, exactly ONE member serves each request — there is deliberately NO
// within-request failover between members (product decision). The failover
// strategy is the explicit opt-in: members are tried in position order until
// one commits a response.
package lb

import (
	"database/sql"
	"fmt"
	"math/rand"
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

// Rule strategies.
const (
	StrategyRoundRobin = "round_robin"
	StrategyRandom     = "random"
	StrategyWeighted   = "weighted"
	StrategyFailover   = "failover"
)

// ValidStrategy reports whether s names a supported strategy.
func ValidStrategy(s string) bool {
	switch s {
	case StrategyRoundRobin, StrategyRandom, StrategyWeighted, StrategyFailover:
		return true
	}
	return false
}

// MinWeight/MaxWeight bound per-member traffic weights (weighted strategy).
const (
	MinWeight = 1
	MaxWeight = 100
)

// Store manages lb_rules rows.
type Store struct{ DB *sql.DB }

func NewStore(database *sql.DB) *Store { return &Store{DB: database} }

// Member is one rule entry joined with live provider info for display.
type Member struct {
	ProviderID    string  `json:"provider_id"`
	Name          string  `json:"name"`
	Type          string  `json:"type"`
	Position      int     `json:"-"`
	Weight        int     `json:"weight,omitempty"`
	ModelOverride string  `json:"model_override,omitempty"`
	HealthStatus  *string `json:"health_status,omitempty"`
}

// RuleMemberInput is the write-path shape for one rule member.
type RuleMemberInput struct {
	ProviderID    string `json:"provider_id"`
	ModelOverride string `json:"model_override"`
	Weight        int    `json:"weight"`
}

// Rule is the UI-facing shape: model, strategy, and its ordered member list.
type Rule struct {
	Model    string   `json:"model"`
	Strategy string   `json:"strategy"`
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

// NormalizeStrategy trims/lowcases a strategy, defaulting empty to round_robin.
func NormalizeStrategy(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return StrategyRoundRobin
	}
	return s
}

// validateStrategyAndInputs is the full write-path validation: strategy must
// name a supported strategy, members must be well-formed for it.
func validateStrategyAndInputs(strategy string, members []RuleMemberInput) error {
	if !ValidStrategy(strategy) {
		return fmt.Errorf("invalid strategy %q (want round_robin, random, weighted, or failover)", strategy)
	}
	return validateRuleInputs(strategy, members)
}

// validateRuleInputs checks member shape against the strategy: provider ids
// required, weights in range, weighted rules require positive weights. The
// same provider may appear multiple times (e.g. two of its models as
// separate options) as long as each (provider, model_override) pair is
// unique — an exact duplicate would be two entries that can never be
// distinguished at request time.
func validateRuleInputs(strategy string, members []RuleMemberInput) error {
	if len(members) > 50 {
		return fmt.Errorf("too many members in rule (max 50)")
	}
	seen := make(map[string]bool, len(members))
	for i, m := range members {
		if m.ProviderID == "" {
			return fmt.Errorf("member %d: provider required", i)
		}
		if m.Weight < 0 || m.Weight > MaxWeight {
			return fmt.Errorf("member %d: weight must be between %d and %d", i, MinWeight, MaxWeight)
		}
		if strategy == StrategyWeighted && m.Weight < MinWeight {
			return fmt.Errorf("member %d: weighted rules require weight >= %d", i, MinWeight)
		}
		if len(m.ModelOverride) > 256 {
			return fmt.Errorf("member %d: model_override too long", i)
		}
		key := memberKey(m)
		if seen[key] {
			if strings.TrimSpace(m.ModelOverride) == "" {
				return fmt.Errorf("provider %q is already a member of this rule", m.ProviderID)
			}
			return fmt.Errorf("provider %q with model %q is already a member of this rule", m.ProviderID, m.ModelOverride)
		}
		seen[key] = true
	}
	return nil
}

// memberKey identifies a member by its (provider, model_override) pair —
// the unit of uniqueness within a rule.
func memberKey(m RuleMemberInput) string {
	return strings.ToLower(strings.TrimSpace(m.ProviderID)) + "\x00" + strings.ToLower(strings.TrimSpace(m.ModelOverride))
}

// ReplaceRule atomically swaps the member set AND strategy for a model.
// Empty members deletes the rule. strategy "" defaults to round_robin. The
// same provider may appear more than once with distinct model overrides;
// exact duplicate (provider, model) pairs are rejected.
func (s *Store) ReplaceRule(model, strategy string, members []RuleMemberInput) error {
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
	strategy = NormalizeStrategy(strategy)
	if err := validateStrategyAndInputs(strategy, members); err != nil {
		return err
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
	for pos, m := range members {
		var exists int
		if err := tx.QueryRow(db.Q(`SELECT COUNT(*) FROM providers WHERE id=?`), m.ProviderID).Scan(&exists); err != nil || exists == 0 {
			return fmt.Errorf("unknown provider %q", m.ProviderID)
		}
		weight := m.Weight
		if weight < MinWeight {
			weight = MinWeight
		}
		if _, err := tx.Exec(db.Q(`INSERT INTO lb_rules(id,model,provider_id,position,created_at,strategy,model_override,weight) VALUES(?,?,?,?,?,?,?,?)`),
			uuid.NewString(), model, m.ProviderID, pos, now, strategy, m.ModelOverride, weight); err != nil {
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
// Members marked health_status=down are still returned (selection filters
// them); if every member is down the full set returns and lets normal
// retry/failure handling report honestly.
func (s *Store) RuleForModel(model string) *Rule {
	model = normalizeModel(model)
	rows, err := s.DB.Query(db.Q(`SELECT lr.provider_id, p.name, p.type, lr.position, lr.strategy, lr.model_override, lr.weight, p.health_status FROM lb_rules lr JOIN providers p ON p.id = lr.provider_id WHERE lr.model=? ORDER BY lr.position ASC`), model)
	if err != nil {
		return nil
	}
	defer rows.Close()
	rule := &Rule{Model: model, Strategy: StrategyRoundRobin}
	for rows.Next() {
		var m Member
		var strategy, override string
		var hs sql.NullString
		if err := rows.Scan(&m.ProviderID, &m.Name, &m.Type, &m.Position, &strategy, &override, &m.Weight, &hs); err != nil {
			continue
		}
		if rule.Strategy == "" || rule.Strategy == StrategyRoundRobin {
			// First row wins; rows are written with identical strategy.
			rule.Strategy = strategy
		}
		m.ModelOverride = override
		if m.Weight < MinWeight {
			m.Weight = MinWeight
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

// RuleForModelOrGroup first checks for a per-model load-balancer rule. If none
// exists, it falls back to a model group with a matching name. This lets
// user-created groups ("subagent-dispatcher") be used directly as the model
// field in chat requests while still allowing explicit per-model rules to win.
func (s *Store) RuleForModelOrGroup(model string) *Rule {
	if rule := s.RuleForModel(model); rule != nil {
		return rule
	}
	return s.ruleForGroup(model)
}

// AllRules lists every configured rule with joined provider metadata,
// ordered by model then position.
func (s *Store) AllRules() ([]Rule, error) {
	rows, err := s.DB.Query(db.Q(`SELECT lr.model, lr.provider_id, p.name, p.type, lr.position, lr.strategy, lr.model_override, lr.weight, p.health_status FROM lb_rules lr JOIN providers p ON p.id = lr.provider_id ORDER BY lr.model ASC, lr.position ASC`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byModel := map[string]*Rule{}
	var order []string
	for rows.Next() {
		var model, pid, pname, ptype, strategy, override string
		var pos, weight int
		var hs sql.NullString
		if err := rows.Scan(&model, &pid, &pname, &ptype, &pos, &strategy, &override, &weight, &hs); err != nil {
			continue
		}
		m := Member{ProviderID: pid, Name: pname, Type: ptype, Position: pos, ModelOverride: override, Weight: weight}
		if m.Weight < MinWeight {
			m.Weight = MinWeight
		}
		if hs.Valid {
			m.HealthStatus = &hs.String
		}
		r, ok := byModel[model]
		if !ok {
			r = &Rule{Model: model, Strategy: strategy}
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

// Candidate is one ordered serving option for a request: the resolved
// provider plus the rule member that selected it. Carrying the member lets
// callers apply THAT member's model override — with multiple members per
// provider, a provider id alone no longer identifies the option.
type Candidate struct {
	*models.Provider
	Member Member
}

// ModelOverrideForRequest returns the upstream model id this candidate
// should send (its member's override, or "" to keep the requested model).
func (c Candidate) ModelOverrideForRequest() string {
	return c.Member.ModelOverride
}

// SelectCandidates orders the rule's healthy members for this request
// according to the rule's strategy and resolves each to a provider. The
// caller serves the first candidate; only failover rules walk further.
// Down members are filtered; if every member is down the full set returns
// and lets normal retry/failure handling report honestly.
func (s *Store) SelectCandidates(rule *Rule) []Candidate {
	if rule == nil || len(rule.Members) == 0 {
		return nil
	}
	ordered := s.orderMembers(rule)
	out := make([]Candidate, 0, len(ordered))
	for _, m := range ordered {
		p, err := (&providerFetcher{s}).byID(m.ProviderID)
		if err != nil || p == nil {
			continue
		}
		out = append(out, Candidate{Provider: p, Member: m})
	}
	return out
}

// Select orders the rule's healthy members and returns just the providers
// (legacy entry point — prefer SelectCandidates when per-member model
// overrides matter).
func (s *Store) Select(rule *Rule) []*models.Provider {
	cands := s.SelectCandidates(rule)
	out := make([]*models.Provider, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Provider)
	}
	return out
}

// orderMembers is the pure strategy-ordering core of Select (no DB access):
// it filters down members, applies the strategy ordering, and returns the
// walk order for this request.
func (s *Store) orderMembers(rule *Rule) []Member {
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

	ordered := make([]Member, len(eligible))
	switch rule.Strategy {
	case StrategyFailover:
		copy(ordered, eligible)
	case StrategyRandom:
		// Fisher-Yates on a copy; rand is auto-seeded (Go 1.20+).
		shuffled := make([]Member, len(eligible))
		copy(shuffled, eligible)
		rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		ordered = shuffled
	case StrategyWeighted:
		// Weighted random pick of the primary member, then the rest in
		// position order after it. The primary lands proportionally to
		// weight; remaining members only serve on retriable failure of the
		// primary (caller-side failover), preserving availability.
		total := 0
		for _, m := range eligible {
			w := m.Weight
			if w < MinWeight {
				w = MinWeight
			}
			total += w
		}
		pick := rand.Intn(total)
		pivot := 0
		for i, m := range eligible {
			w := m.Weight
			if w < MinWeight {
				w = MinWeight
			}
			if pick < w {
				pivot = i
				break
			}
			pick -= w
		}
		out := make([]Member, 0, len(eligible))
		out = append(out, eligible[pivot])
		for i := range eligible {
			if i != pivot {
				out = append(out, eligible[i])
			}
		}
		ordered = out
	default: // round_robin and anything unrecognized
		start := NextStart(len(eligible))
		for i := 0; i < len(eligible); i++ {
			ordered[i] = eligible[(start+i)%len(eligible)]
		}
	}
	return ordered
}

// RotateProviders is the legacy round-robin entry point retained for
// compatibility; equivalent to Select on a round_robin rule.
func (s *Store) RotateProviders(rule *Rule) []*models.Provider {
	return s.Select(rule)
}

// ModelOverrideFor returns the model id to send upstream for providerID
// within rule (the member's override, else the rule's model name). ok=false
// when the provider is not a rule member.
//
// Deprecated for routing decisions on rules that may repeat a provider: it
// resolves by provider id alone, so with several members on one provider it
// always answers with the FIRST match. The request path uses
// Candidate.ModelOverrideForRequest instead; this remains for simple
// lookups (single-member-per-provider rules, tests).
func (r *Rule) ModelOverrideFor(providerID string) (string, bool) {
	if r == nil {
		return "", false
	}
	for _, m := range r.Members {
		if m.ProviderID == providerID {
			if m.ModelOverride != "" {
				return m.ModelOverride, true
			}
			return r.Model, true
		}
	}
	return "", false
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
