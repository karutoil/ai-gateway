package resilience

import "github.com/prometheus/client_golang/prometheus"
import "github.com/prometheus/client_golang/prometheus/promauto"

// BillableRetrySkipsTotal counts retries/failovers the gateway DECLINED because
// the request was a billable, non-idempotent create (NonRetryable).
//
// It exists so over-billing is observable rather than invisible. Before the
// noRetry/noFailover mode existed, every one of these events would have been
// a SECOND upstream generation (and a second charge) for one client request —
// so a non-zero rate here is "the gateway is protecting the operator from
// duplicate spend", and a sudden spike is "the upstream is flaky during image
// generates". The label set is deliberately bounded: endpoint and the
// upstream class of the failure (status family), never a model name or an
// upstream message (both client/provider influenced).
var billableRetrySkips = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "gateway",
	Name:      "billable_retry_skips_total",
	Help:      "Retries/failovers declined for billable non-idempotent creates, by endpoint and upstream status family",
}, []string{"endpoint", "status"})

// statusFamily folds an upstream status into a bounded label value.
func statusFamily(status int) string {
	switch {
	case status == 0:
		return "transport"
	case status == 429:
		return "429"
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	default:
		return "other"
	}
}

// IncBillableRetrySkip records one declined retry. Unknown endpoint values are
// folded to "other" so a caller cannot mint unbounded label cardinality.
func IncBillableRetrySkip(endpoint string, status int) {
	switch endpoint {
	case "images.generations", "images.edits", "images", "systemone":
	default:
		endpoint = "other"
	}
	billableRetrySkips.WithLabelValues(endpoint, statusFamily(status)).Inc()
}
