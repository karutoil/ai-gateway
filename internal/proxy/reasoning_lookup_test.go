package proxy

import (
	"testing"

	"ai-gateway/internal/db"
	"ai-gateway/internal/models"
	"ai-gateway/internal/provider"
)

// Regression: /v1/models advertises qualified ids ("provider/model") while
// provider_models stores bare model ids. Reasoning validation must still find
// the model's capabilities when the client sends the qualified spelling,
// otherwise a reasoning-capable model is rejected with "does not support
// reasoning".
func TestValidateReasoningQualifiedModelID(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i)
	}
	ps := provider.NewStore(database, master)
	p, err := ps.Create("opencodego", models.ProviderOpenAI, "http://upstream.test/v1", "sk-test")
	if err != nil {
		t.Fatal(err)
	}
	now := "2026-01-01T00:00:00Z"
	if _, err := database.Exec(`INSERT INTO provider_models(id, provider_id, model_id, reasoning, reasoning_type, reasoning_levels, source, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		"m1", p.ID, "glm-5.3-flash", true, "effort", `["low","high","max"]`, "manual", now, now); err != nil {
		t.Fatal(err)
	}
	h := newLegacyHandler(ps, database)

	const qualified = "opencodego/glm-5.3-flash"

	reasoning, rType, levels, _ := h.getReasoningConfig(p.ID, qualified)
	if !reasoning {
		t.Fatal("getReasoningConfig: expected reasoning=true for qualified model id")
	}
	if rType != "effort" || len(levels) != 3 {
		t.Fatalf("getReasoningConfig: unexpected config type=%q levels=%v", rType, levels)
	}

	// max is an advertised level: must pass with the qualified id...
	body := []byte(`{"model":"` + qualified + `","reasoning_effort":"max","messages":[]}`)
	if err := h.validateReasoning(p.ID, qualified, body); err != nil {
		t.Fatalf("validateReasoning: expected qualified-id effort=max to pass, got %v", err)
	}
	// ...and with the bare id.
	if err := h.validateReasoning(p.ID, "glm-5.3-flash", body); err != nil {
		t.Fatalf("validateReasoning: expected bare-id effort=max to pass, got %v", err)
	}
	// unsupported level must still be rejected.
	bad := []byte(`{"model":"` + qualified + `","reasoning_effort":"ultra","messages":[]}`)
	if err := h.validateReasoning(p.ID, qualified, bad); err == nil {
		t.Fatal("validateReasoning: expected unsupported effort=ultra to be rejected")
	}
	// reasoning=false must still reject, qualified or not.
	if _, err := database.Exec(`UPDATE provider_models SET reasoning=0 WHERE id='m1'`); err != nil {
		t.Fatal(err)
	}
	if err := h.validateReasoning(p.ID, qualified, body); err == nil {
		t.Fatal("validateReasoning: expected reasoning=false model to be rejected")
	}
}
