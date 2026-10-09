package otel

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

type Metrics interface {
	IncRequests(provider, model, endpoint string, status int)
	ObserveLatency(provider, endpoint string, d time.Duration)
	IncCacheHit(hit bool)
	// IncZeroCost records a completed request whose recorded cost is $0, by
	// reason. Reasons are a closed set (see ZeroCost* constants) so this
	// counter stays at bounded cardinality — deliberately NO model label,
	// since model names are client-influenced. Use request_logs to find which
	// model is unpriced; use this metric to know it is happening at all.
	IncZeroCost(reason string)
	// ObserveRequest records the cost, token, TTFT and cache-disposition
	// instruments for one completed request.
	ObserveRequest(rm RequestMetrics)
}

// Token types emitted on gateway_tokens_total. Input is split by cache
// disposition so a cache discount is visible in PromQL without a join.
const (
	TokenPrompt     = "prompt"      // billed at the standard input rate
	TokenCompletion = "completion"  // output tokens
	TokenCacheRead  = "cache_read"  // input served from an upstream prompt cache
	TokenCacheWrite = "cache_write" // input written into an upstream prompt cache
	TokenReasoning  = "reasoning"   // reasoning/thinking tokens (subset of output)
)

// RequestMetrics is the per-request measurement set. CacheStatus is the
// gateway response-cache disposition (hit|miss|bypass) and is carried as a
// LABEL on the cost counter rather than a separate metric, so "what did the
// cache save us" is one PromQL query instead of a cross-family join.
type RequestMetrics struct {
	Provider    string
	Model       string
	Endpoint    string
	CacheStatus string
	CostUSD     float64
	Prompt      int
	Completion  int
	CacheRead   int
	CacheWrite  int
	Reasoning   int
	// TtftMs is upstream time-to-first-chunk. Only meaningful for streaming:
	// a buffered response has no first-token moment, so 0 means "not
	// applicable" and is NOT recorded. Reporting a fake value would drag the
	// histogram toward zero for every non-streaming request.
	TtftMs int64
}

// Zero-cost dispositions. The distinction that matters: catalog_miss is
// billable traffic the gateway could not price, so it records $0 and thereby
// bypasses every per-key budget (enforcement sums request_logs.cost_usd).
// free_model is a priced-and-genuinely-zero model; no_usage/no_catalog are
// benign. Operators alert on catalog_miss alone.
const (
	ZeroCostCatalogMiss = "catalog_miss" // unpriced but billable — actionable
	ZeroCostFreeModel   = "free_model"   // catalog hit, both prices 0
	ZeroCostNoCatalog   = "no_catalog"   // catalog store not wired
	ZeroCostNoUsage     = "no_usage"     // zero tokens; nothing to price
)

var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_requests_total",
		Help: "Total gateway requests by provider, model, endpoint, status",
	}, []string{"provider", "model", "endpoint", "status"})
	latencyHist = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_latency_ms",
		Help:    "Gateway latency in milliseconds",
		Buckets: []float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000},
	}, []string{"provider", "endpoint"})
	cacheHits = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_cache_hits_total",
		Help: "Cache hits vs misses",
	}, []string{"result"})
	zeroCost = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_zero_cost_requests_total",
		Help: "Completed requests recorded at $0 cost, by reason (catalog_miss = unpriced but billable)",
	}, []string{"reason"})

	// Money and tokens as first-class instruments. Most gateway metric sets
	// export only requests and latency, which makes spend unalertable in
	// Prometheus — cost lives in the DB and dashboard and nowhere else.
	costUSD = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_cost_usd_total",
		Help: "Estimated upstream spend in USD, by cache disposition",
	}, []string{"provider", "model", "endpoint", "cache_status"})
	tokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_tokens_total",
		Help: "Tokens processed by type (prompt excludes cached input; cache_read/cache_write are input served by an upstream prompt cache)",
	}, []string{"provider", "model", "endpoint", "token_type"})
	// Buckets follow the OpenTelemetry GenAI latency boundaries (0.01s…81.92s)
	// expressed in milliseconds, so TTFT drops into an existing convention
	// rather than an invented one.
	ttftHist = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_ttft_ms",
		Help:    "Upstream time to first token in milliseconds (streaming requests only)",
		Buckets: []float64{10, 20, 40, 80, 160, 320, 640, 1280, 2560, 5120, 10240, 20480, 40960, 81920},
	}, []string{"provider", "model"})
)

// maxLabelValue bounds every label value we mint. Label VALUES are bounded in
// bytes even when the number of distinct values is not: an unbounded string
// copied into a label is a memory-growth vector, and this repo's model names
// are client-influenced (routing rules bound the reachable set in practice,
// but nothing bounds the length). 128 bytes follows Apache APISIX, which
// truncates client-supplied model label values to the same size.
const maxLabelValue = 128

