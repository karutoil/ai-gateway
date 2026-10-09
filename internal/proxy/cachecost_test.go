package proxy

// Tests for the normalized billing split: the two upstream families report
// cached tokens with opposite semantics, and conflating them either
// double-charges Anthropic or under-charges OpenAI.

import (
	"context"
	"testing"

	"ai-gateway/internal/catalog"
)

func TestCacheSplitFromUsageOpenAISubset(t *testing.T) {
	// OpenAI: prompt_tokens INCLUDES cached_tokens, so cached must be
	// subtracted out of the fresh bucket — never added.
	u := map[string]interface{}{
		"prompt_tokens":     float64(1000),
		"completion_tokens": float64(500),
		"prompt_tokens_details": map[string]interface{}{
			"cached_tokens": float64(400),
		},
	}
	s := cacheSplitFromUsage(u)
	if s.Fresh != 600 || s.CacheRead != 400 || s.CacheWrite != 0 || s.Completion != 500 {
		t.Fatalf("openai split = %+v, want Fresh=600 CacheRead=400 CacheWrite=0 Completion=500", s)
	}
	// Cache tokens are input tokens, so the total must be unchanged by the
	// split — this is the invariant that keeps logged tokens consistent.
	if s.TotalInput() != 1000 {
		t.Fatalf("TotalInput = %d, want 1000", s.TotalInput())
	}
}

func TestCacheSplitFromUsageAnthropicAdditive(t *testing.T) {
	// Anthropic: input_tokens EXCLUDES the cache fields, so they add.
	u := map[string]interface{}{
		"input_tokens":                float64(1000),
		"output_tokens":               float64(500),
		"cache_read_input_tokens":     float64(400),
		"cache_creation_input_tokens": float64(100),
	}
	s := cacheSplitFromUsage(u)
	if s.Fresh != 1000 || s.CacheRead != 400 || s.CacheWrite != 100 || s.Completion != 500 {
		t.Fatalf("anthropic split = %+v, want Fresh=1000 CacheRead=400 CacheWrite=100 Completion=500", s)
	}
	if s.TotalInput() != 1500 {
		t.Fatalf("TotalInput = %d, want 1500 (cache is additional)", s.TotalInput())
	}
}

func TestCacheSplitClampsInconsistentUpstream(t *testing.T) {
	// A cached count larger than the prompt total would yield a negative
	// fresh bucket and a negative charge; clamp instead of trusting it.
	u := map[string]interface{}{
		"prompt_tokens":     float64(100),
		"completion_tokens": float64(10),
		"prompt_tokens_details": map[string]interface{}{
			"cached_tokens": float64(500),
		},
	}
	s := cacheSplitFromUsage(u)
	if s.Fresh != 0 || s.CacheRead != 100 {
		t.Fatalf("clamped split = %+v, want Fresh=0 CacheRead=100", s)
	}
}

func TestCacheSplitFromBodyResponsesShape(t *testing.T) {
	// Responses API frames nest usage under "response"; extractUsage does not
	// read that shape, so the split parser must.
	body := []byte(`{"response":{"usage":{"input_tokens":800,"output_tokens":200,"input_tokens_details":{"cached_tokens":300}}}}`)
	s := cacheSplitFromBody(body)
	if s.Fresh != 500 || s.CacheRead != 300 || s.Completion != 200 {
		t.Fatalf("responses split = %+v, want Fresh=500 CacheRead=300 Completion=200", s)
	}
	// Malformed bodies degrade to an empty split rather than panicking.
	if got := cacheSplitFromBody([]byte("not json")); got.Any() {
		t.Fatalf("malformed body produced usage: %+v", got)
	}
}

func TestStreamSplitMatchesFoldedTotals(t *testing.T) {
	// Anthropic: the SSE harvest folds cache read AND write into promptTok,
	// so both come back out of the fresh bucket.
	got := streamSplit(1500, 500, usageDetail{CacheRead: 400, CacheWrite: 100})
	want := catalog.CacheSplit{Fresh: 1000, CacheRead: 400, CacheWrite: 100, Completion: 500}
	if got != want {
		t.Fatalf("anthropic stream split = %+v, want %+v", got, want)
	}

	// OpenAI: cached is already inside promptTok and must not be removed
	// twice — only the read is subtracted, and cache write stays 0.
	got = streamSplit(1000, 500, usageDetail{CacheRead: 400})
	want = catalog.CacheSplit{Fresh: 600, CacheRead: 400, Completion: 500}
	if got != want {
		t.Fatalf("openai stream split = %+v, want %+v", got, want)
	}

	// A conservative over-count must never drive fresh negative.
	if s := streamSplit(100, 0, usageDetail{CacheRead: 400}); s.Fresh != 0 {
		t.Fatalf("negative fresh not clamped: %+v", s)
	}
}

