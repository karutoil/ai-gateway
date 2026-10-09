package proxy

// Tests for zero-cost instrumentation in costForModel. The metric exists so
// the "unpriced but billable" blind spot is alertable: budget enforcement sums
// request_logs.cost_usd, so traffic the catalog cannot price records $0 and
// consumes no quota. These cases also pin that instrumentation changed no
// return value — every branch still yields the same cost as before.

import (
	"path/filepath"
	"testing"
	"time"

	"ai-gateway/internal/catalog"
	"ai-gateway/internal/db"
	"ai-gateway/internal/otel"
)

type recordingMetrics struct {
	zeroReasons []string
	observed    []otel.RequestMetrics
}

func (m *recordingMetrics) IncRequests(string, string, string, int)      {}
func (m *recordingMetrics) ObserveLatency(string, string, time.Duration) {}
func (m *recordingMetrics) IncCacheHit(bool)                             {}
func (m *recordingMetrics) IncZeroCost(reason string)                    { m.zeroReasons = append(m.zeroReasons, reason) }
func (m *recordingMetrics) ObserveRequest(rm otel.RequestMetrics) {
	m.observed = append(m.observed, rm)
}

func newCostHandler(t *testing.T) (*Handler, *recordingMetrics) {
	t.Helper()
	// File-backed, not ":memory:": each pooled SQLite connection to :memory:
	// gets its own empty database, so a fixture inserted on one connection is
	// invisible to the catalog store's query on another.
	database, err := db.Open(filepath.Join(t.TempDir(), "zerocost.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	m := &recordingMetrics{}
	return &Handler{DB: database, CatalogStore: catalog.NewStore(database), Metrics: m}, m
}

func TestCostForModelZeroReasons(t *testing.T) {
	h, m := newCostHandler(t)

	// Zero tokens: nothing to price, and NOT a pricing failure.
	if got := h.costForModel("openai/gpt-test", catalog.CacheSplit{}); got != 0 {
		t.Fatalf("no-usage cost = %v, want 0", got)
	}
	if len(m.zeroReasons) != 1 || m.zeroReasons[0] != otel.ZeroCostNoUsage {
		t.Fatalf("no-usage reasons = %v, want [no_usage]", m.zeroReasons)
	}

	// Catalog store absent: config-level, distinct from a per-model miss.
	h.CatalogStore = nil
	if got := h.costForModel("openai/gpt-test", catalog.CacheSplit{Fresh: 10, Completion: 5}); got != 0 {
		t.Fatalf("no-catalog cost = %v, want 0", got)
	}
	if len(m.zeroReasons) != 2 || m.zeroReasons[1] != otel.ZeroCostNoCatalog {
		t.Fatalf("no-catalog reasons = %v", m.zeroReasons)
	}
	h.CatalogStore = catalog.NewStore(h.DB)

	// Empty catalog + real tokens = the actionable footgun.
	m.zeroReasons = nil
	if got := h.costForModel("openai/unknown-model", catalog.CacheSplit{Fresh: 100, Completion: 50}); got != 0 {
		t.Fatalf("catalog-miss cost = %v, want 0", got)
	}
	if len(m.zeroReasons) != 1 || m.zeroReasons[0] != otel.ZeroCostCatalogMiss {
		t.Fatalf("catalog-miss reasons = %v, want [catalog_miss]", m.zeroReasons)
	}
}

func TestCostForModelFallbackSuppressesCatalogMiss(t *testing.T) {
	h, m := newCostHandler(t)
	// Operator-configured fallback pricing must produce real cost and must NOT
	// report catalog_miss — the blind spot is closed by configuration.
	if _, err := h.DB.Exec(`INSERT INTO system_config(key,value,updated_at) VALUES('price_fallback_input_usd_per_1m','1.0',?)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO system_config(key,value,updated_at) VALUES('price_fallback_output_usd_per_1m','2.0',?)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got := h.costForModel("openai/unknown-model", catalog.CacheSplit{Fresh: 1_000_000, Completion: 1_000_000})
	if got != 3.0 {
		t.Fatalf("fallback cost = %v, want 3.0", got)
	}
	if len(m.zeroReasons) != 0 {
		t.Fatalf("fallback recorded zero-cost reasons: %v", m.zeroReasons)
	}
}

// insertCatalogModel writes a COMPLETE catalog row. Every column must carry a
// non-NULL value: catalog.Store.Get scans nullable columns (description,
// family, context_window, modalities, knowledge_cutoff, updated_at) straight
// into non-nullable Go types, so a row with any of them NULL fails the scan and
// the model becomes invisible to FindBestMatch — i.e. silently unpriceable.
// The models.dev sync always writes explicit values, so this only bites rows
// inserted by other means; the complete row here mirrors what sync produces.
func insertCatalogModel(t *testing.T, h *Handler, id, input, output string) {
	t.Helper()
	_, err := h.DB.Exec(`INSERT INTO models_catalog(
		id,provider,name,description,family,context_window,max_output,
		input_cost,output_cost,cache_read_cost,cache_write_cost,
		reasoning,tool_call,structured_output,attachment,modalities,
		open_weights,knowledge_cutoff,updated_at,
		reasoning_type,reasoning_levels,reasoning_output_limits)
		VALUES(?,'test','Test Model','desc','test-family',128000,4096,
		?,?,0,0,0,0,0,0,'["text"]',0,'2024-01-01',?,'','','')`,
		id, input, output, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
}

func TestCostForModelPricesAndFree(t *testing.T) {
	h, m := newCostHandler(t)
	insertCatalogModel(t, h, "openai/gpt-test", "1.0", "2.0")
	insertCatalogModel(t, h, "local/free-test", "0", "0")

	// Priced: real cost, no zero-cost reason recorded.
	if got := h.costForModel("openai/gpt-test", catalog.CacheSplit{Fresh: 1_000_000, Completion: 1_000_000}); got != 3.0 {
		t.Fatalf("priced cost = %v, want 3.0", got)
	}
	if len(m.zeroReasons) != 0 {
		t.Fatalf("priced model recorded zero-cost reasons: %v", m.zeroReasons)
	}

	// Genuinely free: counted, but under a reason that keeps catalog_miss
	// alerts free of noise.
	if got := h.costForModel("local/free-test", catalog.CacheSplit{Fresh: 1000, Completion: 1000}); got != 0 {
		t.Fatalf("free cost = %v, want 0", got)
	}
	if len(m.zeroReasons) != 1 || m.zeroReasons[0] != otel.ZeroCostFreeModel {
		t.Fatalf("free reasons = %v, want [free_model]", m.zeroReasons)
	}
}

// A nil Metrics field must not panic — tests and slim builds leave it unset.
func TestCostForModelNilMetrics(t *testing.T) {
	h, _ := newCostHandler(t)
	h.Metrics = nil
	if got := h.costForModel("openai/unknown-model", catalog.CacheSplit{Fresh: 100, Completion: 50}); got != 0 {
		t.Fatalf("cost = %v, want 0", got)
	}
}
