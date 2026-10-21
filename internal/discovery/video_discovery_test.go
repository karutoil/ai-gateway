package discovery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/models"
)

// TestDiscoverGoogleSeedsVideoModels proves a Google provider always gains the
// curated Veo models, flagged as video (and NOT as image or chat). The image
// sibling test covers the image half; without this the video flag could regress
// to zero and video models would disappear from /v1/models and become
// unroutable for /v1/videos.
func TestDiscoverGoogleSeedsVideoModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v1beta/models") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"name": "models/gemini-2.5-flash", "displayName": "Gemini 2.5 Flash"},
			},
		})
	}))
	defer upstream.Close()

	s, ps, _, cleanup := newDiscoveryService(t)
	defer cleanup()
	p, err := ps.Create("goog", models.ProviderGoogle, upstream.URL, "google-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Discover(p.ID); err != nil {
		t.Fatalf("discover: %v", err)
	}
	list, err := s.List(p.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]models.ProviderModel{}
	for _, m := range list {
		byID[m.ModelID] = m
	}
	for _, id := range []string{"veo-3.1-generate-preview", "veo-2.0-generate-001"} {
		m, ok := byID[id]
		if !ok {
			t.Errorf("%s not seeded", id)
			continue
		}
		if !m.Video {
			t.Errorf("%s not flagged as a video model", id)
		}
		if m.Image {
			t.Errorf("%s must not be flagged as an image model too", id)
		}
	}
	if m, ok := byID["gemini-2.5-flash"]; ok && m.Video {
		t.Error("a chat model must not be flagged as a video model")
	}
}

// TestDiscoverXAISeedsVideoModels is the xAI mirror (Grok Imagine video).
func TestDiscoverXAISeedsVideoModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"grok-3","object":"model"}]}`))
	}))
	defer upstream.Close()

	s, ps, _, cleanup := newDiscoveryService(t)
	defer cleanup()
	p, err := ps.Create("xai", models.ProviderXAI, upstream.URL+"/v1", "xai-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Discover(p.ID); err != nil {
		t.Fatalf("discover: %v", err)
	}
	list, err := s.List(p.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]models.ProviderModel{}
	for _, m := range list {
		byID[m.ModelID] = m
	}
	m, ok := byID["grok-imagine-video"]
	if !ok {
		t.Fatal("grok-imagine-video not seeded")
	}
	if !m.Video {
		t.Fatal("grok-imagine-video not flagged as a video model")
	}
	if m.Image {
		t.Error("grok-imagine-video must not be flagged as an image model")
	}
	if byID["grok-3"].Video {
		t.Error("grok-3 must not be flagged as a video model")
	}
}

// TestEnrichFlagsVideoModel proves enrichment marks a row from the curated video
// registry even when the catalog has no entry for it (the "before the next sync"
// window).
func TestEnrichFlagsVideoModel(t *testing.T) {
	s, ps, _, cleanup := newDiscoveryService(t)
	defer cleanup()
	p, err := ps.Create("xai", models.ProviderXAI, "https://api.x.ai/v1", "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO provider_models(id, provider_id, model_id, display_name, source, created_at, updated_at) VALUES(?,?,?,?,?,?,?)`,
		"row-vid", p.ID, "sora-2", "Sora 2", "discovered", "2026-01-01", "2026-01-01"); err != nil {
		t.Fatal(err)
	}
	if err := s.Enrich("row-vid"); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	var video, image int
	var modalities string
	if err := s.db.QueryRow(`SELECT video, image, COALESCE(modalities,'') FROM provider_models WHERE id=?`, "row-vid").
		Scan(&video, &image, &modalities); err != nil {
		t.Fatal(err)
	}
	if video == 0 {
		t.Fatal("curated video model not flagged by enrichment")
	}
	if image != 0 {
		t.Fatal("a video model must not also be an image model")
	}
	if !strings.Contains(modalities, "video") {
		t.Fatalf("modalities = %q, want a video output", modalities)
	}
}
