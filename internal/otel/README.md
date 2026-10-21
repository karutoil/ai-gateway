# `internal/otel` — metrics label contract

Everything the gateway exports to Prometheus lives here. This file is the
authoritative label-set reference; if you add a label or an endpoint, update it
in the same commit.

The package registers instruments on the **default Prometheus registry**
(`promauto`), so `promhttp.Handler()` in `cmd/gateway/main.go` exposes them at
`GET /metrics` (gated by `METRICS_PROTECT` / dashboard auth when set).

## Instruments

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `gateway_requests_total` | counter | `provider`, `model`, `endpoint`, `status` | Completed requests. `status` is folded to `2xx` / `4xx` / `5xx` / `other`. |
| `gateway_latency_ms` | histogram | `provider`, `endpoint` | End-to-end request latency in ms. Buckets 5ms … 10m (the long tail exists for images and video content). |
| `gateway_cache_hits_total` | counter | `result` (`hit` / `miss`) | Exact-match response cache disposition. |
| `gateway_cost_usd_total` | counter | `provider`, `model`, `endpoint`, `cache_status` | Estimated upstream spend. `cache_status` ∈ `hit` / `miss` / `bypass` / `unknown`. |
| `gateway_tokens_total` | counter | `provider`, `model`, `endpoint`, `token_type` | Token counts. `token_type` ∈ `prompt` / `completion` / `cache_read` / `cache_write` / `reasoning`. |
| `gateway_ttft_ms` | histogram | `provider`, `model` | Upstream time-to-first-token, streaming requests only. Never observed for buffered responses. |
| `gateway_zero_cost_requests_total` | counter | `reason` | Completed requests recorded at `$0`. `reason` ∈ `catalog_miss` (billable but unpriced — alert on this), `free_model`, `no_catalog`, `no_usage`, `other`. |

`gateway_billable_retry_skips_total{endpoint,status}` is emitted by
`internal/resilience` with the same endpoint vocabulary.

## Label cardinality rules

* **`endpoint` is a closed set.** `EndpointLabel()` folds anything not declared
  in `otel.go` to `other`. The current set:

  | Value | Route |
  | --- | --- |
  | `chat.completions` | `POST /v1/chat/completions` (+ bare alias) |
  | `messages` | `POST /v1/messages` (Anthropic-native) |
  | `responses` | `POST /v1/responses` (+ bare alias) |
  | `embeddings` | `POST /v1/embeddings` |
  | `images.generations` | `POST /v1/images/generations` |
  | `images.edits` | `POST /v1/images/edits` |
  | `systemone` | `POST /v1/systemone` |
  | `videos` | `POST /v1/videos` (create) |
  | `videos.poll` | `GET /v1/videos/{id}` |
  | `videos.list` | `GET /v1/videos` |
  | `videos.content` | `GET /v1/videos/{id}/content` |
  | `videos.delete` | `DELETE /v1/videos/{id}` |
  | `other` | folded fallback |

* **`model` is client-influenced** but is already on the legacy label set; it is
  truncated to 128 bytes (`maxLabelValue`) rather than enumerated. Do not add
  model names to any *other* label.
* **Never put these in a label:** upstream error text, prompt/response bodies,
  API keys, key prefixes, org ids, job ids, request ids, URLs. All are
  unbounded, client- or provider-controlled, or sensitive.
* Provider and model values are truncated to 128 bytes. Adding a new
  `token_type`, `cache_status` or `reason` value is a code change here — that is
  deliberate, so all four stay enumerable.

## Per-endpoint requests + latency for the new surfaces

`IncRequests(provider, model, endpoint, status)` and
`ObserveLatency(provider, endpoint, d)` already express everything needed for
the image/video/systemone endpoints — no new instrument was required. Use the
`Endpoint*` constants from this package rather than string literals so the
closed set stays honest:

```go
start := time.Now()
status := h.writeSomething(w, r)
h.Metrics.IncRequests(p.ID, sentModel, otel.EndpointVideosPoll, status)
h.Metrics.ObserveLatency(p.ID, otel.EndpointVideosPoll, time.Since(start))
```

Useful queries:

```promql
# Request rate per video route
sum by (endpoint) (rate(gateway_requests_total{endpoint=~"videos.*"}[5m]))

# p95 latency per endpoint across providers
histogram_quantile(0.95, sum by (le, endpoint) (rate(gateway_latency_ms_bucket[5m])))

# Error ratio on video content downloads
sum(rate(gateway_requests_total{endpoint="videos.content",status="5xx"}[5m]))
  / sum(rate(gateway_requests_total{endpoint="videos.content"}[5m]))
```

Notes specific to the video routes:

* Create (`videos`) additionally writes a `request_logs` row, because it is the
  billable event.
* Poll / list / content / delete are metric-only by design: a poll loop would
  otherwise flood `request_logs` with rows that carry no tokens and no cost.
  Use `gateway_requests_total{endpoint="videos.poll"}` for poll traffic and the
  text log (`middleware.Logger`) for per-request detail.
* `videos.list` and `videos.delete` have no provider (they are served from the
  gateway's own `video_jobs` table), so `provider` is empty for them. Empty is
  a legitimate label value; filter with `provider=""` rather than `provider=~".+"`.
