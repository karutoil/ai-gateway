package catalog

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
)

const ModelsDevURL = "https://models.dev/api.json"

type RawModel struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	Family           string `json:"family"`
	Attachment       bool   `json:"attachment"`
	Reasoning        bool   `json:"reasoning"`
	ReasoningOptions []struct {
		Type   string   `json:"type"` // effort, toggle
		Values []string `json:"values"`
	} `json:"reasoning_options"`
	ToolCall         bool   `json:"tool_call"`
	StructuredOutput bool   `json:"structured_output"`
	Temperature      bool   `json:"temperature"`
	Knowledge        string `json:"knowledge"`
	ReleaseDate      string `json:"release_date"`
	LastUpdated      string `json:"last_updated"`
	Modalities       struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	OpenWeights bool `json:"open_weights"`
	Limit       struct {
		Context int `json:"context"`
		Output  int `json:"output"`
		Input   int `json:"input"`
	} `json:"limit"`
	Cost struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cache_read"`
		CacheWrite float64 `json:"cache_write"`
	} `json:"cost"`
}

type RawProvider struct {
	ID     string              `json:"id"`
	Name   string              `json:"name"`
	API    string              `json:"api"`
	Models map[string]RawModel `json:"models"`
}

type Store struct {
	db     *sql.DB
	client *http.Client
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db, client: &http.Client{Timeout: 30 * time.Second}}
}

