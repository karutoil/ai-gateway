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

var groupNameRe = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)

func normalizeGroupName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// orgClause returns the SQL fragment and bind argument used to narrow a group
// query to a single organization. An empty orgID (global admin / runtime
// resolution / single-tenant) matches every group; a scoped orgID matches its
// own groups plus global (org_id IS NULL) shared groups.
func orgClause(orgID string) (string, interface{}) {
	if orgID == "" {
		return "1=1", nil
	}
	return "(org_id = ? OR org_id IS NULL)", orgID
}

// groupArg returns the variadic bind args for orgClause. When the clause takes
// no parameter it contributes no entries so the dynamic SQL stays balanced.
func groupArg(org interface{}) []interface{} {
	if org == nil {
		return nil
	}
	return []interface{}{org}
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
		return fmt.Errorf("group name may only contain a-z, 0-9, '.', '_', '-'")
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

	// Reject replacing a group owned by another organization, and remember its owner
	// so the delete below is scoped correctly.
	var owner string
	_ = tx.QueryRow(db.Q(`SELECT org_id FROM model_groups WHERE name=?`), name).Scan(&owner)
	if orgID != "" && owner != "" && owner != orgID {
		return nil, fmt.Errorf("group %q belongs to another organization", name)
	}

	// Remove existing group and members by name (scoped to the caller's org).
	memDeleteQ := `DELETE FROM model_group_members WHERE group_id IN (SELECT id FROM model_groups WHERE name=? AND org_id IS NULL)`
	if owner == orgID {
		if orgID == "" {
			memDeleteQ = `DELETE FROM model_group_members WHERE group_id IN (SELECT id FROM model_groups WHERE name=?)`
		} else {
			memDeleteQ = `DELETE FROM model_group_members WHERE group_id IN (SELECT id FROM model_groups WHERE name=? AND (org_id = ? OR org_id IS NULL))`
		}
	}
	if _, err := tx.Exec(db.Q(memDeleteQ), append([]interface{}{name}, groupArg(owner)...)...); err != nil {
		return nil, err
	}
	groupDeleteQ := `DELETE FROM model_groups WHERE name=? AND org_id IS NULL`
	if owner == orgID {
		if orgID == "" {
			groupDeleteQ = `DELETE FROM model_groups WHERE name=?`
		} else {
			groupDeleteQ = `DELETE FROM model_groups WHERE name=? AND (org_id = ? OR org_id IS NULL)`
		}
	}
	if _, err := tx.Exec(db.Q(groupDeleteQ), append([]interface{}{name}, groupArg(owner)...)...); err != nil {
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

	return s.GroupByName(name, orgID)
}

// DeleteGroup removes a model group and its members scoped to orgID. An empty
// orgID (global admin) may delete any group; a scoped orgID only its own or
// a global (org_id IS NULL) group.
func (s *Store) DeleteGroup(name, orgID string) error {
	if s.DB == nil {
		return fmt.Errorf("lb store unavailable")
	}
	name = normalizeGroupName(name)
	clause, org := orgClause(orgID)
	delQ := `DELETE FROM model_group_members WHERE group_id IN (SELECT id FROM model_groups WHERE name=` + "?" + ` AND ` + clause + `)`
	if _, err := s.DB.Exec(db.Q(delQ), append([]interface{}{name}, groupArg(org)...)...); err != nil {
		return err
	}
	groupDelQ := `DELETE FROM model_groups WHERE name=` + "?" + ` AND ` + clause
	_, err := s.DB.Exec(db.Q(groupDelQ), append([]interface{}{name}, groupArg(org)...)...)
	return err
}

// IsGroup reports whether a model group visible to orgID has this name.
func (s *Store) IsGroup(name, orgID string) bool {
	if s.DB == nil {
		return false
	}
	var n int
	clause, org := orgClause(orgID)
	q := `SELECT COUNT(*) FROM model_groups WHERE name=` + "?" + ` AND ` + clause
	args := append([]interface{}{normalizeGroupName(name)}, groupArg(org)...)
	err := s.DB.QueryRow(db.Q(q), args...).Scan(&n)
	return err == nil && n > 0
}

// GroupByName returns the full group record (scoped to orgID) with joined
// provider metadata. An empty orgID matches any group.
func (s *Store) GroupByName(name, orgID string) (*ModelGroup, error) {
	if s.DB == nil {
		return nil, fmt.Errorf("lb store unavailable")
	}
	name = normalizeGroupName(name)
	clause, org := orgClause(orgID)
	q := `SELECT id, name, display_name, strategy, created_at, org_id FROM model_groups WHERE name=` + "?" + ` AND ` + clause
	args := append([]interface{}{name}, groupArg(org)...)
	var g ModelGroup
	var orgv sql.NullString
	err := s.DB.QueryRow(db.Q(q), args...).
		Scan(&g.ID, &g.Name, &g.DisplayName, &g.Strategy, &g.CreatedAt, &orgv)
	if err != nil {
		return nil, err
	}
	if orgv.Valid {
		g.OrgID = &orgv.String
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

// AllGroups returns every model group visible to orgID with its members. An empty
// orgID returns groups across all organizations.
func (s *Store) AllGroups(orgID string) ([]ModelGroup, error) {
	if s.DB == nil {
		return nil, fmt.Errorf("lb store unavailable")
	}
	clause, org := orgClause(orgID)
	q := `SELECT id, name, display_name, strategy, created_at, org_id FROM model_groups WHERE ` + clause + ` ORDER BY name`
	args := append([]interface{}{}, groupArg(org)...)
	rows, err := s.DB.Query(db.Q(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []ModelGroup
	groupIdx := map[string]int{} // group ID -> slice index
	for rows.Next() {
		var g ModelGroup
		var orgv sql.NullString
		if err := rows.Scan(&g.ID, &g.Name, &g.DisplayName, &g.Strategy, &g.CreatedAt, &orgv); err != nil {
			continue
		}
		if orgv.Valid {
			g.OrgID = &orgv.String
		}
		groupIdx[g.ID] = len(groups)
		groups = append(groups, g)
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
		if idx, ok := groupIdx[gid]; ok {
			groups[idx].Members = append(groups[idx].Members, m)
		}
	}
	return groups, nil
}

// ruleForGroup converts a model group into a Rule suitable for the proxy's
// candidate selection. Members are ordered by position and the group strategy
// is applied. Runtime resolution treats groups as global (empty orgID).
func (s *Store) ruleForGroup(name string) *Rule {
	g, err := s.GroupByName(name, "")
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