func TestSplitOrFallbackPreservesUnrecognisedShapes(t *testing.T) {
	// A body the split parser cannot read must still bill at the input rate
	// rather than silently dropping to $0 and bypassing budgets.
	got := splitOrFallback(catalog.CacheSplit{}, 700, 300)
	if got.Fresh != 700 || got.Completion != 300 {
		t.Fatalf("fallback = %+v, want Fresh=700 Completion=300", got)
	}
	// A real split is passed through untouched.
	real := catalog.CacheSplit{Fresh: 600, CacheRead: 400, Completion: 500}
	if got := splitOrFallback(real, 1000, 500); got != real {
		t.Fatalf("fallback overrode a valid split: %+v", got)
	}
	// No usage at all stays empty (priced as no_usage, not as a phantom).
	if got := splitOrFallback(catalog.CacheSplit{}, 0, 0); got.Any() {
		t.Fatalf("zero-token fallback invented usage: %+v", got)
	}
}

// TestCostForModelCacheDiscountEndToEnd drives the whole path: a catalog with
// a cache-read discount must produce a strictly lower cost than the flat
// input-rate arithmetic it replaced.
func TestCostForModelCacheDiscountEndToEnd(t *testing.T) {
	h, _ := newCostHandler(t)
	insertCatalogModel(t, h, "cachevendor/cached-model", "1.0", "2.0")
	if _, err := h.DB.Exec(`UPDATE models_catalog SET cache_read_cost=0.25 WHERE id='cachevendor/cached-model'`); err != nil {
		t.Fatal(err)
	}

	// 1000 prompt of which 400 cached, 500 completion.
	split := cacheSplitFromUsage(map[string]interface{}{
		"prompt_tokens":     float64(1000),
		"completion_tokens": float64(500),
		"prompt_tokens_details": map[string]interface{}{
			"cached_tokens": float64(400),
		},
	})
	got := h.costForModel("cachevendor/cached-model", split)
	want := (600*1.0 + 400*0.25 + 500*2.0) / 1_000_000
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("cache-aware cost = %v, want %v", got, want)
	}
	naive := (1000*1.0 + 500*2.0) / 1_000_000
	if !(got < naive) {
		t.Fatalf("cost %v not discounted below naive %v", got, naive)
	}
}

// TestObserveRequestCarriesSplit pins the metric wiring end to end: the token
// split must be derived from the folded prompt total, because emitting the
// folded value as "prompt" would double-count cached tokens that are also
// reported under their own token_type.
func TestObserveRequestCarriesSplit(t *testing.T) {
	h, m := newCostHandler(t)
	// Folded prompt of 1000 = 600 fresh + 400 cached.
	h.logRequestExtendedBodies(context.Background(), "", "prov-1", "some-model", "chat.completions", 200, 123, 1000, 500, 0.0017, false, nil, nil, &logMeta{
		CacheRead:   400,
		CacheStatus: "miss",
		TtftMs:      250,
	})
	if len(m.observed) != 1 {
		t.Fatalf("observed %d requests, want 1", len(m.observed))
	}
	rm := m.observed[0]
	if rm.Prompt != 600 {
		t.Fatalf("prompt = %d, want 600 (folded total minus cached)", rm.Prompt)
	}
	if rm.CacheRead != 400 || rm.Completion != 500 {
		t.Fatalf("cache/completion = %d/%d, want 400/500", rm.CacheRead, rm.Completion)
	}
	if rm.CostUSD != 0.0017 || rm.CacheStatus != "miss" || rm.TtftMs != 250 {
		t.Fatalf("cost/cache_status/ttft = %v/%q/%d", rm.CostUSD, rm.CacheStatus, rm.TtftMs)
	}
	if rm.Provider != "prov-1" || rm.Model != "some-model" {
		t.Fatalf("identity = %q/%q", rm.Provider, rm.Model)
	}
}

// TestObserveRequestClampsFresh guards the invariant that the fresh bucket can
// never go negative when an upstream reports a cache count above the prompt
// total, which would otherwise underflow the token counter.
func TestObserveRequestClampsFresh(t *testing.T) {
	h, m := newCostHandler(t)
	h.logRequestExtendedBodies(context.Background(), "", "prov-1", "m", "chat.completions", 200, 5, 100, 10, 0, false, nil, nil, &logMeta{CacheRead: 400, CacheWrite: 50})
	if len(m.observed) != 1 {
		t.Fatalf("observed %d, want 1", len(m.observed))
	}
	if got := m.observed[0].Prompt; got != 0 {
		t.Fatalf("prompt = %d, want 0 (clamped)", got)
	}
}

// TestObserveRequestAnthropicBothBuckets checks the Anthropic shape, where the
// harvest folds cache READ and WRITE into the prompt total, so both must come
// back out of the fresh bucket.
func TestObserveRequestAnthropicBothBuckets(t *testing.T) {
	h, m := newCostHandler(t)
	// input 1000 + cache_read 400 + cache_write 100 folded => 1500.
	h.logRequestExtendedBodies(context.Background(), "", "prov-1", "m", "messages", 200, 5, 1500, 500, 0.002, true, nil, nil, &logMeta{CacheRead: 400, CacheWrite: 100})
	rm := m.observed[0]
	if rm.Prompt != 1000 || rm.CacheRead != 400 || rm.CacheWrite != 100 {
		t.Fatalf("anthropic split = prompt %d, read %d, write %d; want 1000/400/100", rm.Prompt, rm.CacheRead, rm.CacheWrite)
	}
	if rm.Prompt+rm.CacheRead+rm.CacheWrite != 1500 {
		t.Fatalf("buckets do not sum to the folded total: %d", rm.Prompt+rm.CacheRead+rm.CacheWrite)
	}
}