func (s *Store) Count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM models_catalog`).Scan(&n)
	return n, err
}

func (s *Store) List(filter string, providerFilter string, reasoningOnly bool, limit int, offset int) ([]models.CatalogModel, error) {
	where := "1=1"
	args := []interface{}{}
	if filter != "" {
		where += " AND (id LIKE ? OR name LIKE ? OR family LIKE ?)"
		like := "%" + filter + "%"
		args = append(args, like, like, like)
	}
	if providerFilter != "" {
		where += " AND provider = ?"
		args = append(args, providerFilter)
	}
	if reasoningOnly {
		where += " AND reasoning = " + db.BoolLit(true)
	}
	query := fmt.Sprintf(`SELECT id, provider, name, description, family, context_window, max_output, input_cost, output_cost, cache_read_cost, cache_write_cost, reasoning, tool_call, structured_output, attachment, modalities, open_weights, knowledge_cutoff, updated_at, reasoning_type, reasoning_levels, reasoning_output_limits FROM models_catalog WHERE %s ORDER BY provider, id LIMIT ? OFFSET ?`, where)
	args = append(args, limit, offset)
	rows, err := s.db.Query(db.Rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.CatalogModel
	for rows.Next() {
		var m models.CatalogModel
		var rt, rl, rol sql.NullString
		if err := rows.Scan(&m.ID, &m.Provider, &m.Name, &m.Description, &m.Family, &m.ContextWindow, &m.MaxOutput, &m.InputCost, &m.OutputCost, &m.CacheReadCost, &m.CacheWriteCost, &m.Reasoning, &m.ToolCall, &m.StructuredOutput, &m.Attachment, &m.Modalities, &m.OpenWeights, &m.KnowledgeCutoff, &m.UpdatedAt, &rt, &rl, &rol); err != nil {
			return nil, err
		}
		if rt.Valid {
			m.ReasoningType = rt.String
		}
		if rl.Valid {
			m.ReasoningLevels = rl.String
		}
		if rol.Valid {
			m.ReasoningOutputLimits = rol.String
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *Store) Get(id string) (*models.CatalogModel, error) {
	var m models.CatalogModel
	var rt, rl, rol sql.NullString
	err := s.db.QueryRow(db.Q(`SELECT id, provider, name, description, family, context_window, max_output, input_cost, output_cost, cache_read_cost, cache_write_cost, reasoning, tool_call, structured_output, attachment, modalities, open_weights, knowledge_cutoff, updated_at, reasoning_type, reasoning_levels, reasoning_output_limits FROM models_catalog WHERE id=?`), id).Scan(&m.ID, &m.Provider, &m.Name, &m.Description, &m.Family, &m.ContextWindow, &m.MaxOutput, &m.InputCost, &m.OutputCost, &m.CacheReadCost, &m.CacheWriteCost, &m.Reasoning, &m.ToolCall, &m.StructuredOutput, &m.Attachment, &m.Modalities, &m.OpenWeights, &m.KnowledgeCutoff, &m.UpdatedAt, &rt, &rl, &rol)
	if err != nil {
		return nil, err
	}
	if rt.Valid {
		m.ReasoningType = rt.String
	}
	if rl.Valid {
		m.ReasoningLevels = rl.String
	}
	if rol.Valid {
		m.ReasoningOutputLimits = rol.String
	}
	return &m, nil
}

func (s *Store) GetByShortID(shortID string) (*models.CatalogModel, error) {
	if m, err := s.Get(shortID); err == nil {
		return m, nil
	}
	// Gateway-qualified IDs carry the *gateway provider* prefix
	// ("oc1/muse-spark-1.3-contributor"), which never matches catalog IDs
	// (keyed by models.dev provider namespace). Strip to the suffix before
	// the suffix match, otherwise every cost lookup misses and bills $0.
	trimmed := strings.TrimSpace(shortID)
	if i := strings.LastIndex(trimmed, "/"); i >= 0 && i+1 < len(trimmed) {
		if m, err := s.Get(trimmed[i+1:]); err == nil {
			return m, nil
		}
		trimmed = trimmed[i+1:]
	}
	if trimmed == "" {
		return nil, sql.ErrNoRows
	}
	var m models.CatalogModel
	var rt, rl, rol sql.NullString
	// Prefer a priced row: the suffix match can hit many providers and an
	// arbitrary LIMIT 1 may return a zero-price mirror (e.g. ollama-cloud),
	// which again bills $0 despite priced rows existing.
	err := s.db.QueryRow(db.Q(`SELECT id, provider, name, description, family, context_window, max_output, input_cost, output_cost, cache_read_cost, cache_write_cost, reasoning, tool_call, structured_output, attachment, modalities, open_weights, knowledge_cutoff, updated_at, reasoning_type, reasoning_levels, reasoning_output_limits FROM models_catalog WHERE id LIKE ? ORDER BY (CASE WHEN input_cost>0 OR output_cost>0 THEN 0 ELSE 1 END), id LIMIT 1`), "%/"+trimmed).Scan(&m.ID, &m.Provider, &m.Name, &m.Description, &m.Family, &m.ContextWindow, &m.MaxOutput, &m.InputCost, &m.OutputCost, &m.CacheReadCost, &m.CacheWriteCost, &m.Reasoning, &m.ToolCall, &m.StructuredOutput, &m.Attachment, &m.Modalities, &m.OpenWeights, &m.KnowledgeCutoff, &m.UpdatedAt, &rt, &rl, &rol)
	if err != nil {
		return nil, err
	}
	if rt.Valid {
		m.ReasoningType = rt.String
	}
	if rl.Valid {
		m.ReasoningLevels = rl.String
	}
	if rol.Valid {
		m.ReasoningOutputLimits = rol.String
	}
	return &m, nil
}

// NormalizeModelID strips reseller channel tags and routing prefixes,
// returning the bare upstream slug ("[aws][量] grok-4.6" -> "grok-4.6").
// Reseller upstreams decorate every model ID with billing-channel markers
// ("[Kiro3][正价] claude-opus-4-6 [不补]"); without stripping, no catalog
// lookup can match and discovery records zero context, pricing and
// capability flags. When several whitespace-separated tokens remain, the
// last one wins: decorations lead ("[tag] slug"), never trail, unbracketed.
func NormalizeModelID(id string) string {
	s := strings.TrimSpace(id)
	for {
		changed := false
		for strings.HasPrefix(s, "[") {
			end := strings.Index(s, "]")
			if end < 0 {
				break
			}
			s = strings.TrimSpace(s[end+1:])
			changed = true
		}
		for strings.HasSuffix(s, "]") {
			start := strings.LastIndex(s, "[")
			if start < 0 {
				break
			}
			s = strings.TrimSpace(s[:start])
			changed = true
		}
		// Gateway routing prefix ("oc1/slug", "ck-default/[aws] slug").
		if i := strings.LastIndex(s, "/"); i >= 0 && i+1 < len(s) {
			if tail := strings.TrimSpace(s[i+1:]); tail != "" && tail != s {
				s = tail
				changed = true
			}
		}
		if !changed || s == "" {
			break
		}
	}
	if f := strings.Fields(s); len(f) > 1 {
		s = f[len(f)-1]
	}
	return s
}

// signalsThinking reports whether a model slug names a reasoning variant.
func signalsThinking(slug string) bool {
	return strings.Contains(strings.ToLower(slug), "think")
}

// ensureReasoning forces the reasoning flag on a catalog row matched for a
// thinking-named slug whose base row is not flagged (e.g. only the
// non-thinking base exists in the snapshot).
func ensureReasoning(m *models.CatalogModel) *models.CatalogModel {
	if m == nil {
		return nil
	}
	m.Reasoning = true
	if m.ReasoningType == "" || m.ReasoningType == "none" {
		m.ReasoningType = "toggle"
		m.ReasoningLevels = `["off","on"]`
		if m.ReasoningOutputLimits == "" {
			m.ReasoningOutputLimits = "{}"
		}
	}
	return m
}

// thinkAlternates proposes the alternate "-think"/"-thinking" spelling:
// the catalog snapshot mixes both ("x-think" vs upstream "x-thinking").
func thinkAlternates(slug string) []string {
	low := strings.ToLower(slug)
	if strings.HasSuffix(low, "-thinking") {
		return []string{slug[:len(slug)-3]}
	}
	if strings.HasSuffix(low, "-think") {
		return []string{slug + "ing"}
	}
	return nil
}

// cutThinkingSuffix strips a trailing "-thinking"/"-think", reporting the base.
func cutThinkingSuffix(slug string) (string, bool) {
	low := strings.ToLower(slug)
	if strings.HasSuffix(low, "-thinking") {
		return slug[:len(slug)-len("-thinking")], true
	}
	if strings.HasSuffix(low, "-think") {
		return slug[:len(slug)-len("-think")], true
	}
	return "", false
}

// vAlternates proposes "-vN" <-> "-N" spelling variants
// ("deepseek-3.2" vs catalog "deepseek-v3.2").
func vAlternates(slug string) []string {
	var out []string
	if stripped := stripVPrefix(slug); stripped != slug {
		out = append(out, stripped)
	}
	if inserted := insertVPrefix(slug); inserted != slug {
		out = append(out, inserted)
	}
	return out
}

func stripVPrefix(slug string) string {
	var b strings.Builder
	for i := 0; i < len(slug); {
		if slug[i] == '-' && i+2 < len(slug) && slug[i+1] == 'v' && slug[i+2] >= '0' && slug[i+2] <= '9' {
			b.WriteByte('-')
			i += 2
			continue
		}
		b.WriteByte(slug[i])
		i++
	}
	return b.String()
}

func insertVPrefix(slug string) string {
	for i := 0; i+1 < len(slug); i++ {
		if slug[i] == '-' && slug[i+1] >= '0' && slug[i+1] <= '9' {
			return slug[:i+1] + "v" + slug[i+1:]
		}
	}
	return slug
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}

// FindBestMatch resolves a (possibly reseller-tagged) model ID to the closest
// catalog row. It returns the match kind:
//
//	"exact"      — verbatim or provider-suffix match, same as Get/GetByShortID
//	"normalized" — match after stripping channel tags ("[aws] grok-4.6")
//	"wildcard"   — approximate: think/think spelling, "-vN" variant,
//	               thinking base, or dated-version containment
//	               ("claude-opus-4-1" -> "…/claude-opus-4-1-20250805")
//
// Callers should record "wildcard" distinctly (provider_models.source =
// "enriched-wildcard") so estimated pricing is distinguishable from exact
// catalog data. Unknown slugs return sql.ErrNoRows.
func (s *Store) FindBestMatch(modelID string) (*models.CatalogModel, string, error) {
	if m, sub := s.trySlug(modelID, signalsThinking(modelID)); m != nil {
		if sub == "" {
			return m, "exact", nil
		}
		return m, "wildcard", nil
	}
	norm := NormalizeModelID(modelID)
	if norm == "" {
		return nil, "", sql.ErrNoRows
	}
	thinking := signalsThinking(norm)
	if norm != strings.TrimSpace(modelID) {
		if m, sub := s.trySlug(norm, thinking); m != nil {
			if sub == "" {
				return m, "normalized", nil
			}
			return m, "wildcard", nil
		}
	}
	// Dated-version containment ("claude-opus-4-1" -> "…-4-1-20250805").
	if len(norm) >= 4 {
		if m, err := s.getByContains(norm); err == nil {
			if thinking {
				m = ensureReasoning(m)
			}
			return m, "wildcard", nil
		}
	}
	// Segment backoff for suffixed variants ("gemini-3.5-flash-high" ->
	// "gemini-3.5-flash", "glm-5.2-venice" -> "glm-5.2").
	for cand := backoffSegment(norm); len(cand) >= 6; cand = backoffSegment(cand) {
		if m, _ := s.trySlug(cand, thinking); m != nil {
			if thinking {
				m = ensureReasoning(m)
			}
			return m, "wildcard", nil
		}
	}
	return nil, "", sql.ErrNoRows
}

// trySlug runs the per-slug match tiers: verbatim/suffix hits return
// subkind "", approximate (think spelling, "-vN" variant, thinking base)
// hits return "wild".
func (s *Store) trySlug(slug string, thinking bool) (*models.CatalogModel, string) {
	if m, err := s.Get(slug); err == nil {
		return m, ""
	}
	if m, err := s.GetByShortID(slug); err == nil {
		return m, ""
	}
	for _, alt := range thinkAlternates(slug) {
		if m, err := s.GetByShortID(alt); err == nil {
			return ensureReasoning(m), "wild"
		}
	}
	for _, alt := range vAlternates(slug) {
		if m, err := s.GetByShortID(alt); err == nil {
			if thinking {
				m = ensureReasoning(m)
			}
			return m, "wild"
		}
	}
	if base, ok := cutThinkingSuffix(slug); ok && base != "" {
		if m, err := s.GetByShortID(base); err == nil {
			return ensureReasoning(m), "wild"
		}
		for _, alt := range vAlternates(base) {
			if m, err := s.GetByShortID(alt); err == nil {
				return ensureReasoning(m), "wild"
			}
		}
		if m, err := s.getByContains(base); err == nil {
			return ensureReasoning(m), "wild"
		}
	}
	return nil, ""
}

// backoffSegment drops the last dash-separated segment
// ("gemini-3.5-flash-high" -> "gemini-3.5-flash").
func backoffSegment(slug string) string {
	if i := strings.LastIndex(slug, "-"); i > 0 {
		return slug[:i]
	}
	return ""
}

// getByContains matches dated or suffixed catalog variants of a slug,
// preferring priced rows then the shortest (closest-version) ID.
// Case-insensitive: reseller slugs do not always match snapshot casing.
func (s *Store) getByContains(slug string) (*models.CatalogModel, error) {
	pat := "%" + escapeLike(slug) + "%"
	var m models.CatalogModel
	var rt, rl, rol sql.NullString
	err := s.db.QueryRow(db.Q(`SELECT id, provider, name, description, family, context_window, max_output, input_cost, output_cost, cache_read_cost, cache_write_cost, reasoning, tool_call, structured_output, attachment, modalities, open_weights, knowledge_cutoff, updated_at, reasoning_type, reasoning_levels, reasoning_output_limits FROM models_catalog WHERE LOWER(id) LIKE LOWER(?) ESCAPE '\' ORDER BY (CASE WHEN input_cost>0 OR output_cost>0 THEN 0 ELSE 1 END), LENGTH(id), id LIMIT 1`), pat).Scan(&m.ID, &m.Provider, &m.Name, &m.Description, &m.Family, &m.ContextWindow, &m.MaxOutput, &m.InputCost, &m.OutputCost, &m.CacheReadCost, &m.CacheWriteCost, &m.Reasoning, &m.ToolCall, &m.StructuredOutput, &m.Attachment, &m.Modalities, &m.OpenWeights, &m.KnowledgeCutoff, &m.UpdatedAt, &rt, &rl, &rol)
	if err != nil {
		return nil, err
	}
	if rt.Valid {
		m.ReasoningType = rt.String
	}
	if rl.Valid {
		m.ReasoningLevels = rl.String
	}
	if rol.Valid {
		m.ReasoningOutputLimits = rol.String
	}
	return &m, nil
}