// promLabel normalises a label value for export.
func promLabel(v string) string {
	if len(v) <= maxLabelValue {
		return v
	}
	return v[:maxLabelValue]
}

type PrometheusMetrics struct {
	meter metric.Meter
}

func NewMetrics() Metrics {
	return &PrometheusMetrics{meter: otel.Meter("ai-gateway")}
}

func (m *PrometheusMetrics) IncRequests(provider, model, endpoint string, status int) {
	requestsTotal.WithLabelValues(promLabel(provider), promLabel(model), promLabel(endpoint), prometheusLabel(status)).Inc()
	if m.meter != nil {
		counter, _ := m.meter.Int64Counter("gateway.requests.total")
		if counter != nil {
			counter.Add(nil, 1)
		}
	}
}

func (m *PrometheusMetrics) ObserveLatency(provider, endpoint string, d time.Duration) {
	latencyHist.WithLabelValues(promLabel(provider), promLabel(endpoint)).Observe(float64(d.Milliseconds()))
}

// ObserveRequest exports the money, token and TTFT view of one request.
//
// The legacy gateway_requests_total / gateway_latency_ms / gateway_cache_hits_total
// instruments keep their existing label sets on purpose: adding cache_status to
// them would break every dashboard and recording rule already built on them.
// Cache disposition therefore rides on the new cost counter, which is the
// query that actually matters ("what is the cache saving us").
func (m *PrometheusMetrics) ObserveRequest(rm RequestMetrics) {
	provider, model, endpoint := promLabel(rm.Provider), promLabel(rm.Model), promLabel(rm.Endpoint)

	if rm.CostUSD != 0 {
		costUSD.WithLabelValues(provider, model, endpoint, cacheStatusLabel(rm.CacheStatus)).Add(rm.CostUSD)
	}
	add := func(kind string, n int) {
		if n != 0 {
			tokensTotal.WithLabelValues(provider, model, endpoint, kind).Add(float64(n))
		}
	}
	add(TokenPrompt, rm.Prompt)
	add(TokenCompletion, rm.Completion)
	add(TokenCacheRead, rm.CacheRead)
	add(TokenCacheWrite, rm.CacheWrite)
	add(TokenReasoning, rm.Reasoning)

	// Streaming only: 0 means "no first token observed", which is the normal
	// case for buffered responses, not a zero-latency one.
	if rm.TtftMs > 0 {
		ttftHist.WithLabelValues(provider, model).Observe(float64(rm.TtftMs))
	}
}

// cacheStatusLabel folds an unknown disposition into "unknown" so the label
// cannot grow past the four dispositions the gateway can actually record.
func cacheStatusLabel(s string) string {
	switch s {
	case "hit", "miss", "bypass":
		return s
	case "":
		return "unknown"
	default:
		return "unknown"
	}
}

func (m *PrometheusMetrics) IncCacheHit(hit bool) {
	if hit {
		cacheHits.WithLabelValues("hit").Inc()
	} else {
		cacheHits.WithLabelValues("miss").Inc()
	}
	if m.meter != nil {
		counter, _ := m.meter.Int64Counter("gateway.cache.hits")
		if counter != nil && hit {
			counter.Add(nil, 1)
		}
	}
}

// Seed every reason at zero so the series exist before the first occurrence.
// Without this a CounterVec stays unexported until one label set is used, and
// an operator's `gateway_zero_cost_requests_total{reason="catalog_miss"} > 0`
// alert (and any dashboard) reads "no data" rather than a meaningful 0.
func init() {
	for _, r := range []string{ZeroCostCatalogMiss, ZeroCostFreeModel, ZeroCostNoCatalog, ZeroCostNoUsage} {
		zeroCost.WithLabelValues(r).Add(0)
	}
}

// IncZeroCost counts a request whose recorded cost is $0. Unknown reasons are
// folded into "other" so a future caller cannot mint unbounded label values.
func (m *PrometheusMetrics) IncZeroCost(reason string) {
	switch reason {
	case ZeroCostCatalogMiss, ZeroCostFreeModel, ZeroCostNoCatalog, ZeroCostNoUsage:
	default:
		reason = "other"
	}
	zeroCost.WithLabelValues(reason).Inc()
}

func prometheusLabel(status int) string {
	if status >= 200 && status < 300 {
		return "2xx"
	}
	if status >= 400 && status < 500 {
		return "4xx"
	}
	if status >= 500 {
		return "5xx"
	}
	return "other"
}

var _ Metrics = (*PrometheusMetrics)(nil)
