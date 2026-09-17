package discovery

import (
	"testing"

	"ai-gateway/internal/antigravity"
	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
)

func testAntigravityService(t *testing.T) (*Service, *models.Provider) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i + 9)
	}
	ps := provider.NewStore(database, mk)
	p, err := ps.CreateWithOrg("ag", models.ProviderAntigravity, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return &Service{db: database, providerStore: ps}, p
}

// The per-model Enrich endpoint must restore antigravity enrichment from the
// static catalog — never wipe the row through the models.dev catalog, which
// has no antigravity entries. Runtime variants resolve to their public base.
func TestEnrichAntigravityVariantRowUsesBase(t *testing.T) {
	s, p := testAntigravityService(t)
	_, err := s.db.Exec(`INSERT INTO provider_models(id, provider_id, model_id, display_name, source, created_at, updated_at) VALUES(?,?,?,?,?,?,?)`,
		"row-v", p.ID, "gemini-3.8-flash-high", "gemini-3.8-flash-high", "discovered", "2026-01-01", "2026-01-01")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.Enrich("row-v"); err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	var (
		display, source string
		ctx, maxOut     int
		inCost, outCost float64
		reasoning       bool
	)
	err = s.db.QueryRow(`SELECT display_name, context_window, max_output, input_cost, output_cost, reasoning, source FROM provider_models WHERE id=?`,
		"row-v").Scan(&display, &ctx, &maxOut, &inCost, &outCost, &reasoning, &source)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	want := antigravity.PublicModels[0]
	if display != want.Name || ctx != want.ContextWindow || maxOut != want.MaxTokens {
		t.Errorf("identity/context = %q/%d/%d, want %q/%d/%d",
			display, ctx, maxOut, want.Name, want.ContextWindow, want.MaxTokens)
	}
	if inCost != want.InputCost || outCost != want.OutputCost {
		t.Errorf("costs = %v/%v, want %v/%v", inCost, outCost, want.InputCost, want.OutputCost)
	}
	if !reasoning || source != "enriched" {
		t.Errorf("reasoning/source = %v/%q, want true/enriched", reasoning, source)
	}
}

// Rediscovery must not clobber operator overrides.
func TestUpsertAntigravityPreservesManual(t *testing.T) {
	s, p := testAntigravityService(t)
	_, err := s.db.Exec(`INSERT INTO provider_models(id, provider_id, model_id, display_name, context_window, source, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		"row-m", p.ID, "gemini-3.8-flash", "My Flash", 42, "manual", "2026-01-01", "2026-01-01")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	pm := antigravity.PublicModel{ID: "gemini-3.8-flash", Name: "Gemini 3.8 Flash (Antigravity)", ContextWindow: 1048576, MaxTokens: 65536}
	if err := s.upsertAntigravity(p, pm, rawModel{ID: pm.ID, OwnedBy: "antigravity"}); err != nil {
		t.Fatalf("upsertAntigravity: %v", err)
	}
	var display string
	var ctx int
	if err := s.db.QueryRow(`SELECT display_name, context_window FROM provider_models WHERE id=?`, "row-m").Scan(&display, &ctx); err != nil {
		t.Fatalf("select: %v", err)
	}
	if display != "My Flash" || ctx != 42 {
		t.Errorf("manual row clobbered: %q/%d", display, ctx)
	}
}
