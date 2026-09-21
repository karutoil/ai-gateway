package lb

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"ai-gateway/internal/db"

	"github.com/google/uuid"
)

// ModelGroup is a user-creatable alias that maps a single group name to an
// ordered list of provider/model members. It is resolved in the same routing
// path as per-model lb_rules: when a client sends the group name as the model,
// the gateway expands it to the member list and applies the group's strategy.
type ModelGroup struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	DisplayName string    `json:"display_name"`
	Strategy    string    `json:"strategy"`
	CreatedAt   time.Time `json:"created_at"`
	OrgID       *string   `json:"org_id,omitempty"`
	Members     []Member  `json:"members"`
}

var groupNameRe = regexp.MustCompile(`^[a-z0-9._/-]{1,64}$`)

func normalizeGroupName(n string) string {
	return strings.ToLower(strings.TrimSpace(n))
}

func validateGroupName(n string) error {
	n = normalizeGroupName(n)
	if n == "" {
		return fmt.Errorf("group name required")
	}
	if len(n) > 64 {
		return fmt.Errorf("group name too long (max 64)")
	}
	if !groupNameRe.MatchString(n) {
		return fmt.Errorf("group name may only contain a-z, 0-9, '.', '_', '-', '/'")
	}
	return nil
}

// ReplaceGroup creates or replaces a model group keyed by name. Members must
// contain unique provider IDs and the group strategy is validated.
func (s *Store) ReplaceGroup(name, displayName, strategy, orgID string, members []RuleMemberInput) (*ModelGroup, error) {
	if s.DB == nil {
		return nil, fmt.Errorf("lb store unavailable")
	}
	name = normalizeGroupName(name)
	if err := validateGroupName(name); err != nil {
		return nil, err
	}
	strategy = NormalizeStrategy(strategy)
	if err := validateStrategyAndInputs(strategy, members); err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("members required")
	}
	for i, m := range members {
		if m.ModelOverride == "" {
			return nil, fmt.Errorf("member %d: model_override is required for group members", i)
		}
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Remove existing group and members by name.
	if _, err := tx.Exec(db.Q(`DELETE FROM model_group_members WHERE group_id IN (SELECT id FROM model_groups WHERE name=?)`), name); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(db.Q(`DELETE FROM model_groups WHERE name=?`), name); err != nil {
		return nil, err
	}

	groupID := uuid.NewString()
	now := time.Now().UTC()
	var orgSQL interface{}
	if orgID == "" {
		orgSQL = nil
	} else {
		orgSQL = orgID
	}
	if _, err := tx.Exec(db.Q(`INSERT INTO model_groups(id,name,display_name,strategy,created_at,org_id) VALUES(?,?,?,?,?,?)`),
		groupID, name, displayName, strategy, now, orgSQL); err != nil {
		return nil, err
	}

	for pos, m := range members {
		for _, prev := range members[:pos] {
			if prev.ProviderID == m.ProviderID {
				return nil, fmt.Errorf("duplicate provider in group")
			}
		}
		var exists int
		if err := tx.QueryRow(db.Q(`SELECT COUNT(*) FROM providers WHERE id=?`), m.ProviderID).Scan(&exists); err != nil || exists == 0 {
			return nil, fmt.Errorf("unknown provider %q", m.ProviderID)
		}
		weight := m.Weight
		if weight < MinWeight {
			weight = MinWeight
		}
		if _, err := tx.Exec(db.Q(`INSERT INTO model_group_members(id,group_id,provider_id,position,model_override,weight,created_at) VALUES(?,?,?,?,?,?,?)`),
			uuid.NewString(), groupID, m.ProviderID, pos, m.ModelOverride, weight, now); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.GroupByName(name)
}

// DeleteGroup removes a model group and its members by name.
func (s *Store) DeleteGroup(name string) error {
	if s.DB == nil {
		return fmt.Errorf("lb store unavailable")
	}
	name = normalizeGroupName(name)
	_, err := s.DB.Exec(db.Q(`DELETE FROM model_groups WHERE name=?`), name)
	return err
}

// IsGroup reports whether a model group with this name exists.
func (s *Store) IsGroup(name string) bool {
	if s.DB == nil {
		return false
	}
	var n int
	err := s.DB.QueryRow(db.Q(`SELECT COUNT(*) FROM model_groups WHERE name=?`), normalizeGroupName(name)).Scan(&n)
	return err == nil && n > 0
}

// GroupByName returns the full group record with joined provider metadata.
func (s *Store) GroupByName(name string) (*ModelGroup, error) {
	if s.DB == nil {
		return nil, fmt.Errorf("lb store unavailable")
	}
	name = normalizeGroupName(name)
	var g ModelGroup
	var org sql.NullString
	err := s.DB.QueryRow(db.Q(`SELECT id, name, display_name, strategy, created_at, org_id FROM model_groups WHERE name=?`), name).
		Scan(&g.ID, &g.Name, &g.DisplayName, &g.Strategy, &g.CreatedAt, &org)
	if err != nil {
		return nil, err
	}
	if org.Valid {
		g.OrgID = &org.String
	}
	rows, err := s.DB.Query(db.Q(`SELECT mgm.provider_id, p.name, p.type, mgm.position, mgm.model_override, mgm.weight, p.health_status FROM model_group_members mgm JOIN providers p ON p.id = mgm.provider_id WHERE mgm.group_id=? ORDER BY mgm.position ASC`), g.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m Member
		var override string
		var hs sql.NullString
		if err := rows.Scan(&m.ProviderID, &m.Name, &m.Type, &m.Position, &override, &m.Weight, &hs); err != nil {
			continue
		}
		m.ModelOverride = override
		if m.Weight < MinWeight {
			m.Weight = MinWeight
		}
		if hs.Valid {
			m.HealthStatus = &hs.String
		}
		g.Members = append(g.Members, m)
	}
	return &g, nil
}

// AllGroups returns every model group with its members.
func (s *Store) AllGroups() ([]ModelGroup, error) {
	if s.DB == nil {
		return nil, fmt.Errorf("lb store unavailable")
	}
	rows, err := s.DB.Query(db.Q(`SELECT id, name, display_name, strategy, created_at, org_id FROM model_groups ORDER BY name`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []ModelGroup
	groupByID := map[string]*ModelGroup{}
	for rows.Next() {
		var g ModelGroup
		var org sql.NullString
		if err := rows.Scan(&g.ID, &g.Name, &g.DisplayName, &g.Strategy, &g.CreatedAt, &org); err != nil {
			continue
		}
		if org.Valid {
			g.OrgID = &org.String
		}
		groups = append(groups, g)
		groupByID[g.ID] = &groups[len(groups)-1]
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(groups) == 0 {
		return groups, nil
	}

	// Single query for all members.
	mrows, err := s.DB.Query(db.Q(`SELECT mgm.group_id, mgm.provider_id, p.name, p.type, mgm.position, mgm.model_override, mgm.weight, p.health_status FROM model_group_members mgm JOIN providers p ON p.id = mgm.provider_id ORDER BY mgm.group_id, mgm.position`))
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var gid, override string
		var m Member
		var hs sql.NullString
		if err := mrows.Scan(&gid, &m.ProviderID, &m.Name, &m.Type, &m.Position, &override, &m.Weight, &hs); err != nil {
			continue
		}
		m.ModelOverride = override
		if m.Weight < MinWeight {
			m.Weight = MinWeight
		}
		if hs.Valid {
			m.HealthStatus = &hs.String
		}
		if g, ok := groupByID[gid]; ok {
			g.Members = append(g.Members, m)
		}
	}
	return groups, nil
}

// ruleForGroup converts a model group into a Rule suitable for the proxy's
// candidate selection. Members are ordered by position and the group strategy
// is applied.
func (s *Store) ruleForGroup(name string) *Rule {
	g, err := s.GroupByName(name)
	if err != nil || g == nil {
		return nil
	}
	rule := &Rule{
		Model:    g.Name,
		Strategy: g.Strategy,
		Members:  g.Members,
	}
	return rule
}
