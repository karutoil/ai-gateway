package discovery

import (
	"testing"

	"ai-gateway/internal/catalog"
	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
)

// The umans channel namespaces every model id ("umans-<base>"), lab mode wraps
// it as "umans-<base>-lab", and models.dev keys base rows without the
// decorations. Enrichment must land context/costs/reasoning on the row (from
// the base catalog row) instead of leaving it a zeroed "discovered" row.
func TestEnrichUmansLabRowResolvesBase(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i + 11)
	}
	ps := provider.NewStore(database, mk)
	p, err := ps.CreateWithOrg("umans", models.ProviderOpenAICompatible, "https://api.umans.ai/v1", "sk-test", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	cs := catalog.NewStore(database)
	body := []byte(`{
		"zai": {"id":"zai","name":"zai","api":"openai","models":{
			"glm-5.3-flash": {"id":"zai/glm-5.3-flash","name":"GLM-5.3-Flash","cost":{"input":0.075,"output":0.25},"limit":{"context":1000000,"output":131072},"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","high","max"]}],"tool_call":true}
		}}
	}`)
	if _, err := cs.SyncFromBytes(body); err != nil {
		t.Fatalf("catalog sync: %v", err)
	}
	s := &Service{db: database, providerStore: ps, catalogStore: cs}

	_, err = s.db.Exec(`INSERT INTO provider_models(id, provider_id, model_id, display_name, source, created_at, updated_at) VALUES(?,?,?,?,?,?,?)`,
		"row-lab", p.ID, "umans-glm-5.3-flash-lab", "umans-glm-5.3-flash-lab", "discovered", "2026-01-01", "2026-01-01")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.Enrich("row-lab"); err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	var (
		ctx, maxOut     int
		inCost, outCost float64
		reasoning       bool
		reasoningType   string
		source          string
	)
	err = s.db.QueryRow(`SELECT context_window, max_output, input_cost, output_cost, reasoning, reasoning_type, source FROM provider_models WHERE id=?`,
		"row-lab").Scan(&ctx, &maxOut, &inCost, &outCost, &reasoning, &reasoningType, &source)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if ctx != 1000000 || maxOut != 131072 {
		t.Errorf("context = %d/%d, want 1000000/131072", ctx, maxOut)
	}
	if inCost != 0.075 || outCost != 0.25 {
		t.Errorf("costs = %v/%v, want 0.075/0.25", inCost, outCost)
	}
	if !reasoning || reasoningType != "effort" {
		t.Errorf("reasoning = %v/%q, want true/effort", reasoning, reasoningType)
	}
	// Approximate decoration-stripping matches must stay distinguishable from
	// exact catalog data.
	if source != "enriched-wildcard" {
		t.Errorf("source = %q, want enriched-wildcard", source)
	}

	// Exact channel rows (non-lab sibling ids) enrich as exact.
	_, err = s.db.Exec(`INSERT INTO provider_models(id, provider_id, model_id, display_name, source, created_at, updated_at) VALUES(?,?,?,?,?,?,?)`,
		"row-exact", p.ID, "umans-ai-coding-plan/umans-kimi-k2.7", "umans-kimi-k2.7", "discovered", "2026-01-01", "2026-01-01")
	if err != nil {
		t.Fatalf("seed exact: %v", err)
	}
	body = []byte(`{
		"umans-ai-coding-plan": {"id":"umans-ai-coding-plan","name":"umans-ai-coding-plan","api":"openai","models":{
			"umans-kimi-k2.7": {"id":"umans-ai-coding-plan/umans-kimi-k2.7","name":"Kimi K2.7","cost":{"input":0,"output":0},"limit":{"context":262144,"output":262144},"reasoning":true,"tool_call":true}
		}}
	}`)
	if _, err := cs.SyncFromBytes(body); err != nil {
		t.Fatalf("catalog sync exact: %v", err)
	}
	if err := s.Enrich("row-exact"); err != nil {
		t.Fatalf("Enrich exact: %v", err)
	}
	err = s.db.QueryRow(`SELECT context_window, source FROM provider_models WHERE id=?`, "row-exact").Scan(&ctx, &source)
	if err != nil {
		t.Fatalf("select exact: %v", err)
	}
	if ctx != 262144 || source != "enriched" {
		t.Errorf("exact row = ctx %d source %q, want 262144/enriched", ctx, source)
	}
}
