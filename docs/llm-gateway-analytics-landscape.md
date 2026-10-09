# LLM Gateway / Observability Analytics Landscape (2025–2026)

Scope: what best-in-class LLM gateway + observability platforms ship for **analytics, cost tracking, and observability**, to define table stakes for a self-hosted AI gateway.

Method: every claim below is sourced from a fetched documentation page (URL inline). Items that could not be confirmed from a fetched page are explicitly flagged `[UNVERIFIED]`. Docs snapshots retrieved 2026-09; "as of 2025-2026" reflects current published docs.

Two products were additionally verified **against source, not prose**: findings marked **[SRC]** were read directly from OSS repositories (Helicone's `web/` dashboard components and ClickHouse migrations; LiteLLM's proxy route definitions, schemas and `prometheus.py` from a local 1.95.0 install). Portkey and Kong were verified against their **public OpenAPI and plugin specs** because their doc pages are JS-gated. Where docs and code disagree, the disagreement is reported rather than smoothed over.

Legend: ✅ verified · ⚠️ partial / caveated · ❌ not offered / not documented · `[UNVERIFIED]` could not confirm · **[SRC]** verified from source or machine-readable spec

Companion deep-dive reports in this workspace (longer, per-product): `portkey-analytics-observability-report.md`, `litellm-spend-budgets-analytics-prometheus-report.md`, `langfuse-vs-langsmith-observability-report.md`.

**174 unique source URLs across 20 domains.**

---

## 1. Helicone

