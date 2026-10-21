package catalog

import (
	"testing"

	"ai-gateway/internal/db"
)

// TestSeedDecisionModelsAndLookup locks in the decision-model catalog seeding:
// curated System One models must be listable and priced (input-only) and must
// resolve from every spelling clients use (bare, namespaced, @cf/ qualified).
func TestSeedDecisionModelsAndLookup(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)

	n, err := s.SeedDecisionModels()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n < 5 {
		t.Fatalf("expected the curated registry to seed several models, got %d", n)
	}

	m, _, err := s.FindBestMatch("clef")
	if err != nil {
		t.Fatalf("clef lookup: %v", err)
	}
	if !m.Decision || m.InputCost != 0.240 || m.OutputCost != 0 {
		t.Fatalf("unexpected clef row: %+v", m)
	}
	if m.Provider != "cloudflare" {
		t.Fatalf("clef provider = %q", m.Provider)
	}

	for _, id := range []string{"@cf/cloudflare/clef-flash", "cloudflare/clef-omni", "kev-4b", "jev-latest"} {
		mm, _, err := s.FindBestMatch(id)
		if err != nil {
			t.Errorf("%s lookup: %v", id, err)
			continue
		}
		if !mm.Decision {
			t.Errorf("%s not marked decision", id)
		}
		if mm.OutputCost != 0 {
			t.Errorf("%s output cost = %v, want 0 (decision models bill input only)", id, mm.OutputCost)
		}
	}

	// Idempotent: re-seeding must not duplicate rows.
	before, _ := s.Count()
	if _, err := s.SeedDecisionModels(); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	after, _ := s.Count()
	if after != before {
		t.Fatalf("re-seed changed row count: %d -> %d", before, after)
	}
}

// TestSyncMarksDecisionModelsByRegistry ensures a models.dev sync also flags
// decision models (even without an explicit decision field) so a fresh
// catalog is correct before the curated seed runs.
func TestSyncMarksDecisionModelsByRegistry(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)

	body := []byte(`{
		"cloudflare": {"id":"cloudflare","name":"cloudflare","api":"openai","models":{
			"clef": {"id":"clef","name":"Clef","cost":{"input":0.24,"output":0}}
		}},
		"openai": {"id":"openai","name":"openai","api":"openai","models":{
			"gpt-4o": {"id":"gpt-4o","name":"GPT-4o","cost":{"input":2.5,"output":10}}
		}}
	}`)
	if _, err := s.SyncFromBytes(body); err != nil {
		t.Fatal(err)
	}

	clef, err := s.Get("cloudflare/clef")
	if err != nil {
		t.Fatalf("clef row missing: %v", err)
	}
	if !clef.Decision {
		t.Fatalf("clef not flagged decision after sync")
	}
	gpt, err := s.Get("openai/gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if gpt.Decision {
		t.Fatalf("gpt-4o incorrectly flagged decision")
	}
}
