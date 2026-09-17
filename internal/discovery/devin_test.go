package discovery

import (
	"database/sql"
	"testing"

	"ai-gateway/internal/db"
	"ai-gateway/internal/devin"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
)

func testDevinService(t *testing.T) (*Service, *models.Provider) {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i + 7)
	}
	ps := provider.NewStore(database, mk)
	p, err := ps.CreateWithOrg("devin", models.ProviderDevin, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return &Service{db: database, providerStore: ps}, p
}

func seedWipedRow(t *testing.T, s *Service, providerID, rowID, modelID, source string) {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO provider_models(id, provider_id, model_id, display_name, source, created_at, updated_at) VALUES(?,?,?,?,?,?,?)`,
		rowID, providerID, modelID, modelID, source, "2026-01-01", "2026-01-01")
	if err != nil {
		t.Fatalf("seed %s: %v", modelID, err)
	}
}

// Collapsed base rows must carry real enrichment: reasoning metadata,
// context window, and an honest source — not an "enriched" row with empty
// reasoning_levels (the model-reasoninglevel gap).
func TestUpsertDevinStoresReasoningLevels(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i + 7)
	}
	ps := provider.NewStore(database, mk)
	p, err := ps.CreateWithOrg("devin", models.ProviderDevin, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	s := &Service{db: database}
	g := devin.CollapsedModel{
		ID: "swe-2", Name: "SWE-2",
		ContextWindow: 200000, MaxTokens: 64000,
		ImageInput: true, Reasoning: true, ReasoningType: "effort",
		ToolCalls: true, Levels: []string{"high", "max"},
		Routing: map[string]string{"high": "swe-2-high", "max": "swe-2-max"},
	}
	if err := s.upsertDevin(p, g); err != nil {
		t.Fatalf("upsertDevin: %v", err)
	}
	var (
		display                string
		ctx, maxOut            int
		reasoning              bool
		rType, rLevels, source string
		routing                sql.NullString
	)
	err = database.QueryRow(`SELECT display_name, context_window, max_output, reasoning, reasoning_type, reasoning_levels, source, reasoning_routing FROM provider_models WHERE provider_id=? AND model_id=?`,
		p.ID, "swe-2").Scan(&display, &ctx, &maxOut, &reasoning, &rType, &rLevels, &source, &routing)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if display != "SWE-2" || ctx != 200000 || maxOut != 64000 {
		t.Errorf("identity/context = %q/%d/%d", display, ctx, maxOut)
	}
	if !reasoning || rType != "effort" || rLevels != `["high","max"]` || source != "enriched" {
		t.Errorf("enrichment = reasoning:%v type:%q levels:%q source:%q",
			reasoning, rType, rLevels, source)
	}
	if !routing.Valid || routing.String != `{"high":"swe-2-high","max":"swe-2-max"}` {
		t.Errorf("routing = %+v, want effort map", routing)
	}
	// No per-variant rows: the discovery contract is one row per base.
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM provider_models WHERE provider_id=? AND model_id LIKE 'swe-2-%'`, p.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("variant rows = %d (err %v), want 0", n, err)
	}
}

func TestPruneDevinVariants(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i + 7)
	}
	ps := provider.NewStore(database, mk)
	p, err := ps.CreateWithOrg("devin", models.ProviderDevin, "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	s := &Service{db: database}
	seed := func(modelID, source string) {
		t.Helper()
		_, err := database.Exec(`INSERT INTO provider_models(id, provider_id, model_id, display_name, source, created_at, updated_at) VALUES(?,?,?,?,?,?,?)`,
			modelID+"-id", p.ID, modelID, modelID, source, "2026-01-01", "2026-01-01")
		if err != nil {
			t.Fatalf("seed %s: %v", modelID, err)
		}
	}
	seed("swe-2", "enriched")
	seed("swe-2-max", "enriched")   // pre-collapse leftover: pruned
	seed("swe-2-high", "manual")    // operator-owned: kept
	seed("swe-1-7-low", "enriched") // base not stored: kept
	s.pruneDevinVariants(p.ID, map[string]devin.CollapsedModel{"swe-2": {ID: "swe-2"}})
	remaining := map[string]bool{}
	rows, err := database.Query(`SELECT model_id FROM provider_models WHERE provider_id=?`, p.ID)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err == nil {
			remaining[m] = true
		}
	}
	rows.Close()
	for _, want := range []string{"swe-2", "swe-2-high", "swe-1-7-low"} {
		if !remaining[want] {
			t.Errorf("%s pruned, want kept", want)
		}
	}
	if remaining["swe-2-max"] {
		t.Error("swe-2-max kept, want pruned")
	}
}

