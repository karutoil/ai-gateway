package discovery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-gateway/internal/catalog"
	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
)

func newDiscoveryService(t *testing.T) (*Service, *provider.Store, *catalog.Store, func()) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i)
	}
	ps := provider.NewStore(database, mk)
	cs := catalog.NewStore(database)
	return New(database, ps, cs), ps, cs, func() { database.Close() }
}

// TestDiscoverGoogleSeedsImageModels proves a Google provider discovers its
// native listing AND always gains the curated image models (Nano Banana /
// Imagen), flagged as image models.
func TestDiscoverGoogleSeedsImageModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v1beta/models") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("key") != "google-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"name": "models/gemini-2.5-flash", "displayName": "Gemini 2.5 Flash"},
				{"name": "models/gemini-2.5-flash-image", "displayName": "Nano Banana"},
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
	n, err := s.Discover(p.ID)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if n == 0 {
		t.Fatal("expected models discovered")
	}

	list, err := s.List(p.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]models.ProviderModel{}
	for _, m := range list {
		byID[m.ModelID] = m
	}
	for _, id := range []string{"gemini-2.5-flash-image", "imagen-4.0-generate-001", "imagen-4.0-fast-generate-001"} {
		m, ok := byID[id]
		if !ok {
			t.Errorf("%s not discovered", id)
			continue
		}
		if !m.Image {
			t.Errorf("%s not flagged as an image model", id)
		}
	}
	if m, ok := byID["gemini-2.5-flash"]; ok && m.Image {
		t.Error("gemini-2.5-flash (chat) must not be flagged as an image model")
	}
}

// TestDiscoverXAISeedsImageModels proves an [OI]-compatible xAI provider keeps
// its discovered chat models and gains the curated Grok image models.
func TestDiscoverXAISeedsImageModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
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
	if _, ok := byID["grok-3"]; !ok {
		t.Error("discovered chat model grok-3 missing")
	}
	for _, id := range []string{"grok-2-image", "grok-imagine-image-2.0"} {
		m, ok := byID[id]
		if !ok {
			t.Errorf("%s not seeded", id)
			continue
		}
		if !m.Image {
			t.Errorf("%s not flagged as an image model", id)
		}
	}
	if m := byID["grok-3"]; m.Image {
		t.Error("grok-3 must not be flagged as an image model")
	}
}

// TestEnrichFlagsImageModel proves the curated image registry marks a row even
// when the catalog snapshot has no entry for it.
func TestEnrichFlagsImageModel(t *testing.T) {
	s, ps, _, cleanup := newDiscoveryService(t)
	defer cleanup()
	p, err := ps.Create("xai", models.ProviderXAI, "https://api.x.ai/v1", "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO provider_models(id, provider_id, model_id, display_name, source, created_at, updated_at) VALUES(?,?,?,?,?,?,?)`,
		"row-img", p.ID, "grok-2-image", "Grok 2 Image", "discovered", "2026-01-01", "2026-01-01"); err != nil {
		t.Fatal(err)
	}
	if err := s.Enrich("row-img"); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	var img int
	if err := s.db.QueryRow(`SELECT image FROM provider_models WHERE id=?`, "row-img").Scan(&img); err != nil {
		t.Fatal(err)
	}
	if img == 0 {
		t.Fatal("curated image model not flagged by enrichment")
	}
}
