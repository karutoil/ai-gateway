package discovery

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ai-gateway/internal/cache"
	"ai-gateway/internal/catalog"
	"ai-gateway/internal/db"
	"ai-gateway/internal/decision"
	"ai-gateway/internal/imagegen"
	"ai-gateway/internal/models"
	"ai-gateway/internal/netguard"
	"ai-gateway/internal/provider"
	"ai-gateway/internal/videogen"

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
		// Hardened client: discovery sends the provider's decrypted key, and the
		// default redirect policy would forward x-api-key/api-key to any host a
		// 302 named (see netguard.HardenedClient).
		client: netguard.HardenedClient(15 * time.Second),
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
	// Antigravity uses OAuth + Cloud Code Assist catalog, not /v1/models.
	if p.Type == models.ProviderAntigravity {
		return s.discoverAntigravity(p)
	}
	// Devin uses OAuth + Connect-proto model configs, not /v1/models.
	if p.Type == models.ProviderDevin {
		return s.discoverDevin(p)
	}
	// Cloudflare Workers AI has no /v1/models listing for decision models
	// (the model is addressed in the path), so seed the curated Clef family.
	if p.Type == models.ProviderCloudflare {
		return s.seedDecisionModels(p)
	}
	// Google Gemini exposes its own models listing (models/… with the key in
	// the query string), not an [OI] /models route. Probe it, then always
	// merge the curated image registry so Nano Banana / Imagen rows exist
	// even when the listing omits them.
	if p.Type == models.ProviderGoogle {
		return s.discoverGoogle(p)
	}
	apiKey, err := s.providerStore.DecryptKey(p)
	if err != nil {
		return 0, err
	}
	var fetched []rawModel
	// Multi-protocol providers (OpenCode Go/Zen) list different models per
	// endpoint family. Probe both dialects and merge so one provider entry
	// discovers its chat, responses, and messages models together.
	if isMultiProvider(p) {
		seen := map[string]bool{}
		for _, m := range s.fetchOpenAI(p, apiKey) {
			if !seen[m.ID] {
				seen[m.ID] = true
				fetched = append(fetched, m)
			}
		}
		for _, m := range s.fetchAnthropic(p, apiKey) {
			if !seen[m.ID] {
				seen[m.ID] = true
				fetched = append(fetched, m)
			}
		}
	} else {
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
	}
	if len(fetched) == 0 {
		// A generic systemone provider (Kev / llama.cpp) normally exposes
		// /v1/models; when it does not, fall back to the curated registry so
		// its decision models are still routable.
		if p.Type == models.ProviderSystemOne {
			return s.seedDecisionModels(p)
		}
		// xAI exposes /v1/models for chat but the curated image models
		// (grok-2-image, grok-imagine) may not appear in every listing.
		if p.Type == models.ProviderXAI {
			return s.seedImageModels(p, "xai")
		}
		return 0, fmt.Errorf("no models discovered (check provider base_url and key)")
	}
	count := 0
	for _, m := range fetched {
		if err := s.upsert(p, m); err == nil {
			count++
		}
	}
	if p.Type == models.ProviderXAI {
		// xAI is [OI]-compatible: discovered chat models plus the curated
		// image models (which bill per image).
		if n, err := s.seedImageModels(p, "xai"); err == nil {
			count += n
		}
		if n, err := s.seedVideoModels(p, "xai"); err == nil {
			count += n
		}
	}
	if s.Cache != nil && count > 0 {
		s.Cache.Invalidate("models:")
	}
	return count, nil
}