// The per-model Enrich endpoint must restore devin enrichment from the
// provider source — never wipe the row through the models.dev catalog,
// which has no devin entries (ctx/costs/levels all zero out).
func TestEnrichDevinRowRestoresWipedRow(t *testing.T) {
	s, p := testDevinService(t)
	seedWipedRow(t, s, p.ID, "row-1", "swe-2", "discovered")
	if err := s.Enrich("row-1"); err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	var (
		display, rType, rLevels, source string
		ctx, maxOut                     int
		inCost, outCost                 float64
		reasoning                       bool
	)
	err := s.db.QueryRow(`SELECT display_name, context_window, max_output, input_cost, output_cost, reasoning, reasoning_type, reasoning_levels, source FROM provider_models WHERE id=?`,
		"row-1").Scan(&display, &ctx, &maxOut, &inCost, &outCost, &reasoning, &rType, &rLevels, &source)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if display != "SWE-2" || ctx != 200000 || maxOut != 64000 {
		t.Errorf("identity/context = %q/%d/%d, want SWE-2/200000/64000", display, ctx, maxOut)
	}
	if !reasoning || rType != "effort" || source != "enriched" {
		t.Errorf("reasoning = %v/%q/%q, want true/effort/enriched", reasoning, rType, source)
	}
	if rLevels != `["minimal","low","medium","high","xhigh","max"]` {
		t.Errorf("levels = %q, want fallback defaults", rLevels)
	}
	if inCost != 0 || outCost != 0 {
		t.Errorf("costs = %v/%v, want 0/0 (seat billing)", inCost, outCost)
	}
}

// Enrich on a stale variant row applies its base's metadata so the row stays
// functional until the next discovery prunes it.
func TestEnrichDevinVariantRowUsesBase(t *testing.T) {
	s, p := testDevinService(t)
	seedWipedRow(t, s, p.ID, "row-v", "swe-2-max", "enriched")
	if err := s.Enrich("row-v"); err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	var rLevels, source string
	var reasoning bool
	if err := s.db.QueryRow(`SELECT reasoning, reasoning_levels, source FROM provider_models WHERE id=?`,
		"row-v").Scan(&reasoning, &rLevels, &source); err != nil {
		t.Fatalf("select: %v", err)
	}
	if !reasoning || rLevels == "" || rLevels == "[]" || source != "enriched" {
		t.Errorf("variant enrichment = %v/%q/%q, want populated base metadata", reasoning, rLevels, source)
	}
}

// Rediscovery must not clobber operator overrides.
func TestUpsertDevinPreservesManual(t *testing.T) {
	s, p := testDevinService(t)
	_, err := s.db.Exec(`INSERT INTO provider_models(id, provider_id, model_id, display_name, context_window, source, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		"row-m", p.ID, "swe-2", "My SWE", 12345, "manual", "2026-01-01", "2026-01-01")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	g := devin.CollapsedModel{ID: "swe-2", Name: "SWE-2", ContextWindow: 200000, MaxTokens: 64000, Levels: []string{"max"}}
	if err := s.upsertDevin(p, g); err != nil {
		t.Fatalf("upsertDevin: %v", err)
	}
	var display string
	var ctx int
	if err := s.db.QueryRow(`SELECT display_name, context_window FROM provider_models WHERE id=?`, "row-m").Scan(&display, &ctx); err != nil {
		t.Fatalf("select: %v", err)
	}
	if display != "My SWE" || ctx != 12345 {
		t.Errorf("manual row clobbered: %q/%d", display, ctx)
	}
}
