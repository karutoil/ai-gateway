package discovery

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ai-gateway/internal/cache"
	"ai-gateway/internal/catalog"
	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"

	"github.com/google/uuid"
)

type Service struct {
	db            *sql.DB
	providerStore *provider.Store
	catalogStore  *catalog.Store
	client        *http.Client
	Cache         cache.Cache
}

func New(db *sql.DB, ps *provider.Store, cs *catalog.Store) *Service {
	return &Service{
		db: db, providerStore: ps, catalogStore: cs,
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

type rawModelList struct {
	Data []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
		Created int64  `json:"created"`
	} `json:"data"`
	Object string `json:"object"`
}

// Discover fetches /v1/models from provider and upserts provider_models, enriching from catalog
func (s *Service) Discover(providerID string) (int, error) {
	p, err := s.providerStore.GetByID(providerID)
	if err != nil {
		return 0, err
	}
	apiKey, err := s.providerStore.DecryptKey(p)
	if err != nil {
		return 0, err
	}
	var fetched []rawModel
	switch p.Type {
	case models.ProviderAnthropic:
		fetched = s.fetchAnthropic(p, apiKey)
	case models.ProviderAzure:
		fetched = s.fetchAzure(p, apiKey)
		if len(fetched) == 0 {
			fetched = s.fetchOpenAI(p, apiKey)
		}
	default:
		fetched = s.fetchOpenAI(p, apiKey)
		if len(fetched) == 0 && p.Type == models.ProviderAnthropic {
			fetched = s.fetchAnthropic(p, apiKey)
		}
	}
	if len(fetched) == 0 {
		return 0, fmt.Errorf("no models discovered (check provider base_url and key)")
	}
	count := 0
	for _, m := range fetched {
		if err := s.upsert(p, m); err == nil {
			count++
		}
	}
	if s.Cache != nil && count > 0 {
		s.Cache.Invalidate("models:")
	}
	return count, nil
}

func (s *Service) fetchOpenAI(p *models.Provider, apiKey string) []rawModel {
	target := strings.TrimRight(p.BaseURL, "/") + "/models"
	// if base_url ends with /v1/models already? our store normalizes base_url without trailing slash, e.g., https://ckff.dev/v1, then +/models = /v1/models correct. If base_url is https://ckff.dev, +/models = /models wrong. So try both.
	urls := []string{target}
	if !strings.Contains(p.BaseURL, "/v1") {
		urls = append(urls, strings.TrimRight(p.BaseURL, "/")+"/v1/models")
	}
	for _, u := range urls {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := s.client.Do(req)
		if err != nil || resp.StatusCode != 200 {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var list rawModelList
		if json.Unmarshal(body, &list) == nil && len(list.Data) > 0 {
			var out []rawModel
			for _, d := range list.Data {
				out = append(out, rawModel{ID: d.ID, OwnedBy: d.OwnedBy})
			}
			return out
		}
		// try anthropic-style response for openai compatible (some return different shape)
		// fallback to generic map
		var generic map[string]interface{}
		if json.Unmarshal(body, &generic) == nil {
			if data, ok := generic["data"].([]interface{}); ok {
				var out []rawModel
				for _, item := range data {
					if mm, ok := item.(map[string]interface{}); ok {
						id, _ := mm["id"].(string)
						owned, _ := mm["owned_by"].(string)
						if id != "" {
							out = append(out, rawModel{ID: id, OwnedBy: owned})
						}
					}
				}
				if len(out) > 0 {
					return out
				}
			}
		}
	}
	return nil
}

func (s *Service) fetchAzure(p *models.Provider, apiKey string) []rawModel {
	base := strings.TrimRight(p.BaseURL, "/")
	urls := []string{base + "/models?api-version=2024-02-01", base + "/models"}
	for _, u := range urls {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("api-key", apiKey)
		resp, err := s.client.Do(req)
		if err != nil || resp.StatusCode != 200 {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var list rawModelList
		if json.Unmarshal(body, &list) == nil && len(list.Data) > 0 {
			var out []rawModel
			for _, d := range list.Data {
				out = append(out, rawModel{ID: d.ID, OwnedBy: d.OwnedBy})
			}
			return out
		}
		var generic map[string]interface{}
		if json.Unmarshal(body, &generic) == nil {
			if data, ok := generic["data"].([]interface{}); ok {
				var out []rawModel
				for _, item := range data {
					if mm, ok := item.(map[string]interface{}); ok {
						id, _ := mm["id"].(string)
						owned, _ := mm["owned_by"].(string)
						if id != "" {
							out = append(out, rawModel{ID: id, OwnedBy: owned})
						}
					}
				}
				if len(out) > 0 {
					return out
				}
			}
		}
	}
	return nil
}

func (s *Service) fetchAnthropic(p *models.Provider, apiKey string) []rawModel {
	target := strings.TrimRight(p.BaseURL, "/") + "/v1/models"
	if strings.HasSuffix(p.BaseURL, "/v1/models") {
		target = p.BaseURL
	} else if !strings.Contains(p.BaseURL, "/v1") {
		target = strings.TrimRight(p.BaseURL, "/") + "/v1/models"
	}
	req, _ := http.NewRequest("GET", target, nil)
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := s.client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var list rawModelList
	if json.Unmarshal(body, &list) == nil && len(list.Data) > 0 {
		var out []rawModel
		for _, d := range list.Data {
			out = append(out, rawModel{ID: d.ID, OwnedBy: d.OwnedBy})
		}
		return out
	}
	// generic anthropic models shape: {data: [{id:...}]}
	var generic map[string]interface{}
	if json.Unmarshal(body, &generic) == nil {
		if data, ok := generic["data"].([]interface{}); ok {
			var out []rawModel
			for _, item := range data {
				if mm, ok := item.(map[string]interface{}); ok {
					id, _ := mm["id"].(string)
					if id == "" {
						id, _ = mm["display_name"].(string)
					}
					if id != "" {
						out = append(out, rawModel{ID: id})
					}
				}
			}
			return out
		}
	}
	return nil
}

type rawModel struct {
	ID      string
	OwnedBy string
}

func (s *Service) upsert(p *models.Provider, m rawModel) error {
	// enrich from catalog
	var ctx, maxOut int
	var inputCost, outputCost float64
	var reasoning, toolCall, structuredOutput, attachment bool
	var modalities, reasoningType, reasoningLevels, reasoningLimits string
	source := "discovered"
	if s.catalogStore != nil {
		if cm, err := s.catalogStore.Get(m.ID); err == nil {
			ctx, maxOut = cm.ContextWindow, cm.MaxOutput
			inputCost, outputCost = cm.InputCost, cm.OutputCost
			reasoning, toolCall, structuredOutput, attachment = cm.Reasoning, cm.ToolCall, cm.StructuredOutput, cm.Attachment
			modalities = cm.Modalities
			reasoningType, reasoningLevels, reasoningLimits = cm.ReasoningType, cm.ReasoningLevels, cm.ReasoningOutputLimits
			source = "enriched"
		} else if cm, err := s.catalogStore.GetByShortID(m.ID); err == nil {
			ctx, maxOut = cm.ContextWindow, cm.MaxOutput
			inputCost, outputCost = cm.InputCost, cm.OutputCost
			reasoning, toolCall, structuredOutput, attachment = cm.Reasoning, cm.ToolCall, cm.StructuredOutput, cm.Attachment
			modalities = cm.Modalities
			reasoningType, reasoningLevels, reasoningLimits = cm.ReasoningType, cm.ReasoningLevels, cm.ReasoningOutputLimits
			source = "enriched"
		}
	}
	// check existing to preserve manual overrides
	var existingID string
	var existingSource string
	err := s.db.QueryRow(db.Q(`SELECT id, source FROM provider_models WHERE provider_id=? AND model_id=?`), p.ID, m.ID).Scan(&existingID, &existingSource)
	if err == nil && existingSource == "manual" {
		// don't overwrite manual
		return nil
	}
	if err == nil {
		_, err = s.db.Exec(db.Q(`UPDATE provider_models SET display_name=?, owned_by=?, context_window=?, max_output=?, input_cost=?, output_cost=?, reasoning=?, tool_call=?, structured_output=?, attachment=?, modalities=?, reasoning_type=?, reasoning_levels=?, reasoning_output_limits=?, source=?, updated_at=? WHERE id=?`),
			m.ID, m.OwnedBy, ctx, maxOut, inputCost, outputCost, reasoning, toolCall, structuredOutput, attachment, modalities, reasoningType, reasoningLevels, reasoningLimits, source, time.Now().UTC(), existingID)
		return err
	}
	id := uuid.NewString()
	_, err = s.db.Exec(db.Q(`INSERT INTO provider_models(id, provider_id, model_id, display_name, owned_by, context_window, max_output, input_cost, output_cost, reasoning, tool_call, structured_output, attachment, modalities, reasoning_type, reasoning_levels, reasoning_output_limits, source, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		id, p.ID, m.ID, m.ID, m.OwnedBy, ctx, maxOut, inputCost, outputCost, reasoning, toolCall, structuredOutput, attachment, modalities, reasoningType, reasoningLevels, reasoningLimits, source, time.Now().UTC(), time.Now().UTC())
	return err
}

// List returns provider_models with provider join, filtered
func (s *Service) List(providerID, q string) ([]models.ProviderModel, error) {
	where := "1=1"
	args := []interface{}{}
	if providerID != "" {
		where += " AND pm.provider_id = ?"
		args = append(args, providerID)
	}
	if q != "" {
		where += " AND (pm.model_id LIKE ? OR pm.display_name LIKE ? OR p.name LIKE ? OR (p.name || '/' || pm.model_id) LIKE ?)"
		like := "%" + q + "%"
		args = append(args, like, like, like, like)
	}
	rows, err := s.db.Query(db.Q(`SELECT pm.id, pm.provider_id, pm.model_id, pm.display_name, pm.owned_by, pm.context_window, pm.max_output, pm.input_cost, pm.output_cost, pm.cache_read_cost, pm.cache_write_cost, pm.reasoning, pm.tool_call, pm.structured_output, pm.attachment, pm.modalities, pm.source, pm.created_at, pm.updated_at, pm.reasoning_type, pm.reasoning_levels, pm.reasoning_output_limits, p.name FROM provider_models pm JOIN providers p ON p.id=pm.provider_id WHERE `+where+` ORDER BY p.name ASC, pm.model_id ASC LIMIT 500`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.ProviderModel
	for rows.Next() {
		var pm models.ProviderModel
		var cc, cw sql.NullFloat64
		var rn, tl, so, at sql.NullBool
		var rt, rl, rol sql.NullString
		var provName string
		if err := rows.Scan(&pm.ID, &pm.ProviderID, &pm.ModelID, &pm.DisplayName, &pm.OwnedBy, &pm.ContextWindow, &pm.MaxOutput, &pm.InputCost, &pm.OutputCost, &cc, &cw, &rn, &tl, &so, &at, &pm.Modalities, &pm.Source, &pm.CreatedAt, &pm.UpdatedAt, &rt, &rl, &rol, &provName); err != nil {
			continue
		}
		if cc.Valid {
			pm.CacheReadCost = cc.Float64
		}
		if cw.Valid {
			pm.CacheWriteCost = cw.Float64
		}
		if rn.Valid {
			pm.Reasoning = rn.Bool
		}
		if tl.Valid {
			pm.ToolCall = tl.Bool
		}
		if so.Valid {
			pm.StructuredOutput = so.Bool
		}
		if at.Valid {
			pm.Attachment = at.Bool
		}
		if rt.Valid {
			pm.ReasoningType = rt.String
		}
		if rl.Valid {
			pm.ReasoningLevels = rl.String
		}
		if rol.Valid {
			pm.ReasoningOutputLimits = rol.String
		}
		pm.ProviderName = provName
		out = append(out, pm)
	}
	return out, nil
}

func (s *Service) Enrich(providerModelID string) error {
	var pm models.ProviderModel
	var cc, cw sql.NullFloat64
	var rn, tl, so, at sql.NullBool
	var rt, rl, rol sql.NullString
	err := s.db.QueryRow(db.Q(`SELECT pm.model_id, pm.display_name FROM provider_models pm WHERE pm.id=?`), providerModelID).Scan(&pm.ModelID, &pm.DisplayName)
	if err != nil {
		return err
	}
	mID := pm.ModelID
	var ctx, maxOut int
	var inputCost, outputCost float64
	var reasoning, toolCall, structuredOutput, attachment bool
	var modalities, reasoningType, reasoningLevels, reasoningLimits string
	if s.catalogStore != nil {
		if cm, err := s.catalogStore.Get(mID); err == nil {
			ctx, maxOut = cm.ContextWindow, cm.MaxOutput
			inputCost, outputCost = cm.InputCost, cm.OutputCost
			reasoning, toolCall, structuredOutput, attachment = cm.Reasoning, cm.ToolCall, cm.StructuredOutput, cm.Attachment
			modalities = cm.Modalities
			reasoningType, reasoningLevels, reasoningLimits = cm.ReasoningType, cm.ReasoningLevels, cm.ReasoningOutputLimits
		} else if cm, err := s.catalogStore.GetByShortID(mID); err == nil {
			ctx, maxOut = cm.ContextWindow, cm.MaxOutput
			inputCost, outputCost = cm.InputCost, cm.OutputCost
			reasoning, toolCall, structuredOutput, attachment = cm.Reasoning, cm.ToolCall, cm.StructuredOutput, cm.Attachment
			modalities = cm.Modalities
			reasoningType, reasoningLevels, reasoningLimits = cm.ReasoningType, cm.ReasoningLevels, cm.ReasoningOutputLimits
		}
	}
	_, err = s.db.Exec(db.Q(`UPDATE provider_models SET context_window=?, max_output=?, input_cost=?, output_cost=?, reasoning=?, tool_call=?, structured_output=?, attachment=?, modalities=?, reasoning_type=?, reasoning_levels=?, reasoning_output_limits=?, updated_at=? WHERE id=?`),
		ctx, maxOut, inputCost, outputCost, reasoning, toolCall, structuredOutput, attachment, modalities, reasoningType, reasoningLevels, reasoningLimits, time.Now().UTC(), providerModelID)
	_ = cc
	_ = cw
	_ = rn
	_ = tl
	_ = so
	_ = at
	_ = rt
	_ = rl
	_ = rol
	return err
}

func (s *Service) UpdateManual(id string, upd models.ProviderModel) error {
	_, err := s.db.Exec(db.Q(`UPDATE provider_models SET display_name=?, owned_by=?, context_window=?, max_output=?, input_cost=?, output_cost=?, reasoning=?, tool_call=?, structured_output=?, attachment=?, modalities=?, reasoning_type=?, reasoning_levels=?, reasoning_output_limits=?, source=?, updated_at=? WHERE id=?`),
		upd.DisplayName, upd.OwnedBy, upd.ContextWindow, upd.MaxOutput, upd.InputCost, upd.OutputCost, upd.Reasoning, upd.ToolCall, upd.StructuredOutput, upd.Attachment, upd.Modalities, upd.ReasoningType, upd.ReasoningLevels, upd.ReasoningOutputLimits, "manual", time.Now().UTC(), id)
	return err
}

func (s *Service) Delete(id string) error {
	_, err := s.db.Exec(db.Q(`DELETE FROM provider_models WHERE id=?`), id)
	return err
}

func (s *Service) AddManual(providerID, modelID string, upd models.ProviderModel) (string, error) {
	id := uuid.NewString()
	ctx, maxOut := upd.ContextWindow, upd.MaxOutput
	if ctx == 0 && maxOut == 0 && s.catalogStore != nil {
		if cm, err := s.catalogStore.Get(modelID); err == nil {
			ctx = cm.ContextWindow
			maxOut = cm.MaxOutput
		}
	}
	_, err := s.db.Exec(db.Q(`INSERT INTO provider_models(id, provider_id, model_id, display_name, owned_by, context_window, max_output, input_cost, output_cost, cache_read_cost, cache_write_cost, reasoning, tool_call, structured_output, attachment, modalities, reasoning_type, reasoning_levels, reasoning_output_limits, source, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		id, providerID, modelID, upd.DisplayName, upd.OwnedBy, ctx, maxOut, upd.InputCost, upd.OutputCost, upd.CacheReadCost, upd.CacheWriteCost, upd.Reasoning, upd.ToolCall, upd.StructuredOutput, upd.Attachment, upd.Modalities, upd.ReasoningType, upd.ReasoningLevels, upd.ReasoningOutputLimits, "manual", time.Now().UTC(), time.Now().UTC())
	return id, err
}

func (s *Service) DiscoverAll() (int, error) {
	// Materialize provider IDs FIRST and close the rows before issuing any
	// per-provider queries: SQLite runs with MaxOpenConns(1), so iterating an
	// open result set while Discover() needs a connection deadlocks the sole
	// connection and wedges the entire gateway.
	rows, err := s.db.Query(db.Q(`SELECT id FROM providers`))
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	total := 0
	for _, id := range ids {
		if n, err := s.Discover(id); err == nil {
			total += n
		}
	}
	return total, nil
}
