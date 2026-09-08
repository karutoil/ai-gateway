package discovery

import (
	"context"
	"fmt"
	"time"

	"ai-gateway/internal/devin"
	"ai-gateway/internal/models"
)

// discoverDevin seeds provider_models from the static fallback catalog, then
// merges live GetCliModelConfigs results when OAuth is connected.
func (s *Service) discoverDevin(p *models.Provider) (int, error) {
	count := 0
	for _, m := range devin.PublicModels {
		if err := s.upsertDevin(p, m.ID, m.Name, m.ContextWindow, m.MaxTokens); err == nil {
			count++
		}
	}
	for _, m := range s.fetchDevinLive(p) {
		if err := s.upsertDevin(p, m.ID, m.Name, m.ContextWindow, m.MaxTokens); err == nil {
			count++
		}
	}
	if s.Cache != nil && count > 0 {
		s.Cache.Invalidate("models:")
	}
	if count == 0 {
		return 0, fmt.Errorf("no models discovered (check provider base_url and key)")
	}
	return count, nil
}

func (s *Service) upsertDevin(p *models.Provider, modelID, displayName string, ctx, maxOut int) error {
	if err := s.upsert(p, rawModel{ID: modelID, OwnedBy: "devin"}); err != nil {
		return err
	}
	// Devin bills via seat/quota: zero token costs, generous reasoning defaults.
	_, _ = s.db.Exec(`UPDATE provider_models SET display_name=?, owned_by=?, context_window=?, max_output=?, input_cost=?, output_cost=?, cache_read_cost=?, cache_write_cost=?, reasoning=?, tool_call=?, attachment=?, source=?, updated_at=? WHERE provider_id=? AND model_id=?`,
		displayName, "devin", ctx, maxOut, 0, 0, 0, 0, true, true, true, "enriched", time.Now().UTC(), p.ID, modelID)
	return nil
}

// fetchDevinLive returns live model configs with a fresh OAuth session token.
func (s *Service) fetchDevinLive(p *models.Provider) []devin.DiscoveredModel {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	access, _, _, err := s.providerStore.EnsureFreshAccess(ctx, p, s.client)
	if err != nil || access == "" {
		return nil
	}
	base := p.BaseURL
	if base == "" {
		base = devin.Host()
	}
	live, err := devin.FetchModels(access, base, s.client)
	if err != nil {
		return nil
	}
	return live
}
