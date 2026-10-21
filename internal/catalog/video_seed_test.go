package catalog

import (
	"testing"

	"ai-gateway/internal/db"
	"ai-gateway/internal/videogen"
)

// TestSeedVideoModelsAndLookup locks in video-model catalog seeding. There was
// no test for SeedVideoModels at all: a regression there would make video models
// invisible in /v1/models, unpriceable, and (worse) unclassified for the
// capability guards.
func TestSeedVideoModelsAndLookup(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)

	n, err := s.SeedVideoModels()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n < 5 {
		t.Fatalf("expected the curated registry to seed several video models, got %d", n)
	}
	if want := len(videogen.All()); n != want {
		t.Fatalf("seeded %d rows, registry has %d", n, want)
	}

	for _, id := range []string{
		"openai/sora-2", "sora-2-pro", "google/veo-3.1-generate-preview",
		"veo-2.0-generate-001", "xai/grok-imagine-video",
	} {
		m, _, err := s.FindBestMatch(id)
		if err != nil {
			t.Errorf("%s lookup: %v", id, err)
			continue
		}
		if !m.Video {
			t.Errorf("%s not marked video", id)
		}
		if m.Image || m.Decision {
			t.Errorf("%s wrongly flagged image=%v decision=%v", id, m.Image, m.Decision)
		}
		if m.InputCost != 0 || m.OutputCost != 0 {
			t.Errorf("%s token cost = %v/%v, want 0/0 (per-second billing)", id, m.InputCost, m.OutputCost)
		}
	}

	// Every seeded row's modalities must say video, otherwise the
	// modalities-driven capability guards would disagree with the flag.
	for _, vm := range videogen.All() {
		m, _, err := s.FindBestMatch(vm.CatalogID())
		if err != nil {
			t.Errorf("%s lookup: %v", vm.CatalogID(), err)
			continue
		}
		if !videogen.VideoModality(m.Modalities) {
			t.Errorf("%s modalities %q lack a video output", vm.CatalogID(), m.Modalities)
		}
		if !videogen.VideoOnlyFromModalities(m.Modalities) {
			t.Errorf("%s modalities %q should be video-only", vm.CatalogID(), m.Modalities)
		}
	}

	// Idempotent: re-seeding must not duplicate rows.
	before, _ := s.Count()
	if _, err := s.SeedVideoModels(); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	after, _ := s.Count()
	if after != before {
		t.Fatalf("re-seed changed row count: %d -> %d", before, after)
	}
}

// TestAllThreeCapabilitySeedsCoexist proves the three curated seeds write
// disjoint capability flags, so no model is accidentally a chat, image,
// decision *and* video model at once (which would make the guards ambiguous).
func TestAllThreeCapabilitySeedsCoexist(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)
	for name, fn := range map[string]func() (int, error){
		"decision": s.SeedDecisionModels,
		"image":    s.SeedImageModels,
		"video":    s.SeedVideoModels,
	} {
		if _, err := fn(); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	type row struct{ id string }
	var overlaps []string
	rows, err := database.Query(`SELECT id FROM models_catalog WHERE (decision+image+video) <> 0 AND (decision+image+video) <> 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id); err != nil {
			t.Fatal(err)
		}
		overlaps = append(overlaps, r.id)
	}
	if len(overlaps) > 0 {
		t.Fatalf("models flagged with more than one capability: %v", overlaps)
	}

	var decision, image, video int
	if err := database.QueryRow(`SELECT
		COALESCE(SUM(decision),0), COALESCE(SUM(image),0), COALESCE(SUM(video),0) FROM models_catalog`).
		Scan(&decision, &image, &video); err != nil {
		t.Fatal(err)
	}
	if decision == 0 || image == 0 || video == 0 {
		t.Fatalf("capability seeds incomplete: decision=%d image=%d video=%d", decision, image, video)
	}
}
