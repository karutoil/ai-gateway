package otel

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// gatherFamily returns the metric family with the given name from the default
// registry (promauto registers there), or nil. Uses the gatherer rather than
// prometheus/testutil so the package needs no new dependency.
func gatherFamily(t *testing.T, name string) *dto.MetricFamily {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

// endpointLabelsFor returns every distinct endpoint label value currently
// exported for the named family.
func endpointLabelsFor(t *testing.T, family string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	f := gatherFamily(t, family)
	if f == nil {
		return out
	}
	for _, m := range f.GetMetric() {
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "endpoint" {
				out[lp.GetValue()] = true
			}
		}
	}
	return out
}

// TestEndpointLabelFoldsUnknownToOther pins the bounded-cardinality guarantee
// for the endpoint dimension: a caller cannot mint a new label value by
// passing an arbitrary string, exactly like IncZeroCost's reason handling.
func TestEndpointLabelFoldsUnknownToOther(t *testing.T) {
	known := []string{
		EndpointChatCompletions, EndpointMessages, EndpointResponses,
		EndpointEmbeddings, EndpointImagesGen, EndpointImagesEdits,
		EndpointSystemOne, EndpointVideos, EndpointVideosPoll,
		EndpointVideosList, EndpointVideosContent, EndpointVideosDelete,
	}
	for _, k := range known {
		if got := EndpointLabel(k); got != k {
			t.Fatalf("EndpointLabel(%q) = %q, want it preserved", k, got)
		}
	}
	for _, unknown := range []string{
		"", "videos.polll", "images", "videos.", "some/new/route",
		"videos.poll?job=abc", strings.Repeat("x", 500),
	} {
		if got := EndpointLabel(unknown); got != EndpointOther {
			t.Fatalf("EndpointLabel(%q) = %q, want %q", unknown, got, EndpointOther)
		}
	}
}

// TestUnknownEndpointNeverReachesTheRegistry asserts the folding is applied by
// the exported instruments, not just by the helper: a bogus endpoint must
// appear as "other" in gateway_requests_total, gateway_latency_ms and
// gateway_cost_usd_total, so the endpoint label stays a closed set in practice.
func TestUnknownEndpointNeverReachesTheRegistry(t *testing.T) {
	m := NewMetrics().(*PrometheusMetrics)
	sentinel := "bogus-endpoint-" + strings.Repeat("z", 8)

	m.IncRequests("prov-card", "model-card", sentinel, http.StatusOK)
	m.ObserveLatency("prov-card", sentinel, 5*time.Millisecond)
	m.ObserveRequest(RequestMetrics{
		Provider: "prov-card", Model: "model-card", Endpoint: sentinel,
		CostUSD: 0.5, Prompt: 10, Completion: 5, CacheStatus: "miss",
	})

	for _, family := range []string{"gateway_requests_total", "gateway_latency_ms", "gateway_cost_usd_total"} {
		labels := endpointLabelsFor(t, family)
		if labels[sentinel] {
			t.Fatalf("%s exported the raw sentinel endpoint value; folding is not applied", family)
		}
		if !labels[EndpointOther] {
			t.Fatalf("%s did not export endpoint=%q for an unknown endpoint", family, EndpointOther)
		}
	}
}

// TestEveryEndpointIsExpressible documents the contract the proxy relies on:
// each new endpoint can carry both a request count and a latency observation,
// so per-endpoint requests+latency exists for the image/video surfaces without
// any new instrument.
func TestEveryEndpointIsExpressible(t *testing.T) {
	m := NewMetrics().(*PrometheusMetrics)
	endpoints := []string{
		EndpointImagesGen, EndpointImagesEdits, EndpointSystemOne,
		EndpointVideos, EndpointVideosPoll, EndpointVideosList,
		EndpointVideosContent, EndpointVideosDelete,
	}
	for _, ep := range endpoints {
		m.IncRequests("p", "m", ep, http.StatusOK)
		m.ObserveLatency("p", ep, time.Second)
	}

	requests := endpointLabelsFor(t, "gateway_requests_total")
	latency := endpointLabelsFor(t, "gateway_latency_ms")
	for _, ep := range endpoints {
		if !requests[ep] {
			t.Fatalf("gateway_requests_total missing endpoint=%q", ep)
		}
		if !latency[ep] {
			t.Fatalf("gateway_latency_ms missing endpoint=%q", ep)
		}
		if EndpointLabel(ep) != ep {
			t.Fatalf("endpoint %q is not in the declared closed set", ep)
		}
	}
}

// TestLatencyBucketsCoverSlowImageAndVideoRequests pins that the latency
// histogram can resolve the multi-minute tail: without buckets past 10s every
// image generation and video content download lands in +Inf, which makes p95
// unreadable for exactly the endpoints we just added metrics for.
func TestLatencyBucketsCoverSlowImageAndVideoRequests(t *testing.T) {
	f := gatherFamily(t, "gateway_latency_ms")
	if f == nil || len(f.GetMetric()) == 0 {
		t.Fatal("gateway_latency_ms family not registered with any series")
	}
	buckets := f.GetMetric()[0].GetHistogram().GetBucket()
	if len(buckets) == 0 {
		t.Fatal("gateway_latency_ms has no buckets")
	}
	max := buckets[len(buckets)-1].GetUpperBound()
	// The image first-byte budget defaults to 300s; require the histogram to
	// resolve at least that far so a 300s image request is not +Inf.
	if max < 300000 {
		t.Fatalf("gateway_latency_ms top bucket = %vms, want >= 300000ms (image budget)", max)
	}
}