// discoverGoogle probes Google's native models listing and merges the curated
// image registry. The listing (GET {base}/v1beta/models?key=…) carries
// `models/…` names; image models are matched against the curated registry so
// they are flagged as image models even without modality metadata.
func (s *Service) discoverGoogle(p *models.Provider) (int, error) {
	apiKey, err := s.providerStore.DecryptKey(p)
	if err != nil {
		return 0, err
	}
	count := 0
	if modelsList := s.fetchGoogle(p, apiKey); len(modelsList) > 0 {
		for _, m := range modelsList {
			if err := s.upsert(p, m); err == nil {
				count++
			}
		}
	}
	if n, err := s.seedImageModels(p, "google"); err == nil {
		count += n
	}
	if n, err := s.seedVideoModels(p, "google"); err == nil {
		count += n
	}
	if count == 0 {
		return 0, fmt.Errorf("no models discovered (check provider base_url and key)")
	}
	if s.Cache != nil {
		s.Cache.Invalidate("models:")
	}
	return count, nil
}

// fetchGoogle lists Gemini models (GET {base}/v1beta/models?key=API_KEY),
// which returns {"models":[{"name":"models/gemini-…"}]}.
func (s *Service) fetchGoogle(p *models.Provider, apiKey string) []rawModel {
	base := strings.TrimRight(p.BaseURL, "/")
	if strings.HasSuffix(base, "/v1beta") {
		base = strings.TrimSuffix(base, "/v1beta")
	}
	target := base + "/v1beta/models?key=" + url.QueryEscape(apiKey)
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return nil
	}
	resp, err := s.client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	resp.Body.Close()
	var list struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if json.Unmarshal(body, &list) != nil {
		return nil
	}
	var out []rawModel
	for _, m := range list.Models {
		id := strings.TrimPrefix(m.Name, "models/")
		if id == "" {
			continue
		}
		out = append(out, rawModel{ID: id, OwnedBy: p.Name})
	}
	return out
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
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
	base := strings.TrimRight(p.BaseURL, "/")
	var urls []string
	switch {
	case strings.HasSuffix(base, "/v1/models"):
		urls = []string{base}
	case strings.Contains(base, "/v1"):
		// Base already carries a version prefix (e.g. https://ckff.dev/v1):
		// appending another /v1 would build /v1/v1/models (404).
		urls = []string{base + "/models"}
	default:
		urls = []string{base + "/v1/models"}
	}
	for _, target := range urls {
		req, _ := http.NewRequest("GET", target, nil)
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := s.client.Do(req)
		if err != nil || resp.StatusCode != 200 {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
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
				if len(out) > 0 {
					return out
				}
			}
		}
	}
	return nil
}

type rawModel struct {
	ID      string
	OwnedBy string
	// Decision forces the decision flag on, for models seeded from the
	// curated registry rather than discovered from an upstream listing.
	Decision bool
	// Image forces the image flag on, for models seeded from the curated
	// image registry rather than discovered from an upstream listing.
	Image bool
	// Video forces the video flag on, for models seeded from the curated
	// video registry rather than discovered from an upstream listing.
	Video bool
}