func (s *Store) FetchAndSync() (int, error) {
	resp, err := s.client.Get(ModelsDevURL)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("models.dev status %d", resp.StatusCode)
	}
	// limit to 20MB
	resp.Body = io.NopCloser(io.LimitReader(resp.Body, 20<<20))
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	var providers map[string]RawProvider
	if err := json.Unmarshal(body, &providers); err != nil {
		return 0, err
	}
	return s.syncFromProviders(providers)
}

func (s *Store) SyncFromBytes(body []byte) (int, error) {
	var providers map[string]RawProvider
	if err := json.Unmarshal(body, &providers); err != nil {
		return 0, err
	}
	return s.syncFromProviders(providers)
}

func (s *Store) syncFromProviders(providers map[string]RawProvider) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(db.Q(`INSERT INTO models_catalog(id, provider, name, description, family, context_window, max_output, input_cost, output_cost, cache_read_cost, cache_write_cost, reasoning, tool_call, structured_output, attachment, modalities, open_weights, knowledge_cutoff, updated_at, reasoning_type, reasoning_levels, reasoning_output_limits) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`) + db.UpsertEnd([]string{"id"}, []string{"provider", "name", "description", "family", "context_window", "max_output", "input_cost", "output_cost", "cache_read_cost", "cache_write_cost", "reasoning", "tool_call", "structured_output", "attachment", "modalities", "open_weights", "knowledge_cutoff", "updated_at", "reasoning_type", "reasoning_levels", "reasoning_output_limits"}))
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	count := 0
	now := time.Now().UTC()
	for provID, prov := range providers {
		for _, m := range prov.Models {
			modalitiesBytes, _ := json.Marshal(m.Modalities)
			fullID := m.ID
			if len(fullID) < 3 || !containsSlash(fullID) {
				fullID = provID + "/" + m.ID
			}
			ctx := m.Limit.Context
			if ctx == 0 {
				ctx = m.Limit.Input
			}
			out := m.Limit.Output
			reasoningType, reasoningLevelsJSON, reasoningLimitsJSON := parseReasoningOptions(m)
			_, err := stmt.Exec(fullID, provID, m.Name, m.Description, m.Family, ctx, out, m.Cost.Input, m.Cost.Output, m.Cost.CacheRead, m.Cost.CacheWrite, m.Reasoning, m.ToolCall, m.StructuredOutput, m.Attachment, string(modalitiesBytes), m.OpenWeights, m.Knowledge, now, reasoningType, reasoningLevelsJSON, reasoningLimitsJSON)
			if err != nil {
				return count, err
			}
			count++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.db.Exec(db.Q(`INSERT INTO system_config(key,value,updated_at) VALUES('models_last_sync',?,?)`)+db.UpsertEnd([]string{"key"}, []string{"value", "updated_at"}), now.Format(time.RFC3339), now)
	return count, nil
}

func parseReasoningOptions(m RawModel) (string, string, string) {
	if !m.Reasoning {
		return "none", "[]", "{}"
	}
	if len(m.ReasoningOptions) == 0 {
		// reasoning without options → single toggle
		b, _ := json.Marshal([]string{"on"})
		return "toggle", string(b), "{}"
	}
	// prefer first option
	opt := m.ReasoningOptions[0]
	if opt.Type == "toggle" {
		b, _ := json.Marshal([]string{"off", "on"})
		return "toggle", string(b), "{}"
	}
	// effort type
	if len(opt.Values) > 0 {
		b, _ := json.Marshal(opt.Values)
		// default per-level output limits as empty map, UI will allow setting
		return opt.Type, string(b), "{}"
	}
	b, _ := json.Marshal([]string{"low", "medium", "high"})
	return "effort", string(b), "{}"
}

func containsSlash(s string) bool {
	for _, c := range s {
		if c == '/' {
			return true
		}
	}
	return false
}

// CostFor computes cost USD given tokens and model
func CostFor(m *models.CatalogModel, promptTokens, completionTokens int) float64 {
	if m == nil {
		return 0
	}
	return float64(promptTokens)*m.InputCost/1_000_000 + float64(completionTokens)*m.OutputCost/1_000_000
}
