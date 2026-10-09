# Portkey — Analytics, Observability, Logs, Cost/Token Tracking & Guardrails Analytics (2025–2026)

**Scope/branding note:** Portkey's docs now live at `docs.portkey.ai` under the **Prisma AIRS AI Gateway** brand (Palo Alto Networks). Plan tiers are named inconsistently across pages: `Developer`/`Production` (observability pages) vs `Dev`/`Pro`/`Enterprise` (feature-comparison table). Both are reported as written.
**Primary sources:** live pages at `docs.portkey.ai` + the public OpenAPI spec at `https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml` (40,535 lines, downloaded and grepped locally).

---

## 1. Dashboard charts/breakdowns, metric names, dimensions

### 1.1 Dashboard tabs

| Tab | Documented content | Source |
|---|---|---|
| Overview | "70,000ft view": cost, tokens used, **mean latency**, requests, users, top models | [analytics](https://docs.portkey.ai/docs/product/observability/analytics) |
| Users | Derived from the `user` param in OpenAI SDK requests or the `_user` metadata key. Explicitly **end-user** analytics, *not* internal team usage | [analytics](https://docs.portkey.ai/docs/product/observability/analytics) |
| Errors | Auto-captured API **and Accuracy** errors; error rates; count of requests **rescued** by gateway strategies | [analytics](https://docs.portkey.ai/docs/product/observability/analytics) |
| Cache | Latency improvement and cost savings from cache | [analytics](https://docs.portkey.ai/docs/product/observability/analytics) |
| Feedback | Feedback collected via logs dashboard or API | [analytics](https://docs.portkey.ai/docs/product/observability/analytics) |
| **Summary** | Group by "any dimension — AI Service, model, metadata key, and more"; per-value aggregates: **total requests, total cost, average latency, success rate, average tokens, last seen** | [analytics](https://docs.portkey.ai/docs/product/observability/analytics) |
| **Guardrail analytics** | Exists per changelog only — see §5.5 | [changelog/frontend](https://docs.portkey.ai/docs/changelog/frontend) |

Metric-count claims: observability overview says "**21+ key metrics**" ([observability](https://docs.portkey.ai/docs/product/observability)); the integrations/budget page says "**40+ crucial metrics**" ([budget-limits](https://docs.portkey.ai/docs/product/observability/budget-limits)). **"Executive Reporting"** = Enterprise-only "Executive-only dashboards to track AI adoption, ROI, and strategic impact" ([enterprise-offering](https://docs.portkey.ai/docs/product/enterprise-offering)), matching the Enterprise-only **"FinOps + Executive Dashboard"** row ([feature-comparison](https://docs.portkey.ai/docs/product/product-feature-comparison)).

### 1.2 Time-series graph endpoints — the authoritative metric list

Base URL `https://api.portkey.ai/v1` (self-hosted: `SELF_HOSTED_CONTROL_PLANE_URL`). All `GET`, tag `Analytics > Graphs`, **all share one identical filter parameter set** (§6.3). Tagged **`Analytics [BETA]`** in the docs index ([admin-apis](https://docs.portkey.ai/docs/_llms/latest/admin-apis)).

| Endpoint | Summary fields | Data-point fields |
|---|---|---|
| `/analytics/graphs/requests` | `total` | `timestamp`, `total` |
| `/analytics/graphs/cost` | `avg`, `p50`, `p90`, `p99` | `timestamp`, `avg`, `p50`, `p90`, `p99` |
| `/analytics/graphs/latency` | `avg`, `p50`, `p90`, `p99` | `timestamp`, `avg`, `p50`, `p90`, `p99` |
| `/analytics/graphs/tokens` | `total`, `avg` | `timestamp`, `total`, `avg` |
| `/analytics/graphs/users` | `total` (unique users) | `timestamp`, `total` |
| `/analytics/graphs/users/requests` | `total`, `unique`, `avg` | `timestamp`, `avg` |
| `/analytics/graphs/errors` | `total` | `timestamp`, `total` |
| `/analytics/graphs/errors/rate` | percentage error rate | — |
| `/analytics/graphs/errors/stacks` | `total` | `timestamp`, `stats[]` → `response_status_code`, `count` |
| `/analytics/graphs/errors/status-codes` | `total_errors`, `unique_error_codes` | `status_code`, `count` |
| `/analytics/graphs/requests/rescued` | `retry`, `fallback` | `timestamp`, `retry[]` (`retry_success_count`), `fallback[]` |
| `/analytics/graphs/cache/hit-rate` | `total`, `rate` (%) | `timestamp`, `simple_hits`, `semantic_hits` |
| `/analytics/graphs/cache/latency` | summary object | `timestamp`, `avg` (cache-hit latency ms) |
| `/analytics/graphs/feedbacks` | `total` | `timestamp`, `total` |
| `/analytics/graphs/feedbacks/scores` | `total` | `score`, `total` |
| `/analytics/graphs/feedbacks/weighted` | — | weighted feedback, defined **`(value * score)`** |
| `/analytics/graphs/feedbacks/ai-models` | — | feedback per `ai_model` |
| `/analytics/summary/cache` | `hits`, `avg_latency`, `total_requests`, **`cache_speedup`** (% speedup vs non-cached) | — |

Sources: [openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml); readable pages: [Get requests data](https://docs.portkey.ai/docs/api-reference/admin-api/control-plane/analytics/graphs-time-series-data/get-requests-data), [Get cost data](https://docs.portkey.ai/docs/api-reference/admin-api/control-plane/analytics/graphs-time-series-data/get-cost-data), [Get latency data](https://docs.portkey.ai/docs/api-reference/admin-api/control-plane/analytics/graphs-time-series-data/get-latency-data), [Get tokens data](https://docs.portkey.ai/docs/api-reference/admin-api/control-plane/analytics/graphs-time-series-data/get-tokens-data), [Get cache hit rate data](https://docs.portkey.ai/docs/api-reference/admin-api/control-plane/analytics/graphs-time-series-data/get-cache-hit-rate-data), [Get status code data](https://docs.portkey.ai/docs/api-reference/admin-api/control-plane/analytics/graphs-time-series-data/get-status-code-data).

**Latency metrics: `avg`, `p50`, `p90`, `p99` only — no p95, no TTFT, no percentiles for TTFT** in the analytics API ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)). TTFT exists only as a Prometheus approximation recipe (§8.1). p95 appears only in PromQL *examples*.
**Doc bug:** `/analytics/graphs/cost`'s `summary` field descriptions say "Average **latency** in ms" / "50th percentile **latency**" — the cost endpoint reuses the latency schema descriptions verbatim ([Get cost data](https://docs.portkey.ai/docs/api-reference/admin-api/control-plane/analytics/graphs-time-series-data/get-cost-data)).

### 1.3 Group-by / breakdown dimensions (exact)

| Group-by | Endpoint | Response fields |
|---|---|---|
| Metadata key (path param) | `GET /analytics/groups/metadata/{metadataKey}` | `metadata_value`, `requests`, `cost`, `avg_tokens`, `avg_weighted_feedback`, `requests_with_feedback`, `last_seen` |
| AI model | `GET /analytics/groups/ai-models` | `ai_model`, `requests` |
| Provider | `GET /analytics/groups/provider` | `provider` (schema text: "the virtual key (provider)"), `requests` |
| User (`_user`) | `GET /analytics/groups/users` | `user`, `requests`, `cost` |
| **Workspace** | `GET /analytics/groups/workspaces` | `workspace_slug`, `requests`, `cost`, plus **`is_quota_exceeded`** ("Whether the metric retention quota was exceeded for the requested range") |
| **MCP server** | `GET /analytics/groups/mcp` | `name` (MCP server), `requests` |
| **A2A (agent-to-agent)** | `GET /analytics/groups/a2a` | `name` (agent), `requests` |

Source: [openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml); docs page [Get Metadata Grouped Data](https://docs.portkey.ai/docs/api-reference/admin-api/control-plane/analytics/groups-paginated-data/get-metadata-grouped-data); real sample response body in [track-costs-using-metadata](https://docs.portkey.ai/docs/guides/use-cases/track-costs-using-metadata).

**Verified ABSENT as group-bys:** virtual key, API key, status code, cache status, guardrail/verdict, config, prompt, span. Status code has a dedicated graph but no group endpoint; virtual key / API key / config / prompt exist only as filters ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).

**Platform policy group-by vocabulary** (richer, for Usage/Rate Limit policies): `api_key`, `workspace_id` (group-by only), `virtual_key`, `provider`, `config`, `prompt`, `model`, `metadata.*`; MCP target adds `mcp_server`, `mcp_tool` ([budget-policies](https://docs.portkey.ai/docs/product/enterprise-offering/budget-policies)).

### 1.4 Gateway-state sub-signals in Logs UI + response headers

Logs **Status** column states ([logs](https://docs.portkey.ai/docs/product/observability/logs)):

| Option | Inactive | Active states |
|---|---|---|
| Cache | Cache Disabled | Cache Miss, Cache Refreshed, Cache Hit, Cache Semantic Hit |
| Retry | Retry Not Triggered | Retry Success on {x} Tries, Retry Failed |
| Fallback | Fallback Disabled | Fallback Active |
| Loadbalance | Loadbalancer Disabled | Loadbalancer Active |

Response headers sent on every request ([response-schema](https://docs.portkey.ai/docs/api-reference/inference-api/response-schema)): `x-portkey-trace-id`; `x-portkey-retry-attempt-count` (3 means 4 total calls); `x-portkey-cache-status` (`HIT` | `SEMANTIC HIT` | `MISS` | `SEMANTIC MISS` | `DISABLED` | `REFRESH`); `x-portkey-last-used-option-index` (jsonpath of the nested target index).

---

## 2. Metadata / tags: headers, limits, filtering

| Property | Documented value | Source |
|---|---|---|
| REST header | `x-portkey-metadata` (JSON) | [headers](https://docs.portkey.ai/docs/api-reference/inference-api/headers), [metadata](https://docs.portkey.ai/docs/product/observability/metadata) |
| SDK keys | `metadata` (Py/Node); `createHeaders({metadata})` for OpenAI SDK | [metadata](https://docs.portkey.ai/docs/product/observability/metadata) |
| Number of keys | "You can send **any number** of metadata keys"; "No fixed limit—send as many pairs as needed" | [metadata](https://docs.portkey.ai/docs/product/observability/metadata), [metadata-use-cases](https://docs.portkey.ai/docs/guides/use-cases/metadata-use-cases) |
| Value type/length | All values **strings**, max **128 characters** | [metadata](https://docs.portkey.ai/docs/product/observability/metadata), [metadata-use-cases](https://docs.portkey.ai/docs/guides/use-cases/metadata-use-cases) |
| Special keys | `_user` powers user analytics (auto-copied from a body-level `user` field; explicit `_user` wins). Also documented as special: `_prompt`, `_organisation`, `_environment` | [metadata](https://docs.portkey.ai/docs/product/observability/metadata), [headers](https://docs.portkey.ai/docs/api-reference/inference-api/headers) |
| Precedence (gateway ≥ v1.10.20) | **Workspace > API key > request** (workspace wins) | [enforcing-request-metadata](https://docs.portkey.ai/docs/product/administration/enforcing-request-metadata), [metadata-use-cases](https://docs.portkey.ai/docs/guides/use-cases/metadata-use-cases) |
| Precedence (< v1.10.20) | Reversed (request highest) | [enforcing-request-metadata](https://docs.portkey.ai/docs/product/administration/enforcing-request-metadata) |
| W3C baggage | `baggage: userId=alice,environment=production` merged into metadata; existing `x-portkey-metadata` takes precedence; values URL-decoded | [traces](https://docs.portkey.ai/docs/product/observability/traces) |
| Self-hosted header injection | `HEADERS_TO_METADATA=x-request-id,x-caller-service,x-environment` (Enterprise self-hosting, gateway 2.5.0+) | [metadata](https://docs.portkey.ai/docs/product/observability/metadata), [metadata-use-cases](https://docs.portkey.ai/docs/guides/use-cases/metadata-use-cases) |
| Masking | `x-portkey-sensitive-headers` masks (hashes) named header values in request/response/trace logs (case-insensitive; does **not** forward upstream); org-wide via gateway env `ORGANISATION_HEADERS_TO_MASK` | [headers](https://docs.portkey.ai/docs/api-reference/inference-api/headers) |

**Filtering by metadata:**
- Dashboard: `Meta` is filter #5 — "The metadata properties sent to Portkey" ([filters](https://docs.portkey.ai/docs/product/observability/filters)). Concrete UI gesture: select **`>Meta`** in the search bar, e.g. `_user = customer_12345`, combinable with other filters + time range ([track-costs-using-metadata](https://docs.portkey.ai/docs/guides/use-cases/track-costs-using-metadata)).
- API: shared query param **`metadata`** = "Stringifed json object with key value metadata pairs", e.g. `{"_user":"user_1", "env": "staging"}` — accepted by every analytics graph and group endpoint ([Get Metadata Grouped Data](https://docs.portkey.ai/docs/api-reference/admin-api/control-plane/analytics/groups-paginated-data/get-metadata-grouped-data), [openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).
- Export: `metadata` is both a `GenerationsFilterSchema` filter and a `requested_data` output field ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).
- **Enforcement (Enterprise):** JSON Schema **draft-07** requirements for API-key metadata and workspace metadata, enforced at API-key/workspace create **and update**. Constraints: top-level `"type":"object"` required; `"additionalProperties": true` **must** be present and `true`; every property must be `"type":"string"`. `required`, `default`, `enum` supported. Existing keys/workspaces not retro-affected ([enforcing-request-metadata](https://docs.portkey.ai/docs/product/administration/enforcing-request-metadata)).
- **Caveat:** "Metadata appears in Portkey Logs and dashboards for workspace members. Do not put secrets, full PII, or credentials in metadata" ([metadata-use-cases](https://docs.portkey.ai/docs/guides/use-cases/metadata-use-cases)).

---

## 3. Logs: contents, retention, export, search, API

### 3.1 Log entry contents

UI fields: "timestamp, request type, LLM used, tokens generated, **thinking tokens** and cost"; multimodal logs also show the sent and generated images; clicking opens a side panel with the entire raw request/response. Each log has a unique shareable URL. Config/Prompt IDs are shown separately and deep-link to their playgrounds. `Replay` re-runs a request in a fresh playground — inactive when the endpoint is not `/chat/completions`,`/completions`,`/embeddings`, when the provider is archived, or when the request originates from a prompt template called inside a Config target ([logs](https://docs.portkey.ai/docs/product/observability/logs)).

**Machine-readable `LogObject`** (`GET /v1/logs/{logId}`) ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)):
```
_id, request{url, method, headers, body, portkeyHeaders},
response{status, headers, body, responseTime, lastUsedOptionJsonPath},
organisation_id, created_at, metrics (AnalyticsMetrics),
finalUntransformedRequest, originalResponse, transformedRequest
  (last three only when debug logging enabled / on failure)
```

**`AnalyticsMetrics`** (full property list, verbatim): `id`, `organisation_id`, `organisation_name`, `prompt_id`, `prompt_version_id`, `config_id`, `created_at`, `is_success` (200–299), `ai_org`, `ai_model`, `req_units`, `res_units`, `total_units`, `cost`, `cost_currency` (default `USD`), `request_url`, `request_method`, `response_status_code`, `response_time` (ms), `is_proxy_call`, `cache_status` (`HIT`/`MISS`/`DISABLED`/`SEMANTIC HIT`), `cache_type` (e.g. `semantic`), `stream_mode` (1/0), `retry_success_count`, `trace_id`, `span_id`, `span_name`, `parent_span_id`, `mode` (`single`/`loadbalance`/`fallback`), `virtual_key`, `source`, `runtime`, `runtime_version`, `sdk_version`, `config`, `internal_trace_id`, `last_used_option_index`, `config_version_id`, `prompt_slug`, `workspace_slug`, `log_store_file_path_format`, `metadata.key[]`, `metadata.value[]`, `api_key_id`, plus timings `request_parsing_time`, `pre_processing_time`, `cache_processing_time`, `response_parsing_time`, `gateway_processing_time`, `upstream_response_time` ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).
**There is NO guardrail field in `AnalyticsMetrics`.**

`GET /v1/logs/{logId}` query params: `path_format` (`v1` default | `v2`; `created_at` required for v2), `created_at` (ISO 8601), and **`type`** — enum `[hooks]`, "Log type used to locate the correct storage path. Currently only `hooks` is supported; omit for standard request logs" ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)). This is the only machine-visible hook/guardrail log surface. Changelog confirms "Generation log responses now include **hook check data** (guardrail check results and errors) from the log store" ([changelog/backend v1.25.0](https://docs.portkey.ai/docs/changelog/backend)).

### 3.2 What is NOT stored

- **`DO NOT TRACK`**: set `debug=false` on the client or send `x-portkey-debug: false`. Then "only high-level statistics like **tokens used, cost, and latency** will be recorded, while the actual request and response content will be omitted from the logs" ([logs](https://docs.portkey.ai/docs/product/observability/logs)).
- **Enterprise Request Logging modes** ([configuring-request-logging](https://docs.portkey.ai/docs/product/administration/configuring-request-logging)): **Full Logging** (complete payloads, full response content, all metrics + metadata) vs **Metrics Only (Privacy Mode)** (usage stats — tokens, latency, costs — plus request metadata and error information without sensitive content). Workspace override toggle `Allow respective workspace managers to toggle Request Logging`; precedence workspace > organisation; Full → Metrics Only is **not retroactive**.

### 3.3 Retention & volume — documented inconsistently

| Source | Developer / Dev | Production / Pro | Enterprise |
|---|---|---|---|
| [logs](https://docs.portkey.ai/docs/product/observability/logs) | 10k logs/month, **3-day** retention | 100k logs/month + $9 per extra 100k, **30-day** | Unlimited |
| [traces](https://docs.portkey.ai/docs/product/observability/traces) | same as logs | same as logs | Unlimited |
| [analytics](https://docs.portkey.ai/docs/product/observability/analytics) | **30 days** | **365 days** | Unlimited |
| [feature-comparison](https://docs.portkey.ai/docs/product/product-feature-comparison) | Retention Period = 3 Days | 30 Days | Custom |

→ **Analytics page (30d/365d) contradicts Logs page (3d/30d).** Plausibly analytics retention ≠ log retention, but the pages both label it simply "retention". The `is_quota_exceeded` flag on the workspace group endpoint confirms metric retention is quota-enforced ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).
**Treat `logs.md`/`traces.md` (3 / 30 / unlimited) as the *log*-retention answer and `analytics.md` (30 / 365 / unlimited) as the *analytics*-retention answer**, flagging the conflict ([logs](https://docs.portkey.ai/docs/product/observability/logs), [traces](https://docs.portkey.ai/docs/product/observability/traces), [analytics](https://docs.portkey.ai/docs/product/observability/analytics), [feature-comparison](https://docs.portkey.ai/docs/product/product-feature-comparison)).

Self-hosted retention controls documented are **non-numeric** ([enterprise components](https://docs.portkey.ai/docs/product/enterprise-offering/components), [changelog/enterprise](https://docs.portkey.ai/docs/changelog/enterprise)):
- `LOG_STORE_FILE_PATH_FORMAT` — `v1` (default) flat `30/<organisation-id>/<log-id>.json`; `v2` time-hierarchical `30/<organisation-id>/<workspace-slug>/<year>/<month>/<day>/<hour>/<log-id>.json`. Docs state v2 "makes it easier to manage retention policies and query logs for specific time periods"; changing it **only affects newly written logs** (no migration); **v2 unsupported for air-gapped deployments with `LOG_STORE=control_plane`**. Introduced in Gateway **v1.17.0**.
- `LOG_STORE_OBJECT_LOCK_RETENTION_ENABLED="true"` for S3 log stores (Gateway **v1.11.9**).
- Log-store circuit breaker (BETA) for S3-compatible stores: `LOG_STORE_CONNECTION_FAILURE_THRESHOLD` (default 5), `LOG_STORE_CONNECTION_BACKOFF_MS` (default 60000), `LOG_STORE_CONNECTION_TIMEOUT_MS` (default 15000).
Feature table additionally: **Alerts** = Pro+; **FinOps + Executive Dashboard**, **Configurable Retention Periods**, **Configurable exports to datalakes** = Enterprise only ([feature-comparison](https://docs.portkey.ai/docs/product/product-feature-comparison)).

### 3.4 Logs API — what exists (and what does not)

Public OpenAPI contains **only two log paths** plus the export set ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)):

| Method + path | Purpose |
|---|---|
| `POST /v1/logs` (self-hosted `SELF_HOSTED_GATEWAY_URL/logs`) | **Insert** one log or an array (`CustomLog`) |
| `GET /v1/logs/{logId}` | Fetch one log (`LogObject`); supports `type=hooks` |
| `POST /v1/logs/exports` | Create export job |
| `POST /v1/logs/exports/{id}/start` | Start processing |
| `GET /v1/logs/exports/{id}` | Export status |
| `POST /v1/logs/exports/{id}/cancel` | Cancel in-progress export |
| `GET /v1/logs/exports/{id}/download` | Signed download URL |
| `DELETE /v1/logs/exports/{id}` | Delete export job |
| `GET /v1/logs/exports/field-restrictions?workspace_id=...` | Preflight allowed fields (**docs only — absent from the spec**) |

**No documented `GET /v1/logs` list/search endpoint.** A full grep of the 40k-line spec and every docs index found no log-listing or log-search REST endpoint — programmatic retrieval is via `logs/exports` (async JSONL) or `GET /logs/{id}` ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml), [admin-apis index](https://docs.portkey.ai/docs/_llms/latest/admin-apis)). **Log search/filtering is UI-only.**
`CustomLog` (insert): `request{url*, method, headers, body*}`, `response{status, headers, body*, response_time}`, `metadata{trace_id, span_id, span_name, + arbitrary strings}` (* required). Docs expand: `request` accepts `url, provider, headers, method (defaults to post), body`; `response` accepts `status (defaults 200), headers, body, time, streamingMode (defaults false), response_time`; `metadata` accepts `organization`, `user`, tracing info, any `key:value` ([Insert a Log](https://docs.portkey.ai/docs/api-reference/admin-api/data-plane/logs/insert-a-log), [openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).

### 3.5 Log export

- **Plan:** "only available on the **enterprise** plan". Self-hosted Enterprise requires **Data Service** enabled ([logs-export](https://docs.portkey.ai/docs/product/observability/logs-export)).
- **Format: JSONL only.** "Exported logs are provided in JSONL format (JSON Lines), where each line is a valid JSON object" ([logs-export](https://docs.portkey.ai/docs/product/observability/logs-export)). The deprecated page states exports "can not process exports in any other formats" ([logs-export-deprecated](https://docs.portkey.ai/docs/product/observability/logs-export-deprecated)).
- **Limit: 50,000 logs per export job**; `page_size` "max: 50000, default: 50000" ([logs-export](https://docs.portkey.ai/docs/product/observability/logs-export), [openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).
- **UI-selectable fields:** ID, Trace ID, Created At, Request, Response, AI Provider, AI Model, Request Tokens, Response Tokens, Total Tokens, Cost, Cost Currency, Response Time, Status Code, Config, Prompt Slug, Metadata ([logs-export](https://docs.portkey.ai/docs/product/observability/logs-export)).
- **API `requested_data` enum:** `id, trace_id, created_at, request, response, is_success, ai_org, ai_model, req_units, res_units, total_units, request_url, cost, cost_currency, response_time, response_status_code, mode, config, prompt_slug, metadata` ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)). **No guardrail field.**
- **Job statuses:** `draft`, `in_progress`, `success`, `failed`, `stopped` ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)); UI shows Draft / Success / Failure with "Start Again" retry ([logs-export](https://docs.portkey.ai/docs/product/observability/logs-export)).
- **Field restrictions:** org-level `export_settings.deniedFields`, e.g. `{"export_settings":{"deniedFields":["request","response","metadata"]}}`. Valid ids: `id, trace_id, created_at, request, response, ai_org, ai_model, req_units, res_units, total_units, request_url, cost, cost_currency, response_time, response_status_code, config, prompt_slug, metadata`. Workspaces may set their own only if the org enabled the `export_settings` workspace override. Creating/updating an export that requests a denied field is rejected ([logs-export](https://docs.portkey.ai/docs/product/observability/logs-export)).
- **Scopes:** API key needs `logs.export`; for completion logs the `completion` scope may also be required ([logs-export](https://docs.portkey.ai/docs/product/observability/logs-export)).
- **CSV: NOT documented.** Zero occurrences of "csv" across the docs index files and the 40k-line OpenAPI spec; JSONL only, explicitly. The deprecated page states verbatim: "Portkey only supports data exports in the `JSONL` format, and can not process exports in any other formats at the moment" ([logs-export](https://docs.portkey.ai/docs/product/observability/logs-export), [logs-export-deprecated](https://docs.portkey.ai/docs/product/observability/logs-export-deprecated), [openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).
- **⚠️ Two field-name discrepancies in Portkey's own docs:** the prose page's code samples filter on `filters.created_at.gte/.lte` and request `request_tokens`/`response_tokens`/`total_tokens`/`status_code`/`ai_provider`, while the OpenAPI schema uses `time_of_generation_min`/`time_of_generation_max` and `req_units`/`res_units`/`total_units`/`response_status_code`/`ai_org` ([logs-export](https://docs.portkey.ai/docs/product/observability/logs-export) vs [create-a-log-export](https://docs.portkey.ai/docs/api-reference/admin-api/data-plane/logs/log-exports-beta/create-a-log-export)).
- **Deprecated page documents a broader column set** than the current API enum, incl. `cache_status` (SEMANTIC HIT/HIT/MISS/DISABLED), `cache_type` (SIMPLE/SEMANTIC), `stream_mode` (TRUE/FALSE), `retry_success_count`, `mode` (SINGLE/FALLBACK/LOADBALANCE), `provider`, `runtime`, `runtime_version`, `sdk_version`, `prompt_version_id`, `metadata.key`, `metadata.value` ([logs-export-deprecated](https://docs.portkey.ai/docs/product/observability/logs-export-deprecated)).
- **Export storage backends (self-hosted Data Service):** data-service v1.7.1 added **Azure Blob Storage** as a log-export backend alongside the existing S3 options; v1.4.1 made log exports read logs by the path-format identifier from Gateway v1.17.0; v1.1.1 added log-export filtering and an end limit on export records ([changelog/data-service](https://docs.portkey.ai/docs/changelog/data-service)).

### 3.5b OTLP log **ingest** (inbound) — `/v1/logs/otel`

Portkey also accepts OTLP data *in*. OpenLIT's documented setup points `OTEL_EXPORTER_OTLP_ENDPOINT` at **`https://api.portkey.ai/v1/logs/otel`** with `OTEL_EXPORTER_OTLP_HEADERS: x-portkey-api-key=...`, so external OpenTelemetry instrumentation sends into Portkey ([openlit](https://docs.portkey.ai/docs/integrations/tracing-providers/openlit)). Portkey's own auto-instrumentation (BETA) is `Portkey(api_key=..., instrumentation=True)` with CrewAI and LangGraph support, and the docs note "we're rolling out support for more frameworks and will also be exposing the otel collector endpoint to view the traces and logs" ([auto-instrumentation](https://docs.portkey.ai/docs/product/observability/auto-instrumentation)).

### 3.6 Tracing / sessions

OTel-compliant tracing; requests sharing a trace ID are grouped as **spans**. Tree mirrors OTel: `traceId → parentSpanId → spanId, spanName` ([traces](https://docs.portkey.ai/docs/product/observability/traces)).

| Key (Node) | Python | Required |
|---|---|---|
| `traceId` | `trace_id` | YES |
| `spanId` | `span_id` | NO |
| `spanName` | `span_name` | NO |
| `parentSpanId` | `parent_span_id` | NO |

Headers: `x-portkey-trace-id`, `x-portkey-span-id`, `x-portkey-span-name`, `x-portkey-parent-span-id` ([traces](https://docs.portkey.ai/docs/product/observability/traces), [openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)). **W3C Trace Context**: `traceparent: version-trace_id-parent_id-trace_flags` → trace_id becomes `x-portkey-trace-id`, parent_id becomes `x-portkey-parent-span-id`, a new span_id is generated, `span_name` auto-set to `METHOD /path`; Portkey-specific headers win over `traceparent` ([traces](https://docs.portkey.ai/docs/product/observability/traces)).
Gateway retries/fallbacks are **automatically grouped into one trace** with failed and succeeded attempts as chronological spans; trace-level aggregate cost is exposed; LangChain (`LangchainCallbackHandler`) and LlamaIndex (`LlamaIndexCallbackHandler`) handlers ship ([traces](https://docs.portkey.ai/docs/product/observability/traces)). `trace_id`/`span_id` are first-class analytics **filters** ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).

---

## 4. Cost & token analytics

### 4.1 Cost calculation

- Centralised **pricing JSON**; auto-updates on latest versions; **cached 24 hours** then refreshed from the central control plane; automatic pricing updates need gateway **1.12+** ([cost-management](https://docs.portkey.ai/docs/product/observability/cost-management)).
- Cost = input-token cost + output-token cost at provider rates, tracked real-time per request and aggregated ([cost-management](https://docs.portkey.ai/docs/product/observability/cost-management)).
- Pricing database: **Portkey Models** — "open-source pricing and configuration database for 2,300+ LLMs across 35+ providers" (OpenAPI tag says "2300+ LLMs across 40+ providers") ([portkey-models](https://docs.portkey.ai/docs/product/model-catalog/portkey-models), [openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).
- **Unpriced models:** log shows **0 cents** in COST and the request **does not count toward budget limits** ([cost-management](https://docs.portkey.ai/docs/product/observability/cost-management)).
- **Streaming caveat:** with `stream: true` providers return no usage → tokens and cost are 0. Fix: `stream_options: {include_usage: true}`. Affects all OpenAI-compatible providers incl. Azure OpenAI ([cost-management](https://docs.portkey.ai/docs/product/observability/cost-management)).
- Cadence: SaaS instant; Hybrid automatic every 24h (gateway 1.12+); Air-gapped legacy/configurable ([cost-management](https://docs.portkey.ai/docs/product/observability/cost-management)).

### 4.2 Custom pricing — three mechanisms

| Mechanism | What it does | Source |
|---|---|---|
| **Override Model Details** | Set custom **Input Cost** / **Output Cost per 1M tokens (USD)** for any base model, from Integration → Model Provisioning. "Once you save the changes, Portkey will use these new rates to calculate the cost of all subsequent requests made with this model" | [model-overrides](https://docs.portkey.ai/docs/product/model-catalog/model-overrides) |
| **Adding Custom Models** | Fine-tuned / self-hosted / private models with their own pricing | [cost-management](https://docs.portkey.ai/docs/product/observability/cost-management), [budget-limits](https://docs.portkey.ai/docs/product/observability/budget-limits) |
| **Pricing Adjustments** | Discount/markup **multiplier** per Integration (and optionally per model). `1` unchanged, `0.8` = 20% discount, `1.2` = 20% markup. Gateway **2.7.0+**; per-model **2.22.0+**. Keys mirror Portkey Models pricing keys: `default`, `request_token`, `response_token`, `cache_read_input_token`, `cache_write_input_token`, `reasoning_token`, `request_audio_token`/`response_audio_token`/`cache_read_audio_input_token`, `*_text_token`, `*_image_token`, `prediction_accepted_token`/`prediction_rejected_token`, `image.default`, `additional_units.<key>` (`web_search`, `file_search`, `input_image`, `input_video_standard`). **A model-level adjustment FULLY REPLACES the Integration-level one — keys are not merged**; an omitted key falls back to `default` *within that object*, not to the Integration value | [pricing-adjustments](https://docs.portkey.ai/docs/product/model-catalog/pricing-adjustments) |

API: `PUT https://api.portkey.ai/v2/integrations/{integrationId}` with `{"pricing_adjustments":{"multiplier":{...}}}`; `null` clears. "the adjusted cost flows into logs, analytics, and budget tracking" ([pricing-adjustments](https://docs.portkey.ai/docs/product/model-catalog/pricing-adjustments)).

### 4.3 Budget limits — exact semantics

Configured on **Integrations** (Model Catalog; Virtual Keys deprecated) at the **Workspace Provisioning** step and cascading to all Providers created from that Integration ([budget-limits](https://docs.portkey.ai/docs/product/observability/budget-limits)).

| Aspect | Documented behaviour | Source |
|---|---|---|
| Types | **Cost (USD)** or **token-based** | [budget-limits](https://docs.portkey.ai/docs/product/observability/budget-limits) |
| Minimums | Integration: cost **$1**, tokens **100**. Policies: cost `1`, tokens `100`, requests `1` | [budget-limits](https://docs.portkey.ai/docs/product/observability/budget-limits), [budget-policies](https://docs.portkey.ai/docs/product/enterprise-offering/budget-policies) |
| Hard vs soft | **HARD** — "once reached, will automatically expire the key to prevent further usage and overspending"; requests blocked until reset/manual clear | [budget-limits](https://docs.portkey.ai/docs/product/observability/budget-limits), [enforcing-limits-and-budgets](https://docs.portkey.ai/docs/guides/use-cases/enforcing-limits-and-budgets) |
| HTTP status | **412 Precondition Failed**; "Blocked immediately. No spend is incurred." | [enforcing-limits-and-budgets](https://docs.portkey.ai/docs/guides/use-cases/enforcing-limits-and-budgets), [error-codes](https://docs.portkey.ai/docs/api-reference/inference-api/error-codes) |
| Retroactivity | Applies **only to requests made after the limit is set** | [budget-limits](https://docs.portkey.ai/docs/product/observability/budget-limits) |
| Editability | "Once set, budget limits **cannot be edited** by any organization member" | [budget-limits](https://docs.portkey.ai/docs/product/observability/budget-limits) |
| Reset periods | `No Periodic Reset`, `Reset Weekly`, `Reset Monthly`; policies add `periodic_reset_days` (**1–365**) and `next_usage_reset_at` (ISO 8601). `periodic_reset` and `periodic_reset_days` are **mutually exclusive** | [budget-limits](https://docs.portkey.ai/docs/product/observability/budget-limits), [budget-policies](https://docs.portkey.ai/docs/product/enterprise-offering/budget-policies) |
| Reset timing | Integration/API-key: weekly **Sunday 12 AM UTC**, monthly **1st 12 AM UTC**. Policies: weekly **Monday 12:00 AM UTC**, monthly **1st 12:00 AM UTC**; `periodic_reset_days` resets every N days at **00:00 UTC** | [budget-limits](https://docs.portkey.ai/docs/product/observability/budget-limits), [budget-policies](https://docs.portkey.ai/docs/product/enterprise-offering/budget-policies) |
| Alert thresholds | Cost thresholds in USD, token thresholds in tokens; "The API key continues to function until the full budget limit is reached"; email to configured recipients | [enforce-budget-and-rate-limit](https://docs.portkey.ai/docs/product/administration/enforce-budget-and-rate-limit) |
| ⚠️ Policy alert caveat | For **policies**: `alert_threshold` only **writes an event to audit logs** — "Email and other proactive alerts for threshold crossings are **work in progress**; they are not sent today" | [budget-policies](https://docs.portkey.ai/docs/product/enterprise-offering/budget-policies) |
| Default recipients | Org administrators, org owners, the API key creator/owner; extra addresses addable | [enforce-budget-and-rate-limit](https://docs.portkey.ai/docs/product/administration/enforce-budget-and-rate-limit) |
| Propagation | "Apply to every workspace where this integration is enabled" checkbox | [budget-limits](https://docs.portkey.ai/docs/product/observability/budget-limits) |
| Usage read-back | `UsageLimitsPolicy.value_key_usage_map` → per entity `{current_usage, status: active|exhausted, is_threshold_alerts_sent, is_exhausted_alerts_sent}` | [openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml) |
| Manual per-entity reset | `GET /v1/policies/usage-limits/{policyId}/entities?page_size=50` → `id`, `value_key` (e.g. `metadata._user:username`), `current_usage`; then `PUT /v1/policies/usage-limits/{policyId}/entities/{entityId}/reset` | [budget-policies](https://docs.portkey.ai/docs/product/enterprise-offering/budget-policies) |
| Availability | API-key & workspace budget/rate limits: **Enterprise + select Pro**. Usage/Rate Limit Policies: **Enterprise Self-Hosting only**, gateway ≥ 1.17.0 | [enforce-budget-and-rate-limit](https://docs.portkey.ai/docs/product/administration/enforce-budget-and-rate-limit), [budget-policies](https://docs.portkey.ai/docs/product/enterprise-offering/budget-policies) |

### 4.4 Five independent enforcement levels

"Portkey lets you enforce limits at **five independent levels**. Every request passes through each applicable check before reaching the provider" ([enforcing-limits-and-budgets](https://docs.portkey.ai/docs/guides/use-cases/enforcing-limits-and-budgets)):

1. **API Key** — budget, token, rate limits (min/hour/day/week)
2. **Workspace** — budget, token, rate limits. *"Request-count budgets are not available at the workspace level — cost and token budgets only."*
3. **Integration (Provider)** — budget, token, rate limits; supports per-workspace sub-limits under a shared integration ceiling
4. **Usage Limit Policies** — budget, token, request count; conditions + group_by; resets weekly/monthly/every N days/never
5. **Rate Limit Policies** — rpm/rph/rpd/**rpw** on requests or tokens; conditions + group_by

| Type | Measures | Resets |
|---|---|---|
| Budget | Dollar spend | Weekly / monthly / every N days / never |
| Token | Tokens used | Weekly / monthly / every N days / never |
| Request count | Number of calls | Weekly / monthly / every N days / never *(policies only)* |
| Rate limit | Requests or tokens per window | Automatically — sliding window |

**When hit:** budget/token/request cap → **412**; rate limit → **429**; key past expiry → **401**. "All checks happen before a request reaches the provider. A blocked request costs nothing." ([enforcing-limits-and-budgets](https://docs.portkey.ai/docs/guides/use-cases/enforcing-limits-and-budgets), [error-codes](https://docs.portkey.ai/docs/api-reference/inference-api/error-codes)).

**Policy condition/group-by keys** ([budget-policies](https://docs.portkey.ai/docs/product/enterprise-offering/budget-policies)): LLM — `api_key`, `virtual_key`, `provider`, `config`, `prompt`, `model` (wildcards `@openai/gpt-4o`, `@anthropic/*`), `endpoint_type` (rate limits only: `chatComplete`, `embed`, `imageGenerate`), `metadata.*`; `workspace_id` group-by only. MCP — `api_key`, `workspace_id`, `mcp_server`, `mcp_tool`, `metadata.*` (`api_key` unavailable in OAuth MCP flows). Conditions AND; values within a condition OR; `excludes` supported. `model` matching/bucketing uses the **request** model name, not the provider response name.

### 4.5 Rate limits

- Request-based or token-based; per minute / hour / day on Integrations and API keys, plus **per week (`rpw`)** on policies ([budget-limits](https://docs.portkey.ai/docs/product/observability/budget-limits), [enforcing-limits-and-budgets](https://docs.portkey.ai/docs/guides/use-cases/enforcing-limits-and-budgets)).
- "Setting the limit to **0** disables the provider"; limits apply immediately; "Once set, rate limits **cannot be edited**"; exceeding → requests rejected with a specific error code until reset ([budget-limits](https://docs.portkey.ai/docs/product/observability/budget-limits)).
- Workspace rate limit: "all subsequent requests from that workspace will be rejected until the time interval resets, **regardless of which API key is used**" ([enforce-workspace-budget-limits](https://docs.portkey.ai/docs/product/administration/enforce-workspace-budget-limts-and-rate-limits)).
- Admin API: `portkey.admin.workspaces.update(workspace_id=..., usage_limits=[{type, credit_limit, alert_threshold, periodic_reset|periodic_reset_days|next_usage_reset_at}], rate_limits=[{type, unit: rpm|rph|rpd, value}])` ([enforce-workspace-budget-limits](https://docs.portkey.ai/docs/product/administration/enforce-workspace-budget-limts-and-rate-limits)).

---

## 5. Guardrails analytics

### 5.1 Guardrail checks that ship

**BASIC (deterministic)** ([list-of-guardrail-checks](https://docs.portkey.ai/docs/product/guardrails/list-of-guardrail-checks)):
- *Text/Format (Input+Output):* **Regex Match** (`rule`), **Sentence Count** (`minSentences`,`maxSentences`), **Word Count** (`minWords`,`maxWords`), **Character Count** (`minCharacters`,`maxCharacters`), **Uppercase** (`not`), **Lowercase Detection** (`format`), **Ends With** (`Suffix`)
- *Data/Structure (Output only):* **JSON Schema** (`schema`), **JSON Keys** (`keys`,`operator`), **Valid URLs** (`onlyDNS`), **Contains Code** (`format`), **Not Null** (`not`), **Contains** (`words`,`operator`)
- *Security/Auth (Input only):* **JWT Token Validator**, **Request Parameters Check** (`tools`,`params`) ([request-parameters-check](https://docs.portkey.ai/docs/integrations/guardrails/request-parameters-check))
- *Routing/Control (Input only):* **Model Whitelist** (`Models`,`Inverse`), **Model Rules**, **Allowed Request Types** (`allowedTypes`,`blockedTypes`), **Required Metadata Keys**, **Required Metadata Key-Value Pairs** (`operator` all/any/none; `matchType` exact/contains/containsAll/regex)
- *Media:* **Inline Image URLs** (`providers`,`maxSizeBytes`=20971520,`timeoutMs`=30000,`failOnError`=false), Input only
- *Extensibility:* **Webhook** (Input+Output; `webhookURL`, `headers`), **Log** (Output only; `logURL`)

**PRO (LLM-based)**: **Moderate Content** (`categories`, input only), **Check Language** (`language`, input only), **Detect PII** (`categories`, in+out), **Detect Gibberish** (in+out) ([list-of-guardrail-checks](https://docs.portkey.ai/docs/product/guardrails/list-of-guardrail-checks)). **No native jailbreak or secret-detection check** — prompt injection/jailbreak/secret checks appear only via partners (Azure Shield Prompt, Pillar, Lasca, Acuvity) ([guardrails](https://docs.portkey.ai/docs/product/guardrails)). PRO check IDs are **not documented** (names only).

**Documented check IDs**: `default.regexMatch`, `default.regexReplace`, `default.contains`, `default.endsWith`, `default.webhook`, `default.jsonSchema`, `default.jsonKeys`, `default.sentenceCount`, `default.wordCount`, `default.characterCount`, `default.containsCode`, `default.validUrls`, `default.isAllLowerCase`, `default.alluppercase`, `default.notNull`, `default.requiredMetadataKeys`, `default.requiredMetadataKeyPairs`, `default.requestParametersCheck`, `default.jwt`, `default.modelWhitelist`, `default.allowedRequestTypes` ([create-guardrail](https://docs.portkey.ai/docs/api-reference/admin-api/control-plane/guardrails/create-guardrail), [mcp-gateway/guardrails](https://docs.portkey.ai/docs/product/mcp-gateway/guardrails)).
The public OpenAPI `GuardrailCheck.id` enum additionally lists the full PRO + PARTNER set: `portkey.moderateContent`, `portkey.language`, `portkey.pii`, `portkey.gibberish`; `sydelabs.sydeguard`, `aporia.validateProject`, `pillar.scanPrompt`, `pillar.scanResponse`, `patronus.phi`, `patronus.pii`, `patronus.isConcise`, `patronus.isHelpful`, `patronus.isPolite`, `patronus.noApologies`, `patronus.noGenderBias`, `patronus.noRacialBias`, `patronus.retrievalAnswerRelevance`, `patronus.toxicity`, `patronus.custom`, `mistral.moderateContent`, `bedrock.guard`, `promptfoo.guard`, `promptfoo.pii`, `promptfoo.harm`, `acuvity.scan`, `lasso.classify`, `azure.contentSafety`, `azure.pii`, `panw-prisma-airs.intercept` ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).

**MCP tool-call checks (18, `target:"mcp_tools"`)** — same policy engine as LLM: `default.regexMatch`, `default.regexReplace`, `default.contains`, `default.endsWith`, `default.webhook`, `default.jsonSchema`, `default.jsonKeys`, `default.sentenceCount`, `default.wordCount`, `default.characterCount`, `default.containsCode`, `default.validUrls`, `default.isAllLowerCase`, `default.alluppercase`, `default.notNull`, `default.requiredMetadataKeys`, `default.requiredMetadataKeyPairs`, `default.requestParametersCheck`. LLM-only checks (PII, moderation, language, 3rd-party) are **not** available for MCP ([mcp-gateway/guardrails](https://docs.portkey.ai/docs/product/mcp-gateway/guardrails)).

**Plan-tier gating for guardrails:** "available on all plans" — **Developer = `BASIC`** guardrails; **Production = `BASIC`, `PARTNER`, `PRO`**; **Enterprise = all + custom** ([guardrails](https://docs.portkey.ai/docs/product/guardrails)). The feature table separately confirms: *Deterministic Guardrails* = Dev+; *Partner Guardrails* = Dev+; *Portkey LLM Guardrails with PII/PHI Redaction* = **Pro+ only** ([feature-comparison](https://docs.portkey.ai/docs/product/product-feature-comparison)).

### 5.2 What is recorded per check — `hook_results`

Canonical structure returned for sync (`async=false`) runs ([guardrails](https://docs.portkey.ai/docs/product/guardrails)):

- **Top level:** `"hook_results": { "before_request_hooks": [...], "after_request_hooks": [...] }`
- **Per guardrail:** `verdict` (bool; true only if ALL checks pass), `id` (e.g. `pg-check-cfa540`), `transformed` (bool — e.g. PII redaction), `checks[]`, `feedback` (`value`, `weight`, `metadata.successfulChecks` / `failedChecks` / `erroredChecks` — comma-separated check-id strings), `execution_time` (ms), `async` (bool), `type` (always `"guardrail"`), `created_at`, `deny` (bool)
- **Per check:** `data` (check-specific object), `verdict` (bool), `id` (e.g. `default.sentenceCount`), `execution_time` (ms), `transformed` (bool), `created_at`, `log` (string|null), `error` (string|object); streaming examples also carry `fail_on_error` (bool)

Verbatim sample ([guardrails](https://docs.portkey.ai/docs/product/guardrails)):
```json
"hook_results": { "before_request_hooks": [ {
  "verdict": true, "id": "sentence_and_word_check_guardrail", "transformed": false,
  "checks": [ { "data": { "sentenceCount":1,"minCount":1,"maxCount":99999,"not":false,"verdict":true,
      "explanation":"The sentence count (1) is within the specified range of 1 to 99999.",
      "textExcerpt":"when does the flight from new york to bengaluru land tomorrow, ..." },
    "verdict": true, "id": "default.sentenceCount", "execution_time": 0, "transformed": false,
    "created_at": "2025-05-20T08:00:59.492Z", "log": null } ],
  "feedback": { "value":5,"weight":1, "metadata": { "successfulChecks":"default.sentenceCount, default.wordCount",
      "failedChecks":"", "erroredChecks":"" } },
  "execution_time": 0, "async": false, "type": "guardrail", "created_at": "2025-05-20T08:00:59.492Z", "deny": false } ],
  "after_request_hooks": [ ... ] }
```
Errored-check example (streaming, [guardrails](https://docs.portkey.ai/docs/product/guardrails)): check `"id":"default.regexMatch"`, `"verdict":false`, `data{explanation, regexPattern:"*", not:false, textExcerpt}`, `"error":{"name":"SyntaxError","message":...}`, `"execution_time":0`, `"fail_on_error":false` — note the **guardrail-level `verdict` stayed `true`**.
Check-specific `data` payloads documented: sentence/word/character counts + thresholds + `explanation` + `textExcerpt`; regexMatch `regexPattern`; **Request Parameters Check** failure payload `blockedToolsFound` (each with `type`,`name`,`reasons`), `blockedParamsFound` (`param`,`value`,`reasons`), `explanation`, with reason codes `type_blocked`, `name_blocked`, `type_not_allowed`, `name_not_allowed`, `key_blocked`, `key_not_allowed`, `value_blocked`, `value_not_allowed` ([request-parameters-check](https://docs.portkey.ai/docs/integrations/guardrails/request-parameters-check)).
**No numeric score/confidence:** "A check ONLY returns a boolean (`Yes`/`No`) verdict" ([guardrails](https://docs.portkey.ai/docs/product/guardrails)); the only numeric quality signal is `feedback.value` (−10…10) and `weight` (0…1).

**Streaming output guardrails** are informational only — Portkey does **not** trigger server-side retry/fallback/deny; results arrive as one extra SSE chunk after `[DONE]`, and require `x-portkey-strict-open-ai-compliance: false` ([streaming-output-guardrails](https://docs.portkey.ai/docs/guides/use-cases/streaming-output-guardrails)). **Async guardrails' results appear ONLY in Portkey logs, never in the API response** ([streaming-output-guardrails](https://docs.portkey.ai/docs/guides/use-cases/streaming-output-guardrails)).

### 5.3 Where guardrail data surfaces in Logs UI

The log drawer has a **`Feedback & Guardrails`** tab showing Overview (how many checks **passed/failed**), **Verdict** per check, and **Latency** (round-trip per check), plus feedback `Value`, `Weight`, `Metadata Key & Value`, `successfulChecks`, `failedChecks`, `erroredChecks` ([guardrails](https://docs.portkey.ai/docs/product/guardrails)). PII logs render as `✓ pii - 1 successful` / `No PII` ([guardrail-pii-use-case](https://docs.portkey.ai/docs/guides/use-cases/guardrail-pii-use-case)). Changelog: logs now highlight guardrail checks and make "guardrail-flagged events more prominent" ([changelog/2025/november](https://docs.portkey.ai/docs/changelog/2025/november)). The API surface for hook logs is `GET /v1/logs/{logId}?type=hooks` ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).

### 5.4 Guardrail outcomes → HTTP status codes and feedback analytics

| Status | Meaning | Source |
|---|---|---|
| `446` | Guardrail checks failed (**request denied**) — source: Portkey | [error-codes](https://docs.portkey.ai/docs/api-reference/inference-api/error-codes) |
| `246` | Guardrail checks failed (**request successful**) — source: Portkey | [error-codes](https://docs.portkey.ai/docs/api-reference/inference-api/error-codes) |

Verdict × Deny matrix ([guardrails](https://docs.portkey.ai/docs/product/guardrails), [capabilities](https://docs.portkey.ai/docs/product/guardrails/capabilities)): PASS/FALSE→**200**; PASS/TRUE→**200**; FAIL/FALSE→**246**; FAIL/TRUE→**446**. A newer `soft_deny_200` flag returns HTTP 200 with the error as a chat completion ([changelog/enterprise](https://docs.portkey.ai/docs/changelog/enterprise)).

These codes flow into `/analytics/graphs/errors`, `/analytics/graphs/errors/stacks`, `/analytics/graphs/errors/status-codes` ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).

**Guardrail verdicts auto-write feedback.** `GuardrailActions` (public API) and the UI's "On Success → Send Feedback" / "On Failure → Send Feedback" actions build an **"Evals dataset" of guardrail results** ([guardrails](https://docs.portkey.ai/docs/product/guardrails), [openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)):
```yaml
GuardrailActions:
  deny: false        # default
  async: false       # API default; UI default is TRUE
  on_success: { feedback: { value:  5, weight: 1, metadata: "" } }   # defaults
  on_fail:    { feedback: { value: -5, weight: 1, metadata: "" } }   # defaults
```
Those records appear in the Feedback analytics tab and `/analytics/graphs/feedbacks*` (incl. `weighted` = `(value * score)`), and are the only first-class way guardrail outcomes become a *numeric, filterable* analytics dimension (`weighted_feedback_min`/`weighted_feedback_max`) ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml), [feedback](https://docs.portkey.ai/docs/product/observability/feedback), [analytics](https://docs.portkey.ai/docs/product/observability/analytics)).

### 5.5 Guardrail-specific analytics dashboard — exists per changelog, undocumented

- The Analytics docs describe only Overview, Users, Errors, Cache, Feedback, Summary — **no guardrail tab or metric** ([analytics](https://docs.portkey.ai/docs/product/observability/analytics)).
- **The one product statement:** frontend changelog **v1.10.0 (2026-09-11): "Added Guardrail analytics dashboard"** ([changelog/frontend](https://docs.portkey.ai/docs/changelog/frontend)). **No docs page, endpoint, or metric name describes it.** It is absent from the docs index ([docs index](https://docs.portkey.ai/docs/_llms/latest/docs)), and `/docs/product/observability/guardrails` and `/docs/product/guardrails/analytics` both 404. So violation counts, pass/fail rate, guardrail-triggered counts and per-guardrail-id breakdowns are **not documented**.
- **Data substrate:** ClickHouse — "The AI Gateway writes request metrics and **guardrail execution results**… The Backend reads all data to serve the analytics UI"; storage table row: "**ClickHouse** | AI Gateway (metrics, **guardrail results**, feedback) | Per-request analytics, **guardrail execution outcomes**" ([scm-architecture](https://docs.portkey.ai/docs/self-hosting/hybrid-deployments/scm-architecture)). The Helm chart exposes a **dedicated `ANALYTICS_GENERATION_HOOKS_TABLE`** alongside `ANALYTICS_LOG_TABLE` and `ANALYTICS_FEEDBACK_TABLE` — i.e. hook/guardrail results have their own analytics table ([helm chart README](https://raw.githubusercontent.com/Portkey-AI/helm-chart/main/helm/enterprise/README.md)).
- **Real guardrail metric surface = OTel spans:** "Guardrail checks now emit dedicated OTel spans under the **`portkey.guardrail.*`** namespace, linked to the parent generation trace, with attributes for **phase, verdict, action, deny, async, and per-check events**. Gated behind `EXPERIMENTAL_GEN_AI_OTEL_TRACES_ENABLED`" ([changelog/enterprise v2.12.0](https://docs.portkey.ai/docs/changelog/enterprise)); v2.13.0 adds tenant attributes on "Gen AI and guardrail OTel spans … org ID, workspace, and environment — so spans can be filtered, grouped, and alerted on per-tenant" ([changelog/enterprise](https://docs.portkey.ai/docs/changelog/enterprise)). These spans are **not** documented on the OTel product pages ([opentelemetry](https://docs.portkey.ai/docs/product/observability/opentelemetry), [complete-logs](https://docs.portkey.ai/docs/product/enterprise-offering/otel/complete-logs)).
- **Prometheus:** no guardrail-labelled metric. Guardrails appear only as a *component* of `pre_request_processing_duration_milliseconds` ("**Guardrails Retrieval**: Fetching and applying content safety policies") ([prometheus-metrics](https://docs.portkey.ai/docs/self-hosting/prometheus-metrics)).

**Conclusion:** Portkey has **no documented** guardrail-analytics dashboard, no documented pass/fail-rate or per-guardrail breakdown metric, and **no guardrail filter or group-by dimension**. A "Guardrail analytics dashboard" is claimed in the frontend changelog only. Guardrail outcomes are observable via (a) `hook_results` / `type=hooks` logs + the log drawer's Feedback & Guardrails tab, (b) HTTP status codes 446/246 in error analytics, (c) auto-written feedback values from `on_success`/`on_fail`, and (d) `portkey.guardrail.*` OTel spans (Enterprise, experimental, undocumented in the OTel pages).

### 5.6 Guardrail latency impact

- Per-check `execution_time` (ms) and per-guardrail `execution_time` in `hook_results`; log drawer shows per-check round-trip latency ([guardrails](https://docs.portkey.ai/docs/product/guardrails)).
- Async adds **no** latency (log only); sync "Adds guardrail check latency"; `Sequential=TRUE` "Adds latency as checks run one at a time" ([capabilities](https://docs.portkey.ai/docs/product/guardrails/capabilities), [guardrails](https://docs.portkey.ai/docs/product/guardrails)).
- Timeouts: default guardrail execution timeout **5 s**, plus a `timeout` param for checks that fetch internally ([changelog/enterprise v1.10.8](https://docs.portkey.ai/docs/changelog/enterprise)); webhook `timeout` default **3000 ms**, and on timeout defaults to `verdict: true` ([bring-your-own-guardrails](https://docs.portkey.ai/docs/integrations/guardrails/bring-your-own-guardrails)); "configurable timeouts for Partner & Pro Guardrails" ([list-of-guardrail-checks](https://docs.portkey.ai/docs/product/guardrails/list-of-guardrail-checks)); Akto default 5000 ms ([changelog/enterprise](https://docs.portkey.ai/docs/changelog/enterprise)); "Complex regex patterns may impact processing latency" ([pii-redaction](https://docs.portkey.ai/docs/product/guardrails/pii-redaction)).
- **No aggregate guardrail-latency metric/dashboard is documented** ([prometheus-metrics](https://docs.portkey.ai/docs/self-hosting/prometheus-metrics), [analytics](https://docs.portkey.ai/docs/product/observability/analytics)).

### 5.7 Guardrail actions, hooks and endpoints

**Action types** ([guardrails](https://docs.portkey.ai/docs/product/guardrails)): **Async** (UI default **TRUE**; FALSE = sync on request/response), **Deny** (default **FALSE**), **Sequential** (default FALSE = parallel), **On Success → Send Feedback**, **On Failure → Send Feedback**.
**Hook placement:** input = `input_guardrails` / `before_request_hooks`; output = `output_guardrails` / `after_request_hooks`. Raw JSON fields: `type:"guardrail"`, `id`, `checks[{id,parameters}]`, `deny`, `async`, `sequential`, `on_success.feedback{value,weight}`, `on_fail.feedback` ([creating-raw-guardrails-in-json](https://docs.portkey.ai/docs/product/guardrails/creating-raw-guardrails-in-json)); inline hooks carrying their own `checks` run as-is with `id` used as the log label ([bring-your-own-guardrails](https://docs.portkey.ai/docs/integrations/guardrails/bring-your-own-guardrails)). API `actions` verbs documented: `onFail: block|log`, `on_fail: deny`, `on_success: continue`, plus `message` ([create-guardrail](https://docs.portkey.ai/docs/api-reference/admin-api/control-plane/guardrails/create-guardrail)).
**MCP:** `run_on` = `["input"]`/`["output"]`/both ([mcp-gateway/guardrails](https://docs.portkey.ai/docs/product/mcp-gateway/guardrails)) — backend changelog instead says `beforeToolCall`/`afterToolCall` ([changelog/backend](https://docs.portkey.ai/docs/changelog/backend)). Blocked MCP call → JSON-RPC error code **`-32446`** with `data.guardrail_id`, `data.reason` ([mcp-gateway/guardrails](https://docs.portkey.ai/docs/product/mcp-gateway/guardrails)).
**Batches:** guardrails run async with states `validating` → `in_progress` → `processing_post_request_guardrails` → `completed`; initial `hook_results` returned on create ([guardrails-for-batches](https://docs.portkey.ai/docs/product/guardrails/guardrails-for-batches)). **Embeddings: input guardrails only** ([embedding-guardrails](https://docs.portkey.ai/docs/product/guardrails/embedding-guardrails), [capabilities](https://docs.portkey.ai/docs/product/guardrails/capabilities)).

**CRUD endpoints:** `POST /v1/guardrails`; `GET /v1/guardrails` (query `workspace_id`, `organisation_id`, `page_size` ≤1000 default 100, `current_page`; MCP doc adds `target`, `search`, `id`); `GET /v1/guardrails/{guardrailId}`; `PUT /v1/guardrails/{guardrailId}`; `DELETE /v1/guardrails/{guardrailId}` — a guardrail cannot be deleted while used in workspace/org defaults including `mcp_input_guardrails`/`mcp_output_guardrails` ([create-guardrail](https://docs.portkey.ai/docs/api-reference/admin-api/control-plane/guardrails/create-guardrail), [list-guardrails](https://docs.portkey.ai/docs/api-reference/admin-api/control-plane/guardrails/list-guardrails), [openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).
**MCP mappings:** `PUT /v1/guardrails/{guardrailId}/mcp-servers` (declarative bulk sync; returns `{changed,added,updated,removed}`), `PUT /v1/guardrails/{guardrailId}/mcp-servers/{mcpServerId}` (returns `map_id`), `GET /v1/guardrails/{guardrailId}/mcp-servers` ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml), [mcp-gateway/guardrails](https://docs.portkey.ai/docs/product/mcp-gateway/guardrails)).
**Org governance:** `PUT|GET /v1/workspace-exclusions/input-guardrails` and `/output-guardrails` (+`override_existing`) ([enforce-org-level-guardrails](https://docs.portkey.ai/docs/product/administration/enforce-orgnization-level-guardrails), [changelog/backend v1.22.0](https://docs.portkey.ai/docs/changelog/backend)); `GET|PUT /v1/admin/organisation/defaults` for default `input_guardrails`/`output_guardrails` ([changelog/backend](https://docs.portkey.ai/docs/changelog/backend)).
**Scopes:** `guardrails.create`, `guardrails.invoke`, `organisation_guardrails.create/read/list/update/delete`, `organisation_exclusions.update/list` ([changelog/backend](https://docs.portkey.ai/docs/changelog/backend)).
**No guardrail-analytics API endpoint exists.**

---

## 6. Time range, filter operators, grouping, saved views, comparison

### 6.1 Dashboard filters (15 dimensions, verbatim)

1. **Model Used** (AI provider + model) · 2. **Cost** (in **cents**) · 3. **Tokens** (total) · 4. **Status** (API status from the provider) · 5. **Meta** · 6. **Avg Weighted Feedback** · 7. **Provider** · 8. **Config** (Config ID) · 9. **Trace ID** · 10. **Time Range** · 11. **API Key** · 12. **Prompt ID** · 13. **Cache Status** · 14. **Workspace** · 15. **Saved Filters** ([filters](https://docs.portkey.ai/docs/product/observability/filters)). Caveat: "Depending on your role in the organization, you may have access to different filters." **Guardrail is not a filter dimension.**

### 6.2 Saved filters

"Save any set of filters directly from the search bar on the Logs or Analytics pages… **Saved filters are accessible to all organization members**, who can also edit, rename, or delete them as needed. Share saved filters with teammates" ([filters](https://docs.portkey.ai/docs/product/observability/filters)).

### 6.3 API filter parameter set (identical across all analytics graphs + groups)

Per [Get Metadata Grouped Data](https://docs.portkey.ai/docs/api-reference/admin-api/control-plane/analytics/groups-paginated-data/get-metadata-grouped-data) and [openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml). Comma-separated values are OR lists.

| Query param | Semantics |
|---|---|
| `workspace_slug` | **required**. Ignored for a workspace API key; with an org API key and no slug, the **default workspace** is used |
| `time_of_generation_min` / `time_of_generation_max` | **required**; ISO 8601 `YYYY-MM-DDTHH:MM:SS±HH:MM`. The only time control in the API — **arbitrary ranges, no named presets** |
| `total_units_min` / `total_units_max` | total tokens range |
| `cost_min` / `cost_max` | **cents** |
| `prompt_token_min` / `prompt_token_max` | prompt tokens |
| `completion_token_min` / `completion_token_max` | completion tokens |
| `status_code` | comma-separated, e.g. `401,403` |
| `weighted_feedback_min` / `weighted_feedback_max` | range **−10 … 10** |
| `virtual_keys` | comma-separated virtual key slugs |
| `configs` | comma-separated config slugs |
| `api_key_ids` | comma-separated API key UUIDs |
| `metadata` | stringified JSON, e.g. `{"_user":"user_1", "env": "staging"}` |
| `ai_org_model` | comma-separated `provider__model` — separator is **double underscore**, e.g. `openai__gpt-3.5-turbo,azure-openai__gpt-35-turbo` |
| `deployment_id` | comma-separated deployment UUIDs |
| `trace_id`, `span_id` | comma-separated |
| `prompt_slug` | comma-separated prompt slugs |
| `current_page`, `page_size` | pagination (groups); export `page_size` max 50000 |
| `order_by`, `order_by_type` | declared in the shared parameter components (list endpoints such as audit logs) |

**Filter operators:** the analytics API is **range/equality only** — min/max bounds, comma-separated OR lists, exact-match JSON metadata. **No documented contains/negation operators** for analytics filtering. The nearest richer operators are Usage/Rate Limit policy `conditions[].excludes` and `value` wildcards `"*"` ([budget-policies](https://docs.portkey.ai/docs/product/enterprise-offering/budget-policies)).
**`cache_status` is NOT an analytics API parameter** — dashboard-only ([filters](https://docs.portkey.ai/docs/product/observability/filters)) and present per-log in `AnalyticsMetrics.cache_status` ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).

### 6.4 Comparison to previous period — not documented

No page or API parameter provides period-over-period comparison. The only comparisons found are hand-written PromQL in the self-hosted Prometheus docs, e.g. `rate(request_count[7d]) - rate(request_count[7d] offset 7d)` and `histogram_quantile(0.95, ...) - histogram_quantile(0.95, ... offset 24h)` ([prometheus-metrics](https://docs.portkey.ai/docs/self-hosting/prometheus-metrics)). A UI "compare to previous period" toggle is **[UNVERIFIED]**.

### 6.5 Named time ranges — not documented

No page lists concrete presets (e.g. "Last 24 hours / 7 days / 30 days"). Logs Export only says "**Time Range**: Select the period for which you want to export logs" ([logs-export](https://docs.portkey.ai/docs/product/observability/logs-export)); Filters only says "**Time Range**: The date and time range for the analytics & logs" ([filters](https://docs.portkey.ai/docs/product/observability/filters)). **[UNVERIFIED]**.

---

## 7. Exports & API endpoints

**Base URL:** `https://api.portkey.ai/v1` (`servers`: "Portkey API Public Endpoint"). Self-hosted control plane = `SELF_HOSTED_CONTROL_PLANE_URL`; self-hosted data plane/gateway = `SELF_HOSTED_GATEWAY_URL` ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml), [Insert a Log](https://docs.portkey.ai/docs/api-reference/admin-api/data-plane/logs/insert-a-log)). Reference root: [Introduction](https://docs.portkey.ai/docs/api-reference/admin-api/introduction). Spec also at [OpenAPI Specification](https://docs.portkey.ai/docs/api-reference/admin-api/open-api-specification) and `https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml` ([llms.txt](https://portkey.ai/docs/llms.txt)).
**Auth:** header `x-portkey-api-key` (scheme `Portkey-Key`) ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).

### Analytics (complete)
```
GET /analytics/graphs/requests
GET /analytics/graphs/cost
GET /analytics/graphs/latency
GET /analytics/graphs/tokens
GET /analytics/graphs/users
GET /analytics/graphs/users/requests
GET /analytics/graphs/errors
GET /analytics/graphs/errors/rate
GET /analytics/graphs/errors/stacks
GET /analytics/graphs/errors/status-codes
GET /analytics/graphs/requests/rescued
GET /analytics/graphs/cache/hit-rate
GET /analytics/graphs/cache/latency
GET /analytics/graphs/feedbacks
GET /analytics/graphs/feedbacks/scores
GET /analytics/graphs/feedbacks/weighted
GET /analytics/graphs/feedbacks/ai-models
GET /analytics/summary/cache
GET /analytics/groups/users
GET /analytics/groups/ai-models
GET /analytics/groups/provider
GET /analytics/groups/metadata/{metadataKey}
GET /analytics/groups/mcp
GET /analytics/groups/a2a
GET /analytics/groups/workspaces
```
### Logs / export / feedback / policy / audit / pricing / guardrails
```
POST   /logs                                        (insert; data plane)
GET    /logs/{logId}                                (?type=hooks)
POST   /logs/exports
GET    /logs/exports/{exportId}
POST   /logs/exports/{exportId}/start
POST   /logs/exports/{exportId}/cancel
GET    /logs/exports/{exportId}/download
DELETE /logs/exports/{exportId}
GET    /logs/exports/field-restrictions?workspace_id=...   (docs only; absent from spec)
POST   /feedback     {trace_id*, value* (−10..10), weight (0..1, default 1.0), metadata}
PUT    /feedback/{id}
GET|POST         /policies/usage-limits
GET|PUT|DELETE   /policies/usage-limits/{id}
GET              /policies/usage-limits/{id}/entities
PUT              /policies/usage-limits/{id}/entities/{entityId}/reset
GET|POST         /policies/rate-limits
GET|PUT|DELETE   /policies/rate-limits/{id}
GET    /audit-logs
GET    /model-configs/pricing/{provider}/{model}
POST   /guardrails ; GET /guardrails ; GET|PUT|DELETE /guardrails/{guardrailId}
PUT|GET /guardrails/{guardrailId}/mcp-servers ; PUT /guardrails/{guardrailId}/mcp-servers/{mcpServerId}
PUT|GET /workspace-exclusions/input-guardrails , /workspace-exclusions/output-guardrails
GET|PUT /admin/organisation/defaults
```
Source: [openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml) (verified by direct grep of the downloaded spec).

### Feedback semantics
`POST /v1/feedback` with `trace_id`, `value` (**integer −10…10**), `weight` (float 0…1, default 1.0), `metadata` (JSON; `_user`, `_organisation`, `_prompt`, `_environment` indexed by default). Trace ID comes from the **`x-portkey-trace-id` response header**. Feedback shows as "Feedback Count" and "Value: Weight" pairs per trace-id on the logs page and on the Analytics/Prompt Eval tabs ([feedback](https://docs.portkey.ai/docs/product/observability/feedback), [openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)). Manual feedback can be added directly in the Logs UI ([logs](https://docs.portkey.ai/docs/product/observability/logs)).

### Webhooks
**No observability/log-streaming webhook is documented.** `webhook` appears in the public API only as a **guardrail check type** (`default.webhook`, `WebhookParameters{webhookURL, headers, ...}`, "Fail if webhook returns non-200 status or times out") and as MCP authorization hooks ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)). No `logging`/`webhooks` key exists in the request config schema ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).

### CSV
**Not offered / not documented** — see §3.5.

---

## 8. Prometheus / OpenTelemetry

### 8.1 Prometheus — first-class, Enterprise self-hosted gateway only

Endpoint `/metrics` (GET, `text/plain; version=0.0.4; charset=utf-8`, "Authentication: Typically open"), gated by `ENABLE_PROMETHEUS` (default `true`; when `false` the middleware is not registered and `/metrics` **404s**) ([prometheus-metrics](https://docs.portkey.ai/docs/self-hosting/prometheus-metrics)). All custom metrics carry default labels `app` (from `SERVICE_NAME`) and `env` (from `NODE_ENV`).

**Universal label schema:**

| Label | Meaning | Default |
|---|---|---|
| `method` | HTTP verb | on |
| `endpoint` | normalized path (`/v1/chat/completions`) | on |
| `code` | HTTP status code | on |
| `provider` | upstream provider slug | on |
| `model` | model name | **opt-in** — `PROMETHEUS_INCLUDE_MODEL_LABEL=true` |
| `source` | request origination source | on |
| `stream` | `"true"`/`"false"` | on |
| `cacheStatus` | `hit`, `miss`, `disabled`, `error` | on |
| `config_name` | config slug (only when passed via headers; `N/A` for inline config objects) | **opt-in** — `PROMETHEUS_INCLUDE_CONFIG_NAME_LABEL=true` |
| `api_key_name` | API key name | **opt-in** — `PROMETHEUS_INCLUDE_API_KEY_NAME_LABEL=true` |
| `providerSlug` | Model Catalog provider / virtual key that served the request, distinct from `provider` | **opt-in** — add `providerSlug` to `PROMETHEUS_EXTRA_LABELS` |
| `metadata_*` | dynamic labels from `x-portkey-metadata` | **opt-in** — `PROMETHEUS_INCLUDE_METADATA_LABELS=true` + `PROMETHEUS_LABELS_METADATA_ALLOWED_KEYS=user_id,organization_id,...` |

Metadata labels are auto-prefixed `metadata_` (e.g. `metadata_user_id="user_12345"`), keys validated against the allowlist, with explicit warnings on cardinality, PII and storage cost ([prometheus-metrics](https://docs.portkey.ai/docs/self-hosting/prometheus-metrics)).

**15 custom metrics:**

| # | Metric | Type | Unit / buckets |
|---|---|---|---|
| 1 | `request_count` | Counter | requests |
| 2 | `http_request_duration_seconds` | Histogram | s — buckets `0.1,1,1.5,2,3,5,7,10,15,20,30,45,60,90,120,240,500,1000,3000` |
| 3 | `llm_request_duration_milliseconds` | Histogram | ms |
| 4 | `portkey_processing_time_excluding_last_byte_ms` | Histogram | ms |
| 5 | `llm_last_byte_diff_duration_milliseconds` | Histogram | ms |
| 6 | `portkey_request_duration_milliseconds` | Histogram | ms |
| 7 | `llm_cost_sum` | Gauge | USD |
| 8 | `llm_token_sum` | Gauge | tokens |
| 9 | `authentication_duration_milliseconds` | Histogram | ms |
| 10 | `api_key_rate_limit_check_duration_milliseconds` | Histogram | ms |
| 11 | `pre_request_processing_duration_milliseconds` | Histogram | ms |
| 12 | `post_request_processing_duration_milliseconds` | Histogram | ms |
| 13 | `llm_cache_processing_duration_milliseconds` | Histogram | ms |
| 14 | `grpc_req_conversion_duration_milliseconds` | Histogram | ms |
| 15 | `grpc_res_conversion_duration_milliseconds` | Histogram | ms |

Plus Node.js runtime metrics under the `node_` prefix (`node_process_*`, `node_eventloop_lag_seconds`, `node_gc_duration_seconds` with LLM-tuned buckets `[0.001 … 6000]`) ([prometheus-metrics](https://docs.portkey.ai/docs/self-hosting/prometheus-metrics)).

Documented PromQL covers p50/p95/p99 via `histogram_quantile` on `http_request_duration_seconds_bucket` and `llm_request_duration_milliseconds_bucket`, error-rate ratios, cache-hit-rate and cache-cost-benefit formulas, hourly cost burn (`rate(llm_cost_sum[1h])`), cost-per-model, `topk(10, sum(rate(llm_cost_sum[24h])) by (metadata_user_id))`, monthly projections (`increase(llm_cost_sum[7d]) * 4.33`), and alerting rules (5xx > 5% for 5 m; P95 > 10 s; P95 regression > 500 ms vs 24 h ago; daily budget > 80% of `$DAILY_BUDGET_LIMIT`) ([prometheus-metrics](https://docs.portkey.ai/docs/self-hosting/prometheus-metrics)).

**TTFT:** no dedicated metric. Only an approximation: `histogram_quantile(0.95, rate(llm_request_duration_milliseconds_bucket{stream="true"}[5m])) - histogram_quantile(0.95, rate(llm_last_byte_diff_duration_milliseconds_bucket{stream="true"}[5m]))` ([prometheus-metrics](https://docs.portkey.ai/docs/self-hosting/prometheus-metrics)).
**No SaaS/hosted Prometheus endpoint is documented** — `/metrics` is a self-hosted Enterprise Gateway feature.

### 8.2 OpenTelemetry — inbound compliance + two Enterprise egress features

**Inbound:** the suite is "**OpenTelemetry-compliant**" and implements an OTel-shaped span tree; W3C `traceparent`/`baggage` are accepted, letting Portkey join an existing OTel trace ([observability](https://docs.portkey.ai/docs/product/observability), [traces](https://docs.portkey.ai/docs/product/observability/traces)).

**Egress = two separate features, both Enterprise/self-hosted, both configured only via gateway env vars:**

| Feature | Signal | Env vars | Notes |
|---|---|---|---|
| **Analytics Export** (stable) | Aggregated metrics from ClickHouse analytics | `OTEL_PUSH_ENABLED=true`, `OTEL_ENDPOINT` (e.g. `http://localhost:4318`), `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | [analytics](https://docs.portkey.ai/docs/product/enterprise-offering/otel/analytics) |
| **Complete Logs Export** (**EXPERIMENTAL**) | Full prompt/completion logs as OTLP **spans**, OpenTelemetry **GenAI semantic conventions 1.40.0** | `EXPERIMENTAL_GEN_AI_OTEL_TRACES_ENABLED=true`, `EXPERIMENTAL_GEN_AI_OTEL_EXPORTER_OTLP_ENDPOINT`, `EXPERIMENTAL_GEN_AI_OTEL_EXPORTER_OTLP_HEADERS`, `EXPERIMENTAL_GEN_AI_OTEL_RESOURCE_ATTRIBUTES` | **Portkey appends `/v1/traces`**. **HTTP/JSON only — gRPC explicitly out of scope.** No batching, no retry; export happens asynchronously after the response. [complete-logs](https://docs.portkey.ai/docs/product/enterprise-offering/otel/complete-logs) |

Plus `portkey.guardrail.*` spans (Enterprise, behind the same experimental flag; attributes for phase, verdict, action, deny, async, per-check events; tenant attributes added in v2.13.0) — documented **only in the changelog**, not on the OTel product pages ([changelog/enterprise](https://docs.portkey.ai/docs/changelog/enterprise)).
Sample payloads referenced by the docs index: `sample_log_file.json` and `sample_metric_file.json` under `/docs/images/enterprise/private-cloud-deployments/architecture/` ([llms.txt](https://portkey.ai/docs/llms.txt)).

**⚠️ Documented inconsistency:** `analytics.md` describes OTel *logs* resource attributes, the comparison table calls Analytics Export "aggregated metrics", and the changelog (v1.10.19) says it pushes "Clickhouse analytics (traces and spans)". Per the of-record comparison table the signal type is **METRICS** — treat as flagged ([analytics](https://docs.portkey.ai/docs/product/enterprise-offering/otel/analytics)).

### 8.3 Named third-party observability destinations

| Destination | Status | Evidence |
|---|---|---|
| **Any OTLP collector** | **Documented** — analytics (metrics) + complete logs (spans) | [analytics](https://docs.portkey.ai/docs/product/enterprise-offering/otel/analytics), [complete-logs](https://docs.portkey.ai/docs/product/enterprise-offering/otel/complete-logs) |
| **LangSmith** | **Documented as a direct OTLP push target** (`https://api.smith.langchain.com/otel`) *and* separately as app-side instrumentation | [complete-logs](https://docs.portkey.ai/docs/product/enterprise-offering/otel/complete-logs), [langsmith](https://docs.portkey.ai/docs/integrations/tracing-providers/langsmith) |
| **Datadog** | **Only** as a Complete Logs OTLP endpoint (`https://http-intake.logs.datadoghq.com/api/v2/otlp`, header `DD-API-KEY`) + listed analytics-export consumer. **`tracing-providers/datadog` = 404** | [complete-logs](https://docs.portkey.ai/docs/product/enterprise-offering/otel/complete-logs), [analytics](https://docs.portkey.ai/docs/product/enterprise-offering/otel/analytics), [datadog (404)](https://docs.portkey.ai/docs/integrations/tracing-providers/datadog) |
| **Prometheus** | **First-class** — native `/metrics` on the Enterprise Gateway (§8.1) | [prometheus-metrics](https://docs.portkey.ai/docs/self-hosting/prometheus-metrics) |
| **Grafana** | **Only via OTLP/Prometheus** — listed as "Prometheus/Grafana: Build custom dashboards and alerting". **`tracing-providers/grafana` = 404.** (The Grafana *MCP server* page is unrelated — it governs Grafana as an MCP server through Portkey's MCP gateway) | [analytics](https://docs.portkey.ai/docs/product/enterprise-offering/otel/analytics), [grafana (404)](https://docs.portkey.ai/docs/integrations/tracing-providers/grafana), [grafana-mcp-server](https://docs.portkey.ai/docs/integrations/mcp-servers/grafana-mcp-server) |
| **New Relic** | **Nothing documented. `tracing-providers/new-relic` = 404** | [new-relic (404)](https://docs.portkey.ai/docs/integrations/tracing-providers/new-relic) |
| **S3 / object storage** | Documented as a self-hosted **log store** (`LOG_STORE: s3` / `s3_assume` / `s3_custom` / `gcs` / `wasabi` / `netapp` / `azure` / `mongo`) and as an analytics-export consumer — **not** as a SaaS "push my logs to my bucket" button. **`tracing-providers/s3` = 404** | [helm chart README](https://raw.githubusercontent.com/Portkey-AI/helm-chart/main/helm/enterprise/README.md), [components](https://docs.portkey.ai/docs/product/enterprise-offering/components), [s3 (404)](https://docs.portkey.ai/docs/integrations/tracing-providers/s3) |
| **Splunk** | Listed as an analytics-export consumer ("Integrate with existing SIEM infrastructure") — no setup page | [analytics](https://docs.portkey.ai/docs/product/enterprise-offering/otel/analytics) |
| **"Export to Data Lakes"** | Enterprise plan card ("Easily export all your data to your preferred data lake for long-term storage") + Enterprise-only comparison row "Configurable exports to datalakes" — **no mechanism documented** | [enterprise-offering](https://docs.portkey.ai/docs/product/enterprise-offering), [feature-comparison](https://docs.portkey.ai/docs/product/product-feature-comparison) |

**Which "integrations" merely instrument an app that talks to Portkey:** the whole `docs/integrations/tracing-providers/` family — [Arize AX](https://docs.portkey.ai/docs/integrations/tracing-providers/arize), [Arize Phoenix](https://docs.portkey.ai/docs/integrations/tracing-providers/phoenix), [Pydantic Logfire](https://docs.portkey.ai/docs/integrations/tracing-providers/logfire), [FutureAGI](https://docs.portkey.ai/docs/integrations/tracing-providers/future-agi), [Langfuse](https://docs.portkey.ai/docs/integrations/tracing-providers/langfuse), [LangSmith](https://docs.portkey.ai/docs/integrations/tracing-providers/langsmith), [MLflow](https://docs.portkey.ai/docs/integrations/tracing-providers/ml-flow), [OpenLIT](https://docs.portkey.ai/docs/integrations/tracing-providers/openlit), [OpenTelemetry Python SDK](https://docs.portkey.ai/docs/integrations/tracing-providers/opentelemetry-python-sdk), [HoneyHive](https://docs.portkey.ai/docs/integrations/tracing-providers/honeyhive), [Traceloop/OpenLLMetry](https://docs.portkey.ai/docs/integrations/tracing-providers/traceloop). The Langfuse page is the clearest example of the pattern: it uses `from langfuse.openai import OpenAI` with `base_url=PORTKEY_GATEWAY_URL` and Portkey headers, so **both** Langfuse and Portkey log the request — Portkey does not push to Langfuse ([langfuse](https://docs.portkey.ai/docs/integrations/tracing-providers/langfuse)). OpenLIT is the reverse: it sends **to** Portkey at `https://api.portkey.ai/v1/logs/otel` ([openlit](https://docs.portkey.ai/docs/integrations/tracing-providers/openlit)).

**Self-hosted Prometheus also exposes batch metrics** (Data Service v1.2.2): `batch_count`, `batch_cost`, `batch_input_tokens`, `batch_total_tokens`, `batch_process_time`, `batch_success_row_count`, `batch_failure_row_count`, `batch_row_count`, labelled `provider`, `type` ([changelog/data-service](https://docs.portkey.ai/docs/changelog/data-service)).

---

## 9. Self-hosting

| Aspect | Documented | Source |
|---|---|---|
| Deployment models (new customers) | **Portkey-Managed SaaS** (isolated cluster per org) and **Hybrid** (AI Gateway + data plane in your VPC; Portkey-managed control plane). **Fully Airgapped no longer offered** for new deployments; existing ones still supported | [feature-comparison](https://docs.portkey.ai/docs/product/product-feature-comparison), [cost-management](https://docs.portkey.ai/docs/product/observability/cost-management) |
| SCM mode | Deployment inside **Palo Alto Networks Strata Cloud Manager**: AI Gateway + LLM providers in the customer environment; Dashboard, Backend, MySQL, ClickHouse, Redis and Blob Store in the Portkey-managed Management Plane, connected by private link, with the **SCM API Gateway as sole authentication authority**. Storage table: **ClickHouse** holds gateway metrics, **guardrail results**, feedback; **Blob Store** is written by the AI Gateway **asynchronously post-response** and read by the Backend only for log-detail views via a pointer stored in the ClickHouse record | [scm-architecture](https://docs.portkey.ai/docs/self-hosting/hybrid-deployments/scm-architecture) |
| Hybrid components | Control Plane fully managed by Portkey: "Hosts the web dashboard for managing configurations, tracking analytics, and viewing logs… Stores non-sensitive metadata and **aggregated metrics**". Gateway↔Control Plane sync is a **1-minute heartbeat** fetching prompt templates, routing configs, integrations, providers, API keys as deltas; cache entries lazy-loaded with **7-day TTL** reset on repopulation, `volatile-lru` eviction | [architecture](https://docs.portkey.ai/docs/self-hosting/hybrid-deployments/architecture), [cache-behavior](https://docs.portkey.ai/docs/self-hosting/cache-behavior) |
| Hybrid: where logs live | **Option A** — logs in your VPC blob store; the Control Plane **requests individual logs from the Gateway** (inbound HTTPS) for dashboard detail views. **Option B** — logs in Portkey Cloud; the Gateway encrypts and sends logs to Portkey's Log Store, requiring no inbound connection to your environment. Only "non-sensitive operational metrics (model used, token counts, response times)" cross the boundary; "All prompt content and LLM responses remain within your network". TLS 1.3 between planes, envelope encryption, optional BYOK with AWS KMS | [architecture](https://docs.portkey.ai/docs/self-hosting/hybrid-deployments/architecture) |
| Storage backends | **Analytics Store** = **ClickHouse** (or `control_plane`); env `ANALYTICS_STORE`, `ANALYTICS_STORE_ENDPOINT`, `ANALYTICS_STORE_USER`, `ANALYTICS_STORE_PASSWORD`, `ANALYTICS_LOG_TABLE`, `ANALYTICS_FEEDBACK_TABLE`, **`ANALYTICS_GENERATION_HOOKS_TABLE`**. **Log Store** = MongoDB, AWS S3, Wasabi, GCS, NetApp, Azure Blob (env `LOG_STORE` values `s3`, `s3_assume`, `s3_custom`, `gcs`, `wasabi`, `netapp`, `azure`, `mongo`). **Cache Store** = Redis / AWS ElastiCache / Azure Cache for Redis / GCP Memorystore. **Semantic Cache Store** = Milvus or Pinecone. **Data Service** is a separate optional component enabling custom fine-tuning, custom batches and **data exports** | [components](https://docs.portkey.ai/docs/product/enterprise-offering/components), [helm chart README](https://raw.githubusercontent.com/Portkey-AI/helm-chart/main/helm/enterprise/README.md) |
| Hybrid targets | GKE, AWS EKS, AWS ECS, AWS Marketplace, Azure AKS, Azure ACA | [docs index](https://docs.portkey.ai/docs/_llms/latest/docs) |
| Gateway registration | Self-hosted gateway must be registered with the Portkey Control Plane "to enable configuration sync, analytics, and workspace access control" | [gateway-registration](https://docs.portkey.ai/docs/self-hosting/hybrid-deployments/gateway-registration) |
| **OSS gateway** | Available at `github.com/portkey-ai/gateway`, **MIT licensed** (GitHub API: `"license": {"name": "MIT License", "spdx_id": "MIT"}`, 13,075 stars). Feature table row "Open Source" shows **Logs ✗, Traces ✗, Feedback ✗, Custom Metadata ✗, Filters ✗, Alerts ✗, Retention Period ✗, Config Management ✗, LLM Key Management ✗, Simple Caching ✗, Semantic Caching ✗, all guardrails ✗** — the OSS gateway has **no built-in observability UI**. `Portkey-AI/models` (the pricing database) is also **MIT** (139 stars) | [product-feature-comparison](https://docs.portkey.ai/docs/product/product-feature-comparison), [github.com/portkey-ai/gateway](https://api.github.com/repos/portkey-ai/gateway), [github.com/Portkey-AI/models](https://api.github.com/repos/Portkey-AI/models), [open-source](https://docs.portkey.ai/docs/product/open-source) |
| Enterprise components | Closed-source, distributed via a **gated Docker registry**: the Helm chart requires `imageCredentials` with a username/password "shared by portkey", and several core values are likewise "shared by portkey". Licensing is subscription/entitlement based; SCM mode uses a "per-tenant entitlement table with device and quota tracking" | [components](https://docs.portkey.ai/docs/product/enterprise-offering/components), [helm chart README](https://raw.githubusercontent.com/Portkey-AI/helm-chart/main/helm/enterprise/README.md), [scm-architecture](https://docs.portkey.ai/docs/self-hosting/hybrid-deployments/scm-architecture) |
| OpenAPI spec licence | `license: MIT`, linked to `github.com/Portkey-AI/portkey-openapi/blob/master/LICENSE` (the *spec's* licence — distinct from the gateway repo) | [openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml) |
| Logs Export on self-host | Requires **Data Service** enabled | [logs-export](https://docs.portkey.ai/docs/product/observability/logs-export) |
| Usage/Rate Limit Policies | **Enterprise Self-Hosting only**, gateway ≥ 1.17.0 | [budget-policies](https://docs.portkey.ai/docs/product/enterprise-offering/budget-policies) |
| Retention controls | Non-numeric only, documented: `LOG_STORE_FILE_PATH_FORMAT` (v1/v2), `LOG_STORE_OBJECT_LOCK_RETENTION_ENABLED`, plus plan-tier retention | [changelog/data-service](https://docs.portkey.ai/docs/changelog/data-service) |
| FIPS | FIPS 140-3 container images for Enterprise Gateway and Data Service | [fips-compliant-images](https://docs.portkey.ai/docs/self-hosting/fips-compliant-images) |
| Air-gapped pricing | Customer-managed sources; Helm `app-1.5.0+`, Backend `v1.7.0+`, Enterprise Gateway `v2.0.0+`; legacy | [airgapped/model-pricing](https://docs.portkey.ai/docs/self-hosting/airgapped/model-pricing) |
| Compliance | SOC2, ISO27001, GDPR, HIPAA; BAA signing; VPC managed hosting; private tenancy; configurable retention periods; configurable exports to datalakes — all **Enterprise only** | [feature-comparison](https://docs.portkey.ai/docs/product/product-feature-comparison) |
| Onboarding time | "1-2 weeks" typical; complex deployments 3-4 weeks | [feature-comparison](https://docs.portkey.ai/docs/product/product-feature-comparison) |

**What differs when self-hosted for analytics/logs:** hybrid keeps sensitive LLM data in-VPC with only "non-sensitive metadata and aggregated metrics" in the managed control plane ([architecture](https://docs.portkey.ai/docs/self-hosting/hybrid-deployments/architecture)); hybrid pricing JSON auto-updates every 24 h (gateway 1.12+) vs instant on SaaS ([cost-management](https://docs.portkey.ai/docs/product/observability/cost-management)); the self-hosted gateway additionally exposes `/metrics` for Prometheus (§8.1) and is the target for `POST /logs` insertion ([Insert a Log](https://docs.portkey.ai/docs/api-reference/admin-api/data-plane/logs/insert-a-log)); OTLP egress requires the self-hosted gateway env vars (§8.2); Logs Export needs the Data Service ([logs-export](https://docs.portkey.ai/docs/product/observability/logs-export)).

---

## 9b. Plan-tier matrix (observability-relevant rows, from the feature-comparison table)

Headline plan facts: Dev = free forever, **10K requests/month, no overage allowed**; Pro = **$49/month**, 100K requests/month, **$9/month per additional 100K up to 3M**; Enterprise = custom; Open Source = "No Limit" requests ([feature-comparison](https://docs.portkey.ai/docs/product/product-feature-comparison)).

| Capability | Open Source | Dev (Free) | Pro ($49/mo) | Enterprise |
|---|---|---|---|---|
| Requests/month | No Limit | 10K | 100K | Custom |
| **Logs** | ✗ | ✓ | ✓ | ✓ |
| **Traces** | ✗ | ✓ | ✓ | ✓ |
| Feedback | ✗ | ✓ | ✓ | ✓ |
| Custom Metadata | ✗ | ✓ | ✓ | ✓ |
| Filters | ✗ | ✓ | ✓ | ✓ |
| **Alerts** | ✗ | ✗ | **✓** | ✓ |
| FinOps + Executive Dashboard | ✗ | ✗ | ✗ | **✓** |
| **Retention Period** | ✗ | **3 Days** | **30 Days** | **Custom** |
| Configurable Retention Periods | ✗ | ✗ | ✗ | **✓** |
| Configurable exports to datalakes | ✗ | ✗ | ✗ | **✓** |
| Admin APIs (Control + Data Plane) | ✗ | ✗ | ✗ | **✓** |
| Audit Logs | ✗ | ✗ | ✗ | **✓** |
| SSO (Okta) · SCIM · JWT auth · BYOK · VPC Managed Hosting · Private Tenancy · SOC2/ISO27001/GDPR/HIPAA · BAA · Enforce org-level metadata · Enforce org-level guardrails | ✗ | ✗ | ✗ | **✓** |
| Role Based Access Control | ✗ | ✗ | ✓ | ✓ (Advanced) |
| Deterministic Guardrails | ✗ | ✓ | ✓ | ✓ |
| Partner Guardrails | ✗ | ✓ | ✓ | ✓ |
| Portkey LLM Guardrails w/ PII/PHI Redaction | ✗ | ✗ | **✓** | ✓ |
| Simple Caching | ✗ | ✓ (1-day TTL) | ✓ (unlimited TTL) | ✓ |
| Semantic Caching | ✗ | ✗ | ✓ | ✓ |
| LLM Key Management | ✗ | ✓ | ✓ | ✓ **(with Budgeting & Rate Limiting)** |
| Prompt Templates | ✗ | ✓ (up to 3) | ✓ (unlimited) | ✓ |

All rows: [feature-comparison](https://docs.portkey.ai/docs/product/product-feature-comparison).
**Explicit per-feature gates elsewhere:** Logs Export = Enterprise only ([logs-export](https://docs.portkey.ai/docs/product/observability/logs-export)); Configure Request Logging = Enterprise ([configuring-request-logging](https://docs.portkey.ai/docs/product/administration/configuring-request-logging)); OTel Complete Logs = Enterprise **and Self-Hosted**, experimental ([complete-logs](https://docs.portkey.ai/docs/product/enterprise-offering/otel/complete-logs)); Prometheus `/metrics` = Enterprise (self-hosted) Gateway ([prometheus-metrics](https://docs.portkey.ai/docs/self-hosting/prometheus-metrics)); Budget/rate limits on keys & workspaces = Enterprise + select Pro ([enforce-budget-and-rate-limit](https://docs.portkey.ai/docs/product/administration/enforce-budget-and-rate-limit)); Usage/Rate Limit Policies = Enterprise **Self-Hosting** only, gateway ≥ 1.17.0 ([budget-policies](https://docs.portkey.ai/docs/product/enterprise-offering/budget-policies)).

---

## 10. Adjacent observability surfaces

| Surface | Logged / reported | Source |
|---|---|---|
| **MCP Gateway** | Every MCP request auto-logged: Tool, Parameters, Response, User, Team (workspace), Timestamp, Latency, Status, MCP Server. Filters: MCP server / team / user / tool name / time range / status. Aggregate metrics: requests over time, by server, by team, by user, error rates, **latency percentiles**, most used tools. Auto-injected metadata: `organisation_id`, `organisation_name`, `workspace_id`, `workspace_name`, `workspace_slug`, `user_id`, `api_key_id`, `mcp_server_id`, `mcp.auth.type`. "Traces span LLM calls and tool use" | [mcp-gateway/observability](https://docs.portkey.ai/docs/product/mcp-gateway/observability) |
| **Prompt Observability** | Analytics filterable by `prompt-id`; per-log fields: timestamp, model, request path, user, tokens, cost, status; per-template "Recent" history. **`prompts.render` calls are NOT logged — only `prompts.completions`** | [prompt-observability](https://docs.portkey.ai/docs/product/prompt-engineering-studio/prompt-observability) |
| **Audit Logs** | Enterprise; `GET /audit-logs`; also the sink for policy `alert_threshold` crossings | [audit-logs](https://docs.portkey.ai/docs/product/enterprise-offering/audit-logs), [budget-policies](https://docs.portkey.ai/docs/product/enterprise-offering/budget-policies) |
| **Workspace access control** | Separate permissions for logs access and analytics access per workspace | [configure-logs-access](https://docs.portkey.ai/docs/product/administration/configure-logs-access-permissions-in-workspace), [configure-analytics-access](https://docs.portkey.ai/docs/product/administration/configure-analytics-access-permissions) |
| **Auto-instrumentation [BETA]** | Instrument tracing/logging for multiple LLM/agent frameworks into one place | [auto-instrumentation](https://docs.portkey.ai/docs/product/observability/auto-instrumentation) |
| **Coding Agents** | Governance layer for Claude Code, Codex etc. with "cost controls, observability" | [coding-agent](https://docs.portkey.ai/docs/product/coding-agent) |

---

## Unverified / could not confirm

1. **Named time-range presets** ("Last 24h / 7d / 30d / custom") — only "Time Range" is named; no preset list published ([filters](https://docs.portkey.ai/docs/product/observability/filters), [logs-export](https://docs.portkey.ai/docs/product/observability/logs-export)).
2. **Comparison to previous period** in the UI or API — no documentation; only hand-written PromQL `offset` comparisons ([prometheus-metrics](https://docs.portkey.ai/docs/self-hosting/prometheus-metrics)).
3. **CSV export** — zero "csv" occurrences in the docs indexes and the 40k-line spec; only JSONL, and the deprecated page explicitly says other formats cannot be processed ([logs-export-deprecated](https://docs.portkey.ai/docs/product/observability/logs-export-deprecated)).
4. **p95** in the analytics API/dashboard — only `avg`/`p50`/`p90`/`p99`; p95 appears only in PromQL examples ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml), [prometheus-metrics](https://docs.portkey.ai/docs/self-hosting/prometheus-metrics)).
5. **TTFT as a first-class metric** — absent from the analytics API/dashboard; only a Prometheus approximation recipe ([prometheus-metrics](https://docs.portkey.ai/docs/self-hosting/prometheus-metrics)).
6. **`GET /v1/logs/exports/field-restrictions`** — documented with a cURL example but **absent from the published OpenAPI spec** ([logs-export](https://docs.portkey.ai/docs/product/observability/logs-export) vs [openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).
7. **`GET /v1/logs` list/search endpoint** — not found in the spec or any docs index; log search is UI-only or via exports. An undocumented/internal endpoint cannot be ruled out.
8. **Contradictory retention numbers** — Analytics 30 d/365 d vs Logs 3 d/30 d vs feature table 3 d/30 d/Custom; the two senses of "retention" are never reconciled ([analytics](https://docs.portkey.ai/docs/product/observability/analytics), [logs](https://docs.portkey.ai/docs/product/observability/logs), [feature-comparison](https://docs.portkey.ai/docs/product/product-feature-comparison)).
9. **Numeric log-retention env vars** for self-hosting — none published; only `LOG_STORE_FILE_PATH_FORMAT` and `LOG_STORE_OBJECT_LOCK_RETENTION_ENABLED` ([changelog/data-service](https://docs.portkey.ai/docs/changelog/data-service)).
10. **Plan-tier naming** — `Developer`/`Production`/`Enterprise` vs `Dev (Free Forever)`/`Pro ($49/Month)`/`Enterprise (Custom)`; whether "Production" ≡ "Pro" is not stated.
11. **Alerts feature** — listed as Pro+ ([feature-comparison](https://docs.portkey.ai/docs/product/product-feature-comparison)) but **no Alerts documentation page exists** in the sitemap or llms indexes; concrete alert rule types (error-rate, latency, cost-spike) are unverified. Only budget/token *alert thresholds* are documented (§4.3).
12. **Guardrail analytics dashboard metrics** — a dashboard is claimed by changelog v1.10.0 but no docs page, endpoint, or metric name exists; violation counts, pass/fail rate, guardrail-triggered counts and per-guardrail-id breakdown are unverified. `/docs/product/observability/guardrails` and `/docs/product/guardrails/analytics` both 404 ([changelog/frontend](https://docs.portkey.ai/docs/changelog/frontend)).
13. **Guardrail results as a log/analytics filter or group-by** — not in any documented filter list or API schema ([filters](https://docs.portkey.ai/docs/product/observability/filters), [openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).
14. **Field name of guardrail data inside the stored log document** — only "hook check data … from the log store" is stated ([changelog/backend](https://docs.portkey.ai/docs/changelog/backend)); there is **no top-level `guardrails` field** in the documented log JSON or the export field-id list ([logs-export](https://docs.portkey.ai/docs/product/observability/logs-export)).
15. **PRO guardrail check IDs** (Detect PII / Moderate Content / Check Language / Detect Gibberish) — names only in the check tables; IDs exist in the OpenAPI enum but are not mapped to names.
16. **Per-check numeric score/confidence** — undocumented; checks return boolean verdicts only, plus `feedback.value/weight` ([guardrails](https://docs.portkey.ai/docs/product/guardrails)).
17. **OTel `portkey.guardrail.*` span attribute names** beyond "phase, verdict, action, deny, async, per-check events" — not in the OTel product docs ([changelog/enterprise](https://docs.portkey.ai/docs/changelog/enterprise), [complete-logs](https://docs.portkey.ai/docs/product/enterprise-offering/otel/complete-logs)).
18. **Native jailbreak / secret-detection guardrail checks** — unverified as native; these appear only via partners (Azure Shield Prompt, Pillar, Lasca, Acuvity) ([guardrails](https://docs.portkey.ai/docs/product/guardrails)).
19. **`weighted_feedback` formula** — the OpenAPI summary says `(value * score)` while the Feedback page defines `weight` as a 0–1 float; exact formula unverified ([openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml), [feedback](https://docs.portkey.ai/docs/product/observability/feedback)).
20. **Datadog / New Relic / Grafana / S3 first-party exports** — the tracing-provider pages for these return 404; Datadog is reachable only via the generic OTLP endpoint `https://http-intake.logs.datadoghq.com/api/v2/otlp` with `DD-API-KEY`. Any first-party app/integration beyond OTLP is unverified.
21. **Analytics Export signal type** — docs describe OTel *logs* resource attributes, the comparison table says *aggregated metrics*, and changelog v1.10.19 says "Clickhouse analytics (traces and spans)"; treat as **metrics** per the of-record table but flag the conflict ([analytics](https://docs.portkey.ai/docs/product/enterprise-offering/otel/analytics)).
22. **`x-portkey-metadata` 128-char limit enforcement** — the limit is stated but error behaviour on violation is not documented ([metadata](https://docs.portkey.ai/docs/product/observability/metadata)).
23. **UI-only group-by dimensions** — whether the Summary tab can group by Cache Status or a guardrail dimension is unstated ("any dimension — AI Service, model, metadata key, and more") ([analytics](https://docs.portkey.ai/docs/product/observability/analytics)).
24. **MCP guardrail `run_on` accepted values** — docs say `input`/`output`, backend changelog says `beforeToolCall`/`afterToolCall` ([mcp-gateway/guardrails](https://docs.portkey.ai/docs/product/mcp-gateway/guardrails), [changelog/backend](https://docs.portkey.ai/docs/changelog/backend)).
25. **Doc inconsistency, embeddings** — the embedding-guardrails flow diagram shows "Success Status: 240" and "Fail + Deny=false → 446", contradicting 200/246/446 elsewhere, and advertises "PHI Detection" linking to a PRO table with no PHI check ([embedding-guardrails](https://docs.portkey.ai/docs/product/guardrails/embedding-guardrails) vs [guardrails](https://docs.portkey.ai/docs/product/guardrails)).
26. **Docs-index anomaly** — the published admin-apis index omits the Logs Export endpoints even though they exist in the OpenAPI spec and in product pages ([admin-apis](https://docs.portkey.ai/docs/_llms/latest/admin-apis), [openapi.yaml](https://raw.githubusercontent.com/Portkey-AI/openapi/refs/heads/master/openapi.yaml)).
27. **Analytics Export batch size / push cadence** — no interval, flush window, or batch size documented ([analytics](https://docs.portkey.ai/docs/product/enterprise-offering/otel/analytics), [otel](https://docs.portkey.ai/docs/product/enterprise-offering/otel/otel), [changelog/enterprise](https://docs.portkey.ai/docs/changelog/enterprise)).
28. **Default OTLP protocol for Analytics Export** — `http/protobuf` is documented only as an *opt-in* via `OTEL_EXPORTER_OTLP_PROTOCOL`; the default transport is never stated ([analytics](https://docs.portkey.ai/docs/product/enterprise-offering/otel/analytics), [changelog/enterprise](https://docs.portkey.ai/docs/changelog/enterprise)).
29. **Whether the Analytics Export OTLP path appends a suffix** (`/v1/metrics`, `/v1/logs`) — only the Complete Logs path's `/v1/traces` append is documented ([complete-logs](https://docs.portkey.ai/docs/product/enterprise-offering/otel/complete-logs), [analytics](https://docs.portkey.ai/docs/product/enterprise-offering/otel/analytics)).
30. **Exact plan tier for Analytics Export** — the page says "enterprise customers" but carries no explicit tier badge ([analytics](https://docs.portkey.ai/docs/product/enterprise-offering/otel/analytics)).
31. **Field-name discrepancies in Logs Export** — the prose page uses `request_tokens`/`response_tokens`/`total_tokens`/`status_code`/`ai_provider`/`filters.created_at.gte`, the OpenAPI schema uses `req_units`/`res_units`/`total_units`/`response_status_code`/`ai_org`/`time_of_generation_min`; unresolved in the docs ([logs-export](https://docs.portkey.ai/docs/product/observability/logs-export), [create-a-log-export](https://docs.portkey.ai/docs/api-reference/admin-api/data-plane/logs/log-exports-beta/create-a-log-export)).
32. **`portkey-ai/rubeus`** — linked from the open-source page as the "Contribute here" target, but the GitHub API returned **no repository metadata**; existence and licence unconfirmed ([open-source](https://docs.portkey.ai/docs/product/open-source), [github API](https://api.github.com/repos/portkey-ai/rubeus)).
33. **MCP gateway OTel log export configuration** — announced in the changelog, but the MCP observability page documents no OTel config or env var, and whether it is gated behind `EXPERIMENTAL_GEN_AI_OTEL_TRACES_ENABLED` for MCP specifically is unstated ([changelog/enterprise](https://docs.portkey.ai/docs/changelog/enterprise), [mcp-gateway/observability](https://docs.portkey.ai/docs/product/mcp-gateway/observability)).
34. **SaaS "push my logs to my own S3 bucket"** — S3 is confirmed as a self-hosted *log store* backend, but no cloud-plan export-to-S3 feature is documented (`tracing-providers/s3` = 404) ([s3 (404)](https://docs.portkey.ai/docs/integrations/tracing-providers/s3), [helm chart README](https://raw.githubusercontent.com/Portkey-AI/helm-chart/main/helm/enterprise/README.md)).
35. **`Update a Log Export` vs `/cancel`** — the prose table lists a cancel endpoint while the docs search index surfaced an "Update a Log Export" API page; the update endpoint's exact semantics were not verified ([logs-export](https://docs.portkey.ai/docs/product/observability/logs-export)).
36. **Aggregate guardrail-latency metric** — per-check/per-guardrail `execution_time` is recorded, but no aggregate dashboard or Prometheus metric exists ([prometheus-metrics](https://docs.portkey.ai/docs/self-hosting/prometheus-metrics), [analytics](https://docs.portkey.ai/docs/product/observability/analytics)).