func (s *Service) upsert(p *models.Provider, m rawModel) error {
	e := s.enrichFor(m.ID)
	e.decision = e.decision || m.Decision
	e.image = e.image || m.Image
	e.video = e.video || m.Video
	// check existing to preserve manual overrides
	var existingID string
	var existingSource string
	err := s.db.QueryRow(db.Q(`SELECT id, source FROM provider_models WHERE provider_id=? AND model_id=?`), p.ID, m.ID).Scan(&existingID, &existingSource)
	if err == nil && existingSource == "manual" {
		// don't overwrite manual
		return nil
	}
	if err == sql.ErrNoRows && s.isExcluded(p.ID, m.ID) {
		// Operator removed this model; it comes back only from the recycling
		// bin or a manual add, never from discovery.
		return nil
	}
	if err == nil {
		_, err = s.db.Exec(db.Q(`UPDATE provider_models SET display_name=?, owned_by=?, context_window=?, max_output=?, input_cost=?, output_cost=?, cache_read_cost=?, cache_write_cost=?, reasoning=?, tool_call=?, structured_output=?, attachment=?, modalities=?, reasoning_type=?, reasoning_levels=?, reasoning_output_limits=?, decision=?, image=?, video=?, source=?, updated_at=? WHERE id=?`),
			m.ID, m.OwnedBy, e.ctx, e.maxOut, e.inputCost, e.outputCost, e.cacheReadCost, e.cacheWriteCost, e.reasoning, e.toolCall, e.structuredOutput, e.attachment, e.modalities, e.reasoningType, e.reasoningLevels, e.reasoningLimits, db.BoolInt(e.decision), db.BoolInt(e.image), db.BoolInt(e.video), e.source, time.Now().UTC(), existingID)
		return err
	}
	id := uuid.NewString()
	_, err = s.db.Exec(db.Q(`INSERT INTO provider_models(id, provider_id, model_id, display_name, owned_by, context_window, max_output, input_cost, output_cost, cache_read_cost, cache_write_cost, reasoning, tool_call, structured_output, attachment, modalities, reasoning_type, reasoning_levels, reasoning_output_limits, decision, image, video, source, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		id, p.ID, m.ID, m.ID, m.OwnedBy, e.ctx, e.maxOut, e.inputCost, e.outputCost, e.cacheReadCost, e.cacheWriteCost, e.reasoning, e.toolCall, e.structuredOutput, e.attachment, e.modalities, e.reasoningType, e.reasoningLevels, e.reasoningLimits, db.BoolInt(e.decision), db.BoolInt(e.image), db.BoolInt(e.video), e.source, time.Now().UTC(), time.Now().UTC())
	return err
}

// enrichment is the catalog-derived detail attached to a provider model at
// discovery/enrich time.
type enrichment struct {
	ctx, maxOut                    int
	inputCost, outputCost          float64
	cacheReadCost, cacheWriteCost  float64
	reasoning, toolCall            bool
	structuredOutput, attachment   bool
	modalities                     string
	reasoningType, reasoningLevels string
	reasoningLimits, source        string
	decision                       bool
	image                          bool
	video                          bool
}

// enrichFor resolves catalog detail for any upstream model ID, including
// reseller-tagged ones ("[aws] grok-4.6"). Exact catalog hits record source
// "enriched"; approximate wildcard hits record "enriched-wildcard" so
// estimated pricing stays distinguishable; misses stay "discovered".
func (s *Service) enrichFor(modelID string) enrichment {
	e := enrichment{source: "discovered"}
	if s.catalogStore != nil {
		if cm, kind, err := s.catalogStore.FindBestMatch(modelID); err == nil {
			e.ctx, e.maxOut = cm.ContextWindow, cm.MaxOutput
			e.inputCost, e.outputCost = cm.InputCost, cm.OutputCost
			e.cacheReadCost, e.cacheWriteCost = cm.CacheReadCost, cm.CacheWriteCost
			e.reasoning, e.toolCall, e.structuredOutput, e.attachment = cm.Reasoning, cm.ToolCall, cm.StructuredOutput, cm.Attachment
			e.modalities = cm.Modalities
			e.reasoningType, e.reasoningLevels, e.reasoningLimits = cm.ReasoningType, cm.ReasoningLevels, cm.ReasoningOutputLimits
			e.decision = cm.Decision
			e.image = cm.Image
			e.video = cm.Video
			e.source = "enriched"
			if kind == "wildcard" {
				e.source = "enriched-wildcard"
			}
		}
	}
	// Curated decision registry fallback: a decision model is recognized even
	// when the catalog snapshot has not caught up with the release, so it is
	// listed with the right capabilities and (input-only) pricing.
	if dm, ok := decision.Lookup(modelID); ok {
		e.decision = true
		if e.ctx == 0 {
			e.ctx = dm.ContextWindow
		}
		if e.inputCost == 0 && dm.InputCostPerM > 0 {
			e.inputCost = dm.InputCostPerM
		}
		if e.outputCost == 0 {
			e.outputCost = dm.OutputCostPerM
		}
		if e.modalities == "" {
			e.modalities = dm.ModalitiesJSON()
		}
		if e.source == "discovered" {
			e.source = "decision"
		}
	}
	// Curated image-model registry fallback: an image model is recognized even
	// when the catalog snapshot has not caught up with the release, so it is
	// listed with the right capability (and can be routed to the images
	// endpoint) instead of being mistaken for a chat model.
	//
	// models.dev modality data wins when present: ORing the curated registry in
	// unconditionally flagged rows models.dev reports as text-only, producing a
	// row that claimed image output while advertising `output: ["text"]`.
	if im, ok := imagegen.Lookup(modelID); ok {
		if len(imagegen.OutputModalities(e.modalities)) > 0 {
			e.image = imagegen.ImageModality(e.modalities)
		} else {
			e.image = true
			e.modalities = im.ModalitiesJSON()
		}
		if e.source == "discovered" {
			e.source = "image"
		}
	}
	// Curated video-model registry fallback (same precedence rule as images:
	// models.dev modalities win when present).
	if vm, ok := videogen.Lookup(modelID); ok {
		if len(videogen.OutputModalities(e.modalities)) > 0 {
			e.video = videogen.VideoModality(e.modalities)
		} else {
			e.video = true
			e.modalities = vm.ModalitiesJSON()
		}
		if e.source == "discovered" {
			e.source = "video"
		}
	}
	return e
}

// seedDecisionModels inserts the curated decision-model registry for a
// provider that cannot be discovered over /v1/models (Cloudflare Workers AI,
// or a systemone server with no listing endpoint).
func (s *Service) seedDecisionModels(p *models.Provider) (int, error) {
	count := 0
	for _, dm := range decision.All() {
		if err := s.upsert(p, rawModel{ID: dm.ID, OwnedBy: p.Name, Decision: true}); err == nil {
			count++
		}
	}
	if s.Cache != nil && count > 0 {
		s.Cache.Invalidate("models:")
	}
	return count, nil
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
	rows, err := s.db.Query(db.Q(`SELECT pm.id, pm.provider_id, pm.model_id, pm.display_name, pm.owned_by, pm.context_window, pm.max_output, pm.input_cost, pm.output_cost, pm.cache_read_cost, pm.cache_write_cost, pm.reasoning, pm.tool_call, pm.structured_output, pm.attachment, pm.modalities, pm.source, pm.created_at, pm.updated_at, pm.reasoning_type, pm.reasoning_levels, pm.reasoning_output_limits, pm.decision, pm.image, pm.video, p.name FROM provider_models pm JOIN providers p ON p.id=pm.provider_id WHERE `+where+` ORDER BY p.name ASC, pm.model_id ASC LIMIT 500`), args...)
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
		var dec, img, vid sql.NullInt64
		var provName string
		if err := rows.Scan(&pm.ID, &pm.ProviderID, &pm.ModelID, &pm.DisplayName, &pm.OwnedBy, &pm.ContextWindow, &pm.MaxOutput, &pm.InputCost, &pm.OutputCost, &cc, &cw, &rn, &tl, &so, &at, &pm.Modalities, &pm.Source, &pm.CreatedAt, &pm.UpdatedAt, &rt, &rl, &rol, &dec, &img, &vid, &provName); err != nil {
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
		pm.Decision = dec.Int64 != 0
		pm.Image = img.Int64 != 0
		pm.Video = vid.Int64 != 0
		pm.ProviderName = provName
		out = append(out, pm)
	}
	return out, nil
}

func (s *Service) Enrich(providerModelID string) error {
	var providerID, modelID string
	err := s.db.QueryRow(db.Q(`SELECT provider_id, model_id FROM provider_models WHERE id=?`), providerModelID).Scan(&providerID, &modelID)
	if err != nil {
		return err
	}
	// OAuth providers with bespoke enrichment: the models.dev catalog has no
	// entries for their ids, so the generic path below would wipe the
	// discovery enrichment (context, costs, reasoning levels) to zeros.
	// Re-derive from the provider source instead.
	if s.providerStore != nil {
		if p, perr := s.providerStore.GetByID(providerID); perr == nil && p != nil {
			switch p.Type {
			case models.ProviderDevin:
				if err := s.enrichDevinRow(p, providerModelID, modelID); err == nil {
					return nil
				}
			case models.ProviderAntigravity:
				if err := s.enrichAntigravityRow(providerModelID, modelID); err == nil {
					return nil
				}
			}
		}
	}
	e := s.enrichFor(modelID)
	_, err = s.db.Exec(db.Q(`UPDATE provider_models SET context_window=?, max_output=?, input_cost=?, output_cost=?, cache_read_cost=?, cache_write_cost=?, reasoning=?, tool_call=?, structured_output=?, attachment=?, modalities=?, reasoning_type=?, reasoning_levels=?, reasoning_output_limits=?, decision=?, image=?, video=?, source=?, updated_at=? WHERE id=?`),
		e.ctx, e.maxOut, e.inputCost, e.outputCost, e.cacheReadCost, e.cacheWriteCost, e.reasoning, e.toolCall, e.structuredOutput, e.attachment, e.modalities, e.reasoningType, e.reasoningLevels, e.reasoningLimits, db.BoolInt(e.decision), db.BoolInt(e.image), db.BoolInt(e.video), e.source, time.Now().UTC(), providerModelID)
	return err
}

func (s *Service) UpdateManual(id string, upd models.ProviderModel) error {
	_, err := s.db.Exec(db.Q(`UPDATE provider_models SET display_name=?, owned_by=?, context_window=?, max_output=?, input_cost=?, output_cost=?, reasoning=?, tool_call=?, structured_output=?, attachment=?, modalities=?, reasoning_type=?, reasoning_levels=?, reasoning_output_limits=?, decision=?, image=?, video=?, source=?, updated_at=? WHERE id=?`),
		upd.DisplayName, upd.OwnedBy, upd.ContextWindow, upd.MaxOutput, upd.InputCost, upd.OutputCost, upd.Reasoning, upd.ToolCall, upd.StructuredOutput, upd.Attachment, upd.Modalities, upd.ReasoningType, upd.ReasoningLevels, upd.ReasoningOutputLimits, db.BoolInt(upd.Decision), db.BoolInt(upd.Image), db.BoolInt(upd.Video), "manual", time.Now().UTC(), id)
	return err
}

func (s *Service) Delete(id string) error {
	var providerID, modelID string
	err := s.db.QueryRow(db.Q(`SELECT provider_id, model_id FROM provider_models WHERE id=?`), id).Scan(&providerID, &modelID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	// Snapshot before the delete: the recycling bin restores this exact row.
	snap, ok, err := loadSnapshot(tx, id)
	if err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(db.Q(`DELETE FROM provider_models WHERE id=?`), id); err != nil {
		tx.Rollback()
		return err
	}
	var raw []byte
	if ok {
		raw, err = json.Marshal(snap)
		if err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := insertExclusion(tx, providerID, modelID, raw); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Service) AddManual(providerID, modelID string, upd models.ProviderModel) (string, error) {
	id := uuid.NewString()
	ctx, maxOut := upd.ContextWindow, upd.MaxOutput
	if ctx == 0 && maxOut == 0 && s.catalogStore != nil {
		if cm, _, err := s.catalogStore.FindBestMatch(modelID); err == nil {
			ctx = cm.ContextWindow
			maxOut = cm.MaxOutput
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	// A manual add is the only way back in after a removal.
	if _, err := tx.Exec(db.Q(`DELETE FROM provider_model_exclusions WHERE provider_id=? AND model_id=?`), providerID, modelID); err != nil {
		tx.Rollback()
		return "", err
	}
	_, err = tx.Exec(db.Q(`INSERT INTO provider_models(id, provider_id, model_id, display_name, owned_by, context_window, max_output, input_cost, output_cost, cache_read_cost, cache_write_cost, reasoning, tool_call, structured_output, attachment, modalities, reasoning_type, reasoning_levels, reasoning_output_limits, decision, image, video, source, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		id, providerID, modelID, upd.DisplayName, upd.OwnedBy, ctx, maxOut, upd.InputCost, upd.OutputCost, upd.CacheReadCost, upd.CacheWriteCost, upd.Reasoning, upd.ToolCall, upd.StructuredOutput, upd.Attachment, upd.Modalities, upd.ReasoningType, upd.ReasoningLevels, upd.ReasoningOutputLimits, db.BoolInt(upd.Decision), db.BoolInt(upd.Image), db.BoolInt(upd.Video), "manual", time.Now().UTC(), time.Now().UTC())
	if err != nil {
		tx.Rollback()
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// ListExcluded returns the recycling bin: models an operator removed, which
// discovery will not reinsert. Housekeeping exclusions (pruned variants) have
// no snapshot and are not shown.
func (s *Service) ListExcluded(providerID, q string) ([]models.ExcludedModel, error) {
	where := "e.snapshot IS NOT NULL"
	args := []interface{}{}
	if providerID != "" {
		where += " AND e.provider_id = ?"
		args = append(args, providerID)
	}
	if q != "" {
		where += " AND (e.model_id LIKE ? OR p.name LIKE ? OR (p.name || '/' || e.model_id) LIKE ? OR e.snapshot LIKE ?)"
		like := "%" + q + "%"
		args = append(args, like, like, like, like)
	}
	rows, err := s.db.Query(db.Q(`SELECT e.id, e.provider_id, e.model_id, e.created_at, e.snapshot, p.name FROM provider_model_exclusions e JOIN providers p ON p.id=e.provider_id WHERE `+where+` ORDER BY e.created_at DESC LIMIT 500`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.ExcludedModel
	for rows.Next() {
		var em models.ExcludedModel
		var raw sql.NullString
		if err := rows.Scan(&em.ID, &em.ProviderID, &em.ModelID, &em.RemovedAt, &raw, &em.ProviderName); err != nil {
			continue
		}
		if raw.Valid && raw.String != "" {
			var snap modelSnapshot
			if json.Unmarshal([]byte(raw.String), &snap) == nil {
				pm := models.ProviderModel{
					ID: snap.ID, ProviderID: snap.ProviderID, ProviderName: em.ProviderName,
					ModelID: snap.ModelID, DisplayName: snap.DisplayName, OwnedBy: snap.OwnedBy,
					ContextWindow: snap.ContextWindow, MaxOutput: snap.MaxOutput,
					InputCost: snap.InputCost, OutputCost: snap.OutputCost,
					CacheReadCost: snap.CacheReadCost, CacheWriteCost: snap.CacheWriteCost,
					Reasoning: snap.Reasoning, ToolCall: snap.ToolCall,
					StructuredOutput: snap.StructuredOutput, Attachment: snap.Attachment,
					Modalities: snap.Modalities, Source: snap.Source,
					CreatedAt: snap.CreatedAt, UpdatedAt: snap.UpdatedAt,
					ReasoningType: snap.ReasoningType, ReasoningLevels: snap.ReasoningLevels,
					ReasoningOutputLimits: snap.ReasoningOutputLimits,
				}
				em.Snapshot = &pm
			}
		}
		out = append(out, em)
	}
	return out, nil
}

// Restore puts a recycling-bin model back. A snapshotted row returns exactly
// as it was removed; a legacy exclusion (no snapshot) returns as a plain
// discovered model that the next discovery run will enrich. Restoring clears
// the exclusion, so discovery treats the model normally again.
func (s *Service) Restore(exclusionID string) (string, error) {
	var providerID, modelID string
	var raw sql.NullString
	err := s.db.QueryRow(db.Q(`SELECT provider_id, model_id, snapshot FROM provider_model_exclusions WHERE id=?`), exclusionID).Scan(&providerID, &modelID, &raw)
	if err != nil {
		return "", err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	var newID string
	if raw.Valid && strings.TrimSpace(raw.String) != "" {
		var snap modelSnapshot
		if err := json.Unmarshal([]byte(raw.String), &snap); err != nil {
			tx.Rollback()
			return "", fmt.Errorf("recycling bin snapshot is corrupt")
		}
		newID = uuid.NewString()
		now := time.Now().UTC()
		_, err = tx.Exec(db.Q(`INSERT INTO provider_models(id, provider_id, model_id, display_name, owned_by, context_window, max_output, input_cost, output_cost, cache_read_cost, cache_write_cost, reasoning, tool_call, structured_output, attachment, modalities, reasoning_type, reasoning_levels, reasoning_output_limits, decision, image, video, reasoning_routing, source, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
			newID, providerID, modelID, snap.DisplayName, snap.OwnedBy, snap.ContextWindow, snap.MaxOutput, snap.InputCost, snap.OutputCost, snap.CacheReadCost, snap.CacheWriteCost, snap.Reasoning, snap.ToolCall, snap.StructuredOutput, snap.Attachment, snap.Modalities, snap.ReasoningType, snap.ReasoningLevels, snap.ReasoningOutputLimits, db.BoolInt(snap.Decision), db.BoolInt(snap.Image), db.BoolInt(snap.Video), snap.ReasoningRouting, snap.Source, snap.CreatedAt, now)
	} else {
		newID, err = s.restoreBare(tx, providerID, modelID)
	}
	if err != nil {
		tx.Rollback()
		return "", err
	}
	if _, err := tx.Exec(db.Q(`DELETE FROM provider_model_exclusions WHERE id=?`), exclusionID); err != nil {
		tx.Rollback()
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if s.Cache != nil {
		s.Cache.Invalidate("models:")
	}
	return newID, nil
}

// restoreBare reinserts a model that was excluded before snapshots existed.
func (s *Service) restoreBare(tx *sql.Tx, providerID, modelID string) (string, error) {
	id := uuid.NewString()
	now := time.Now().UTC()
	_, err := tx.Exec(db.Q(`INSERT INTO provider_models(id, provider_id, model_id, display_name, source, created_at, updated_at) VALUES(?,?,?,?,?,?,?)`),
		id, providerID, modelID, modelID, "discovered", now, now)
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

// isMultiProvider mirrors proxy.isMultiProtocolProvider without importing the
// proxy package (which would cycle). Keep the two in sync.
func isMultiProvider(p *models.Provider) bool {
	if p == nil {
		return false
	}
	base := strings.ToLower(strings.TrimSpace(p.BaseURL))
	name := strings.ToLower(strings.TrimSpace(p.Name))
	if strings.Contains(base, "opencode.ai/zen") || strings.Contains(base, "opencode.ai/go") {
		return true
	}
	for _, pre := range []string{"opencode-go", "opencode_go", "opencodego", "opencode-zen", "opencode_zen"} {
		if name == pre || strings.HasPrefix(name, pre+"/") || strings.HasPrefix(name, pre+"-") || strings.HasPrefix(name, pre+"_") {
			return true
		}
	}
	return false
}

// seedImageModels inserts the curated image-generation registry for a provider
// namespace ("google", "xai"). Image models are addressable through
// /v1/images/generations and bill per generated image.
func (s *Service) seedImageModels(p *models.Provider, namespace string) (int, error) {
	count := 0
	for _, im := range imagegen.ForNamespace(namespace) {
		if err := s.upsert(p, rawModel{ID: im.ID, OwnedBy: p.Name, Image: true}); err == nil {
			count++
		}
	}
	if s.Cache != nil && count > 0 {
		s.Cache.Invalidate("models:")
	}
	return count, nil
}

// seedVideoModels inserts the curated video-generation registry for a provider
// namespace ("openai", "google", "xai"). Video models are addressable through
// the async /v1/videos job API and bill per generated second.
func (s *Service) seedVideoModels(p *models.Provider, namespace string) (int, error) {
	count := 0
	for _, vm := range videogen.ForNamespace(namespace) {
		if err := s.upsert(p, rawModel{ID: vm.ID, OwnedBy: p.Name, Video: true}); err == nil {
			count++
		}
	}
	if s.Cache != nil && count > 0 {
		s.Cache.Invalidate("models:")
	}
	return count, nil
}
