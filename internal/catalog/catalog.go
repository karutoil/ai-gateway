package catalog

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	var m models.CatalogModel
	var rt, rl, rol sql.NullString
	err := s.db.QueryRow(db.Q(`SELECT id, provider, name, description, family, context_window, max_output, input_cost, output_cost, cache_read_cost, cache_write_cost, reasoning, tool_call, structured_output, attachment, modalities, open_weights, knowledge_cutoff, updated_at, reasoning_type, reasoning_levels, reasoning_output_limits FROM models_catalog WHERE id LIKE ? LIMIT 1`), "%/"+shortID).Scan(&m.ID, &m.Provider, &m.Name, &m.Description, &m.Family, &m.ContextWindow, &m.MaxOutput, &m.InputCost, &m.OutputCost, &m.CacheReadCost, &m.CacheWriteCost, &m.Reasoning, &m.ToolCall, &m.StructuredOutput, &m.Attachment, &m.Modalities, &m.OpenWeights, &m.KnowledgeCutoff, &m.UpdatedAt, &rt, &rl, &rol)
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
