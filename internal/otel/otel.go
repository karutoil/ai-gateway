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
}

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
)

type PrometheusMetrics struct {
	meter metric.Meter
}

func NewMetrics() Metrics {
	return &PrometheusMetrics{meter: otel.Meter("ai-gateway")}
}

func (m *PrometheusMetrics) IncRequests(provider, model, endpoint string, status int) {
	requestsTotal.WithLabelValues(provider, model, endpoint, prometheusLabel(status)).Inc()
	if m.meter != nil {
		counter, _ := m.meter.Int64Counter("gateway.requests.total")
		if counter != nil {
			counter.Add(nil, 1)
		}
	}
}

func (m *PrometheusMetrics) ObserveLatency(provider, endpoint string, d time.Duration) {
	latencyHist.WithLabelValues(provider, endpoint).Observe(float64(d.Milliseconds()))
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