> **Doc-hygiene note:** `https://docs.helicone.ai/features/caching` **404s**; the live page is `https://docs.helicone.ai/features/advanced-usage/caching` (present in the [sitemap](https://docs.helicone.ai/sitemap.xml)). Also: the cost-tracking page links a `costs/` directory at `github.com/Helicone/helicone/tree/main/costs` that **no longer exists** — the package moved to `packages/cost/`.
>
> **Verification level:** Helicone is the most deeply verifiable product in this report because its dashboard is in the OSS repo. Findings marked **[SRC]** were read directly from `github.com/Helicone/helicone` @ `main` (UI components, ClickHouse migrations, filter definitions) rather than from prose docs.

### 1.1 Dashboard: pages, panels, exact metric labels

**Actual sidebar (authoritative, from `web/components/layout/auth/Sidebar.tsx`)** **[SRC]** [Sidebar.tsx](https://raw.githubusercontent.com/Helicone/helicone/main/web/components/layout/auth/Sidebar.tsx):

| Group | Items (routes) |
|---|---|
| — | **Dashboard** `/dashboard`, **Requests** `/requests` |
| **Segments** | Sessions `/sessions`, Properties `/properties`, Users `/users`, **Cache** `/cache`, **HQL** `/hql` |
| **Improve** | Prompts `/prompts`, Datasets `/datasets`, Playground `/playground` |
| **Monitor** | Rate Limits `/rate-limit`, Alerts `/alerts` |

Other real routes: `settings/reports`, `settings/alerts`, `settings/api-keys`, `settings/webhooks`, `evaluators/[id]`, `properties/[key]`, `users/[id]`, `sessions/[name]/[session_id]`, `datasets/[id]`, `providers`, `vault`, `credits`, `stats`, `wrapped`.
⚠️ **There is no `Reports` route and no `Costs` route.** "Costs" is a *panel plus a `DashboardMode`*, not a page; `settings/reports` is only digest configuration. Any claim of a dedicated Costs tab is unsupported.

**Dashboard panels — exact panel IDs** **[SRC]** [gridLayouts.tsx](https://raw.githubusercontent.com/Helicone/helicone/main/web/components/templates/dashboard/gridLayouts.tsx):

Row-0 metric cards: `cost-req`, `prompt-tokens`, `completion-tokens`, `total-tokens` — labelled exactly **`Avg Cost / Req`**, **`Avg Prompt Tokens / Req`**, **`Avg Completion Tokens / Req`**, **`Avg Total Tokens / Req`**.

| Panel ID | What it plots | Dimensions |
|---|---|---|
| `requests` | requests **and** errors over time (success vs error stacked from HTTP status) | time, status |
| `errors` | "All Errors" breakdown | status code |
| `costs` | cost over time | time |
| `users` | users/requests over time | `user_id`, time |
| `latency` | latency over time | time |
| **`time-to-first-token`** | **TTFT over time** | time |
| **`quantiles`** | **percentile latencies — p50/p75/p95/p99/p99.9 selectable** | time |
| `models` | top models **by requests** | model |
| `models-by-cost` | top models **by cost** | model |
| `top-providers` | provider breakdown | provider |
| `tokens-per-min-over-time` | throughput | time |
| `countries` | **geo breakdown** (`country_code`) | country |
| `scores` / `scores-bool` | numeric + boolean eval score panels | score key |
| `threats` | prompt-injection/security threats | `threat` bool |
| `suggest-more-graphs` | UI affordance | — |

`DashboardMode = "requests" | "costs" | "errors"` renders three distinct dashboard views **[SRC]** (same file).

**Requests page columns** **[SRC]** [initialColumns.tsx](https://raw.githubusercontent.com/Helicone/helicone/main/web/components/templates/requests/initialColumns.tsx): `Created At, Status, Provider, Request, Response, Model, Total Tokens, Prompt Tokens, Completion Tokens, Reasoning Tokens, Latency, TFFT, User, Cost, Feedback, Prompt ID, Country, Prompt Cache Read Tokens, Prompt Cache Write Tokens, Cache Enabled` — and **custom properties render as first-class columns**.

**Properties page** `/properties/[key]` — per-property **total cost, requests, average latency**, plus `propertyTopCosts` / `propertyTopRequests` charts **[SRC]** [PropertyAnalyticsCharts.tsx](https://raw.githubusercontent.com/Helicone/helicone/main/web/components/templates/properties/PropertyAnalyticsCharts.tsx), [segmentation](https://docs.helicone.ai/guides/cookbooks/segmentation).

**Users page** columns: `User ID, Total Cost, Active For, First Active, Last Active, Requests, Avg Reqs / Day, Avg Tokens / Req, Completion Tokens, Prompt Tokens, Rate Limited Count`. Detail view charts: **"Requests count distribution"** and **"Cost distribution"**, with a percentile selector defaulting to **p75** (`p50|p75|p95|p99|p99.9`) **[SRC]** [UserMetrics.tsx](https://raw.githubusercontent.com/Helicone/helicone/main/web/components/templates/users/UserMetrics.tsx).

**Sessions page** columns: `session_name, session_id, created_at, latest_request_created_at, total_cost, prompt_tokens, completion_tokens, total_tokens, total_requests, avg_latency, user_ids`. Detail view = session **tree/path timeline** plus chat rendering. Session metric chart titles **[SRC]** [SessionMetrics.tsx](https://raw.githubusercontent.com/Helicone/helicone/main/web/components/templates/sessions/SessionMetrics.tsx): **`Requests count distribution`**, **`Cost distribution`**, **`Duration distribution`**.

### 1.2 Custom properties

| Item | Verified fact |
|---|---|
| Header | `Helicone-Property-[Name]: <value>`, e.g. `Helicone-Property-Environment: staging` |
| Value rules | **"There are no restrictions on the value"** ([header-directory](https://docs.helicone.ai/helicone-headers/header-directory)) |
| Count limit | **"There are no limits on the number of custom properties you can add."** ([segmentation](https://docs.helicone.ai/guides/cookbooks/segmentation)) |
| Becomes a dimension | Filter, **group metrics/cost by property**, export segmented, **and alert on property values** ([custom-properties](https://docs.helicone.ai/features/advanced-usage/custom-properties)) |
| Case handling | Prefix match is case-insensitive (`helicone-property-`); name taken verbatim after the prefix **[SRC]** [heliconeHeaders.ts](https://raw.githubusercontent.com/Helicone/helicone/main/shared/proxy/heliconeHeaders.ts) |
| Auto-injected keys | `Helicone-Session-Id`, `-Session-Path`, `-Session-Name`, `-Prompt-Id`, `-Sent-To-Posthog` are injected into the property map; `Helicone-User-Id` is documented as a "special reserved property" |
| **Post-hoc update** | **Yes** — `PUT https://api.helicone.ai/v1/request/{requestId}/property` with `{"Environment":"production", …}`; the request id comes from the `Helicone-Id` **response** header |
| API filtering gotcha | **Must** wrap in `request_response_rmt`: `{"filter":{"request_response_rmt":{"properties":{"Environment":{"equals":"production"}}}}}` — omitting the wrapper returns **empty results** |
| SQL storage | `properties Map(LowCardinality(String), String)` on `request_response_rmt`, **bloom-filter indexed on keys and values** **[SRC]** [schema_41](https://raw.githubusercontent.com/Helicone/helicone/main/clickhouse/migrations/schema_41_request_response_replacing_merge_tree.sql) |

Property *discovery* is a materialized view over `arrayJoin(mapKeys(properties))` into an `organization_properties` table, and a `hidden_property_keys` table + dictionary lets orgs **hide keys from the UI** — a nice touch for noisy metadata **[SRC]** [schema_57](https://raw.githubusercontent.com/Helicone/helicone/main/clickhouse/migrations/schema_57_properties_mv.sql), [schema_73](https://raw.githubusercontent.com/Helicone/helicone/main/clickhouse/migrations/schema_73_track_hidden_properties.sql).

### 1.3 User tracking

- Identification via **`Helicone-User-Id`** (emails or UUIDs both valid) ([header-directory](https://docs.helicone.ai/helicone-headers/header-directory)).
- Enrichment via arbitrary `Helicone-Property-*` (docs suggest `UserTier`, `UserType`, `SignupDate`, `Industry`) ([user-metrics](https://docs.helicone.ai/features/advanced-usage/user-metrics)).
- **Typed API: `POST /v1/user/metrics-overview/query`** returns **histograms of `user_cost` and `request_count`**; filter node is `users_view | request_response_rmt`, and `users_view` exposes `user_user_id`, `user_active_for`, `user_first_active`, `user_last_active`, `user_total_requests`, `user_average_requests_per_day_active`, `user_average_tokens_per_request`, `user_total_completion_tokens`, `user_total_prompt_tokens`, `user_cost` ([API ref](https://docs.helicone.ai/rest/user/post-v1usermetrics-overviewquery)).
- Retrieval by user: `POST /v1/request/query` with `{"filter":{"request":{"user_id":{"equals":"…"}}}}`.
- ⚠️ **Cloud Pro gating:** the sidebar upsell lists "**sessions, user analytics, custom properties**" as Pro features **[SRC]** [DesktopSidebar.tsx](https://raw.githubusercontent.com/Helicone/helicone/main/web/components/layout/auth/DesktopSidebar.tsx). The marketing-heavy behavioural content on the user-metrics page (cohorts, A/B testing, activation funnels, auto-segmentation into Power/Casual/New/At-Risk) has **no corresponding panel ID or API field** — treat as aspirational.

### 1.4 Sessions

- Three required headers: **`Helicone-Session-Id`**, **`Helicone-Session-Path`**, **`Helicone-Session-Name`** ([sessions](https://docs.helicone.ai/features/sessions)).
- Paths must start with `/` and use `/`-delimited hierarchy. Philosophy: **conceptual grouping, not chronological** — repeated work of the same kind shares a path, which is what makes the duration-distribution chart color-consistent.
- Groups **all** request types: LLM calls, vector-DB queries, tool calls, "any logged request".
- **API:** `POST /v1/session/metrics/query` returns `session_count`, `session_duration`, `session_cost` histograms plus `average.*`, with `pSize ∈ p50|p75|p95|p99|p99.9` and `useInterquartile`. Also `POST /v1/session/query`, `/v1/session/name/query`, `/v1/session/{id}/feedback`.
- Sessions are materialized in ClickHouse as `sessions_request_response_rmt` with `session_total_cost`, `session_total_tokens`, `session_total_requests`, `session_created_at`, `session_latest_request_created_at`, `session_tag` — filterable/joinable from the request query API.
- ⚠️ Session requests are addressed via the **property map** (`Helicone-Session-Id`), not a dedicated session filter, on `/v1/request/query`.

### 1.5 Caching — exact mechanics, and two real caveats

Headers ([caching](https://docs.helicone.ai/features/advanced-usage/caching), [header-directory](https://docs.helicone.ai/helicone-headers/header-directory)):

| Header | Meaning | Default / limit |
|---|---|---|
| `Helicone-Cache-Enabled: true` | master enable | — |
| `Cache-Control: max-age=N` (or `s-maxage`) | TTL seconds | default **604800** (7 d); **max 31536000** (365 d) |
| `Helicone-Cache-Bucket-Max-Size` | distinct responses stored per request | default `"1"`; **max 20** (enterprise higher); server **errors** above 20 |
| `Helicone-Cache-Seed` | cache namespace (per-user/context) | none |
| `Helicone-Cache-Ignore-Keys` | comma-separated JSON body keys excluded from the cache key | **JSON bodies only** |
| `Helicone-Cache-Save` / `Helicone-Cache-Read` | **present in code, absent from docs** — independently force save-only or read-only | **[SRC]** [cacheSettings.ts](https://raw.githubusercontent.com/Helicone/helicone/main/worker/src/lib/util/cache/cacheSettings.ts) |

- Cache key hashes **seed + request URL + full request body + relevant headers + bucket index**. Exact-match; any parameter change is a new entry.
- **Storage: Cloudflare Workers KV across 300+ edge locations — explicitly "not your infrastructure".** Cloud-only; a self-hoster cannot reuse it. There is a per-org kill switch in code (`orgData.accessDict.cache`).
- Response headers: **`Helicone-Cache: HIT|MISS`**, `Helicone-Cache-Bucket-Idx`; code additionally emits `Helicone-Cache-Latency`.
- A documented composition trick: use `Helicone-Cache-Ignore-Keys: prompt_cache_key` so Helicone's cache works **across** different OpenAI prompt-cache entries — i.e. layer Helicone caching on top of provider prompt caching.

**Cache analytics — yes, two independent paths:**
1. `request_response_rmt` gains **`cache_enabled Bool`** + **`cache_reference_id UUID`**; the API exposes both plus a **derived `cached` boolean** implemented as `cache_reference_id != DEFAULT_UUID`. Requests page has a `Cache Enabled` column and a `Cached Yes/No` filter.
2. A **dedicated `cache_metrics` AggregatingMergeTree**: `cache_hit_count`, **`saved_latency_ms`**, **`saved_completion_tokens`**, **`saved_prompt_tokens`**, `saved_prompt_cache_{read,write}_tokens`, `saved_{prompt,completion}_audio_tokens`, `last_hit`, `first_hit`, `request_body`, `response_body`, keyed `(organization_id, date, hour, request_id)` **[SRC]** [schema_48_cache_metrics.sql](https://raw.githubusercontent.com/Helicone/helicone/main/clickhouse/migrations/schema_48_cache_metrics.sql).

**Cache page KPIs** **[SRC]** [cachePage.tsx](https://raw.githubusercontent.com/Helicone/helicone/main/web/components/templates/cache/cachePage.tsx): **Total Cache Hits**, **Cost Savings ($)**, **Time Saved**, plus cache hit rate (`cacheHits / totalRequests`) and **avg latency vs avg latency cached**.

⚠️ **Caveat 1 — cost on a cache hit is not provably $0.** On a HIT the request **is still logged** as a row, and the logger **forces all token counts to 0** (`prompt_tokens`, `completion_tokens`, cache read/write, audio, `reasoning_tokens`). But `cost` is stored from `costBreakdown.totalCost` **with no cache-hit guard**, while savings are accounted separately by re-running `modelCost()` over the `saved_*` columns. So the safe statement is: *tokens are explicitly zeroed; savings live in a separate `cache_metrics` ledger; the Cache page reports savings separately from total cost.* Whether a hit row contributes $0 to the `cost` column at runtime is **`[UNVERIFIED]`** without a live instance.
⚠️ **Caveat 2 — Cache-page time filtering is day-granular only**: the ClickHouse query compares `date >= 'YYYY-MM-DD'` **[SRC]** [stats.ts](https://raw.githubusercontent.com/Helicone/helicone/main/web/lib/api/cache/stats.ts).

### 1.6 Cost tracking

Two cost systems, explicitly differentiated ([cost-tracking](https://docs.helicone.ai/guides/cookbooks/cost-tracking)):
1. **AI Gateway → "100% accurate"**, via **Model Registry v2** ([helicone.ai/models](https://helicone.ai/models)).
2. **Direct provider/proxy-only → "best effort"**, from the open-source cost repo with "pricing for 300+ models", based on model detection + token counts.

Mechanics ([how-we-calculate-cost](https://docs.helicone.ai/references/how-we-calculate-cost)): OpenAI non-streaming reads the `usage` block; OpenAI streaming needs the stream-usage flag; Anthropic tokens are counted by a Python server.

- **Storage:** `cost` is a `UInt64` column on `request_response_rmt`, stored as an **integer scaled ×1e9** (`COST_PRECISION_MULTIPLIER = 1_000_000_000`). HQL docs instruct dividing by 1e9 for USD — an important gotcha if you query it directly.
- **Pricing model** is per-token and modality-aware: `prompt`, `completion`, `cacheRead`, `cacheWrite`, `cacheMultipliers{cachedInput, write5m, write1h}`, `cacheStoragePerHour`, `thinking`, `request`, plus `image`/`audio`/`video`/`file`/`web_search` sub-pricings and `threshold`-based **pricing tiers** ([model registry schema](https://docs.helicone.ai/rest/models/get-v1public-model-registry-models)). The ×1e9 + modality-aware + tiered design is the most granular cost model alongside Kong's.
- ⚠️ **There is NO `Helicone-Cost-*` header and no per-token cost override header.** The **only** cost-override mechanism is **`Helicone-Model-Override`** ("Overrides the model used to calculate costs and mapping. Useful for when the model does not exist in URL, request or response") — verified in both the header directory and the header parser **[SRC]**. Any claim of Helicone cost-override headers is false. This matters because the user's brief assumed `Helicone-Cost-*` headers exist.
- Per-user cost (`user_cost`), per-property cost (Properties page), per-session cost (`session_total_cost`).
- **Cost alerts:** metric `Cost`, aggregation Sum/Avg/Min/Max/**Percentile**, threshold in dollars, time frame, optional **group-by User/Model/Provider/any custom property**, Min-Requests guard, Requests-page filter system; email + Slack ([alerts](https://docs.helicone.ai/features/alerts)).
- **Cost *limits* (enforcement, not just alerts):** `Helicone-RateLimit-Policy: "[quota];w=[window_s];u=[unit];s=[segment]"` with **`u=cents` for cost-based limiting** and **`s=user` or a custom-property name for per-segment budgets**; window minimum 60 s ([custom-rate-limits](https://docs.helicone.ai/features/advanced-usage/custom-rate-limits)). Response headers `Helicone-RateLimit-Limit/Remaining/Policy`. This is a genuinely elegant design — budget scoping reuses the *same* custom-property namespace as analytics, so a per-tenant budget needs no separate provisioning.

### 1.7 Time range / filters / group-by

**Time ranges (actual UI)** **[SRC]** [themedTimeFilterShadCN.tsx](https://raw.githubusercontent.com/Helicone/helicone/main/web/components/shared/themed/themedTimeFilterShadCN.tsx):
- Presets: **`1h, 3h, 12h, 1d, 3d, 7d, 30d, 90d, 1y`**
- Custom relative: number × `Hour(s)|Day(s)|Week(s)`; custom absolute: calendar date range.
- ⚠️ **Ranges > 31 days are gated behind the `time_filter` Pro feature** (`useProFeature("time_filter")` → upsell dialog).
- **Time bucketing is automatic, not user-chosen** — bucket ∈ `min|hour|day|week|month|year`, derived from range duration **[SRC]** [constants.ts](https://raw.githubusercontent.com/Helicone/helicone/main/web/lib/timeCalculations/constants.ts).

**Filter operators (exact)** **[SRC]** [staticDefinitions.ts](https://raw.githubusercontent.com/Helicone/helicone/main/web/filterAST/filterUIDefinitions/staticDefinitions.ts): text `eq, neq, like, ilike, contains, not-contains`; numeric/timestamp `eq, neq, gt, gte, lt, lte`; boolean `is` (Yes/No); feedback positive/negative; body fields `contains, not-contains, like, ilike`; assets `contains, in`; combinators `and`/`or` with unlimited AST nesting.

**Filterable fields** — three table scopes (same file):
- `request_response_rmt`: Request ID, User ID, Organization ID, Target URL, Request/Response Body, Country Code, Prompt ID, Prompt Version ID, **Provider**, Status Code (200/201/400/401/404/500 presets), Latency (ms), **Cost**, Completion/Prompt Tokens, Prompt Cache Write/Read Tokens, **Reasoning Tokens**, **Time to First Token (ms)**, Threat, **Cache Enabled**, **Cached**, Feedback, AI Gateway, timestamps, Assets
- `sessions_request_response_rmt`: Session ID, Session Name, timestamps, Total Tokens/Requests/Completion/Prompt Tokens, Total Cost, Tags
- `users_view`: User ID, Active For (days), First/Last Active, Total Requests, Avg Requests per Day, Avg Tokens per Request, Total Completion/Prompt Tokens, Total Cost

**Saved filters: yes** — `SaveFilterButton` persists an `OrganizationFilter` per org, with `layoutPage: "dashboard" | "requests"` **[SRC]** [saveFilterButton.tsx](https://raw.githubusercontent.com/Helicone/helicone/main/web/components/templates/dashboard/saveFilterButton.tsx).

⚠️ **Group-by is NOT a general control.** There is no "group by X" selector on the Requests table. Grouping is expressed only as: fixed dashboard panels · the Properties page per-key analytics · **alert Grouping** (User/Model/Provider/any custom property) · or **arbitrary SQL `GROUP BY` via HQL**. If you want flexibility, HQL is the answer — not the UI.

### 1.8 Exports and API

| Surface | Detail | Source |
|---|---|---|
| **Dashboard export** | **XLSX workbook** named `helicone-dashboard-{start}-to-{end}.xlsx` with sheets: **Summary, Requests, Request Status, Costs, Latency, Users, TTFT, Threats, Errors, Tokens, Models, Providers** | **[SRC]** [exportDashboardData.ts](https://raw.githubusercontent.com/Helicone/helicone/main/web/lib/exportDashboardData.ts) |
| Table export | Requests/session/user tables export **CSV or JSONL** | **[SRC]** [exportButton.tsx](https://raw.githubusercontent.com/Helicone/helicone/main/web/components/shared/themed/table/exportButton.tsx) |
| **CLI export** | `npx @helicone/export` — `--start-date`, `--limit`, `--format csv\|json\|jsonl`, `--output`, `--include-body`, `--property k=v`, `--region us\|eu`; **checkpoint/resume, retries, progress+ETA** | [etl](https://docs.helicone.ai/guides/cookbooks/etl) |
| Datasets | JSONL (fine-tuning) and CSV (analysis) | [datasets](https://docs.helicone.ai/features/datasets) |
| REST bulk read | `POST /v1/request/query-clickhouse` — large datasets/bulk export; **`request_response_rmt` wrapper** | [API ref](https://docs.helicone.ai/rest/request/post-v1requestquery-clickhouse) |
| REST point read | `POST /v1/request/query` — small/medium datasets; **`request` wrapper** (the two differ structurally) | [API ref](https://docs.helicone.ai/rest/request/post-v1requestquery) |
| Sessions/users | `POST /v1/session/{query,metrics/query,name/query}`, `POST /v1/user/{query,metrics/query,metrics-overview/query}`, `POST /v1/property/query` | [llms.txt](https://docs.helicone.ai/llms.txt) |
| Alerts API | `POST /v1/alert/create`, `GET /v1/alert/query`, `DELETE /v1/alert` | [alert docs](https://raw.githubusercontent.com/Helicone/helicone/main/docs/rest/alert/post-v1alertcreate.mdx) |
| **HQL (SQL)** | ClickHouse SQL over `request_response_rmt`. `POST /v1/helicone-sql/execute`, `GET /v1/helicone-sql/schema`, `POST /v1/helicone-sql/download` (**signed CSV URL**), saved-query CRUD. Limits: **300,000 rows/query, 30 s timeout, 100 queries/min, 10 CSV downloads/min.** **Gated: "available to selected workspaces"** — request access. Row-level security via ClickHouse row policies + a read-only HQL role. Costs stored ×1e9. | [hql](https://docs.helicone.ai/features/hql) |
| Webhooks | Per-org URL, **sample rate 0–100**, `include_data`, property filters (ALL must match). **HMAC-SHA256** in `helicone-signature`. Payload: `request_id, user_id, request_body, response_body` (+ `request_response_url` S3 signed URL, `model`, `provider`, `metadata{cost,promptTokens,completionTokens,totalTokens,latencyMs}`). Signed URL expires **30 min**; delivery timeout **2 min**; bodies truncated to **10 KB** in the payload. | [webhooks](https://docs.helicone.ai/features/webhooks) |
| PostHog | Set `Helicone-Posthog-Key` + `Helicone-Posthog-Host` → every request emits a `helicone_request` event with model, tokens, latency, cost, request/response data | [posthog](https://docs.helicone.ai/getting-started/integration-method/posthog) |
| Object storage | Request/response bodies in S3-compatible storage; API returns `signed_body_url`. Self-host default MinIO (`S3_ENDPOINT`, bucket `request-response-storage`). `storageLocation ∈ s3 \| clickhouse \| not_stored_exceeded_free`. | [docker](https://docs.helicone.ai/getting-started/self-host/docker) |
| MCP server | `npx @helicone/mcp@latest` + `HELICONE_API_KEY`; tools `query_requests` (filter, offset, limit, sort, includeBodies) and `query_sessions`. | [MCP](https://docs.helicone.ai/integrations/tools/mcp) |
| GraphQL | **Legacy/deprecated** (`heliconeRequest` query exposing `costUSD`, `cacheHits`, `latency`, `properties`) — docs mark it deprecated in favour of the npm export tool | **[SRC]** [export.mdx](https://raw.githubusercontent.com/Helicone/helicone/main/docs/tools/export.mdx) |
| **Not present** | **No S3/object-storage sink for analytics export, no BigQuery/Snowflake connector.** The ETL page states exactly **two** connectors: the CLI tool and the REST API. | [etl](https://docs.helicone.ai/guides/cookbooks/etl) |
| **Scores (eval ingestion)** | `POST /v1/request/{id}/score` — **integer or boolean scores only (floats are rejected)**; also addable from the request detail page; **scores are processed with a 10-minute delay** for analytics aggregation. Stored as `scores Map(LowCardinality(String), Int64)`. | [scores](https://docs.helicone.ai/features/advanced-usage/scores) |

### 1.9 Prometheus / OTEL — precise answer

- ❌ **Helicone exposes no Prometheus scrape endpoint and accepts no OTLP ingestion for LLM telemetry.** No Prometheus/OTLP/OpenTelemetry file, route, or config exists anywhere in the OSS repo tree (**5,894 paths scanned**; the only `prometheus` match is an unrelated blog image path) **[SRC]** [repo tree](https://api.github.com/repos/Helicone/helicone/git/trees/main?recursive=1).
- ⚠️ The **only** Prometheus/Grafana surface is **self-host infrastructure monitoring, not LLM analytics**. The Kubernetes self-host deployment ships a `helicone-monitoring` Helm chart: "Grafana is deployed as part of the **helicone-monitoring** component and provides observability dashboards for monitoring your Helicone deployment. It works alongside Prometheus to collect and visualize metrics from all your services." These charts live in a **separate repo** (`github.com/Helicone/helicone-helm-v3`), not the main OSS tree. ([kubernetes](https://docs.helicone.ai/getting-started/self-host/kubernetes))
- OTel-adjacent: **OpenLLMetry async integration** (`@helicone/async`) — OTel-style instrumentation shipping traces to Helicone over HTTP. **Helicone is the destination, not an OTLP collector endpoint.** Supports OpenAI, Anthropic, Azure OpenAI, Cohere, Bedrock, Google AI Platform. ([OpenLLMetry](https://docs.helicone.ai/getting-started/integration-method/openllmetry))
- Because there is no native metrics endpoint, the community pattern is **webhooks → Prometheus/Grafana** (a webhook fires per completed request carrying cost/latency/tokens).

### 1.10 Self-hosting — what you actually get

| Question | Answer |
|---|---|
| **License** | **Apache License 2.0** (verbatim Apache text; **no `ee/` directory, no BSL/commons-clause, no field-of-use restriction** anywhere in the repo tree) ([open-source](https://docs.helicone.ai/references/open-source), [LICENSE](https://raw.githubusercontent.com/Helicone/helicone/main/LICENSE)) |
| **Is the dashboard in the OSS repo?** | **Yes.** `web/` (Next.js) contains the full dashboard — `pages/dashboard.tsx`, `requests`, `sessions`, `users`, `properties`, `cache`, `hql`, `alerts`, `datasets`, `playground`, `settings/*` |
| Documented architecture | `Web` (Next.js), `Worker` (Cloudflare proxy logging), `Jawn` (Express + Tsoa log-collection API), `Supabase` (app DB + auth), **`ClickHouse`** (analytics DB), **`Minio`** (object storage) ([README](https://raw.githubusercontent.com/Helicone/helicone/main/README.md)) |
| Docker quick start | `docker run -p 3000:3000 -p 8585:8585 -p 9080:9080 helicone/helicone-all-in-one:latest` → dashboard :3000, Jawn API :8585, MinIO :9080. Env `SITE_URL`, `JAWN_URL`, `S3_ENDPOINT`, `S3_BUCKET_NAME`, `BETTER_AUTH_SECRET`, `NEXT_PUBLIC_IS_ON_PREM=true`. Data wiped on restart unless volumes mounted. **Email verification must be done via `psql`; users need a manually created `organization` + `organization_member` row.** ([docker](https://docs.helicone.ai/getting-started/self-host/docker)) |
| ⚠️ **Key gotcha** | **"Jawn no longer proxies LLM traffic: the `/v1/gateway/*` routes have been removed. To proxy requests through a self-hosted deployment, run the AI Gateway alongside Jawn."** Port **8585 requires no authentication** for proxying — restrict via firewall. |
| Kubernetes / Helm | Full stack via `helm compose up`: `helicone-core`, `helicone-infrastructure` (PostgreSQL, Redis, ClickHouse), `helicone-monitoring` (Grafana+Prometheus), `helicone-argocd`; Terraform provisions AWS S3, Aurora, EKS. ⚠️ **The README gates the "production-ready Helm chart" behind `enterprise@helicone.ai`** while the Kubernetes docs describe it publicly — the chart's availability is ambiguous. |
| **Bodies retention** | `request_body` / `response_body` carry a ClickHouse **TTL of 3 months** from `request_created_at`; **analytics columns are retained**. Self-hosters can change the migration. **[SRC]** [schema_41](https://raw.githubusercontent.com/Helicone/helicone/main/clickhouse/migrations/schema_41_request_response_replacing_merge_tree.sql) |
| Cloud-only | (1) **Cloudflare Workers KV edge cache** — not in the self-hosted stack. (2) **HQL** is gated and depends on cloud ClickHouse row policies. (3) PostHog header export + webhooks are in the cloud Worker/Jawn path. (4) Hosted AI Gateway (`ai-gateway.helicone.ai`) — the self-host doc says to run it alongside Jawn but does **not** link an OSS image; `helicone-all-in-one` contains only dashboard + Jawn. |

**OSS analytics schema (the full dimension list available self-hosted)** **[SRC]** [schema_41](https://raw.githubusercontent.com/Helicone/helicone/main/clickhouse/migrations/schema_41_request_response_replacing_merge_tree.sql) + `schema_43/44/47/52/59/63/65/78_*.sql`: `response_id`, `response_created_at`, `latency`, `status`, `completion_tokens`, `prompt_tokens`, `model`, `request_id`, `request_created_at`, `user_id`, `organization_id`, `proxy_key_id`, `threat`, **`time_to_first_token`**, `provider`, `target_url`, `country_code`, **`properties Map`**, `scores Map`, `request_body`, `response_body`, `assets Array`, `updated_at`, **`cost`**, **`prompt_cache_read_tokens`**, **`prompt_cache_write_tokens`**, `prompt/completion_audio_tokens`, **`cache_reference_id`**, **`cache_enabled`**, `prompt_id`, `prompt_version`, `request_referrer`, `is_passthrough_billing`, **`reasoning_tokens`**. ReplacingMergeTree partitioned by month, PK `(organization_id, provider, model, user_id, request_created_at, request_id)`.

> **This schema is the single most useful artifact in the report for schema design** — it is a production, at-scale LLM analytics table (Apache-2.0, readable) that already encodes the field set the whole market converged on: cost, split tokens, cache read/write, reasoning tokens, TTFT, cache reference, country, scores, and a bloom-indexed property map.

---

## 2. Portkey

> **Naming/ownership change worth knowing:** Portkey's docs have moved to `docs.portkey.ai` and the product is now branded **Prisma AIRS AI Gateway** (Palo Alto Networks). Verify branding before quoting this section to a customer.

### 2.1 Dashboard charts / breakdowns

Tabs: **Overview, Users, Errors, Cache, Feedback, Summary** (the group-by view). Marketing copy claims "21+ key metrics" / "40+ crucial metrics". Underlying API: **18 time-series graph endpoints + 7 group-by endpoints**.

| Chart family | Documented |
|---|---|
| Cost, tokens, requests, errors | ✅ time-series graphs |
| **Cache hit rate** | ✅ dedicated Cache tab + graph |
| **Status codes** | ✅ (filterable; **not** a group-by — see below) |
| **Feedback** (avg weighted) | ✅ dedicated Feedback tab |
| Latency | ✅ but **avg / p50 / p90 / p99 only — no p95** |
| **TTFT** | ❌ **not a metric.** Exists only as a Prometheus approximation recipe |
| **Guardrail analytics** | ⚠️ A "Guardrail analytics dashboard" was added in changelog **v1.10.0** but has **no docs page, no documented endpoint, and no metric names** |

Enterprise-only: **"FinOps + Executive Dashboard"** / **"Executive Reporting"**.

**Group-by endpoints exist ONLY for 7 dimensions:** metadata key, ai-model, provider, user, workspace, **MCP server**, **A2A agent**.
**Verified ABSENT as group-bys:** virtual key, API key, status code, cache status, **guardrail/verdict**, config, prompt, span. Note the gap: Portkey can *group* by `user` but **not by virtual key or API key** — the two identities a gateway most needs for chargeback.

### 2.2 Time range / filter / group-by

- **Dashboard filters: 15 dimensions** — Model, Cost (in cents), Tokens, Status, **Meta**, Avg Weighted Feedback, Provider, Config, Trace ID, Time Range, API Key, Prompt ID, Cache Status, Workspace, Saved Filters.
- **API filter set is range/equality only** — no contains/negation operators, a real expressiveness gap vs Langfuse's 10 filter types or LangSmith's 11 operators.
- **Saved filters are org-wide visible and editable by all members** — a governance weakness worth noting (no private views, no per-user scoping).
- `[UNVERIFIED]` Named time-range presets are not documented.
- `[UNVERIFIED]` Comparison-to-previous-period is not documented.

### 2.3 Metadata / tags

- Header: **`x-portkey-metadata`**.
- **Unlimited keys; values must be strings ≤128 chars.**
- Reserved/special keys: **`_user`** (auto-copied from the request body `user` field), plus `_prompt`, `_organisation`, `_environment`.
- **Precedence (gateway ≥ v1.10.20): workspace > API key > request.** Before v1.10.20 the order was *reversed* — a breaking semantic change if you relied on request-level overrides.
- W3C **`baggage`** headers are merged in.
- Self-hosted: `HEADERS_TO_METADATA` env var.
- Enterprise: **JSON-Schema (draft-07) enforcement** on metadata at API-key/workspace create+update — metadata is validated, not free-form.

### 2.4 Logs

- A log's `AnalyticsMetrics` payload carries **~45 fields**: cost, `req_units`/`res_units`/`total_units`, `cache_status`, `retry_success_count`, trace/span ids, and **6 granular timing fields**. Notably **there is no guardrail field in the log record.**
- **Retention is documented three contradictory ways:** the Logs page says 3d/30d/unlimited; the Analytics page says 30d/**365d**/unlimited; the feature table says 3d/30d/Custom. `[UNVERIFIED]` — treat retention as unsettled and confirm contractually.
- **`GET /v1/logs` (list/search) DOES NOT EXIST** in the 40,535-line public OpenAPI spec. Only `POST /v1/logs` (insert) and `GET /v1/logs/{logId}` (+ `?type=hooks`). **Log search is UI-only** — there is no API to query logs.
- Opt out of logging per request with **`x-portkey-debug: false`** ("DO NOT TRACK").
- Enterprise: **Full vs Metrics-Only logging modes.**

### 2.5 Cost tracking + budgets

- Pricing JSON cached **24h**; auto-update requires gateway **1.12+**.
- **Unpriced models log `$0` and are EXCLUDED FROM BUDGETS** — a silent-failure mode: a model missing from the price map cannot consume budget.
- **Streaming cost requires `stream_options: {include_usage: true}`** or cost is recorded as 0.
- **Three custom-pricing mechanisms:** per-model input/output cost override; custom models; and per-Integration **multiplier adjustments** (gateway 2.7.0+, per-model 2.22.0+). Caveat: a **model-level value fully replaces the Integration-level value with no key merging**.
- **Budgets are HARD: exceeding returns `412 Precondition Failed` with "no spend is incurred."** Budgets are **non-retroactive** and — unusually — **cannot be edited once set by any org member**.
- Reset periods: none / weekly / monthly / **every-N-days (1–365)**.
- ⚠️ **For usage-limit *policies*, email alerts on `alert_threshold` are "work in progress; not sent today"** — only an audit-log event fires. Do not rely on policy-threshold email.
- **Five independent enforcement levels:** API key, workspace, Integration, usage-limit policies, rate-limit policies — with distinct **412 / 429 / 401** semantics.

### 2.6 Guardrails analytics — the sharpest finding in this whole report

Portkey ships a **lot** of guardrails but almost **no** guardrail *analytics*:

- **No documented guardrail metrics, no pass/fail rate, no per-guardrail breakdown.** And **`guardrail` is not a filter dimension and not a group-by dimension anywhere.**
- What *does* exist:
  - **`hook_results` is fully documented per guardrail** — `verdict`, `id`, `transformed`, `checks[]`, `execution_time`, `async`, `deny`, `type`, `feedback` — and **per check**: `id`, `verdict`, `data`, `execution_time`, `error`, `log`.
  - Verdicts surface as **HTTP `446` (denied)** and **`246` (failed but served)** — so guardrail denials flow into **error analytics** by status code rather than into a guardrail view.
  - `on_success` / `on_fail` auto-write **feedback** (defaults **+5 / −5**) — the only *numeric, filterable* path to guardrail outcomes.
  - The changelog claims a guardrail analytics dashboard (v1.10.0) with **no docs, endpoint, or metric names**.
  - ClickHouse stores guardrail results in a dedicated **`ANALYTICS_GENERATION_HOOKS_TABLE`**.
  - The only real metric surface is **`portkey.guardrail.*` OTel spans** — **Enterprise only, behind `EXPERIMENTAL_GEN_AI_OTEL_TRACES_ENABLED`** (attributes for phase/verdict/action/deny/async plus per-check events) — and these are **undocumented on the OTel pages**.
  - **Prometheus has zero guardrail metrics.** Guardrail tier gating: Dev = BASIC, Pro = BASIC+PARTNER+PRO, Enterprise = all + custom.

**This is the clearest unmet need across every product surveyed.** Guardrails are a first-class *feature* everywhere but almost nowhere a first-class *analytic*. A self-hosted gateway that ships guardrail verdict analytics (pass/fail rate, which guardrail fired, per-check breakdown, deny vs transform) would be differentiated, not table stakes.

### 2.7 Prometheus / OTLP

- **Prometheus is first-class but self-hosted Enterprise Gateway only:** `/metrics`, `ENABLE_PROMETHEUS`, **15 custom metrics** + Node runtime metrics, with opt-in labels via `PROMETHEUS_INCLUDE_MODEL_LABEL`, `PROMETHEUS_METADATA_LABELS`, `PROMETHEUS_LABELS_METADATA_ALLOWED_KEYS`, `PROMETHEUS_EXTRA_LABELS`. **There is no SaaS Prometheus endpoint.**
- **OTLP egress = two separate Enterprise/self-hosted features, env-var only:**
  1. **Analytics Export** — `OTEL_PUSH_ENABLED`, `OTEL_ENDPOINT`, …
  2. **Complete Logs Export** — **experimental**, GenAI semconv **1.40.0**, **HTTP/JSON only (gRPC explicitly out of scope)**, **no batching and no retry**, and it appends `/v1/traces` to your endpoint.
- **OTLP *ingest*** also exists at `https://api.portkey.ai/v1/logs/otel`.
- Datadog/New Relic/Grafana/S3 tracing-provider doc pages all 404. Datadog works only via generic OTLP (`https://http-intake.logs.datadoghq.com/api/v2/otlp` + `DD-API-KEY`). S3/GCS/Azure/Wasabi/NetApp/Mongo are self-hosted **log stores**, not export destinations.

### 2.8 Exports and API endpoints

- **Logs Export: Enterprise only, JSONL only, 50,000 logs/job**, via an async job API — `POST /v1/logs/exports`, `/start`, `GET /{id}`, `/download`, `/cancel`, `DELETE /{id}`, plus a docs-only `GET /logs/exports/field-restrictions` that is **absent from the spec**.
- **CSV is documented nowhere.** The deprecated export page says verbatim that exports "can not process … any other formats".
- `export_settings.deniedFields` blocks specific fields **org-wide** — a blunt instrument (no per-role redaction).
- **No observability webhook exists** — Portkey's webhooks are for guardrail checks and MCP auth only.
- `[UNVERIFIED]` The documented admin-apis index omits Logs Export entirely; several field names differ between prose and spec.

### 2.9 Self-hosting

- Deployment models: **SaaS / Hybrid / Air-gapped (legacy, no longer offered) / SCM (Strata Cloud Manager)**.
- **The OSS gateway is MIT** (`portkey-ai/gateway`; `Portkey-AI/models` also MIT), but enterprise components are closed, distributed via a gated Docker registry with per-tenant entitlement.
- ⚠️ **The OSS gateway gets ZERO observability** — Logs, Traces, Feedback, Metadata, Filters, Alerts are **all unavailable**. The MIT repo is a routing proxy only.
- Hybrid: **1-minute heartbeat**; only "non-sensitive metadata and aggregated metrics" reach the Portkey control plane. Two log-location options: your own VPC blob store (with the control plane fetching individual logs inbound) or Portkey Cloud.
- Retention in self-hosted is configured by **non-numeric env vars** — `LOG_STORE_FILE_PATH_FORMAT` (v1 flat / v2 time-hierarchical; v2 requires gateway 1.17.0+ and is unsupported for air-gapped `control_plane`), `LOG_STORE_OBJECT_LOCK_RETENTION_ENABLED` — plus log-store circuit-breaker vars. `[UNVERIFIED]` No numeric retention setting is documented.

### 2.10 Not documented (Portkey)

Named time-range presets · comparison-to-previous-period · **CSV export** · **p95 latency** · **TTFT as a metric** · **`GET /v1/logs`** · a documented Guardrail analytics page · a dedicated Alerts page (listed as Pro+ in the plan table but undocumented) · numeric self-hosted retention setting · first-party Datadog/New Relic/Grafana/S3 integrations.

**Documented doc bugs (cite with caution):** the cost graph's schema descriptions say "latency"; retention numbers conflict across three pages; Logs Export field names differ between prose and spec; `field-restrictions` is missing from the spec.

---

---

## 3. LiteLLM Proxy

> **Doc-hygiene note:** LiteLLM restructured its docs. These now **404**: `/docs/proxy/enterprise`, `/docs/proxy/max_budget`, `/docs/proxy/spend_logs_endpoint`, `/docs/proxy/organizations`. Live equivalents: `/docs/enterprise`, `/docs/proxy/users` (Budgets + Rate Limits), `/docs/proxy/cost_tracking`, `/docs/proxy/tag_budgets`, `/docs/proxy/customers`, `/docs/proxy/budget_reset_and_tz`, `/docs/proxy/provider_budget_routing`, `/docs/proxy/prometheus`, `/docs/proxy/logging_spec`, `/docs/proxy/ui_spend_log_settings`, `/docs/proxy/spend_logs_deletion`, `/docs/proxy/alerting`, `/docs/observability/opentelemetry_integration`, `/docs/observability/focus`.
>
> **Verification level:** the findings below were checked against a **real local install (litellm 1.95.0 + litellm_enterprise 0.1.52)** by reading source, not recalled from docs. Several endpoints and ~10 metric names are **code-only with no docs page** — flagged inline.

### 3.1 Spend tracking

**Primary table `LiteLLM_SpendLogs`**, PK `request_id` ([schema.prisma](https://github.com/BerriAI/litellm/blob/main/litellm/proxy/schema.prisma)). Columns: `call_type`, `api_key` (**hashed, never raw**), `spend`, `total_tokens`/`prompt_tokens`/`completion_tokens`, `startTime`, `endTime`, `request_duration_ms`, **`completionStartTime`**, `model`, `model_id`, `model_group`, `custom_llm_provider`, `api_base`, `user`, `metadata`, **`cache_hit`**, **`cache_key`**, `request_tags`, `team_id`, `organization_id`, **`end_user`**, `requester_ip_address`, `messages`, `response`, **`session_id`**, `status`, `agent_id`, `mcp_namespaced_tool_name`, `proxy_server_request`. Indexed on `startTime`, `(startTime, request_id)`, `end_user`, `session_id`.

**Five daily rollup tables** back the analytics endpoints: `LiteLLM_Daily{User,Team,Tag,Organization,EndUser}Spend`, each keyed by date × api_key × model × custom_llm_provider × MCP tool × endpoint, carrying spend, token counts, **cache_read/cache_creation tokens**, compression savings, and api/successful/failed request counts. This is the "rollup table" pattern — worth mirroring rather than aggregating raw logs per query.

**Cost computation:** model cost map `model_prices_and_context_window.json`. ⚠️ Keys are **`input_cost_per_token` / `output_cost_per_token` — PER TOKEN, not per 1M.** No `input_cost_per_million_tokens` key exists. Also supports `cache_read_input_token_cost`, `cache_creation_input_token_cost`, `*_above_200k_tokens` (context-tiered), per-image, per-reasoning-token, per-audio-token, `*_priority`, `*_flex`, `ocr_cost_per_page`, `input_cost_per_second`. Override per deployment via `model_info`; `base_model` remaps the pricing lookup. Tier-aware pricing (Vertex `trafficType`, Bedrock `serviceTier`) is automatic.
- **Streaming is tracked** — `await self.async_success_handler(result=complete_streaming_response)`.
- **Zero-cost models require BOTH costs explicitly `0`** to skip all budget checks; **unset/null does NOT**.
- **Unpriced-but-used requests are flagged, not silently zeroed** — spend=0 plus a WARNING log and the counter **`litellm_zero_cost_requests_total{reason=missing_pricing_key|pricing_not_applied|cost_calculation_error}`**. This is a direct answer to Portkey's silent-$0 problem: expose a metric when pricing is missing.

### 3.2 Budgets

**Scopes:** global proxy (`litellm_settings.max_budget` + `budget_duration` — **fails open if no DB**), virtual key, internal user, `max_internal_user_budget` (default for all internal users), team, **team member** (`max_budget_in_team`; reset via `POST /team/{id}/member/{uid}/reset_spend {"reset_to": n}`), end-user/customer, tag, organization (`budget_id`), budget objects/pricing tiers (`/budget/*`), **per-model per key** (`model_max_budget`), per-model per user, provider budget routing, agent, and **session** (`max_budget_per_session`). Enterprise: Projects, Temporary Budget Increase (key-only).

⚠️ **Critical precedence gotcha:** if a key has a `team_id`, **only team + team-member budgets are enforced — the key owner's personal budget does NOT apply.** v1.94.0 briefly enforced both behind `skip_user_budget_on_team_key`; the flag **and** the enforcement were removed in v1.95.0. Anyone migrating across those versions must re-check their budget expectations.

**`budget_duration`:** `"30s"`, `"30m"`, `"30h"`, `"30d"`; **unset = never resets**. Alignment: `1d` → midnight daily, `7d` → Monday midnight, `30d` → 1st of month; sub-day rolls forward by interval. `litellm_settings.timezone` (IANA, default UTC) and `budget_reset_time: "09:00"` (malformed value **fails startup**). Multi-window budgets via `budget_limits: [{budget_duration, max_budget}]` (e.g. 1h/24h/7d/30d).

**Soft vs hard:** `soft_budget` = **alert only, never blocks** (email, deduped 24h). `max_budget` = hard stop. Error surfaces differ by scope and are worth normalizing in your own product: key → `ExceededTokenBudget`; team → `Budget has been exceeded!` code **400**; per-model user → code **429**; per-model key → `budget_exceeded` code **400**; tag → code **400**; global/user → `ExceededBudget`. **Existing spend counts against a budget added later** (a member who spent $500 unbudgeted and is then given a $100 budget has their next request rejected).

**Enforcement mechanics (the best-specified of any product surveyed):**
- **Budget reservation is ON by default** (`general_settings.disable_budget_reservation: true` to disable). Batch requests cannot be reserved.
- **`general_settings.fail_closed_budget_enforcement: true`** validates against the **authoritative DB on every budgeted request** across key/team/user/org/end-user/tag/per-window; if it cannot verify, it returns **503** — i.e. it fails *closed*, the opposite of the global-budget default.
- This closes exactly the race Cloudflare documents as a limitation ("eventually consistent… a burst of concurrent requests can briefly exceed the limit").

**Alerting:** `alerting: ["slack"|"webhook"|"email"]`; `SLACK_WEBHOOK_URL` / `ALERTING_WEBHOOK_URL` (provider-neutral) / `WEBHOOK_URL`; Teams and Discord via Slack-compatible URLs; health via `GET /health/services?service=slack|webhook`. Webhook events: **`spend_tracked` | `budget_crossed` | `threshold_crossed` (fired at 85% AND 95%) | `projected_limit_exceeded`** (key-only, needs `soft_budget`); `event_group ∈ customer|internal_user|key|team|proxy`. Email max-budget alert defaults to **80%** of `max_budget` (`EMAIL_BUDGET_ALERT_MAX_SPEND_ALERT_PERCENTAGE=0.8`), TTL 86400.

### 3.3 Analytics API endpoints (code-verified; method + path + params)

| Endpoint | Key params / notes |
|---|---|
| `GET /spend/logs` | `api_key`, `user_id`, `request_id`, `start_date`, `end_date`, **`summarize`** (default `true` = aggregate by date; `false` = individual rows). ⚠️ **Legacy endpoint truncates to the 10,000 most recent rows** and sets `x-litellm-spend-logs-truncated: true`. |
| `GET /spend/logs/ui` | The full filter surface: `api_key`, `user_id`, `request_id`, `session_id`, `team_id`, `min_spend`, `max_spend`, `start_date`, `end_date`, `page`, `page_size` (**1–1000**, default 50), `status_filter`, `model`, `model_id`, `model_group`, `key_alias`, `end_user`, `error_code`, `error_message`, `sort_by`, `sort_order` |
| `GET /spend/logs/v2` | Same handler + `group_by_session`. Returns `{data,total,page,page_size,total_pages,total_is_capped}`. Since v1.93.0 **`total` is capped at 10,000 but `data` is never truncated**. Docs call this **"the recommended endpoint for exports and integrations"** — use this, not `/spend/logs`. |
| `GET /spend/logs/ui/{request_id}` | Stored request + response body |
| `GET /spend/logs/session/ui` | `session_id`, `page`, `page_size` — *code-only, no docs page* |
| `GET /spend/keys`, `GET /spend/users` | Caller-scoped by default (`scope_spend_list_endpoints_to_caller` / `legacy_unscoped_spend_list_endpoints`) |
| `GET /spend/tags` | `start_date`, `end_date` |
| `POST /spend/calculate` | Cost calculation for a hypothetical request |
| `POST /global/spend/reset` | **Master key only.** Zeroes `LiteLLM_TeamTable` + `LiteLLM_VerificationToken` spend but **keeps `LiteLLM_SpendLogs`** — i.e. resets counters without destroying the audit trail. Good design. |
| `POST /global/spend/refresh` | Rebuild rollups |
| `GET /global/spend/logs`, `GET /global/spend` | Global views |
| **`GET /global/spend/report`** | `start_date`, `end_date`, **`group_by ∈ {team, customer, api_key}`**, `api_key`, `internal_user_id`, `team_id`, `customer_id`. ⚠️ **THE ONLY license-gated analytics endpoint** — guard `if premium_user is not True: raise ValueError(...)` at `spend_management_endpoints.py` L1041–1043. |
| `GET /global/spend/keys` (`limit`), `/global/spend/teams`, `/global/spend/models` (`limit`), `/global/spend/provider` (`start_date`,`end_date`), `/global/spend/tags` (`tags`), `/global/spend/all_tag_names`, `GET`+`POST /global/spend/end_users`, `/global/all_end_users` | Per-entity global breakdowns. *Mostly code-only.* |
| `GET /global/activity`, `/global/activity/model`, **`/global/activity/cache_hits`**, `/global/activity/exceptions` (`model_group` required), `/global/activity/exceptions/deployment` | Activity + **cache-hit** activity. *Code-only.* |
| `GET /provider/budgets` | Provider-level budget state |
| `GET /user/daily/activity` (+ `/aggregated`), `/team/daily/activity`, `/tag/daily/activity`, `/customer/daily/activity`, `/end_user/daily/activity`, `/organization/daily/activity`, `/gateway/daily/activity` | Daily rollups. Shared impl uses SQL **`GROUPING SETS`** over date × api_key × model × model_group × provider × mcp_tool × endpoint |
| `GET /audit`, `GET /audit/{id}` | ⚠️ **Enterprise** — defined in `litellm_enterprise` only. Params `page`, `page_size` (≤100), `changed_by`, `changed_by_api_key`, `action`, `table_name`, `object_id`, `start_date`, `end_date`, `object_team_id`, `object_key_hash` |
| `/budget/new\|update\|info\|list\|delete`, `GET /budget/settings` | Budget-object CRUD |

**Non-admin access:** a key with `permissions={"get_spend_routes": true}`.

**Customer / end-user ID resolution order** (relevant if you build identity attribution): `x-litellm-customer-id` → `x-litellm-end-user-id` → `user_header_mappings` → `user_header_name` (deprecated) → body `user` → `litellm_metadata.user` → `metadata.user_id` → `safety_identifier`; a JWT `end_user_id_jwt_field` **beats all of them**. `validate_end_user_id_in_db: true` filters junk IDs. ⚠️ Customer budgets are **global per deployment** (one budget across all keys/teams; cannot be scoped to a key or team), and `/customer/update` **silently drops** `tpm_limit`/`rpm_limit`/`budget_duration`.

### 3.4 Per-entity analytics

Breakdown available per **team, user, key, end-user (customer), tag, model, model_group, provider, organization, deployment, user-agent (DAU/WAU/MAU), and cache**. Dimensions: `date`, `api_key`, `model`, `model_group`, `custom_llm_provider`, `mcp_namespaced_tool_name`, `endpoint`. Measures: spend, prompt/completion tokens, **cache read / cache creation tokens**, compression savings, api/successful/failed requests. `GET /key/info` also returns `model_max_budget_usage` — the same value used for enforcement, so UI and enforcement cannot disagree.

### 3.5 Prometheus metrics (the most complete set of any product surveyed)

- Enable: `litellm_settings.callbacks: ["prometheus"]`; `prometheus_client==0.20.0` preinstalled in the Docker image.
- Endpoint **`GET /metrics`**. ⚠️ **Auth is REQUIRED by default since v1.85.0** — disable with `require_auth_for_metrics_endpoint: false`.
- **Dedicated metrics port (v1.101.0+):** `--prometheus_metrics_port` / `PROMETHEUS_METRICS_PORT` — separate process, same metrics, and **NO virtual-key auth on it** (permit only trusted networks). This is an important deployment detail: the auth-free port is a network-trust boundary.
- Multi-worker requires `PROMETHEUS_MULTIPROC_DIR`.
- **Not enterprise-gated** (the Enterprise page lists Prometheus metrics in the OSS column).

**Cost / tokens / cache:**
`litellm_spend_metric` — labels `end_user`, `hashed_api_key`, `api_key_alias`, `model`, `team`, `team_alias`, `user`, `user_email`, `client_ip`, `user_agent`, `requested_model`, `model_id`, `api_provider`, `service_tier`. Plus `litellm_total_tokens_metric`, `litellm_input_tokens_metric`, `litellm_output_tokens_metric`, **`litellm_input_cached_tokens_metric`**, **`litellm_input_cache_creation_tokens_metric`**, `litellm_input_audio_tokens_metric`, **`litellm_output_reasoning_tokens_metric`**, `litellm_output_audio_tokens_metric`, **`litellm_cache_hits_metric`**, **`litellm_cache_misses_metric`**, `litellm_cached_tokens_metric`, `litellm_requests_metric` (deprecated), **`litellm_zero_cost_requests_total`**, `litellm_mcp_tool_calls_total`, `litellm_mcp_tool_call_spend_metric`.

**Budget / quota gauges** (a whole family most products lack): `litellm_team_max_budget_metric`, `litellm_remaining_team_budget_metric`, `litellm_team_budget_remaining_hours_metric`, and the same triple for **api_key**, **user**, **organization**, and **customer**; plus `litellm_remaining_api_key_requests_for_model`, `litellm_remaining_api_key_tokens_for_model`, `litellm_remaining_requests_metric`, `litellm_remaining_tokens_metric`, `litellm_provider_remaining_budget_metric`, `litellm_deployment_tpm_limit`, `litellm_deployment_rpm_limit`. Note the **`*_budget_remaining_hours_*`** metrics — time-to-reset as a gauge is a genuinely useful alerting primitive.

**Proxy / deployment health:** `litellm_proxy_total_requests_metric` (labels incl. `status_code`, `route`, `client_ip`, `user_agent`, `model_id`, `api_provider`; optional `stream`), `litellm_proxy_failed_requests_metric`, `litellm_deployment_success_responses`, `litellm_deployment_failure_responses`, `litellm_deployment_total_requests`, **`litellm_deployment_state`** (0=healthy, 1=partial, 2=complete outage), `litellm_deployment_latency_per_output_token`, `litellm_deployment_cooled_down`, `litellm_deployment_successful_fallbacks`, `litellm_deployment_failed_fallbacks`, `litellm_callback_logging_failures_metric{callback_name}`, **`litellm_guardrail_requests_total` / `errors_total` / `latency_seconds`** (⚠️ note: LiteLLM *does* have guardrail metrics — Portkey does not).

**Latency histograms (seconds):** `litellm_request_total_latency_metric`, `litellm_request_queue_time_seconds`, `litellm_llm_api_latency_metric`, **`litellm_llm_api_time_to_first_token_metric` (streaming only)**, `litellm_overhead_latency_metric`, `litellm_overhead_with_guardrails_latency_metric`.

**Pod / system:** `litellm_in_flight_requests` (+ `GET /health/backlog`), `litellm_admission_{admitted,queued}_requests`, `litellm_admission_rejected_requests_total{reason}`, and per-service `litellm_<service>_{latency,failed_requests_total,total_requests_total}` for redis/postgres/auth/batch_write_to_db/reset_budget_job/router/proxy_pre_call/self via `service_callback: ["prometheus_system"]`; DB queue gauges `litellm_{in_memory,redis}_*_spend_update_queue_size`, `litellm_pod_lock_manager_size`.

**Retention metrics:** `litellm_spend_log_cleanup_{rows_deleted_total,batch_duration_seconds,rows_remaining,batch_failures_total,runs_total{outcome}}` — observability *of* the retention job. Rare and worth copying.

**Cardinality knobs:** `prometheus_metrics_config` (group/metrics/include_labels), `prometheus_exclude_metrics`, `prometheus_exclude_labels`, `prometheus_emit_stream_label`, `enable_end_user_cost_tracking_prometheus_only` (+ `max_series` 10000 / `ttl` 3600 caps — an explicit guard against end-user cardinality blowup), `prometheus_initialize_budget_metrics` (5-min cron), `prometheus_deployment_and_latency_caller_identity ∈ api_key_alias|user_email|both`, `custom_prometheus_metadata_labels`, `custom_prometheus_tags`.

### 3.6 Admin UI

`<base>/ui` — requires master key + DB. Login via `UI_USERNAME`/`UI_PASSWORD` (falls back to master key); harden with a `proxy_admin` user + `disable_env_credential_login: true`; disable entirely with `DISABLE_ADMIN_UI="True"`. 58 shipped routes; verified view labels include **Virtual Keys, Test Key, Models & Endpoints, Model Hub, Usage, Cost Tracking, Cost Optimization, Logs, Logging & Alerts, Guardrails, Guardrails Monitor, Budgets, Tag Management, Teams, Team Usage, Internal Users, Users, Organizations, Customer, Customer Usage, End User, Tag Usage, Projects, Access Groups, Policies, Agents, MCP, Vector Stores, Prompts, Skills, Memory, Workflows, Router Settings, Caching, Playground, API Reference, Audit Logs, Settings, Admin Panel**.

- The **Logs** view *is* `/spend/logs/ui`, so it supports the full filter set in §3.3.
- A gear Settings panel sets "Store Prompts in Spend Logs" + "Retention Period" **live, overriding config**. Request/response content is **OFF by default**.
- The **Usage** page hosts Customer Usage + Endpoint Activity tabs, with Success/Failed tiles and Gateway-Requests-by-Endpoint from `LiteLLM_DailyGatewayRequests` (`proxy_batch_write_at` default **10s**), and renders Personal/Team/Organization/Global scope by role.
- Key budgets: Virtual Keys → Create Key → Optional Settings → **Budget Windows / Per-Model Budgets**. Team soft budget: Teams → team → Settings → Edit Settings.
- **Projects UI is gated by the Enterprise-only `enable_projects_ui` UI setting.**

### 3.7 Logging integrations and OTEL

**51 built-in callbacks**, including `prometheus`, **`otel`**, `langfuse`, `langfuse_otel`, `langsmith`, `datadog`, `datadog_metrics`, `datadog_llm_observability`, `braintrust`, `arize`, `arize_phoenix`, `langtrace`, `galileo`, `opik`, `mlflow`, `weave_otel`, `pagerduty`, `s3_v2`, `gcs_bucket`, `azure_storage`, `aws_sqs`, `gcs_pubsub`, `cloudzero`, **`focus`**, `vantage`, `mavvrik`, `levo`, `newrelic`, `posthog`, `humanloop`, `agentops`, `deepeval`, `openmeter`, `lago`, plus email callbacks (`resend_email`, `sendgrid_email`, `smtp_email`).

Callbacks receive `kwargs["standard_logging_object"]` when payload construction completes (may be omitted on intermediate streaming events). `StandardLoggingPayload` includes `id`, `trace_id`, **`session_id`**, `call_type`, `response_cost`, **`cost_breakdown`**, `status`, **`status_fields` (`llm_api_status` ∈ success|failure; `guardrail_status` ∈ success|guardrail_intervened|guardrail_failed_to_respond|not_run)**, `tokens`, `times`, model fields, `metadata`, `cache_hit`/`cache_key`/**`saved_cache_cost`**, `request_tags`, `end_user`, `requester_ip_address`, `messages`, `response`, `error_str`/`error_information`, `model_parameters`, `hidden_params`. Controls: `x-litellm-call-id`, `turn_off_message_logging`, `x-litellm-enable-message-redaction`, `LiteLLM-Disable-Message-Redaction`, `redact_user_api_key_info`, `langfuse_default_tags`, `metadata.log_raw_request`.

**OTEL:** yes — OTLP **traces** by default, **metrics + logs opt-in**. `callbacks: ["otel"]`; env `OTEL_EXPORTER` (default `console`), `OTEL_ENDPOINT`/`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_HEADERS`, **`OTEL_EXPORTER_OTLP_PROTOCOL` (http/json | http/protobuf | grpc)**, `OTEL_SERVICE_NAME`. gRPC needs `litellm[grpc]`.
- Toggles: `LITELLM_OTEL_INTEGRATION_ENABLE_METRICS` (false), `..._ENABLE_EVENTS` (false), `USE_OTEL_LITELLM_REQUEST_SPAN` (false — since v1.81.0 attributes land on the proxy root span), `OTEL_SEMCONV_STABILITY_OPT_IN=gen_ai_latest_experimental`, `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT`, `OTEL_IGNORE_CONTEXT_PROPAGATION`, `OTEL_DEBUG`.
- **Span tree:** root `Received Proxy Server Request` (SERVER) → optional `litellm_request` → `raw_gen_ai_request` + `guardrail`, plus service-hook spans (auth/router/self/proxy_pre_call/redis/postgres/batch_write_to_db/reset_budget_job/pod_lock_manager); `Failed Proxy Server Request` on error. **W3C `traceparent` honored.**
- **OTLP metrics** (`enable_metrics=True`): `gen_ai.client.operation.duration`, `gen_ai.client.response.duration`, `gen_ai.client.token.usage` (label `gen_ai.token.type`), **`gen_ai.usage.cost` (USD)**, `gen_ai.server.time_to_first_token` (streaming only), `gen_ai.server.time_per_output_token`; labels `gen_ai.operation.name`, `gen_ai.request.model`, `gen_ai.framework="litellm"`. Renamed from the older `gen_ai.client.token.cost` / `.response.time_to_first_token` / `.response.time_per_output_token`.
- **Cost attributes on spans:** `gen_ai.cost.{input_cost, output_cost, total_cost, tool_usage_cost, original_cost, discount_percent, discount_amount, margin_percent, margin_fixed_amount, margin_total_amount}` — note the **margin/discount** attributes, which no other product exposes. Correlation via `litellm.call_id`, `litellm.request.type`. Cardinality control via `callback_settings.otel.attributes.{include_list|exclude_list}` (mutually exclusive; `gen_ai.token.type` cannot be filtered).
- **OTEL v2** opt-in: `LITELLM_OTEL_V2=true` → one trace per request (HTTP → auth → guardrails → LLM → DB writes), with presets for arize/arize_phoenix/langfuse_otel/weave_otel/langtrace/levo/agentops.

### 3.8 Export

- `/spend/logs?summarize=false` and paged `/spend/logs/v2` are **JSON** exports. ⚠️ **There is NO CSV code path anywhere in the spend endpoints, and no UI download button is documented.**
- The CSV route is **FOCUS** (experimental, `callbacks: ["focus"]`): FinOps **FOCUS v1.2**, automatic scheduled export, `FOCUS_FORMAT=parquet|csv`, `FOCUS_PROVIDER=s3|gcs|vantage`, `FOCUS_FREQUENCY=hourly`, `FOCUS_S3_BUCKET_NAME`/`ACCESS_KEY`/`SECRET_KEY` required. This is the FinOps-standard export path — a genuine differentiator vs the other five.
- S3 cold storage of payloads via `s3_v2` → `metadata.cold_storage_object_key`. Audit logs → S3 JSON via `store_audit_logs: true` + `audit_log_callbacks: ["s3_v2"]` at `s3://<bucket>/audit_logs/<date>/<time>_<id>.json`.
- Scheduled reports via `spend_report_frequency` + alert types `spend_reports`/`daily_reports`/`fallback_reports`. Lago billing via `LAGO_API_CHARGE_BY ∈ end_user_id(default)|user_id|team_id`.
- **Retention/deletion is fully OSS** — docs state explicitly "Retention and cleanup are open source… does not check license state."

### 3.9 Enterprise gating — the important part

**OSS (free) includes essentially all analytics:** all spend tracking, all five daily-rollup tables, the entire cost map + custom pricing, **all** `/spend/*` log endpoints, **all** `/global/spend/*` + `/global/activity*` + `/provider/budgets` + all five `/…/daily/activity` endpoints — **except `/global/spend/report`**. Plus all key/user/team/team-member/end-user/org/tag/budget-object budgets, `soft_budget`, `budget_duration`, multi-window `budget_limits`, `model_max_budget`, budget reservation, `fail_closed_budget_enforcement`, Slack/webhook/email alerting incl. `budget_alerts`/`spend_reports`/`daily_reports`, **the full Prometheus `/metrics` incl. the dedicated port**, full OTEL, spend-log retention/deletion/partitioning, and the Admin UI + Logs view.

**Enterprise-only:** (1) **`GET /global/spend/report`** and `/spend/report` — hard `premium_user` guard; (2) audit logs `/audit`; (3) Projects + `enable_projects_ui`; (4) Temporary Budget Increases (key-only); (5) `region_outage_alerts`; (6) **Soft-budget EMAIL alerts** (Slack soft-budget alerts are OSS); (7) managed batches + `litellm_managed_*` metrics; (8) log export to GCS/Azure Blob, per-team log routing, disable-logging-per-team, management-op logs; (9) Organizations, `org_admin`/`team_admin` roles, team-member permissions, RBAC/multi-tenant; (10) SSO (free ≤5 users), JWT auth, IP allowlists, route controls, key rotation, secret managers; (11) specific guardrail callbacks.

⚠️ **Gating conflicts to be aware of:** **tag budgets** and **model-specific budgets per key** are labeled Enterprise in the marketing docs but have **no `premium_user` guard in code** (tag endpoints live in the OSS `tag_management_endpoints.py`; `model_max_budget` is fully documented with an OSS API + UI). Enterprise is enabled purely by `LITELLM_LICENSE="eyJ..."`.
**Version policy (new, 2026-06-29):** only the **4 most recent stable minor lines** are supported (1.86–1.89 as of mid-June 2026) — a real operational constraint for a self-hosted deployment.

---

---

## 4. Langfuse / LangSmith

These are **trace-observability platforms**, not gateways — they are what a gateway exports *to*. For a self-hosted gateway builder they matter twice: as the competitive bar for analytics, and as a destination you should support via OTLP rather than reinvent.

### 4.1 Langfuse

**Data model** ([data-model](https://langfuse.com/docs/observability/data-model)): a trace is "the logical grouping of all observations that share the same `trace_id`". Trace-level attributes (`user_id`, `session_id`, `tags`, `metadata`) "live on **every observation** within the trace; the SDKs propagate them automatically" — one wide observations table where each row carries observation data *plus a copy* of trace-level attributes, explicitly "to keep queries and aggregations fast". v4 derives trace metrics by "counting distinct trace IDs rather than counting rows in a traces table" ([v4 dashboard changes](https://langfuse.com/faq/all/dashboard-changes-in-v4)).

Observation types (10): `event`, `span`, `generation`, `agent`, `tool`, `chain`, `retriever`, `evaluator`, `embedding`, `guardrail` — with `generation`/`embedding` carrying model/token/cost ([observation-types](https://langfuse.com/docs/observability/features/observation-types)). No documented nesting cap. Agent graphs render **Aggregated** (repeated step names collapsed, e.g. `retrieve_docs (3/3)`) or **Expanded** (loops unrolled to a DAG) ([agent-graphs](https://langfuse.com/docs/observability/features/agent-graphs)).

First-class dimensions — **this is the key contrast with LangSmith**:
- `sessionId` — groups traces, enables session replay; US-ASCII <200 chars, longer dropped ([sessions](https://langfuse.com/docs/observability/features/sessions)). **No dedicated v2 sessions endpoint** — you fetch observations sharing a `sessionId` and group client-side.
- `userId` — drives "aggregating metrics such as LLM usage cost by `userId`" ([users](https://langfuse.com/docs/observability/features/users)).
- `tags` — strings ≤200 chars; trace tags = union across observations; **immutable** (cannot be added/edited in the UI after creation) ([tags](https://langfuse.com/docs/observability/features/tags)).
- `metadata` — values ≤200-char strings, "Keys are limited to alphanumeric characters only", over-length dropped ([metadata](https://langfuse.com/docs/observability/features/metadata)).
- `release` (app version) and per-observation `version` (component version); resolution order explicit arg → `LANGFUSE_RELEASE` → auto-detected Vercel/Heroku/Netlify ([releases-and-versioning](https://langfuse.com/docs/observability/features/releases-and-versioning)).
- `environment` — created on first ingest, persistent, **cannot be deleted/renamed in UI**; regex `^(?!langfuse)[a-z0-9-_]+$`, ≤40 chars ([environments](https://langfuse.com/docs/observability/features/environments)).

**Metrics API v2 — `GET /api/public/v2/metrics`** (the machine-readable contract; enums verified from the shipped OpenAPI spec at `https://cloud.langfuse.com/generated/api/openapi.yml`) ([metrics-api](https://langfuse.com/docs/metrics/features/metrics-api)):

- **4 views**: `observations`, `scores-numeric`, `scores-categorical`, `scores-boolean`. The `traces` view "is no longer available in v2".
- **11 aggregations**: `sum`, `avg`, `count`, `max`, `min`, **`p50`, `p75`, `p90`, `p95`, `p99`**, `histogram`.
- **6 granularities**: `auto` (≈50 buckets), `minute`, `hour`, `day`, `week`, `month`.
- **15 observation dimensions**: environment, type, name, level, version, tags, release, traceName, traceRelease, traceVersion, **providedModelName**, promptName, promptVersion, isRootObservation, startTimeMonth.
- **15 observation measures**: count, latency, **streamingLatency**, inputTokens, outputTokens, totalTokens, **outputTokensPerSecond**, **tokensPerSecond**, inputCost, outputCost, **totalCost**, **timeToFirstToken**, countScores.
- Score dimensions/measures: environment, name, source (`API`/`ANNOTATION`/`EVAL`), dataType, configId, timestampMonth/Day, value, traceName, tags, traceRelease, traceVersion, observationName/ModelName/PromptName/PromptVersion; `scores-boolean` adds `booleanValue`; `scores-categorical` uses `stringValue`. Measures: count, value (boolean `avg` = true-rate).
- **High-cardinality dimensions are filter-only, NOT groupable**: `id`, `traceId`, `userId`, `sessionId`, `parentObservationId` (and the score equivalents). v4 additionally requires row limit + sort order for high-cardinality grouping and **forbids them in time series entirely**.
- Limits: `row_limit` default 100, max **1,000**; histogram bins 1–100, default 10. `orderBy` uses `{aggregation}_{measure}` (e.g. `sum_totalCost`) or the time dimension.
- Filter operators: datetime `>,<,>=,<=`; string `=,contains,does not contain,starts with,ends with`; `stringOptions` any of/none of; `arrayOptions` any of/none of/all of; number comparisons; `stringObject`/`numberObject` (with required `key`); boolean `=,<>`; `null` is null/is not null.

**Custom dashboards** ([custom-dashboards](https://langfuse.com/docs/metrics/features/custom-dashboards)): widget axes = Data Source (traces / observations / scores numeric-categorical-boolean) → Metrics (count, latency, cost, scores) → Dimensions (**user**, model, time, trace name…) → Filters → Chart Type. Chart types: Line, Bar, Time Series, Pie; in-table builder adds Area, Ranked, Number ([events-table-charts](https://langfuse.com/docs/observability/features/events-table-charts)).
- Chart-any-table: metric ∈ {Count, Latency, Cost, Tokens}; aggregations — Count always counted; Latency Average/Median(p50)/p95/p99/Max/Min; Cost & Tokens Sum/Average/p95/Max; breakdown ∈ {Total, Model, Name, Level, Type, Environment}.
- **Widget export**: ⋯ menu → **Download as JSON** and **Download data as CSV** (current query result). Widget JSON tagged `{"$langfuseWidget": true, "version": 1}`, dashboard `{"$langfuseDashboard": true, "version": 1}` — **no DB IDs, so definitions are portable across projects/instances** and can live in version control.
- Programmatic dashboard management via `/api/public/unstable/dashboards` + `/dashboard-widgets` — explicitly **unstable**; also CLI and MCP.
- Widget-level environment filter overrides the dashboard selector for that widget only.
- Curated dashboards: Latency, Cost, Usage; **Langfuse Usage Management** shows Total Trace / Observation / Score counts.

**Search syntax** ([filter-search-bar](https://langfuse.com/docs/observability/features/filter-search-bar)) — one line parses into the sidebar filter state and **serializes into the URL**, so links reproduce exact views. Supports `level:ERROR`, `latency:>2`, `startTime:>2026-06-01`, wildcards (`*checkout*`, `checkout*`, `*checkout`, `=checkout` for exact), negation `-environment:production`, `level:(ERROR OR WARNING)`, `tags:(billing AND urgent)`, `metadata.region:eu`, `scores.accuracy:>0.8`, `scores.helpfulness:positive`, `scores."Answer Relevance":>=0.9`, `has:endTime`, and full-text with `input:`/`output:` scoping.

**Alerts** ([alerts](https://langfuse.com/docs/observability/features/alerts)): data source ∈ {Observations, Scores numeric/categorical/boolean}; metric = aggregation + measure ("`avg latency`, `count`, **`p95 cost`**"); filters by model/tags/user ID/environment/boolean value; operators `>,≥,<,≤,=,≠`; required alert threshold + optional warning threshold; windows e.g. 1 hour/1 day/1 week; no-data modes (treat as 0 / keep previous / show NO_DATA / notify after sustained NO_DATA); renotify off or every N min (1–10,080). Channels: **Slack, HMAC-signed Webhook, GitHub Actions `workflow_dispatch`**. Cloud alert limits 2/20/50/100 (Hobby/Core/Pro/Enterprise); self-hosted v4 unlimited.

**Cost tracking** ([token-and-cost-tracking](https://langfuse.com/docs/observability/features/token-and-cost-tracking)): usage details (units per usage type: input, output, `cached_tokens`, audio…) and cost details (USD per usage type) on `generation`/`embedding`. Each is **ingested or inferred**, and "when both are available, ingested values take priority over inferred ones". Model definitions via `match_pattern` regex + per-usage-type prices, CRUD at `/api/public/models`; "User-defined models take priority over models maintained by Langfuse"; defaults maintained in the OSS repo with a **daily automated audit** against official provider sources.
- **Pricing tiers** (the notable feature): Name, Priority (0 reserved for default), Conditions, Prices. Conditions match on usage details (regex on keys, sum, compare gt/gte/lt/lte/eq/neq), model parameters (top-level key vs exact values), or metadata. All conditions must match; priority order then default. Documented examples: Claude Sonnet 4.5 "Large Context" with `input > 200000`; OpenAI Fast mode matching `service_tier ∈ {fast, priority}`.
- Ingest cost directly: `cost_details={"input":1,"cache_read_input_tokens":0.5,"output":1}`; `total` derived as sum of usage types if unset.
- Cost computed **at ingestion** → updated default prices "apply only to new generations".
- Known limitation: inference by tokenizing is unsupported for reasoning models (OpenAI o1 family) — reasoning tokens are billed as output but invisible to Langfuse, so without ingested token counts cost cannot be inferred.
- Separate concept: **spend alerts** monitor your *Langfuse bill*, not LLM cost ([spend-alerts](https://langfuse.com/docs/administration/spend-alerts)).

**Evaluations** ([scores/data-model](https://langfuse.com/docs/evaluation/scores/data-model)): scores have `dataType ∈ {NUMERIC, CATEGORICAL, BOOLEAN, TEXT}`, `source ∈ {API, EVAL, ANNOTATION}`, optional `comment`/`configId`, and "each Score references **exactly one** of Trace, Observation, Session, or DatasetRun". `ScoreConfig` enforces the schema (type, min/max, categories, 1–500 chars) and is **immutable but archivable**.
- LLM-as-a-judge: judge prompt with `{{variables}}` mapped to observation input/output/metadata/tool calls; targets Observations (recommended) or Experiments; **trace-level evaluators are DEPRECATED and stop producing results at the v4 cutover on 2026-11-16**. Executed by the Langfuse **worker** — "there is no option to run managed evaluators outside the worker"; self-hosters need worker egress; scale with `LANGFUSE_EVAL_EXECUTION_WORKER_CONCURRENCY` (default 5).
- Code evaluators (Python/TS) run in a **separate worker queue and sandbox**; self-hosted "are disabled when no dispatcher is set" ([code-evaluators](https://langfuse.com/docs/evaluation/evaluation-methods/code-evaluators)).
- Rules = which incoming observations (filters + sampling rate + ≥1 evaluator); the UI shows 7-day matching volume and estimated judge cost so you can lower sampling.
- Annotation queues require a ScoreConfig; can add traces, observations **or sessions** in bulk ([annotation-queues](https://langfuse.com/docs/evaluation/evaluation-methods/annotation-queues)).
- Datasets/experiments with baseline comparison, per-item regression detection, and CI via `langfuse/experiment-action` raising `RegressionError` on threshold violation.
- **Score Analytics**: single score → count/mean-or-mode/stddev, distribution, trend; two scores → matched count, **Pearson & Spearman**, MAE & RMSE (numeric), **Cohen's Kappa / F1 / Overall Agreement** (categorical+boolean), plus a **Score Comparison Heatmap**. Text scores unsupported ([score-analytics](https://langfuse.com/docs/evaluation/scores/score-analytics)).
- Scores are chartable/filterable at all three levels.
- Billing note: Units = Count of Traces + Observations + **Scores** — including judge-, annotation- and experiment-produced scores ([billable-units](https://langfuse.com/docs/administration/billable-units)).

**Exports** ([export-from-ui](https://langfuse.com/docs/api-and-data-platform/features/export-from-ui), [export-to-blob-storage](https://langfuse.com/docs/api-and-data-platform/features/export-to-blob-storage)):
- UI: "Most tables in Langfuse support batch-exports. **All filters applied to the table will be applied to the export**" — CSV, JSON, all columns.
- Blob storage: S3 / S3-compatible / Azure Blob (**GCS via S3-compatible** endpoint + HMAC, not a service account). Formats **Parquet (default), CSV, JSON, JSONL** (+gzip). Schedules **every 20 min / hourly / daily / weekly**. Modes: full history / from setup date / from custom date. Layout `{prefix}{project-id}/observations_v2/`, `/scores/`, `/manifests/{timestamp}.json`; completion signalled by manifest. **11 observation field groups** (core, basic, time, io, metadata, model, usage, prompt, metrics, trace_context, tools) for column selection. Config via `GET`/`PUT /api/public/integrations/blob-storage`. Parquet caveat: observation files **omit `input_price`/`output_price`/`total_price`** — use `cost_details`/`total_cost`. Availability: Hobby/Core ✗, Pro = Teams add-on, Enterprise ✓, **Self-hosted ✓**.
- MCP server at `https://cloud.langfuse.com/api/public/mcp`: "**Both read and write tools are available by default.**" Tool surface includes queryMetrics, getMetricsSchema, listObservations, listScores/**createScore**, dashboard + widget CRUD, evaluator/evaluation-rule CRUD, annotation queues, datasets, models, alerts, prompts, submitFeedback ([mcp reference](https://mcp.reference.langfuse.com)).
- CLI `npx @langfuse/cli api <resource> <action>` reads/writes traces, prompts, datasets, scores and dashboards — "keep dashboard definitions in version control and apply them across projects and environments."

**OTLP** ([opentelemetry](https://langfuse.com/integrations/native/opentelemetry)): ingest at `/api/public/otel`; trace path `/api/public/otel/v1/traces`. "Supports OTLP over HTTP with both `HTTP/JSON` and `HTTP/protobuf`. **`gRPC` is not supported yet.**" Requires `x-langfuse-ingestion-version: 4` — without it OTEL data "can be delayed by up to 10 minutes". Freshness otherwise "typically available for querying within **15-30 seconds**".

**Prometheus: not documented.** A full-text scan of 59 fetched Langfuse doc pages found **zero** occurrences of "Prometheus" or "Grafana" — no scrape endpoint, exporter, or Grafana integration. `[UNVERIFIED]`/absent.

**Self-hosting** ([self-hosting](https://langfuse.com/self-hosting), [pricing-self-host](https://langfuse.com/pricing-self-host)): core is **MIT, unlimited units**. OSS tier explicitly includes **Enterprise SSO + org-level RBAC**, custom dashboards, alerts (v4+), **scheduled S3 export**, and **Metrics/Observations API v2** free.
- EE license key (`LANGFUSE_EE_LICENSE_KEY`, on both web and worker) gates: **project-level** RBAC roles, protected prompt labels, **data retention policies**, audit logs, server-side masking, UI customization, organization creators, org management API + SCIM, instance management API. The RBAC boundary is org-level (free) vs **project-level** (EE).
- Architecture: Langfuse Web + Langfuse Worker; Postgres (OLTP), **ClickHouse (OLAP)**, Redis/Valkey (cache+queue), S3/blob. Traces batched by Web → written to S3 immediately → queued via Redis → ingested into ClickHouse by Worker; events persisted to S3 before DB writes for recoverability.
- Docker Compose: `docker compose up`, UI on :3000 — "lacks high-availability, scaling capabilities, and backup functionality". Kubernetes Helm requires **K8s ≥1.28** and, from chart **v2.0.0**, cert-manager + the ClickHouse Kubernetes Operator once per cluster; chart preflights ClickHouse CRDs and fails fast.
- SSO is **OIDC only** — "SAML is not supported" on Cloud. Conflicting signal: the self-hosted page description claims "custom OIDC/SAML providers". `[UNVERIFIED]` for self-hosted SAML.

**Retention** ([data-retention](https://langfuse.com/docs/administration/data-retention), [pricing](https://langfuse.com/pricing)): Cloud access windows **Hobby 30 days / Core 90 days / Pro 3 years / Enterprise 3 years**. Automatic retention management = Pro, Enterprise, self-hosted **EE** (Hobby/Core "Not Available"). Per-**project** only — "There is no organization- or team-level retention policy"; minimum **3 days**; nightly deletion keyed on Traces→`timestamp`, Observations→`start_time`, Scores→`timestamp`, Media→`created_at`. Adding a trace to a dataset neither protects it nor resets its window; **datasets and audit logs are unaffected**. Defaults: "Without a retention policy, Langfuse does not automatically delete event data… On self-hosted instances, data is stored **indefinitely** by default."

### 4.2 LangSmith

Docs now live at `docs.langchain.com/langsmith/*` (docs.smith.langchain.com 301s there).

**Data model** ([observability-concepts](https://docs.langchain.com/langsmith/observability-concepts)): "Every unit of work an agent performs… is recorded as a **run**"; a trace is the runs bound by trace ID; multi-turn sessions link as a **thread**; a **trajectory** is a flat, deduplicated, ordered message list. "If you are familiar with OpenTelemetry, you can think of a run as a span." **Hard cap: "Each trace is limited to a maximum of 25,000 runs"** — further runs for that trace are rejected. Run types (7): `chain`, `llm`, `embedding`, `prompt`, `tool`, `retriever`, `parser`.
- Threads: set a `metadata` key named `session_id` **or** `thread_id` — and critically "you must set the thread metadata on **all runs, including child runs** within a trace" or those runs are excluded from thread filtering and **token/cost aggregation**.
- User tracking: `ls_user_id` field = "The LangSmith user the run is attributed to". There is **no separate end-user dimension**.
- Model attribution via reserved `ls_provider` / `ls_model_name` metadata keys.
- **Tags and metadata are NOT propagated** parent↔child in either direction.
- **Release/version tracking is not first-class** — metadata convention only (the docs' own example group-by is `metadata.ls_model_name`).

**Filter language** ([filter-traces](https://docs.langchain.com/langsmith/filter-traces), [trace-query-syntax](https://docs.langchain.com/langsmith/trace-query-syntax)):
- UI: `field:operator value`, implicit AND, **scope selector ∈ Any run / Root run / Thread**. Each scope holds at most one row, so Threads/Runs accept three rows and Traces two.
- 11 operators: `is`, `is not` (`-field:value`), `matches` (`field:~value`, full-text), `does not match`, `wildcard` (`field:value*`), `exists` (`field:*`), `does not exist`, `comparison` (`>,>=,<,<=`), inclusive range `[10 TO 20]`, exclusive range `{10 TO 20}`, mixed range `[10 TO 20}`. Multi-value needs `OR` — "Listing several values inside one clause is not supported."
- Run fields include `latency`, `first_token_time`, `prompt_tokens`, `completion_tokens`, `total_tokens`, `prompt_cost`, `completion_cost`, `total_cost`, and **component breakdowns** `prompt_token_details.cache_read`, `completion_token_details.reasoning`, plus `feedback.<key>.<subfield>`.
- Thread fields: `thread_id`, `turn_count`, `num_errored_turns`, `thread_duration`, `trace_latency_p50`, `trace_latency_p99`, `total_tokens`, `total_cost`. Run fields like name/status/input are **not** available in Thread scope.
- API filter language (distinct): `filter`, `trace_filter` (root run), `tree_filter` (**any** run in tree), `select` projection, plus experimental natural-language `query`. Comparators `eq/neq/gt/gte/lt/lte/has/search`, logical `and`/`or`. Passing `select` and excluding inputs/outputs "can significantly improve query performance and reduce response sizes".
- Saved views: "**A view belongs to the tracing project rather than to the person who created it.**"

**Dashboards** ([dashboards](https://docs.langchain.com/langsmith/dashboards)): three systems — **Prebuilt** (auto per project), **Custom dashboards** (**Cloud US only**), **Custom dashboards (legacy)** (self-hosted + Cloud EU/APAC).
- Prebuilt sections: **Traces** (count, latency, error rates); **LLM Calls** (count and latency); **Cost & Tokens** (total and per-trace token counts and costs, **broken down by token type**); **Tools** (count, error rate, latency by tool name, top 5); **Run Types** (children of root, top 5); **Feedback Scores** (top 5 types; average for numeric, category counts for categorical).
- Custom chart metrics: Count; Latency over `latency_seconds` (Average, Percentile **p50 or p99**); Time to first token over `first_token_seconds` (p50/p99, Average); Tokens Total/Input/Output (Sum, Average, Percentile); Cost Total/Input/Output (Sum, Average, Percentile); Feedback score by key (Average/Min/Max); **Ratio** with numerator and denominator each a metric with its own filter — "Useful for error rate, LLM run share, etc."
- **Group-by attributes: Run Name, Run Type, Tag, Project, Metadata path, Feedback Label.** "Groups are ranked by frequency and capped at the **top 20**." Group-by and multi-metric are **mutually exclusive**; only **one** group-by attribute; donut/ranked-bar/table charts don't support multiple series.
- Filter scopes: default run level, or **Advanced → Trace filter** (root run) / **Tree filter** ("includes the entire trace tree if any run in it matches").
- Visualizations: Line, Stacked bar, KPI, Ranked bar, Donut, Table. Templates: Error rate over time, Average latency by model, Run volume, Token usage over time, Most expensive models. Cloning a prebuilt dashboard "converts its charts into fully editable custom charts".
- Machine contract ([create-chart](https://docs.langchain.com/langsmith/smith-api/charts/create-chart)): `CustomChartMetric` = run_count, latency_p50/p99/avg, first_token_p50/p99, total/prompt/completion_tokens, median_tokens, completion_tokens_p50, prompt_tokens_p50, tokens_p99, completion_tokens_p99, prompt_tokens_p99, feedback, feedback_score_avg, feedback_values, total_cost, prompt_cost, completion_cost, **error_rate**, **streaming_rate**, cost_p50, cost_p99.
- `RunStatsGroupBy` attributes are only **4**: `name`, `run_type`, `tag`, `metadata` (+ optional `path`, `max_groups` **default 5**).
- Alerts: metrics Run Count, Cost, Errors (count **or** percent), Feedback Score, Latency; operators `>=`/`<=`/exceeds; **aggregation window only 5 or 15 minutes**; channels native **Slack (Cloud only)**, **PagerDuty (Events API v2)**, **Dynatrace**, or any HTTP webhook ([alerts](https://docs.langchain.com/langsmith/alerts)).

**Cost tracking** ([cost-tracking](https://docs.langchain.com/langsmith/cost-tracking)): automatic from token counts + model prices for LLM calls, or **manual on any run type** (tool calls, retrieval, non-linear cost functions). Token counts via `usage_metadata` on run metadata or as a `usage_metadata` key in the traced function's **output** — "LangSmith will extract it from the output". Documented fields include `input_tokens`, `output_tokens`, `total_tokens`, `input_token_details: {"cache_read": N}`, and `input_cost`, `input_cost_details`, `output_cost`, `total_cost`.
- UI presents cost as **Input** (cache reads, text, image) / **Output** (reasoning, text, image) / **Other** ("Costs from tool calls, retrieval steps, or any custom runs").
- Custom prices via **model price map** API: `GET /api/v1/model-price-map` + create/update/delete.
- Non-linear pricing: Gemini 2.5 Pro stepwise is supported natively; "For any other model with non-linear pricing, calculate costs client-side and send them as `usage_metadata`."
- **Evaluator spend limits**: per-evaluator weekly cap with org default and per-evaluator override; "When weekly spend reaches the effective limit, LangSmith pauses the evaluator… until the limit resets at **Monday 12AM UTC**" ([online-evaluations-llm-as-judge](https://docs.langchain.com/langsmith/online-evaluations-llm-as-judge)).

**Evaluations**: feedback is "a tag and a score, bound to a run by a unique run ID" — continuous (numerical) or discrete (categorical) ([metric-type](https://docs.langchain.com/langsmith/metric-type)). Evaluators are **workspace-level** and "you can attach a single evaluator to multiple tracing projects and datasets", with sampling/filters/spend limits set **per attachment**. Types: LLM-as-a-judge, Code, **Composite score evaluators**, LangChain tuned evaluators.
- Online evaluation config = filter + **sampling rate** + optional **backfill** ("Apply to past runs from date" — "You can only backfill when you create the rule") + spend limit. **Retention side effect: "When an online evaluator runs on any run within a trace, the trace will be auto-upgraded to extended data retention."**
- Annotation queues: **Single-run** (with **assertions** for run items) and **Pairwise (PAQ)**; rubric feedback keys, optional default dataset, reviewer count/reservation length, "All workspace members review each run", reviewers cannot see each other's feedback.
- Experiments: "Each experiment captures outputs, evaluator scores, and execution traces for every example"; all experiments on a dataset are comparable side by side.
- Feedback is chartable (custom-chart metric Average/Min/Max by key; Feedback Scores prebuilt section; **Feedback Label** is a group-by attribute; Feedback Score is an alert metric).
- **Insights** ("automatically analyzes your traces to detect usage patterns, common agent behaviors, and failure modes") is **Plus/Enterprise only**.

**Exports/APIs** ([trace-query-syntax](https://docs.langchain.com/langsmith/trace-query-syntax)):
- Analytics: `POST /api/v1/runs/query`, `POST /api/v1/runs/stats` ("Get all runs by query in body payload"), `POST /api/v1/runs/group`, `POST /api/v1/runs/group/stats`.
- Dashboards/charts: `POST /api/v1/charts/create` + section/chart CRUD + `org-*` variants; `get-dashboard`, `get-tracing-project-prebuilt-dashboard`.
- Model price map CRUD; granular billable-usage endpoint with `kind=traces` | `kind=langsmith_deployments`, breakdown by workspace/project/user/API key, CSV export endpoint.
- **Bulk export**: trace data to an S3-compatible bucket in **Parquet**, matching the Run data format. Destinations via `POST /api/v1/bulk-exports/destinations`; jobs via `POST /api/v1/bulk-exports` with destination, project/`session_id` or `all_experiments`, time range, **field filtering**, and **filter expressions**; compression default `zstandard`; `format_version: v2_beta` for BigQuery-compatible timestamps. Output Hive-partitioned by export_id, tenant_id, session_id, resource, year, month, day. Scheduling via `interval_hours` (omit `end_time`); spawned exports carry `source_bulk_export_id` and cancelling the source does **not** cancel them. **72-hour runtime timeout**. Quotas: **250 creations/hour/workspace**, **200 active scheduled exports/workspace**.
- **Plan gating change**: "For customers who signed up after **August 3, 2026**, bulk export is only available on the LangSmith **Enterprise** plan. Customers who signed up on or before August 3, 2026, can use bulk export on Plus or Enterprise until **February 1, 2027**."
- **No CSV/JSON export of dashboard aggregates is documented.** `[UNVERIFIED]`/absent — a real gap vs Langfuse and Kong.
- Automations: item type (Runs or Threads) + filter + sampling rate + action ∈ {Add to Annotation Queue, Add to Dataset, Trigger Webhooks, Extend Data Retention}. Order within a rule: "1. Add to annotation queue. 2. Add to dataset. 3. Trigger webhook." Rules poll independently, so cross-rule ordering is not guaranteed. Retention extension per action is **admin-only**.
- MCP: Remote MCP at `https://api.smith.langchain.com/mcp` (OAuth 2.1 + Dynamic Client Registration, or `X-Api-Key`); self-hosted v0.15+ `https://<host>/api/mcp`. `fetch_runs` "Supports filters (`run_type`, `error`, `is_root`), FQL (`filter`, `trace_filter`, `tree_filter`), and ordering".

**OTLP** ([trace-with-opentelemetry](https://docs.langchain.com/langsmith/trace-with-opentelemetry)): `OTEL_EXPORTER_OTLP_ENDPOINT=https://api.smith.langchain.com/otel` with `OTEL_EXPORTER_OTLP_HEADERS="x-api-key=<key>"`; `Langsmith-Project=<name>` routes the project. **Path caveat**: the base URL must NOT include `/v1/traces`, because OTLP/HTTP exporters append it — `.../otel/v1/traces` becomes `/otel/v1/traces/v1/traces` and 404s. Self-hosted: replace host and append `/api/v1`. Process-level metadata via `OTEL_RESOURCE_ATTRIBUTES` — "ideal for any custom metadata you want to tag your traces with, such as a username, request ID, environment, or deployment version."

**Prometheus — supported, but for infrastructure metrics, not trace analytics** ([export-backend](https://docs.langchain.com/langsmith/export-backend)): self-hosted services expose Prometheus-format endpoints — backend `:1984/metrics`, platform-backend `:1986/metrics`, playground `:1988/metrics`, host-backend `:1985/metrics` (**control plane only**); "The frontend does not currently expose metrics." Overall: **"LangSmith self-hosted DOES expose Prometheus-format metrics, but they are infrastructure metrics, not trace analytics."** Agent Server additionally emits OTel metrics with an `lg_api_` prefix (override `METRIC_PREFIX`), scraped via `GET /metrics?format=prometheus`, with `EXPOSE_INTERNAL_METRICS_PROMETHEUS=true` for internal metrics, and Datadog OTLP push via `LSD_DD_API_KEY`. **No Prometheus endpoint for the trace-analytics data model (run counts, cost, latency, feedback) is documented.**

**Self-hosting** ([self-hosted](https://docs.langchain.com/langsmith/self-hosted)): "Self-hosted LangSmith is an add-on to the **Enterprise plan**" — **no open-source or free self-host tier**. Helm on Kubernetes, tested on **GKE, EKS, AKS**; installs PostgreSQL, Redis and **ClickHouse** in-cluster by default (external managed datastores recommended); `langsmithLicenseKey` in values; **"LangSmith requires egress to `https://beacon.langchain.com` for license verification and usage reporting. This is required for LangSmith to function properly."** Containers do not require root. Terraform bundles for AWS/Azure/GCP.
- Enterprise-gated: workspace **RBAC** with custom roles, **ABAC** (currently only `resource_tag_key`), **audit logs** (OCSF v1.7.0 API Activity Class UID 6003, `GET /api/v1/audit-logs`), Insights, bulk export (post-2026-08-03 signups).
- Cloud-only: native Slack alert notifications ("For self-hosted deployments, use the webhook Slack recipe"), static NAT IP allowlisting.
- Cloud has a **25 MB payload cap** (`413` above) and US/EU data regions.

**Retention** ([usage-and-billing](https://docs.langchain.com/langsmith/usage-and-billing)): only **two trace tiers — Base 14 days, Extended 180 days**. "Starting **September 14, 2026**, the maximum long-lived trace retention period for SaaS customers is changing to **180 days**." After retention: traces are inaccessible in UI/API, user data deleted shortly after, though "some metadata associated with each trace may be retained indefinitely for analytics and billing purposes."
- **Auto-upgrades to the pricier extended tier**: feedback via API/SDK with `extend_trace_retention=true` (the LangSmith **UI sends feedback WITHOUT extending retention**); online evaluators that score a trace; automation rules with retention extension (one matching run upgrades the whole trace; a Threads rule upgrades every trace in the matched thread). Manual annotation-queue adds do **not** upgrade by default. Experiments create runs at extended retention by default.
- Retention-independent: **Monitoring** "will continue to work even after a base tier trace's data retention period ends. It is powered by trace metadata that exists for **>30 days**." **Datasets have indefinite retention.**
- Billing meters both all traces and extended-retention traces separately, because a base trace recorded June 30 can be upgraded July 3, landing in a different billing period.
- Self-hosted TTL: Helm `config.ttl.enabled` + `config.ttl.ttl_period_seconds.longlived`/`.shortlived`; chart "defaults to **14 days** for `shortlived` and **400 days** for `longlived`". Tier precedence highest-first: (1) trace upgrade from a run rule, (2) project tier, (3) workspace default, (4) organization default, (5) `shortlived` fallback.
- Audit log retention: "up to **400 days**."
- Plan-tier ingest limits (trace events/hour): Developer no-payment 50,000 / Developer with payment 250,000 / Startup-Plus 500,000 / Enterprise custom; data ingest/hour 500 MB / 2.5 GB / 5.0 GB / custom.

### 4.3 Langfuse vs LangSmith — the comparison that matters

| Axis | Langfuse | LangSmith |
|---|---|---|
| Metric API | **`GET /api/public/v2/metrics`** — 4 views, 15 dims, 15 measures, **11 aggregations incl. p50–p99 + histogram**, 6 granularities | `POST /api/v1/runs/stats` — ~30 `RunStatsSelect` metrics, but **only 4 group-by attributes**, `max_groups` default 5 |
| First-class model/user/provider dims | ✅ `providedModelName`, `userId`, `sessionId`, `release`, `version`, `environment`, `promptName/Version`, `isRootObservation` | ❌ **no** first-class model/provider/user/session/release/environment — all metadata convention (`metadata.ls_model_name`, `ls_user_id`, `session_id`) |
| Group-by `userId`/`sessionId` | Filter-only, not groupable (high-cardinality); v4 forbids in time series | No such dimension at all |
| TTFT | `timeToFirstToken` measure + `streamingLatency`, `outputTokensPerSecond`, `tokensPerSecond` | `first_token_seconds` chart metric; `first_token_p50`/`p99` in run-stats API |
| Percentiles | p50/p75/p90/p95/**p99** | UI exposes only **p50/p99** for latency and TTFT (API enum has more) |
| Error rate | No dedicated measure — compose from `level` filter + count | **First-class `error_rate`** + a `Ratio` metric "Useful for error rate" |
| Aggregate export | ✅ Widget **CSV/JSON**, portable dashboard JSON, scheduled **Parquet/CSV/JSON/JSONL** to S3/Azure | Parquet bulk export to S3, but **no dashboard-aggregate CSV/JSON documented** |
| Prometheus | ❌ not documented (0 hits in 59 pages) | ✅ **but infrastructure metrics only**, not trace analytics |
| OTLP ingest | ✅ HTTP JSON/protobuf, **gRPC not supported**; needs `x-langfuse-ingestion-version: 4` or 10-min delay | ✅ `.../otel` (must not include `/v1/traces`); gRPC unstated |
| Self-host | **MIT core, free**, unlimited units, SSO + org RBAC + alerts + API v2 included | **Enterprise-only paid add-on**, license key + mandatory beacon egress |
| Retention | 30d / 90d / 3y by tier; per-project retention policy is **EE/Pro**; self-host default **indefinite** | **14d base / 180d extended**; auto-upgrade to pricier tier by evaluators/feedback/automations |
| Migration risk | **v4 cutover 2026-11-16** — trace-level evaluators deprecated; legacy `/metrics`, `/traces`, `/observations`, `/ingestion`, scores v2 all sunset | Bulk export moves to Enterprise-only for post-2026-08-03 signups (Plus keeps it to 2027-02-01) |

**Takeaway for a self-hosted gateway:** Langfuse is the stronger *analytics contract* (rich, documented, machine-readable metric/dimension/aggregation enums, portable dashboards, free self-host with the analytics API included) and is the more realistic thing to interoperate with or learn from. LangSmith is thinner on grouping/dimensions and gates its serious analytics behind Enterprise — but it ships two things Langfuse lacks: a **first-class `error_rate` metric** and **evaluator spend caps**. Neither exposes Prometheus metrics *for trace analytics*, which is precisely the gap a gateway's own `/metrics` endpoint fills.

---

---

## 5. Cloudflare AI Gateway

Cloudflare AI Gateway is the closest thing to a "reference implementation" of the minimum viable analytics surface: five dashboard metrics, a time filter, a GraphQL analytics API, a REST logs API, and export.

### 5.1 Dashboard charts / breakdowns

The dashboard shows exactly five metric families, filterable by time ([analytics](https://developers.cloudflare.com/ai-gateway/observability/analytics/)):

| Chart | What it measures |
|---|---|
| **Requests** | Total requests processed by AI Gateway |
| **Token Usage** | Token consumption across requests |
| **Costs** | Cost per AI provider — "track spending, manage budgets, optimize resources" |
| **Errors** | Error count across the gateway |
| **Cached Responses** | **Percentage of responses served from cache** |

Dimensions available in the GraphQL API for the `aiGatewayRequestsAdaptiveGroups` dataset: `model`, `provider`, `gateway`, and time buckets `datetimeMinute` / `datetimeHour`, ordered with `datetimeMinute_ASC`, filtered by `datetimeHour_geq` / `datetimeHour_leq`, with `count` as the aggregate ([analytics](https://developers.cloudflare.com/ai-gateway/observability/analytics/)).

> Note: the documented GraphQL example exposes only `model`, `provider`, `gateway` + time + `count`. **Cost/token/cache/error measures via GraphQL are not shown in the documented example** — the docs only say you "can use GraphQL to query your usage data." `[UNVERIFIED]` whether the dataset exposes cost/token/cache measures and per-user dimensions via GraphQL.

**User Insights dashboard** (a distinct page, all plans, no extra setup) — this is Cloudflare's per-user analytics layer ([user-insights](https://developers.cloudflare.com/ai-gateway/observability/user-insights/)):

Org-wide metrics: **Active users**, **Total requests**, **Adoption rate** (IdP identities with ≥1 request), **Tokens per active user** (median), **Median spend / active user**, **Top 10% request activity** (share of requests by most active users), **Users to review** (cost ≥2× median spend), **Identity coverage** (share of requests attributed to users).

Per-user view: **Spend**, **Requests**, **Tokens**, **Gateway cached requests**, **Errored requests**, **Cache hit rate**, **Sessions** (approx., from request metadata), **Top model**, **Top provider**, **Last seen**, **Active days**, **Identity coverage**.

**Anomaly detection** (documented, unusually concrete): baselines are computed **per session, not per request**; per user it uses the **p95 session cost over the last 30 days** (rolling). A session is flagged only when it exceeds **both** a relative threshold (**>2× the user's own p95 session cost**) **and** an absolute threshold (**above the org-level p99 session cost across all users**). Flagged users surface in a filtered view with triggering sessions and cost. **User Insights does not block requests.** ([user-insights](https://developers.cloudflare.com/ai-gateway/observability/user-insights/))

Also documented: **Log classification** classifies traffic by task and assesses whether the model matched the task ([log-classification](https://developers.cloudflare.com/ai-gateway/observability/log-classification/)).

### 5.2 Time range / filter / group-by

- Dashboard metrics "can be filtered by time" — specific presets (1h/24h/7d/custom) are **not enumerated in the docs** `[UNVERIFIED]`.
- Group-by is expressed via the GraphQL `dimensions { ... }` block; aggregation via `count`. Ordering via `orderBy: [datetimeMinute_ASC]`, pagination via `limit`.
- Logs filterable by custom metadata (see 5.3).

### 5.3 Custom metadata (the "custom properties" equivalent)

- Header: **`cf-aig-metadata`** — a JSON object, e.g. `{"team": "AI", "user": 12345, "test": true}`; also settable via the Workers binding `gateway.metadata` ([custom-metadata](https://developers.cloudflare.com/ai-gateway/observability/custom-metadata/)).
- Types: string, number, boolean. **Objects are not supported.**
- Limit: **5 entries per request**; extras are silently ignored ("only the first five will be saved") ([custom-metadata](https://developers.cloudflare.com/ai-gateway/observability/custom-metadata/), [limits](https://developers.cloudflare.com/ai-gateway/reference/limits/)).
- Reserved namespace: keys starting with `cf.` are reserved. With **Cloudflare Access**, AI Gateway injects the verified Access JWT `sub` claim as **`cf.user_id`** — a *verified* identity, not a self-asserted user id. Guaranteed to be saved even if it must evict the fifth custom entry. Service-token requests and requests with no user subject do **not** get `cf.user_id` ([custom-metadata](https://developers.cloudflare.com/ai-gateway/observability/custom-metadata/)).
- Metadata is searchable/filterable in logs and drives **spend-limit scoping**.

### 5.4 Caching analytics

- Cache disabled by default; enable globally via Settings (**Cache Responses**) or API (`cache_ttl`) ([caching](https://developers.cloudflare.com/ai-gateway/features/caching/)).
- Per-request headers: **`cf-aig-skip-cache`**, **`cf-aig-cache-ttl`** (min **60s**, max **1 month**), **`cf-aig-cache-key`** (custom key; also opts the request into caching).
- Cache status surfaced per response via response header **`cf-aig-cache-status: HIT | MISS`** ([caching](https://developers.cloudflare.com/ai-gateway/features/caching/)).
- Default cache key = SHA-256 over **provider + endpoint + model + provider auth header + full request body** (exact-match; any body difference = separate entry) ([caching](https://developers.cloudflare.com/ai-gateway/features/caching/)).
- **Cached Responses %** is a first-class dashboard chart ([analytics](https://developers.cloudflare.com/ai-gateway/observability/analytics/)).
- Cache is also a **Prometheus/OTEL label dimension** in Kong-style metrics (see Kong §6), but for Cloudflare the cache dimension appears only in the dashboard % chart and in log rows — `[UNVERIFIED]` whether cost is zeroed for cache hits in the cost chart.

### 5.5 Cost tracking + budgets

- Cost metrics "are only available for endpoints where the models return token data and the model name in their responses" ([costs](https://developers.cloudflare.com/ai-gateway/observability/costs/)). **This is an important caveat — cost tracking is response-shape dependent.**
- **Custom costs per request** via header **`cf-aig-custom-cost`** supporting `per_token_in`, `per_token_out`, `per_cache_read_token`, `per_cache_write_token`; unlimited decimal places; overrides public model costs. Providers differ in whether cache tokens are inclusive or separate, and AI Gateway **subtracts cache tokens before applying `per_token_in` when included**, to avoid double counting ([custom-costs](https://developers.cloudflare.com/ai-gateway/configuration/custom-costs/)).
- Custom costs render with an underline in logs so applied custom pricing is identifiable ([custom-costs](https://developers.cloudflare.com/ai-gateway/configuration/custom-costs/)).
- **Spend limits** = real cost-based budgets, distinct from rate limiting ([spend-limits](https://developers.cloudflare.com/ai-gateway/features/spend-limits/)):
  - Scoped by **provider**, **model**, and/or **custom metadata key** (e.g. `user_id`, `team`, `agent_id`, `environment`, or `cf.user_id`).
  - Two modes per dimension: **Split by value** (independent bucket per distinct value — e.g. every agent gets its own budget) and **Filter by value** (rule applies only when dimension equals a specific value).
  - **Up to 20 rules per gateway.** Rolling or fixed time window.
  - Enforcement: **blocks with `429`** until the window resets; alternatively pair with a **Dynamic Route** to **fall back to a cheaper model** instead of blocking.
  - Evaluation happens **before** dispatch, but limits are **eventually consistent** — the current request's cost is recorded after completion, so a concurrent burst can briefly exceed the limit.
  - Applies to both Unified Billing and BYOK requests **for models with known pricing**.
  - Documented limitation: "Cost tracking is a best-effort estimation based on token counts and model pricing. Refer to your provider's dashboard for exact billing amounts."
- Billing APIs exist for Unified Billing: credit balance, invoice history, invoice preview, spending limit, usage history, top-up config/limits/status ([Cloudflare OpenAPI `ai-gateway` tag](https://developers.cloudflare.com/api/resources/ai_gateway/methods/list/)).

### 5.6 Logs

Log rows can contain: **prompt, response, provider, timestamp, status, token usage, cost, duration, user agent**; plus DLP action + matched policy details when DLP fires, and Guardrails evaluation calls ([logging](https://developers.cloudflare.com/ai-gateway/observability/logging/)).

- Logging is **on by default** per gateway; can be disabled globally in Settings, or overridden per request.
- **`cf-aig-collect-log`** — inverse-override the gateway default for a single request.
- **`cf-aig-collect-log-payload`** — `true`/`false`, controls **payload (raw request/response body) storage only**. When `false`, payload storage is skipped but **metadata (token counts, model, provider, status code, cost, duration) is still logged**. This is the documented PII-control switch.
- DLP adds fields: DLP Action (`FLAG`/`BLOCK`), DLP Policies Matched, DLP Profiles Matched, DLP Entries Matched, DLP Check (`REQUEST`/`RESPONSE`/both) ([logging](https://developers.cloudflare.com/ai-gateway/observability/logging/)).

**Major 2026 change — logging moved to Workers Observability.** Customers who created a gateway **on or after September 24, 2026** follow **Workers Logs limits and Workers Logs pricing**; earlier customers stay on **Legacy Logs**. There is an account-level API to migrate: `PATCH /accounts/{account_id}/ai-gateway/logging-state` — *"Migrate AI Gateway logging to Workers Observability"*, with a matching `GET`. ([limits](https://developers.cloudflare.com/ai-gateway/reference/limits/), [Cloudflare OpenAPI](https://developers.cloudflare.com/api/resources/ai_gateway/methods/list/))

Legacy Logs limits: **500 logs/sec/gateway**, **10M logs stored (paid) per gateway**, **100k logs (free) per account**, **10 MB per log** ([limits](https://developers.cloudflare.com/ai-gateway/reference/limits/)).

> **Retention regression worth flagging — this is the single biggest argument for self-hosting.** Under Workers Logs, the **maximum log retention period is 7 days**; Free plan keeps **3 days**, Paid keeps **7 days**. Other Workers Logs limits: **5 billion logs/account/day** (past which a **1% head-based sample** is applied for the remainder of the day) and **256 KB max log size** (larger logs truncated, with `$cloudflare.truncated = true`). Workers Logs also supports a configurable **`head_sampling_rate`** (0–1, default 1) to sample logged requests. ([workers-logs](https://developers.cloudflare.com/workers/observability/logs/workers-logs/))
>
> So Cloudflare's migration path trades a **10M-log store per gateway** for a **7-day window** — a large reduction in historical depth. For a self-hosted gateway, "unlimited retention in your own database" is a direct, defensible advantage over the market leader's default. Note also that **aggregate analytics are unaffected by log retention** (the dashboard/GraphQL metrics are separate), so this is a *raw-log* limitation, not a metrics one. `[UNVERIFIED]` whether the Analytics dashboard itself has any retention cap.

### 5.7 Export + API endpoints (all verified in Cloudflare's OpenAPI spec)

**Logs** ([OpenAPI `AI Gateway Logs`](https://developers.cloudflare.com/api/resources/ai_gateway/methods/list/)):
- `GET /accounts/{account_id}/ai-gateway/gateways/{gateway_id}/logs` — List Gateway Logs
- `GET .../logs/{id}` — log detail
- `GET .../logs/{id}/request` — stored request body
- `GET .../logs/{id}/response` — stored response body
- `PATCH .../logs/{id}` — patch a log (used for feedback)
- `DELETE .../logs` — delete logs

**Analytics**: `POST https://api.cloudflare.com/client/v4/graphql` with the `aiGatewayRequestsAdaptiveGroups` query ([analytics](https://developers.cloudflare.com/ai-gateway/observability/analytics/)).

**Workers Logpush export** ([logpush](https://developers.cloudflare.com/ai-gateway/observability/logging/logpush/)):
- Exports logs to external object storage (e.g. R2) by selecting the **AI Gateway dataset**. Available on **Workers Paid**.
- **End-to-end encrypted**: per-log AES key encrypts the body; the AES key is RSA-encrypted with your uploaded public key (4096-bit RSA, OAEP/SHA-256). You decrypt with your private key. `Metadata`, `RequestBody`, `ResponseBody` are the encrypted fields.
- Limits: **4 Logpush jobs per account**, **1 MB per exported log**. Requires logs enabled on the gateway.

**OpenTelemetry export** ([otel-integration](https://developers.cloudflare.com/ai-gateway/observability/otel-integration/)):
- Exports **trace spans** to any OTLP collector; **JSON (default) or protobuf**.
- Exporter config: URL (required), Headers (optional), Authorization (Secrets Store reference), Content type.
- Span attributes (GenAI semconv): `gen_ai.request.model`, `gen_ai.model.provider`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `gen_ai.prompt_json`, `gen_ai.completion_json`, **`gen_ai.usage.cost`**.
- **Custom metadata is attached as span attributes** (keys beginning `gen_ai.` are reserved and dropped).
- Trace context propagation via **`cf-aig-otel-trace-id`** (32-char hex) and **`cf-aig-otel-parent-span-id`** (16-char hex).
- Documented backends: Honeycomb, Braintrust, Langfuse, Datadog, New Relic.

**Evaluations API**: datasets + evaluations + evaluation-types endpoints under the gateway path, plus human feedback via dashboard, API, and Worker bindings ([llms.txt index](https://developers.cloudflare.com/ai-gateway/llms.txt), [OpenAPI `AI Gateway Evaluations` / `AI Gateway Datasets`](https://developers.cloudflare.com/api/resources/ai_gateway/methods/list/)).

**Audit logs** for gateway config changes (create/delete/update) ([audit-logs](https://developers.cloudflare.com/ai-gateway/reference/audit-logs/)).

**Guardrails**: evaluation calls are recorded in logs; Guardrails analytics beyond that are `[UNVERIFIED]` ([guardrails](https://developers.cloudflare.com/ai-gateway/features/guardrails/), [logging](https://developers.cloudflare.com/ai-gateway/observability/logging/)).

**Self-hosting**: **not available** — Cloudflare AI Gateway is a Cloudflare-network-only service. ❌ for self-host.

---

## 6. Kong AI Gateway / Apache APISIX AI

These two are **API gateways with AI plugins**, not AI-native observability products. Both produce AI metrics and AI log fields, but neither ships a bundled "LLM cost dashboard" in the OSS distribution — they feed external analytics (Kong Konnect, or your own Prometheus/log stack). This distinction is the single most important framing point for both.

### 6.1 Kong AI Gateway

**Plugin-level logging config** (verified in Kong's machine-readable plugin spec, [`_api/plugins/ai-proxy.json`](https://developer.konghq.com/_api/plugins/ai-proxy.json) and [ai-proxy](https://developer.konghq.com/plugins/ai-proxy/)):
- `config.logging.log_statistics` (bool) and `config.logging.log_payloads` (bool). AI Proxy "records usage statistics of the configured LLM provider and model into your selected **Kong log plugin** output" — i.e. it writes into whatever logging policy you have (http-log, kafka-log, file-log, loggly, syslog, datadog, etc.), rather than into a built-in store.
- Optionally records all post-transformation request/response messages. `log_statistics` and `log_payloads` are independent — you can keep statistics while dropping payloads, structurally the same PII lever as Cloudflare's `cf-aig-collect-log-payload`.

**Prometheus metrics — `ai_llm_*` family** ([monitor-ai-llm-metrics](https://developer.konghq.com/ai-gateway/monitor-ai-llm-metrics/)). Exact names and labels:

| Metric | Type | Notes |
|---|---|---|
| `ai_llm_requests_total` | counter | AI requests per provider |
| `ai_llm_cost_total` | counter | **AI request cost per provider/cache** — first-class cost counter |
| `ai_llm_tokens_total` | counter | Label `token_type` = `prompt_tokens` \| `completion_tokens` \| `total_tokens` |
| `ai_llm_provider_latency` / `ai_llm_provider_latency_ms_bucket` | histogram | Provider latency |
| `ai_cache_fetch_latency` | histogram | Cache fetch latency |
| `ai_cache_embeddings_latency` | histogram | Cache embedding latency |

Shared labels: `ai_provider`, `ai_model`, `cache_status` (`hit`/empty), `vector_db`, `embeddings_provider`, `embeddings_model`, `request_mode` (`oneshot`/`stream`/`realtime`), `Workspace`, `consumer`. **`consumer` is the per-caller dimension** — Kong's user/key breakdown rides on the consumer model.

**GenAI OpenTelemetry metrics — `kong.gen_ai.*` namespace** ([ai-otel-metrics](https://developer.konghq.com/ai-gateway/ai-otel-metrics/)):
- `kong.gen_ai.llm.cost` (Sum, unit `{cost}`) — **populated only if `targets[].config.input_cost` and `output_cost` are defined on the AI Model**.
- Histograms: cache fetch time, cache embedding time, RAG fetch time, RAG embedding time, AWS Guardrails processing time; plus MCP metrics (response bytes, errors, ACL allowed/denied) and A2A metrics (requests, duration, response bytes, TTFB, errors, task-state transitions).
- Standard attribute set on AI metrics: `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.operation.name`, `kong.gen_ai.cache.status`, `kong.gen_ai.vector_db`, `kong.gen_ai.embeddings.provider`, `kong.gen_ai.embeddings.model`, `kong.workspace.name`, `kong.auth.consumer.name`, `kong.gen_ai.request.mode`.

**GenAI OTEL span attributes** ([llm-open-telemetry](https://developer.konghq.com/ai-gateway/llm-open-telemetry/)): spans carry inputs, model, outputs, usage, and tool/agent interaction attributes; exported via the OTEL or Zipkin policy to backends like Jaeger. Documented warning that `gen_ai.input.messages` / `gen_ai.output.messages` may contain PII/secrets.

**Model cost management** — the most sophisticated cost model of anything surveyed ([model-cost-management](https://developer.konghq.com/ai-gateway/model-cost-management/)). Pricing configured per **AI Model target**:
- `input_cost`, `output_cost` — **$ per 1M tokens**.
- `cache_read_cost`, `cache_write_cost` — per 1M cache-read / cache-write tokens.
- `cache_write_cost_list` — cache-write priced **by TTL tier** (`ttl: 5m|1h`, `cost`), for providers like Anthropic with tiered cache TTLs. Falls back to `cache_write_cost` if no TTL matches.
- `context_window_factor` — entries of `{above: 200k|1m, input_factor, output_factor}`; **the highest threshold crossed applies**, so a 2M-token request with tiers at 200k and 1m uses the 1m tier.
- `service_tier_factor` — `{tier: priority|flex, factor}`; matched **case-insensitively as a substring** of the provider's reported service tier, longest match wins; standard tier = 1. Factor multiplies the whole request (input, output, and therefore cache).
- Fallback rule: if an extended field is absent → scalar; if present but no entry matches → scalar.

**Konnect Observability** (the analytics product; **premium, Konnect-only**) ([observability](https://developer.konghq.com/observability/)):
- **Explorer** interface with a **dataset dropdown**: **API usage, LLM usage, platform usage, managed cache usage**. "Metrics and groupings will dynamically adjust based on the selected dataset." Explicit purpose includes "**monitor LLM token consumption and costs**." Output can be **saved as a custom report** ([explorer](https://developer.konghq.com/observability/explorer/)).
- **Custom Dashboards**: templates, **Export/Import JSON definition**, **Download as PDF** (reflects current time range and filters), sharing with users/teams at Viewer/Editor/Admin, provisioning via **API and Terraform**. RBAC-gated (Dashboard Viewer/Creator/Editor/Admin, Report Viewer/Creator/Editor/Admin). ([custom-dashboards](https://developer.konghq.com/custom-dashboards/), [custom-dashboards-reference](https://developer.konghq.com/observability/custom-dashboards-reference/))
- Data ingestion is toggleable per control plane (Observability **On** = basic + advanced; **Off** = advanced stops, basic API metrics remain).
- **Konnect MCP Server** and **KAi** can natively access metrics; there is a documented **metrics API endpoint** ([observability](https://developer.konghq.com/observability/)).
- RBAC caveat worth noting as a design lesson: Observability can't resolve Consumer data across member control planes of a control plane group and displays the Consumer as `(deleted)`.

**Metering & Billing** (Konnect, for monetization rather than internal analytics) ([metering-and-billing](https://developer.konghq.com/plugins/metering-and-billing/), [meter-llm-traffic](https://developer.konghq.com/ai-gateway/meter-llm-traffic/)):
- Emits **CloudEvents** usage events; docs explicitly state these "**aren't observability or analytics signals**."
- Per AI request it emits **two events** (request phase + response phase) correlated by `api_request_id`. AI fields: `type` (`request`/`response`), `model`, `provider`, **`tokens`** (prompt tokens on request events, completion tokens on response events), `cache_status`, `http_status`, `ai_plugin_id`, `ai_plugin_name`, `service_id`, `route_id`, `subject_type`.
- Subject (billable identity) resolvable from a **Consumer**, a Dev Portal application, or **any request header** (e.g. `x-customer-id`, `x-tenant-id`). **If the subject can't be resolved the event is dropped.**
- **Custom attributes** from header/query/path for pricing dimensions (department, project, priority).
- Buffered locally, batched, retried with exponential backoff; **stateless — does not persist events across restarts**; events undeliverable within the retry window are dropped.
- Important: **entitlement enforcement is NOT automatic** — "AI Gateway does not automatically block traffic when a customer's entitlement is exhausted." You wire a webhook or attach AI Rate Limiting Advanced yourself.

**Audit log reference** exists for AI Gateway ([ai-audit-log-reference](https://developer.konghq.com/ai-gateway/ai-audit-log-reference/)).

**Logs customization**: Kong documents rewriting the **Nginx template** to filter what reaches access logs (example: regex-stripping email addresses from `$request_uri`), applied via `proxy_access_log` / `KONG_PROXY_ACCESS_LOG`. Notably this only affects Nginx access logs, **not** the logging policies. ([ai-logs](https://developer.konghq.com/ai-gateway/ai-logs/))

**Self-hosting**: Kong Gateway (incl. AI plugins) is self-hostable. **Konnect Observability / Explorer / Custom Dashboards / Metering & Billing are Konnect-only** — so the *analytics product* is not self-hostable, only the metrics/log emission is. This is the crux for a self-hosted gateway: Kong gives you the pipes (Prometheus + OTEL + log plugins), not the dashboard.

### 6.2 Apache APISIX AI (`ai-proxy` / `ai-proxy-multi`)

**Log fields — the richest per-request LLM field set of any gateway surveyed.** APISIX exposes LLM data as **Nginx variables** and, when `logging.summaries` is enabled, also emits them in an **`llm_summary` log object** (names without the `llm_` prefix) consumable by `http-logger` / `kafka-logger` ([ai-proxy](https://apisix.apache.org/docs/apisix/plugins/ai-proxy/), [ai-proxy-multi](https://apisix.apache.org/docs/apisix/plugins/ai-proxy-multi/)):

| Variable | Meaning |
|---|---|
| `request_llm_model` | Model name **specified in the request** |
| `llm_model` | Model name **reported by the LLM response** |
| `request_type` | `traditional_http` \| `ai_chat` \| `ai_stream` |
| `llm_time_to_first_token` | Request send → first token received (ms). On an LLM error response, duration until the error arrived; **stays `0` if the LLM was never reachable** |
| `llm_prompt_tokens` | Prompt tokens |
| `llm_completion_tokens` | Completion tokens |
| `llm_total_tokens` | Prompt + completion |
| `llm_cache_read_input_tokens` | Input tokens read from cache |
| `llm_cache_creation_input_tokens` | Input tokens written to cache |
| `llm_reasoning_tokens` | Reasoning tokens generated |
| `llm_stream` | `true`/`false` |
| `llm_tool_count` | Number of tools provided in the request |
| `llm_has_tool_calls` | `true` when the response contains tool calls |
| `llm_end_user_id` | End-user identifier extracted from the request (e.g. OpenAI `user` field) |
| `llm_content_risk_level` | Content risk level from content moderation |
| `apisix_upstream_response_time` | Full upstream round trip (ms), including 429/5xx |

Plus standard Nginx upstream vars: `upstream_addr`, `upstream_status`, `upstream_response_time`, `upstream_connect_time`, `upstream_header_time`, `upstream_response_length`, `upstream_host`, `upstream_scheme`, `upstream_uri`.

Config knobs ([ai-proxy](https://apisix.apache.org/docs/apisix/plugins/ai-proxy/)): `logging.summaries` (request LLM model, duration, request and response tokens) and `logging.payloads` (request and response payload). Both explicitly "**do not affect `error.log`**."

**Prometheus LLM metrics** ([prometheus](https://apisix.apache.org/docs/apisix/plugins/prometheus/)) — exact names verified:

| Metric | Type | Notes |
|---|---|---|
| `apisix_llm_latency` | histogram | label `type` = **`total`** (full response latency, ai_chat and ai_stream) or **`ttft`** (time to first token, **ai_stream only** — non-streaming exposes no first-token moment) |
| `apisix_llm_prompt_tokens` | counter | Prompt tokens consumed |
| `apisix_llm_completion_tokens` | counter | Completion tokens generated |
| `apisix_llm_prompt_tokens_dist` | histogram | Prompt-token distribution → **p95 prompt size** |
| `apisix_llm_completion_tokens_dist` | histogram | Completion-token distribution |
| `apisix_llm_active_connections` | gauge | Live concurrency |
| `apisix_ai_cache_hits_total` | counter | label `layer` = **`exact`** or **`semantic`** |
| `apisix_ai_cache_misses_total` | counter | Looked up, not served |
| `apisix_ai_cache_bypasses_total` | counter | Bypassed lookup entirely |
| `apisix_ai_cache_embedding_latency` | histogram | Embedding round-trip in the semantic layer |
| `apisix_http_status` | counter | Carries `request_type` and `llm_model` labels for AI traffic |
| `apisix_bandwidth` | counter | Carries `request_type` / `llm_model` |
| `apisix_http_latency` | histogram | labels `type` = `request` \| `upstream` \| `apisix`; carries `request_type` / `llm_model` |

Shared LLM labels: `route`/`route_id`, `service`/`service_id`, **`consumer`**, `node` (for AI plugins this is the **LLM instance name**, e.g. `ai-proxy-openai`, not an IP), `request_type`, **`request_llm_model`**, **`llm_model`** (empty on cache hits, which never reach the LLM).

Two operational details worth copying:
- **Cardinality control**: plugin metadata `disabled_labels` collapses chosen built-in labels to `""` per metric (keyed by metric name: `http_status`, `http_latency`, `bandwidth`, `llm_latency`, `llm_prompt_tokens`, `llm_completion_tokens`, `llm_active_connections`, `llm_prompt_tokens_dist`, `llm_completion_tokens_dist`, and the `ai_cache_*` metrics). Structural labels (`code` on http_status, `type` on http_latency/bandwidth/llm_latency, `layer` on ai_cache_hits_total) **cannot** be disabled. Values collapse to `""` but the label stays registered so existing dashboards and `absent()` alerts keep working.
- **Label truncation**: `request_llm_model` and `llm_model` are derived from **client-supplied** model names and are **truncated to 128 bytes** to bound cardinality. If per-model breakdown isn't needed, disable both.

**No bundled AI analytics dashboard.** APISIX ships a Prometheus endpoint (`/apisix/prometheus/metrics`, default export server on port **9091**) and loggers; the LLM analytics dashboard is yours to build (Grafana etc.). The Apache APISIX Dashboard is a config UI, not an analytics product. ❌ bundled cost/token dashboard.

**Self-hosting**: APISIX is fully self-hosted OSS (Apache-2.0) — metrics and log fields are all available self-hosted. ✅

---

## Cross-cutting standard: OpenTelemetry GenAI semantic conventions

Both Cloudflare and Kong now emit `gen_ai.*` / `gen_ai.*`-namespaced telemetry, so the OTel GenAI conventions are the emerging interop contract. Verified from the current spec repo (the conventions **moved out of the main semantic-conventions repo** and are hosted at [open-telemetry/semantic-conventions-genai](https://github.com/open-telemetry/semantic-conventions-genai); the old URL [now redirects](https://opentelemetry.io/docs/specs/semconv/gen-ai/gen-ai-metrics/)).

**Stability status: `Development`** — not stable. The spec carries the disclaimer "These are initial Generative AI client metric instruments and attributes but more may be added in the future." Treat names as a good default, not a frozen contract ([gen-ai-metrics](https://raw.githubusercontent.com/open-telemetry/semantic-conventions-genai/main/docs/gen-ai/gen-ai-metrics.md)).

**Latency / duration metrics:**

| Metric | Type | Unit | Notes |
|---|---|---|---|
| `gen_ai.client.operation.duration` | Histogram | `s` | Client-side operation duration |
| `gen_ai.client.operation.time_to_first_chunk` | Histogram | `s` | **"SHOULD be reported for streaming calls and SHOULD NOT be reported otherwise"** |
| `gen_ai.client.operation.time_per_output_chunk` | Histogram | `s` | Measured end-of-previous-chunk → end-of-current-chunk |
| `gen_ai.server.request.duration` | Histogram | `s` | Server-side, time-to-last-byte |
| `gen_ai.server.time_to_first_token` | Histogram | `s` | Server-side |
| `gen_ai.server.time_per_output_token` | Histogram | `s` | Decode-phase performance |
| `gen_ai.invoke_workflow.duration` | Histogram | `s` | Workflow |
| `gen_ai.invoke_agent.duration` | Histogram | `s` | Agent |
| `gen_ai.invoke_agent.inference_calls` | — | — | Agent |
| `gen_ai.invoke_agent.tool_calls` | — | — | Agent |
| `gen_ai.execute_tool.duration` | Histogram | `s` | Tool |

Note the deliberate client/server split: `time_to_first_chunk` (client) vs `time_to_first_token` (server). Recommended duration bucket boundaries: `[0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92]` seconds.

**Token metrics** ([gen-ai-token-metrics](https://raw.githubusercontent.com/open-telemetry/semantic-conventions-genai/main/docs/gen-ai/gen-ai-token-metrics.md)) — counters (`{token}`):

| Metric | Meaning |
|---|---|
| `gen_ai.client.inference.usage.input_tokens` | Input (prompt) tokens, **including cached tokens** |
| `gen_ai.client.inference.usage.output_tokens` | Output (completion) tokens, **including reasoning tokens** |
| `gen_ai.client.inference.usage.cache_read.input_tokens` | Input tokens served from a **provider-managed** cache |
| `gen_ai.client.inference.usage.cache_write.input_tokens` | Input tokens written to a provider-managed cache |
| `gen_ai.client.inference.usage.reasoning.output_tokens` | Output tokens used for reasoning (CoT / extended thinking) |

Plus per-operation **histograms** `gen_ai.client.inference.operation.input_tokens` and `...output_tokens`. Note the explicit "including cached tokens" / "including reasoning tokens" semantics — the total counters are inclusive, and cache/reasoning counters are the breakdown. That is a design decision worth mirroring rather than inventing your own.

**Required attributes** on GenAI metrics: `gen_ai.operation.name` and `gen_ai.provider.name` are **Required**; `error.type` is Conditionally Required on error; `gen_ai.request.model` Conditionally Required if available; `gen_ai.response.model` and `server.address` Recommended. Canonical `gen_ai.operation.name` values include `chat`, `text_completion`, `embeddings`, `generate_content`, `invoke_agent`, `invoke_workflow`, `execute_tool`, `retrieval`, `plan`, `fetch_response`. Canonical `gen_ai.provider.name` values include `openai`, `anthropic`, `aws.bedrock`, `azure.ai.openai`, `azure.ai.inference`, `gcp.gemini`, `gcp.vertex_ai`, `gcp.gen_ai`, `cohere`, `deepseek`, `groq`, `mistral_ai`, `moonshot_ai`, `perplexity`, `x_ai`, `ibm.watsonx.ai`.

> `[UNVERIFIED]` Note: there is **no `gen_ai.*` cost metric in the OTel spec.** Cost is not part of the GenAI semantic conventions — Cloudflare invents `gen_ai.usage.cost` and Kong invents `kong.gen_ai.llm.cost`. Cost naming therefore has no standard; expect to define your own and document it.

---

## Design implications for a self-hosted gateway

Assessed against this workspace's gateway (`ARCHITECTURE.md`, `openapi.yaml`, `web/src/pages/`). The gateway already ships a real observability spine: `request_logs` (model/endpoint/status/latency/**ttft**/tokens/cost/stream/error/`cache_hit`), Prometheus `/metrics`, `/api/stats?range=`, `/api/logs`, `/api/logs/group`, `/api/logs/export`, `/api/keys/{id}/analytics`, response cache with `cache_status` hit/miss/bypass, per-key budget quotas + ledger, and an Analytics UI (Avg throughput, Avg TTFT, Gateway response cache, Retry, Upstream prompt cache) with a key modal (Spend, Tokens, Top models, Errors in range, Latency, Retry). Measured against the surveyed field, the gaps are:

**Gap 1 — No user / end-user dimension (highest-impact).** Every surveyed platform attributes usage to a human or agent identity: Helicone `Helicone-User-Id`, Portkey metadata `user`, LiteLLM `end_user` + `user`, Cloudflare `cf-aig-metadata` / reserved `cf.user_id` via Access, Langfuse/LangSmith user IDs, APISIX `llm_end_user_id` (extracted from the OpenAI `user` field, no extra plumbing). This gateway attributes only to `key`. Per-key ≠ per-user: one key can front many users.

**Gap 2 — No tags / custom-property dimension.** The universal group-by escape hatch. All six vendors ship one (Helicone `Helicone-Property-*`, Portkey `x-portkey-metadata`, LiteLLM `metadata` + `request_tags`, Cloudflare `cf-aig-metadata`, Langfuse `tags`/`metadata`, APISIX log fields). Gateway group-by is `model|status|key` only ([logs/group contract](internal/handler/log_filtering_test.go)).

**Gap 3 — No provider in group-by.** `provider` is a dimension in every surveyed dashboard (and in the gateway's own `gateway_requests_total{provider,...}` Prometheus label), but not an `/api/logs/group` option. Cheap fix, immediately useful for cost-per-provider.

**Gap 4 — Prometheus is missing cost, token and TTFT instruments.** Current: `gateway_requests_total{provider,model,endpoint,status}`, `gateway_latency_ms{provider,endpoint}`, `gateway_cache_hits_total{result}`. Missing, ranked by value:
1. a **cost counter** (Kong's `ai_llm_cost_total` analogue) — cost currently lives only in the DB and UI, so it cannot be alerted on in Prometheus;
2. **token counters** split prompt/completion (and cache-read/cache-write/reasoning, per OTel token metrics);
3. a **TTFT histogram** — TTFT is already captured per row but not exported;
4. **`cache_status` as a label on the request/cost counters** rather than only a separate `gateway_cache_hits_total`, which unlocks cost-by-cache-disposition and hit-rate-in-context in one query;
5. latency **percentiles** (the OTel bucket boundaries above are a ready-made choice).

**Gap 5 — OTLP export pending.** Phase 3 lists "OTel trace/OTLP (metrics API scaffold exists; exporter pending)". This is now table stakes across the field (Cloudflare OTLP traces with `gen_ai.usage.cost` + trace-context propagation; Kong `kong.gen_ai.*` OTLP metrics and GenAI span attributes; Langfuse/LangSmith as OTLP destinations). Shipping OTLP would let users skip your dashboard entirely and reuse theirs.

**Gap 6 — Budgets are per-key only.** Surveyed scope options include global, team, user, end-user, tag, model, provider, organization (LiteLLM), and Cloudflare's metadata-scoped budgets with dynamic **per-value** buckets. "Split by value" is the notable primitive: buckets are minted on demand from request metadata, so you get per-user/per-agent budgets without provisioning each one.

**Gap 7 — No scheduled reports / cost alerts.** Helicone (Reports, Alerts), Kong (Reports + dashboard PDF export), Langfuse/LangSmith (monitors/alerts), and Cloudflare (Users to review, anomaly flags) all push rather than only display. The gateway has an HMAC-signed webhook dispatcher already, so an over-quota/cost-threshold event is mostly wiring.

**Gap 8 — No session / trace grouping.** Helicone sessions + session paths, Langfuse sessions, LangSmith threads, Cloudflare approximate sessions. Relevant for agentic workloads where one logical task = many LLM calls.

**Already ahead of several vendors:** per-row `ttft` persistence, `cache_status` including `bypass` as a distinct state (not just hit/miss), streaming-aware caching with replay, and a ledger-backed quota with `429 over_quota_error`.

---

## Unverified / could not confirm

Consolidated. Everything here is either contradicted across sources, documented but absent from code/spec, or simply not published.

### Cross-product
- **Prometheus support for Helicone** — ❌ **now resolved as NO** (5,894-path repo scan found no metrics endpoint; only self-host *infrastructure* Grafana). Was previously unverified.
- **Helicone custom-property key/value length limits** — docs say "no restrictions" and "no limits", but give no byte cap; any HTTP-header ceiling is implied only.
- **APISIX cost tracking** — APISIX exposes token counts but **no cost field** in any documented log variable or metric. Whether cost is computable from APISIX alone is `[UNVERIFIED]`/effectively no.
- **Cloudflare Analytics dashboard retention** — log retention is documented (7 days), but whether the **aggregate metrics** have any retention cap is not.
- **Cloudflare GraphQL measures** — the documented `aiGatewayRequestsAdaptiveGroups` example exposes only `model`, `provider`, `gateway`, time and `count`. Whether cost/token/cache/error **measures** and per-user dimensions are queryable via GraphQL is not shown.
- **Cloudflare Analytics time-range presets** — "can be filtered by time" with no enumerated presets.
- **Cloudflare Guardrails analytics** — Guardrails evaluation calls are logged, but no guardrail dashboard/metrics are documented.
- **Kong Konnect Explorer LLM dataset metric names** — the Explorer "LLM usage" dataset is documented only in prose ("monitor LLM token consumption and costs"); its concrete metric and dimension names are JS-gated and were **not** obtainable. Kong's verifiable metric surface is the `ai_llm_*` Prometheus set and the `kong.gen_ai.*` OTel set.
- **Kong `deployment_state` / license-gated AI analytics** — `[UNVERIFIED]` whether any AI analytics beyond the listed metrics are Enterprise-gated.

### Helicone
- **Whether a cache-HIT row's `cost` is actually $0 at runtime.** Token counts are explicitly zeroed for cache hits, but `cost` is stored from `costBreakdown.totalCost` **with no cache guard**, while savings are computed in a separate `cache_metrics` ledger. Needs a live instance to confirm. Safe statement: *tokens zeroed; savings ledgered separately.*
- **No documented "Costs" page** — "costs" is a dashboard panel + `DashboardMode`; there is no `/costs` route. Any "Costs tab" claim is unsupported.
- **HQL availability tier** — "selected workspaces… Request Access", criteria unpublished.
- **Doc discrepancy:** the HQL page's related-card says "Reports — Build saved charts on top of your data", but the Reports doc and `reportsModal.tsx` implement **only scheduled email/Slack digests** — no custom-chart builder found.
- **Whether the AI Gateway component ships in the OSS repo** for self-hosting alongside Jawn — the self-host doc instructs you to run it but links **no OSS image**; `helicone-all-in-one` contains only dashboard + Jawn. `worker/src/lib/ai-gateway/*` exists in-repo but no published self-host image/instructions were verified.
- **Helm chart obtainability** — the README gates the "production-ready Helm chart" behind `enterprise@helicone.ai`, while the Kubernetes docs describe it publicly.
- **Model count mismatch** — docs claim "300+ models" for best-effort pricing; the registry API example shows `total: 150`.
- **Rich user analytics** (cohorts, retention tables, A/B testing, activation funnels, Power/Casual/New/At-Risk auto-segmentation, retry rate) appear on the user-metrics page with **no corresponding dashboard panel ID or API field** — treat as largely aspirational marketing.
- **User-selectable time-bucket granularity** — bucketing appears automatic (min/hour/day/week/month/year); no bucket control found in the time-filter component.

### Portkey
Named time-range presets · comparison-to-previous-period · **CSV export** (documented nowhere) · **p95 latency** · **TTFT as a metric** · **`GET /v1/logs`** · a documented Guardrail analytics page · a dedicated Alerts page (listed Pro+ in the plan table, absent from every docs index) · numeric self-hosted retention setting · first-party Datadog/New Relic/Grafana/S3 integrations · full `GET /logs/exports/field-restrictions` (documented but **absent from the spec**) · several Logs Export field names differ between prose and spec.
**Doc bugs to cite with caution:** the cost graph's schema descriptions say "latency"; retention is stated three contradictory ways across three pages.

### LiteLLM
- **No CSV from the analytics APIs or UI** — FOCUS is the only CSV path.
- **`input_cost_per_million_tokens` does not exist** — the cost map is per-token only. (Directly contradicts a common assumption.)
- **Code-only, no docs page:** `/spend/logs/v2`, `/spend/logs/session/ui`, `/global/spend/{keys,teams,models,provider,tags,logs,end_users}`, `/global/activity*`, the five `/…/daily/activity` endpoints, `/tag/user-agent/*`.
- **~10 Prometheus metric names exist in `prometheus.py` but appear in no docs table.**
- **Customer budget metrics** are documented but **absent from the 1.95.0 source scan** — likely newer than 1.95.0.
- `OTEL_TRACES_EXPORTER` is **not** a documented env var; the exporter variable is `OTEL_EXPORTER`.
- Province UI filter matrix beyond date range / customer dropdown / project-ID is undocumented; `/docs/proxy/organizations` **404s** (no documented Organizations UI page).
- GitHub `main` line numbers cited are 1.95.0 values and **may have drifted**.
- **Docs-vs-code conflicts flagged deliberately:** tag budgets and model-specific per-key budgets are labeled Enterprise in marketing docs but carry **no `premium_user` guard** in code.
- **LiteLLM version policy** (2026-06-29): only the **4 most recent stable minor lines** are supported — an operational constraint, not a docs ambiguity.

### Langfuse
- **Prometheus scraping — zero occurrences of "Prometheus" or "Grafana" across 59 fetched doc pages.** No scrape endpoint, exporter, or Grafana integration documented.
- **UI time-range presets** — no enumerated list; only a date picker and required `fromTimestamp`/`toTimestamp`.
- **Retention enforcement mechanics** beyond "nightly" (batching, exact scheduling, cutover timing).
- **v3 legacy metric/dimension names** — this report covers v4/v2 only.
- **Per-widget row limits and max charts per dashboard.**
- **Exact health/readiness endpoint paths** — page exists, not read.
- **SAML on self-hosted** — the self-hosted SSO page description claims "custom OIDC/SAML providers" while the Cloud page states "Langfuse supports authentication via OIDC only. SAML is not supported." The pages **conflict**.

### LangSmith
- **OTLP transport set** (HTTP/protobuf vs HTTP/JSON vs gRPC) not stated; the `grpc:` block on that page configures the **collector's** receivers, not LangSmith's endpoint.
- **Dashboard time-bucket granularity** — no enumerated list; docs mention per-chart "bucket size" overrides only.
- **Full `usage_metadata` schema** — the "Usage Metadata Schema" accordion rendered truncated; only fields visible in code examples are listed.
- **List prices / exact plan feature matrix** — the docs' pricing link resolves to a JS-rendered marketing site that was not machine-parsed. Plan *names* are cited; dollar amounts are not verified.
- **JSON/CSV export of dashboard or aggregate data** — no such feature documented.
- **Custom-dashboard parity** — the new experience is **Cloud US only**; EU/APAC Cloud and self-hosted use the legacy experience with fewer chart types and metrics.
- **`longlived` vs `extended` naming equivalence** — implied but never stated; the 400-day Helm default and the 180-day SaaS cap are also different numbers.
- **Self-hosted minimum Kubernetes and datastore versions** — requirement sections exist but exact numbers were not extracted.

---

## Universal table stakes

A feature qualifies only if it appears in **essentially every** product. The list is short and unglamorous — that is the point. Differentiators follow.

### The genuine table stakes

| # | Table stake | Evidence across products |
|---|---|---|
| **T1** | **Per-request cost in currency**, not just tokens | Helicone `cost` (`UInt64`, ×1e9); Portkey `AnalyticsMetrics.cost`; LiteLLM `LiteLLM_SpendLogs.spend`; Langfuse `totalCost`; LangSmith `total_cost`; Cloudflare Costs chart + `cf-aig-custom-cost`; Kong `ai_llm_cost_total` + `kong.gen_ai.llm.cost`. **APISIX is the one exception** — it logs tokens but has no cost field. |
| **T2** | **Token accounting split prompt vs completion** | Universal: Helicone, Portkey (`req_units`/`res_units`), LiteLLM, Langfuse (input/output), LangSmith, Cloudflare, Kong `ai_llm_tokens_total{token_type}`, APISIX `llm_prompt_tokens`/`llm_completion_tokens`. |
| **T3** | **Cache-read / cache-write tokens as a separate breakdown** | Now effectively universal and formalized in OTel (`…usage.cache_read.input_tokens`, `…cache_write…`). Helicone (`prompt_cache_read_tokens`/`prompt_cache_write_tokens`), Langfuse (`cached_tokens`), APISIX (`llm_cache_read_input_tokens`/`llm_cache_creation_input_tokens`), Cloudflare (`per_cache_read_token`/`per_cache_write_token`), Kong (`cache_read_cost`/`cache_write_cost`), LiteLLM. |
| **T4** | **Reasoning tokens as a separate breakdown** | Helicone (`reasoning_tokens` column + Requests-page column + alerts metric), Langfuse, LangSmith (`completion_token_details.reasoning`), APISIX `llm_reasoning_tokens`, LiteLLM `litellm_output_reasoning_tokens_metric`, Kong, OTel `…reasoning.output_tokens`. |
| **T5** | **Cache hit/miss status per request + a hit-rate metric** | Helicone (per-row `cache_enabled`/`cached` + a dedicated `cache_metrics` ledger + Cache page hit rate); Portkey (Cache tab, `cache_status`); LiteLLM (`cache_hit`, `cache_key`, `litellm_cache_hits_metric`); Cloudflare (`cf-aig-cache-status`, Cached Responses % chart); Kong (`cache_status` label); APISIX (`ai_cache_hits_total{layer=exact\|semantic}`, misses, bypasses). |
| **T6** | **Breakdown by model AND by provider** | Universal. Every product groups by both. (Notably **the local gateway's `/api/logs/group` supports `model` but not `provider`**.) |
| **T7** | **Some per-identity attribution dimension** | Universal, though the unit differs: Helicone `Helicone-User-Id`, Portkey `user`/`_user`, LiteLLM `user`/`end_user`/`team_id`/`api_key`, Langfuse `userId`, LangSmith `ls_user_id`, Cloudflare `cf.user_id` (verified Access `sub` claim), Kong `consumer`, APISIX `llm_end_user_id`. |
| **T8** | **Custom metadata/tags as a filter + group-by dimension** | Helicone `Helicone-Property-*` (unlimited, bloom-indexed map); Portkey `x-portkey-metadata`; LiteLLM `metadata`/`request_tags`; Langfuse `tags`/`metadata`; LangSmith tags/metadata path; Cloudflare `cf-aig-metadata` (**max 5**). Kong uses AI Consumer/Workspace + Metering custom attributes; APISIX exposes fixed log fields instead. |
| **T9** | **Time-range filter with time bucketing** | Universal. Granularity differs: Langfuse is most explicit (`auto/minute/hour/day/week/month`); Helicone `1h…1y` presets with **automatic** bucketing; Portkey documents no named presets. |
| **T10** | **Per-request records, retained and queryable** | Universal. Retention depth varies enormously — see T14. |

### Near-universal (one or two holdouts)

| # | Table stake | Coverage | Holdouts |
|---|---|---|---|
| **T11** | **Latency with percentiles** | Helicone (p50/p75/p95/p99/p99.9 — the **widest** set, with a `quantiles` dashboard panel); Langfuse (p50–p99 on every measure); LiteLLM (histograms); Kong (histogram); APISIX (`llm_latency{type=total}`); LangSmith (p50/p99 only); Portkey (**avg/p50/p90/p99 — no p95**). | Cloudflare documents only per-log `duration`; no percentile chart documented. |
| **T12** | **TTFT, streaming-only** | Helicone (dedicated `time-to-first-token` panel + `TFFT` column + filterable field); LiteLLM (`litellm_llm_api_time_to_first_token_metric`, streaming only); Langfuse (`timeToFirstToken` + `streamingLatency`); LangSmith (`first_token_seconds`); APISIX (`llm_latency{type=ttft}`, stream only); Kong (`request_mode`); Cloudflare (logged `duration`). Standardized in OTel (`gen_ai.client.operation.time_to_first_chunk`, explicitly "SHOULD NOT be reported otherwise"). | **Portkey** — no first-class TTFT metric, only a Prometheus approximation recipe. |
| **T13** | **Error / status-code tracking** | Universal. Portkey escalates guardrail verdicts into it via HTTP `446`/`246`; LangSmith has first-class `error_rate`; Langfuse notably **lacks** a dedicated error-rate measure (compose from `level` + count). | — |
| **T14** | **Documented retention beyond a week** | Helicone (analytics columns retained; **bodies TTL 3 months**); LiteLLM (your DB, indefinite; retention job is OSS); Kong/APISIX (your log store); Langfuse (30d/90d/3y; **self-hosted default indefinite**); LangSmith (14d/180d). | **Cloudflare: 3 days free / 7 days paid max.** Portkey: documented **three contradictory ways**. |
| **T15** | **Budget / spend limit with enforcement** | Portkey (hard `412`, non-retroactive, immutable once set); LiteLLM (`max_budget` + `budget_duration`, budget **reservation** on by default, optional `fail_closed_budget_enforcement` → `503`); Cloudflare (spend limits by model/provider/metadata, `429`, split-by-value buckets, 20 rules); Helicone (**`Helicone-RateLimit-Policy` with `u=cents`** — cost-based limiting scoped by custom property); Kong (AI Rate Limiting Advanced + Metering, but **enforcement is NOT automatic**). | **Langfuse and LangSmith are not spend-control products** — they alert, they don't block. |

### Explicitly NOT table stakes

- **Prometheus metrics** — present in LiteLLM, Kong, APISIX, and self-hosted Portkey Enterprise. **Absent from Helicone (confirmed), Cloudflare, Langfuse, and — for analytics — LangSmith.**
- **OTLP export/ingest** — common (Cloudflare egress, Kong egress, LiteLLM, Langfuse ingest, LangSmith ingest, Portkey Enterprise-only) but **absent from APISIX** (log plugins only) and **absent from Helicone** (OpenLLMetry is a one-way destination integration, not a collector).
- **Scheduled reports / digests** — Helicone (email/Slack; weekly/daily/monthly; XLSX dashboard export), LiteLLM (`spend_report_frequency`), Kong (reports + PDF dashboards), Langfuse/LangSmith (alerts). Not universal.
- **Guardrail analytics** — **effectively absent everywhere** (below).
- **Anomaly detection** — Cloudflare only, with a well-specified algorithm.
- **Evaluations / LLM-as-judge** — only Langfuse and LangSmith fully; Cloudflare thinly (evaluations API + human feedback); Helicone minimally (`POST /v1/request/{id}/score`, **integers/booleans only**, 10-minute aggregation delay).
- **SQL over analytics data** — Helicone **HQL** is the standout (ClickHouse SQL, 300k rows / 30 s / 100 qpm, signed CSV download, saved queries) — but it is **gated to "selected workspaces"**. No other product offers SQL.
- **FinOps-standard export** — LiteLLM **FOCUS v1.2** (Parquet/CSV to S3/GCS/Vantage, scheduled) is unique.
- **Cost margin / discount attributes** — LiteLLM's `gen_ai.cost.{discount_percent, margin_percent, margin_total_amount, original_cost}` are unique; everyone else models cost, nobody else models *margin*.

### Two structural insights worth acting on

**1. Nobody ships both dashboards *and* Prometheus for cost/tokens.**

| | Rich dashboard + analytics API | Prometheus cost/token metrics |
|---|---|---|
| **Cloudflare AI Gateway** | ✅ dashboards + GraphQL | ❌ GraphQL only |
| **Helicone** | ✅ dashboards + HQL/REST | ❌ **confirmed none** |
| **Portkey** | ✅ dashboards | ⚠️ self-hosted Enterprise only; **no SaaS endpoint** |
| **Langfuse** | ✅ dashboards + Metrics API v2 | ❌ zero Prometheus mentions in 59 pages |
| **LangSmith** | ✅ dashboards | ⚠️ **infrastructure metrics only** |
| **LiteLLM proxy** | ⚠️ basic Admin UI | ✅ the most complete metric set anywhere |
| **Kong AI Gateway** | ❌ analytics dashboard is Konnect-only, not self-hostable | ✅ `ai_llm_*` + `kong.gen_ai.*` |
| **Apache APISIX** | ❌ no bundled AI analytics dashboard | ✅ rich LLM metrics |

The analytics-native products expose REST/GraphQL + dashboards but **not** Prometheus. The infrastructure-gateway products expose Prometheus but **not** dashboards. A self-hosted gateway shipping **both** — a real dashboard *and* a complete Prometheus metric set including cost and tokens — occupies ground none of the eight holds. This gateway already emits `/metrics` and already has an Analytics UI, so it is closer to that position than any single competitor.

**2. Guardrails are a first-class feature everywhere and a first-class analytic almost nowhere.** Portkey ships extensive guardrails with **no guardrail metric, no pass/fail rate, no per-guardrail breakdown, and `guardrail` as neither a filter nor a group-by dimension** — a changelog claims a guardrail analytics dashboard with no docs, endpoint, or metric names. Cloudflare logs guardrail evaluation calls but documents no guardrail dashboard. Only LiteLLM ships guardrail metrics (`litellm_guardrail_requests_total`/`errors_total`/`latency_seconds`) and `guardrail_status` in its standard logging payload. **The data is captured market-wide; the analytics are missing.** Persisting guardrail verdicts as first-class columns plus a guardrail dashboard (pass/fail over time, which guardrail fired, per-check breakdown, deny vs transform, latency added) would be ahead of both Portkey and Cloudflare on a feature they market heavily.

> **Scope caveat:** Cloudflare AI Gateway and Portkey are primarily SaaS (Cloudflare cannot be self-hosted at all; Portkey's MIT OSS gateway has **zero** observability). For a *self-hosted* product the realistic analytics competitive set is **LiteLLM + Kong + APISIX + self-hosted Langfuse + self-hosted Helicone** (Apache-2.0, dashboard included).
