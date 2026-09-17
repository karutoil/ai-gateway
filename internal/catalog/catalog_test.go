package catalog

import (
	"testing"

	"ai-gateway/internal/db"
)

// Gateway-qualified IDs ("oc1/muse-spark-1.3-contributor") carry the
// gateway's own provider prefix, which never matches catalog IDs keyed by
// models.dev namespace. The lookup must strip to the suffix — and prefer a
// priced row, since the suffix match can hit zero-price mirrors.
func TestGetByShortIDGatewayQualifiedPrefersPriced(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	s := NewStore(database)

	body := []byte(`{
		"meta": {"id":"meta","name":"meta","api":"openai","models":{
			"muse-spark-1.3-contributor": {"id":"meta/muse-spark-1.3-contributor","name":"Muse Spark","cost":{"input":0.1,"output":0.2}}
		}},
		"ollama-cloud": {"id":"ollama-cloud","name":"ollama-cloud","api":"openai","models":{
			"muse-spark-1.3-contributor": {"id":"ollama-cloud/muse-spark-1.3-contributor","name":"Muse Spark","cost":{"input":0,"output":0}}
		}}
	}`)
	if _, err := s.SyncFromBytes(body); err != nil {
		t.Fatal(err)
	}

	// Gateway-qualified form, as recorded in request_logs.model.
	m, err := s.GetByShortID("oc1/muse-spark-1.3-contributor")
	if err != nil {
		t.Fatalf("gateway-qualified lookup failed: %v", err)
	}
	if m.InputCost != 0.1 || m.OutputCost != 0.2 {
		t.Fatalf("gateway-qualified lookup hit wrong row: %s in=%.4f out=%.4f", m.ID, m.InputCost, m.OutputCost)
	}

	// Bare short ID must also prefer the priced row over the zero-price mirror.
	m, err = s.GetByShortID("muse-spark-1.3-contributor")
	if err != nil {
		t.Fatalf("short lookup failed: %v", err)
	}
	if m.InputCost != 0.1 || m.OutputCost != 0.2 {
		t.Fatalf("short lookup hit zero-price row: %s in=%.4f out=%.4f", m.ID, m.InputCost, m.OutputCost)
	}

	if got := CostFor(m, 1_000_000, 1_000_000); got < 0.299 || got > 0.301 {
		t.Fatalf("CostFor = %v, want ~0.3", got)
	}
}
