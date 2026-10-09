package otel

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// zeroCostCount reads the current value of gateway_zero_cost_requests_total
// for one reason from the default registry. Uses the gatherer rather than
// prometheus/testutil so the package needs no new dependency.
func zeroCostCount(t *testing.T, reason string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "gateway_zero_cost_requests_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "reason" && lp.GetValue() == reason {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// TestIncZeroCostFoldsUnknownReasons pins the bounded-cardinality guarantee:
// the metric carries a closed reason set, so no caller can mint label values.
func TestIncZeroCostFoldsUnknownReasons(t *testing.T) {
	m := NewMetrics().(*PrometheusMetrics)

	// Every reason must be exported at 0 before any increment, so alerting
	// expressions and dashboards see a value rather than "no data".
	for _, r := range []string{ZeroCostCatalogMiss, ZeroCostFreeModel, ZeroCostNoCatalog, ZeroCostNoUsage} {
		if got := zeroCostCount(t, r); got != 0 {
			t.Fatalf("reason %q not pre-seeded at 0: %v", r, got)
		}
	}

	for _, r := range []string{ZeroCostCatalogMiss, ZeroCostFreeModel, ZeroCostNoCatalog, ZeroCostNoUsage} {
		before := zeroCostCount(t, r)
		m.IncZeroCost(r)
		if got := zeroCostCount(t, r); got != before+1 {
			t.Fatalf("reason %q: counter %v, want %v", r, got, before+1)
		}
	}

	before := zeroCostCount(t, "other")
	m.IncZeroCost("something-new")
	if got := zeroCostCount(t, "other"); got != before+1 {
		t.Fatalf("unknown reason not folded into other: %v, want %v", got, before+1)
	}

	// The bogus value itself must never reach the registry.
	if got := zeroCostCount(t, "something-new"); got != 0 {
		t.Fatalf("unfolded reason leaked into registry: %v", got)
	}
}

// gatherValue reads one labelled sample of a counter from the default registry.
func gatherValue(t *testing.T, metric, labelName, labelValue string) (float64, bool) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != metric {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == labelName && lp.GetValue() == labelValue {
					return m.GetCounter().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

// histogramCount counts observations recorded against one labelled histogram.
func histogramCount(t *testing.T, metric, labelName, labelValue string) uint64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != metric {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == labelName && lp.GetValue() == labelValue {
					return m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

// TestObserveRequestInstruments covers the money/token/TTFT view. Cost and
// tokens as first-class Prometheus instruments are the gap versus most
// gateways, whose /metrics carries requests and latency only.
func TestObserveRequestInstruments(t *testing.T) {
	m := NewMetrics().(*PrometheusMetrics)
	m.ObserveRequest(RequestMetrics{
		Provider:    "prov-x",
		Model:       "model-y",
		Endpoint:    "chat.completions",
		CacheStatus: "hit",
		CostUSD:     0.25,
		Prompt:      600,
		Completion:  500,
		CacheRead:   400,
		CacheWrite:  100,
		Reasoning:   50,
		TtftMs:      250,
	})

	if got, ok := gatherValue(t, "gateway_cost_usd_total", "cache_status", "hit"); !ok || got != 0.25 {
		t.Fatalf("cost counter = %v (present=%v), want 0.25", got, ok)
	}
	for kind, want := range map[string]float64{
		TokenPrompt: 600, TokenCompletion: 500, TokenCacheRead: 400,
		TokenCacheWrite: 100, TokenReasoning: 50,
	} {
		if got, ok := gatherValue(t, "gateway_tokens_total", "token_type", kind); !ok || got != want {
			t.Fatalf("token_type=%s = %v (present=%v), want %v", kind, got, ok, want)
		}
	}
	if got := histogramCount(t, "gateway_ttft_ms", "model", "model-y"); got != 1 {
		t.Fatalf("ttft observations = %d, want 1", got)
	}
}

// TestTTFTNotRecordedForNonStreaming pins the streaming-only rule: a buffered
// response has no first-token moment, so recording 0 would drag the histogram
// toward zero for every non-streaming request.
func TestTTFTNotRecordedForNonStreaming(t *testing.T) {
	m := NewMetrics().(*PrometheusMetrics)
	before := histogramCount(t, "gateway_ttft_ms", "model", "buffered-model")
	m.ObserveRequest(RequestMetrics{Provider: "p", Model: "buffered-model", Endpoint: "chat.completions", Prompt: 10, TtftMs: 0})
	if after := histogramCount(t, "gateway_ttft_ms", "model", "buffered-model"); after != before {
		t.Fatalf("non-streaming request recorded TTFT: %d -> %d", before, after)
	}
}

// TestObserveRequestSkipsZeroValues keeps the series space free of all-zero
// samples that would otherwise be created by every request.
func TestObserveRequestSkipsZeroValues(t *testing.T) {
	m := NewMetrics().(*PrometheusMetrics)
	m.ObserveRequest(RequestMetrics{Provider: "p2", Model: "free-model", Endpoint: "chat.completions", CostUSD: 0})
	if _, ok := gatherValue(t, "gateway_cost_usd_total", "model", "free-model"); ok {
		t.Fatal("zero-cost request minted a cost series")
	}
	if _, ok := gatherValue(t, "gateway_tokens_total", "model", "free-model"); ok {
		t.Fatal("zero-token request minted a token series")
	}
}

// TestLabelValuesAreBounded pins the length cap: model names are
// client-influenced, and an unbounded label value is a memory-growth vector.
func TestLabelValuesAreBounded(t *testing.T) {
	long := strings.Repeat("m", 400)
	m := NewMetrics().(*PrometheusMetrics)
	m.ObserveRequest(RequestMetrics{Provider: "p3", Model: long, Endpoint: "chat.completions", CostUSD: 1})

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	found := false
	for _, f := range families {
		if f.GetName() != "gateway_cost_usd_total" {
			continue
		}
		for _, mm := range f.GetMetric() {
			for _, lp := range mm.GetLabel() {
				if lp.GetName() != "model" {
					continue
				}
				found = true
				if len(lp.GetValue()) > maxLabelValue {
					t.Fatalf("model label length %d exceeds cap %d", len(lp.GetValue()), maxLabelValue)
				}
			}
		}
	}
	if !found {
		t.Fatal("no model label recorded")
	}
}

// TestCacheStatusLabelFoldsUnknown keeps the disposition label closed.
func TestCacheStatusLabelFoldsUnknown(t *testing.T) {
	for in, want := range map[string]string{
		"hit": "hit", "miss": "miss", "bypass": "bypass",
		"": "unknown", "weird-new-state": "unknown",
	} {
		if got := cacheStatusLabel(in); got != want {
			t.Fatalf("cacheStatusLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
