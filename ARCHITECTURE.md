

# Unified AI Gateway — Architecture & Phase Plan

> One domain to rule all AI APIs. Fast Go gateway + beautiful control plane.

## Vision
Single gateway (`https://ai.yourdomain.com`) that speaks **OpenAI**, **OpenAI Responses**, and **Anthropic** natively. Any client (OpenAI SDK, Anthropic SDK, Vercel AI SDK, LangChain, curl) just changes `baseURL` + uses a gateway `sk-gw-*` key. Gateway routes to configured upstream providers.

```
[Client: OpenAI SDK] ─┐
[Client: Anthropic SDK] ├─> [ AI Gateway :8080 ] ──> [ Provider: OpenAI ]
[Client: curl ] ───────┘       │  ├─ auth (gateway keys)         ├─> [ Provider: Anthropic ]
                               │  ├─ provider registry            ├─> [ Provider: Azure OpenAI ]
                               │  ├─ translation layer            └─> [ Provider: OpenAI-compatible (Groq, Ollama, etc.) ]
                               │  ├─ streaming proxy (SSE)
                               │  ├─ cache / resilience (Phase 2)
                               │  └─ admin API + UI (embedded)
                               └─ SQLite + AES-GCM at rest → Postgres (Phase 3)
```

---

## Snapshot — Where We Actually Are

| Phase | State | Evidence |
|-------|-------|----------|
| **Phase 1 — Foundation** | ✅ DONE | `go test ./... ok`, proxy + translate httptest, smoke.sh live against mock upstream |
| **Phase 1.5 — Model Intelligence** | ✅ DONE (current) | 4619 models synced from models.dev, Aliases, Enriched /v1/models, token/cost logging, per-key RPM, health checker, Models Explorer UI |
| **Build** | ✅ GREEN | `go vet ./...` clean, `go test ./...` pass (2/2 packages with tests) |
| **Known debt before next phase** | ⚠️ 9 packages at 0% coverage, raw SQL migrations, in-memory rate-limit window lost on restart, no cache/budget/audit interfaces yet | Requires **Phase 1.6** buffer (see below) |

> **Conclusion audit:** No big missing *feature* before Phase 2 beyond debt hardening. But a short **Phase 1.6 Hardening + Phase 2.5 Pre-Enterprise** buffer must be inserted to avoid rework. No extra Phase 0/1 needed. Roadmap below is dependency-ordered and buildable straight through.

---

## Folder Structure

### Current (v1.5 reality)
```
ai-gateway/
  cmd/gateway/main.go          # entry, chi router, embedded UI, rate-limit rpmCache
  internal/
    config/     # env + MASTER_KEY/JWT_SECRET derivation, PORT/DATABASE_URL
    db/         # sqlite open/migrate (WAL, single-conn), schema + idempotent ALTERs
    models/     # Provider, GatewayKey, RequestLog, CatalogModel, ModelAlias, ProviderModel
    provider/   # Store CRUD + Encrypt/Decrypt (AES-GCM) + Resolve() + health.StartHealthChecker
    apikey/     # Generate (sk-gw- + 32B hex), Hash (SHA256), Verify, Prefix
    auth/       # MakeToken/VerifyToken HS256, AdminMiddleware, PasswordEqual
    proxy/      # proxyWithMetrics, costForProviderModel, validateReasoning, Chat/Completions/Embeddings/Models/Messages/Responses
    translate/  # Anthropic<->OpenAI, Responses->Chat, ExtractModel/IsStreaming/ExtractReasoningEffort
    middleware/ # RequestID, Recovery, Logger, GatewayAuth, GatewayRateLimit (sliding window)
    handler/    # admin.go (Providers/Keys/Stats/Logs), catalog.go (Catalog/Aliases/Settings), discovery.go (ProviderModels)
    catalog/    # Store: FetchAndSync models.dev, List/Get/GetByShortID, CostFor, parseReasoningOptions
    discovery/  # Service: Discover/DiscoversAll, List, upsert, Enrich, AddManual
  web/          # Vite + React + Tailwind, pages: Dashboard/Providers/Keys/Models/Playground/Logs, Signal Terminal theme
  scripts/      # smoke.sh, mock_upstream2.py, fix_ckff.sh
  ARCHITECTURE.md  README.md  Makefile  go.mod
```

### Planned evolution (no breaking renames, additive)
```
internal/
  cache/        # Phase 1.6 stub → Phase 2 impl  (interface Cache { Get/Set/Invalidate })
  resilience/   # Phase 1.6 stub → Phase 2 impl  (RetryPolicy, FallbackChain, CircuitBreaker)
  budget/       # Phase 1.6 stub → Phase 2 impl  (Daily quota check, cost rollup)
  audit/        # Phase 1.6 table + stub → Phase 3 expanded
  otel/         # Phase 1.6 stub → Phase 2 metrics + Phase 3 tracing
  db/migrations/# Phase 1.6 versioned migrations → Phase 2.5 postgres dialect
web/src/pages/
  Analytics.tsx # Phase 2
  Settings.tsx  # Phase 1.6 (system_config UI)
  Teams.tsx     # Phase 3 placeholder
```
*Existing imports stay valid — new packages are additive interfaces so Phase 2 never rewrites Phase 1.5 code.*

---

## Data Model Evolution

### Phase 1 SQLite (original)
```sql
providers(id TEXT PK, name TEXT UNIQUE, type TEXT, base_url TEXT, api_key_enc BLOB, created_at DATETIME)
gateway_keys(id TEXT PK, name TEXT, prefix TEXT, hash TEXT UNIQUE, last_used_at DATETIME, created_at DATETIME, revoked_at DATETIME)
request_logs(id TEXT PK, key_prefix TEXT, provider_id TEXT, model TEXT, endpoint TEXT, status INT, latency_ms INT, created_at DATETIME)
```

### Phase 1.5 additions (shipped)
```sql
providers.health_status TEXT, last_health TEXT
gateway_keys.rate_limit_rpm INTEGER DEFAULT 60
request_logs.prompt_tokens INT, completion_tokens INT, total_tokens INT, cost_usd REAL, is_stream BOOL
models_catalog(id TEXT PK, provider TEXT, name TEXT, description TEXT, family TEXT, context_window INT, max_output INT,
               input_cost REAL, output_cost REAL, cache_read_cost REAL, cache_write_cost REAL,
               reasoning BOOL, tool_call BOOL, structured_output BOOL, attachment BOOL, modalities TEXT,
               open_weights BOOL, knowledge_cutoff TEXT, updated_at DATETIME,
               reasoning_type TEXT, reasoning_levels TEXT, reasoning_output_limits TEXT)
model_aliases(alias TEXT PK, target TEXT, created_at DATETIME)
system_config(key TEXT PK, value TEXT, updated_at DATETIME)
provider_models(id TEXT PK, provider_id TEXT FK, model_id TEXT, display_name TEXT, owned_by TEXT,
                context_window INT, max_output INT, input_cost REAL, output_cost REAL,
                cache_read_cost REAL, cache_write_cost REAL, reasoning BOOL, tool_call BOOL, structured_output BOOL,
                attachment BOOL, modalities TEXT, source TEXT, created_at DATETIME, updated_at DATETIME,
                reasoning_type TEXT, reasoning_levels TEXT, reasoning_output_limits TEXT,
                UNIQUE(provider_id, model_id))
-- indexes: idx_gateway_keys_hash/prefix, idx_models_catalog_provider, idx_request_logs_model/created, idx_provider_models_*
```

### Phase 1.6 (hardening — DDL only, no breaking changes)
```sql
-- versioned migrations table (new)
schema_migrations(version INT PK, dirty BOOL)
-- audit before RBAC (need provenance for Phase 3)
audit_logs(id TEXT PK, actor TEXT, action TEXT, target_type TEXT, target_id TEXT, meta TEXT, created_at DATETIME)
-- budget prerequisites ahead of Phase 2 analytics (nullable, safe to add now)
ALTER TABLE gateway_keys ADD COLUMN daily_token_limit INTEGER;
ALTER TABLE gateway_keys ADD COLUMN daily_cost_limit_cents INTEGER;
ALTER TABLE gateway_keys ADD COLUMN monthly_cost_limit_cents INTEGER;
-- operational hardening
ALTER TABLE system_config ADD COLUMN description TEXT; -- optional, idempotent
```
*All Phase 1.6 migrations are additive + nullable → zero-downtime, no data loss, sqlite → postgres friendly (TEXT PK → UUID, DATETIME → TIMESTAMPTZ mapping noted in migration notes).*

### Phase 2 additions (Resilience & Control)
```sql
-- cache is external (Redis) or in-memory, no new table needed; optionally:
cache_entries(cache_key TEXT PK, body BLOB, status INT, headers TEXT, created_at DATETIME, expires_at DATETIME) -- only if opting for sqlite-backed cache stub
-- budget enforcement uses gateway_keys columns added in 1.6 + daily rollup view (no new table, query request_logs)
-- optional pre-aggregated rollup for fast analytics dashboard:
usage_rollups(bucket TEXT PK, -- YYYY-MM-DD:provider:model
              day TEXT, provider_id TEXT, model TEXT, prompt_tokens INT, completion_tokens INT, cost_usd REAL, req_count INT)
```

### Phase 2.5 (Pre-Enterprise)
```sql
-- org scaffold ahead of Phase 3 teams (nullable org_id on existing keys/providers for incremental adoption)
organizations(id TEXT PK, name TEXT UNIQUE, created_at DATETIME)
memberships(id TEXT PK, org_id TEXT FK, user_id TEXT, role TEXT, created_at DATETIME)
-- keep admin_users env-only until Phase 3, just scaffold
ALTER TABLE gateway_keys ADD COLUMN org_id TEXT REFERENCES organizations(id);
ALTER TABLE providers ADD COLUMN org_id TEXT REFERENCES organizations(id);
```

### Phase 3 (Enterprise)
```sql
-- Postgres dialect toggle (same schema, types adapted), row-level org isolation, SSO, billing source tables
-- billing_subscriptions, invoices, etc. out of scope for now but migration path reserved
```

---

## Middleware Chain

### Phase 1.5 actual
```
RequestID -> Recovery -> Logger -> securityHeaders -> CORS -> 
  /health (public)
  /api/*  -> AdminMiddleware(JWT via Authorization Bearer or gw_token cookie) -> handler
  /v1/*   -> GatewayAuth(sk-gw-*) -> GatewayRateLimit(sliding window 60s per prefix, rpm from DB via 30s cache -> 429 Retry-After) -> Proxy
  /*      -> NotFound -> serveWeb (embed.FS fallback to index.html)
```
- `GatewayRateLimit` currently in-memory; loses state on restart (documented debt, Phase 2 may add Redis-backed or DB-persisted window).
- `rpmCache` (sync.Map 30s TTL) avoids per-request DB hit.

### Phase 2 (adds, no reordering)
```
RequestID -> Recovery -> Logger -> securityHeaders -> CORS ->
  ... GatewayAuth -> BudgetCheck(daily/monthly limits) -> CacheLookup (if GET /v1/models or idempotent) -> GatewayRateLimit -> Resilience (Retry/Fallback/CircuitBreaker) -> Proxy -> AuditLog
                                             -> /metrics (OTel stub, Phase 1.6) -> Prometheus
```

---

## Translation Spec (stable since Phase 1)
| Inbound | Upstream OpenAI | Upstream Anthropic | Notes |
|---|---|---|---|
| OpenAI chat | passthrough | Anthropic translate: system+messages, tools->tool_use | stream: `data: {...}` |
| Anthropic messages | OpenAI translate: messages flatten, max_tokens default 1024 | passthrough (`anthropic-version: 2023-06-01`, `x-api-key`) | tool_call preserved |
| Responses API | Chat translate: `input`->messages, `instructions`->system, `reasoning.effort`->`reasoning_effort` | Anthropic translate | Store response.id mapping if needed |
| Models | aggregate from all providers + catalog enrichment + aliases | — | dedupe by id, alias as gateway-alias |

Reasoning mapping: OpenAI `reasoning_effort` (low/medium/high/max) <-> Anthropic `thinking.effort` or legacy `budget_tokens` via budgetToEffort heuristic; validation via catalog `reasoning_levels/limits`.

---

## Phase Roadmap (dependency-ordered, buildable straight through)

### Phase 1 — Foundation (MVP: providers, keys, proxy) — ✅ DONE
**Goal: prove the pipe.** Bring providers, create keys, hit gateway as OpenAI/Anthropic and get responses. *Do not revisit.*

**Shipped:**
- Provider CRUD (openai|anthropic|azure|openai_compatible), AES-GCM, base_url per-provider, SSRF block on metadata hosts, validateBaseURL
- Gateway keys sk-gw- + 32B hex, SHA256 hash, prefix index, list/revoke, last_used_at update
- Proxy endpoints with model-prefix / X-Provider hint / Default() health-aware ordering: chat/completions, completions, embeddings (OpenAI only), models (aggregate), messages, responses; SSE passthrough with flush
- Translation layer + streaming translators (anthropic->openai, openai->anthropic)
- Admin auth single ADMIN_PASSWORD → JWT HS256 (24h), cookie gw_token
- SQLite WAL, migrations idempotent, single-conn limiting, request_logs
- UI: Dashboard, Providers, Keys, Playground (SSE waveform), Logs — Signal Terminal theme
- Health: GET /health (db ping), GET /api/stats, GET /api/logs

**Exit gates (all met):**
1. POST /api/providers → 201, GET /api/providers lists them
2. POST /api/keys → sk-gw-* shown once, hashed at rest
3. curl /v1/chat/completions non-stream → 200 upstream passthrough
4. curl /v1/chat/completions stream:true → SSE
5. curl /v1/messages (Anthropic) via OpenAI upstream → translated 200
6. curl /v1/responses → translated 200
7. UI can do all without curl
8. go test ./... passes (proxy, translate httptest with mock upstream)

**OUT (intentionally deferred):** caching, retries/fallback, circuit breaker, quotas/billing, orgs/RBAC, OTel tracing, Postgres, guardrails — all stay OUT until 1.6/2.

---

### Phase 1.5 — Model Intelligence & Core Hardening (current) — ✅ DONE
**Goal: models become first-class.** Discover via models.dev, track costs, harden gateway daily DX. *Why 1.5 not 2: caching/retry are heavier; these are missing core pieces (can't pick model without limits/costs, can't bill without tokens, need aliases for DX).*

**Shipped:**
- models.dev ingestion: boot-sync if empty + POST /api/models/sync, RawProvider→CatalogModel, modalities JSON, fullID provider/model fallback, ~4619 unique
- GET /api/models/catalog?q=&provider=&reasoning=&limit=&offset=, GET /by-id, GET /status, GET/PUT /settings (system_config)
- Virtual aliases: model_aliases CRUD, resolveAlias() before Resolve(), opencode/ prefix stripping, gateway-alias in Models
- Enriched GET /v1/models: provider_models join first, then catalog enrichment, then live upstream fallback, then aliases
- Token & cost tracking: extractUsage (OpenAI + Anthropic + stream chunks), costForProviderModel (provider_models first, then catalog), request_logs extended, stats/logs totals
- Rate limiting: gateway_keys.rate_limit_rpm default 60, PUT /keys/{id}/rate-limit, GatewayRateLimit sliding window, 429 rate_limit_error
- Provider health: StartHealthChecker 5m ticker, /models probe with real key, health_status/last_health, UI dots
- Provider models: Discover (OpenAI + Anthropic fetch), provider_models table + enrich/manual flows, source enum
- UI 1.5: Models Explorer (provider/search, context/output/cost columns, reasoning badges, alias manager), Dashboard new cards, health dots, RPM editor, Playground autocomplete from provider_models + cost/context badge, Logs tokens/cost

**Validation present:** replaceModelInBody, reasoning validation (getReasoningConfig, validateReasoning checks effort + limits vs max_tokens), SSRF block on base_url.

**Exit gates (met):**
- GET /api/models/catalog?q=gpt&provider=openai&reasoning=true → 200 with costs
- POST /api/models/aliases {alias:fast,target:openai/gpt-4o-mini} → alias resolves in /v1/chat/completions
- GET /v1/models merges correctly and is not empty even with 0 providers (catalog fallback)
- Streaming + non-stream both log prompt/completion/cost correctly
- Rate limit: 61st req in 60s → 429 with Retry-After
- Health dots show up/down/unknown after 10s + 5m
- go test ./... still green

**No extra pre-phase needed before 1.5 — it is done.** *However, do not start Phase 2 until 1.6 gates pass (below).*

---

### Phase 1.6 — Hardening & Scaffolding Buffer — ⏳ REQUIRED BEFORE PHASE 2 (NEW, ~4–6 days, no user-facing features)
**Goal: close debt so Phase 2 never rewrites foundation.** This is the *only* missing phase you must add before smooth build out. Skip it and caching/retry will be built on untestable, unmigratable ground.

**Why it exists:** 9 packages have 0% test coverage, migrations are raw Exec (not versioned), error responses vary by endpoint, config silently uses weak defaults, audit trail missing before RBAC, and Phase 2 interfaces (Cache/Resilience/Budget) don't exist yet — Phase 2 would duplicate logic without them.

**IN — Scaffold only (interfaces, tables, tests, docs; no heavy runtime):**
1. **Build gates** (must be GREEN before leaving 1.6):
   - `go vet ./...` clean (already ✓ as of audit), `go test ./... -count=1` green, add tests to reach ≥30% on apikey/auth/config/provider/catalog/middleware/handler (httptest only, no live deps). CI runs `make test` on every push.
2. **Versioned migrations** (additive, no downtime):
   - Introduce `schema_migrations` table + ordered migration runner (or golang-migrate embedded). Port existing idempotent ALTERs into `001_initial.sql`, 1.6 DDL is `002_hardening.sql`. Document sqlite→postgres type map (TEXT PK → TEXT/UUID, DATETIME → TIMESTAMPTZ).
3. **Unified error envelope** (so resilience can classify):
   - All handlers use `writeJSONError(w, msg, type, code)` with `type ∈ authentication_error | invalid_request_error | rate_limit_error | proxy_error | not_found_error`. 4xx never retried, 5xx/429 retried.
4. **Validation layer**:
   - Request caps: keep 10MB global but add per-endpoint MaxBytesReader (e.g., /v1/models GET no body), model string ≤256 chars, alias regex `^[a-zA-Z0-9._/-]{1,64}$`, base_url SSRF allowlist doc. translate.ExtractModel fast-path already, add length check there.
5. **Config hardening**:
   - Fail-fast if `ADMIN_PASSWORD==admin123` && `ENV==production` (or at least ERROR log + /health exposes `config_ok:false`). Strength check on MASTER_KEY (32B hex) and JWT_SECRET (≥32 chars). Return explicit error on hex decode fail (already) + doc env table in README.
6. **Audit scaffold** (table only, minimal handler):
   - `audit_logs` table (see DDL) + `internal/audit` interface `Recorder { Log(actor, action, target, meta) }` with sqlite impl. Wire to PUT/POST/DELETE on /api/providers, /api/keys, /api/models/aliases. Not exposed in UI yet (Phase 3).
7. **Interface extraction** (no behavior change, just so Phase 2 drops in):
   - `internal/cache.Cache{ Get(key string)([]byte,int,http.Header,bool); Set(...); Invalidate(pattern string)}` — Phase 1.6 ships `NoopCache` + `MemoryCache` stub.
   - `internal/resilience.RetryPolicy{ ShouldRetry(attempt int, status int) bool; Backoff(attempt int) time.Duration }` + `CircuitBreaker{ Allow(providerID string) bool; Record(...)}`.
   - `internal/budget.Limiter{ Check(prefix string, promptTokens int) error; RecordUsage(...)}` stub reading gateway_keys.*limit columns added in same phase.
   - `internal/otel.Metrics{ IncRequests(...); ObserveLatency(...) }` stub + `/metrics` endpoint returning prometheus placeholder (later OTEL).
8. **DB abstraction doc** (not code churn yet):
   - Document that `sql.DB` usage is through `Store` interfaces and queries are portable (no sqlite-specific AUTOINCREMENT, use TEXT PK + uuid, no RETURNING until Postgres). Add `db.Dialect()` helper returning `sqlite` now.
9. **Docs**:
   - Generate OpenAPI snippet (openapi.yaml) for /v1/* and /api/* from handler routes (manual, not codegen yet). Add ENV table to README.

**OUT (stay deferred to Phase 2):** actual Redis wiring, retry loops, fallback chains, budget enforcement, analytics aggregates — only *interfaces* in 1.6.

**Folder scaffolding added in 1.6 (all contain "_stub.go" + interface + noop impl, compiles but not wired):**
- `internal/cache/cache.go` + `memory.go`
- `internal/resilience/retry.go` + `circuit.go`
- `internal/budget/budget.go`
- `internal/audit/audit.go`
- `internal/otel/otel.go`
- `internal/db/migrations/001_initial.sql`, `002_hardening.sql`
- `web/src/pages/Settings.tsx` (thin wrapper over /api/models/settings, behind admin auth)

**Exit gates (all must be true before Phase 2 starts):**
- [ ] `make test` green with new package coverage ≥30% and no new `go vet` warnings
- [ ] `schema_migrations` shows 002 applied on fresh + migrated existing DB (idempotent)
- [ ] audit_logs row appears after POST /api/providers (curl check)
- [ ] Cache/Resilience/Budget interfaces compile, tests use fakes, but live proxy behavior unchanged (curl before/after identical)
- [ ] README env table complete, /health returns `version:1.6.0` + db:up

*If you skip 1.6, Phase 2 will still "work" but you'll pay in rewrite churn when Redis latency forces cache-key redesign and when Postgres requires migration tooling — 1.6 is the smooth-build-out insurance.*

---

### Phase 2 — Resilience & Control — Depends on 1.6 Gates
**Goal: gateway survives upstream blips and gives operators control without second-guessing costs.** *Rescoped from old description: rate limiting & cost metering already shipped in 1.5, so Phase 2 does not repeat them — it adds the missing resilience primitives.*

**Depends on:** 1.6 gates (error envelope, migrations, interfaces, audit table). No other blocker.

**IN (build in order, parallel lanes noted):**
1. **Caching (lanes A)** — interface already in 1.6, now wire:
   - In-memory LRU for GET /v1/models (TTL 30s) + idempotent POST dedupe (cache by hash(model+messages) for non-stream, 10s TTL, opt-in header `X-Cache: hit`).
   - Redis adapter (env REDIS_URL) behind same Cache interface; if empty, fall back to MemoryCache. No body size >1MB cached. Invalidate on POST /providers/:id/discover.
   - UI badge: "cached" vs "live" on /v1/models + Playground.
2. **Retries / Fallback / Load balancing / Circuit breaker (lane A → B):**
   - Retry policy: 2 retries on 5xx/429/BadGateway only, exponential backoff 200ms→1s, respect Retry-After, never on 4xx. Tie to unified error envelope from 1.6.
   - Fallback chain: per-alias or per-config `fallbacks: [modelB, modelC]` stored in system_config or model_aliases extended (alias→CSV). Try next provider if circuit allows.
   - Circuit breaker: per provider, 5 failures in 60s → open 30s (health checker already pings, reuse). Expose via GET /api/providers (health_status=circuit_open).
   - Load balancing: when multiple providers serve same model_id (provider_models), round-robin among healthy ones (Resolve() already health-aware, extend to atomic counter).
3. **Budget quotas & metering (lane B, depends on 1.6 budget columns):**
   - Enforce daily_token_limit / daily_cost_limit_cents / monthly_cost_limit_cents per key (check in BudgetCheck middleware before proxy, update atomically after log). 429 over_quota_error.
   - Rollup job (hourly): materialize usage_rollups from request_logs for fast analytics; expose GET /api/stats?range=24h/7d/30d with tokens, cost, latency p50/p95, top models.
   - UI: new Analytics page (tokens/cost over time, top keys/models, error rate), budget editors on Keys page.
4. **Observability & guardrails (lane C, parallel):**
   - OTel: otel.Metrics now emits via OTEL_EXPORTER_OTLP_ENDPOINT if set; otherwise logs. Add trace IDs to proxy spans (RequestID propagates as traceparent).
   - /metrics (prometheus) with req_count, latency_histogram, upstream_error_count, cache_hit_rate.
   - Redaction: middleware redacts Authorization header and logs only prefix; future PII filter toggle in system_config (guardrails placeholder).
5. **Hardened health & docs:**
   - Readiness vs liveness: GET /health (liveness), GET /ready (checks db + provider probe cache). 
   - OpenAPI full publish at /openapi.yaml (expanded from 1.6 stub).

**OUT (stay deferred to 2.5/3):** orgs/teams/RBAC, SSO, multi-region, Postgres primary, billing invoices — not in Phase 2 even if tempting.

**Data changes:** usage_rollups (optional), no breaking column adds (budget columns added in 1.6, not here).

**Exit gates:**
- [ ] Non-stream repeat POST /v1/chat/completions with same payload within 10s returns X-Cache: HIT from MemoryCache (header test)
- [ ] Upstream returns 500 twice then 200 → client sees 200 after retries (mock upstream test)
- [ ] Provider A down (503) with fallback to B → client sees B’s response and log shows fallback_used
- [ ] 5 failures to provider → 6th request immediately returns circuit_open without hitting upstream (log)
- [ ] Key with daily_token_limit=100 rejects 101st token with 429 over_quota_error (httptest)
- [ ] GET /api/stats?range=7d returns correct 7-day rollup (seeded logs test)
- [ ] /metrics exposes histogram, /ready reflects db down

---

### Phase 2.5 — Pre-Enterprise Scaffolding — ⏳ BUFFER BEFORE PHASE 3 (NEW, ~3–4 days)
**Goal: make Phase 3 a small line extension, not a big bang.** Incrementally plant org/RBAC/Postgres seams while still running SQLite + single-admin live.

**Why it exists:** Jumping straight from single-admin + SQLite to teams + RBAC + Postgres would require rewriting auth, migrations, and multi-tenant isolation in one blast. 2.5 plants the nullable seams so Phase 3 only tightens them.

**IN (scaffold only, no UI for teams yet):**
1. **DB dialect abstraction:** formalize `db.Dialect()` switch, ensure Store interfaces accept `context.Context` (prep for Postgres tx), note pg type adaptors (TEXT→UUID, DATETIME→TIMESTAMPTZ). No Postgres deploy yet — still sqlite in prod, but new code compiles under both.
2. **Org scaffold:** create `organizations` + `memberships` tables (see DDL), add nullable `org_id` to providers/keys. All existing rows get org_id=NULL meaning "global". New providers can optionally include org_id if header X-Org present (admin only, no RBAC enforcement yet).
3. **RBAC stub:** define roles `"admin"|"member"|"readonly"` in code + middleware `RequireRole(...)` that currently no-ops (logs decision) but is wired to routes. Document matrix for Phase 3.
4. **Webhook dispatcher stub:** `internal/webhook.Dispatcher{ Emit(event string, payload any) }` noop, emit on audit_logs inserts; config via WEBHOOK_URL env if set.
5. **Provider/models multi-org query filter:** ensure List() respects org_id when set, otherwise global — tested but not exposed in UI yet.

**OUT:** real SSO, team UI, row-level enforcement, Postgres primary — all Phase 3.

**Exit gates:**
- [ ] Existing gateway boots with 2.5 migrations applied, old data unchanged (NULL org_id)
- [ ] New provider with X-Org header stores org_id, list filtered by org when queried with same header
- [ ] RequireRole logs but still allows request (stub pass)
- [ ] Webhook stub compile, dispatched events visible in logs when WEBHOOK_URL set

---

### Phase 3 — Enterprise — Depends on 2.5 Gates
**Goal: multi-tenant, compliant, deployable for teams.**

**Depends on:** 2 + 2.5 gates. Cannot start before org stubs exist (see above).

**IN (order matters):**
1. **Postgres support:** honoring DATABASE_URL postgres:// switches dialect to pg (migrations run via same `schema_migrations`, type adapt). Primary is Postgres if URL scheme is postgres://, else sqlite. No sqlite removal — keeps dev/test fast.
2. **Teams / RBAC / Org isolation:** Admin can create orgs + invite via POST /api/orgs, memberships enforced: keys/providers scoped to org, GET /api/* filtered, RequireRole enforced (admin can write, member can write providers/keys within org, readonly can read stats/logs only).
3. **SSO/OAuth:** provider config for OIDC (env OIDC_ISSUER, OIDC_CLIENT_ID), JWT now includes org + role; fallback to ADMIN_PASSWORD still works for bootstrap.
4. **Observability full:** OTel tracing (trace across gateway→upstream with span attributes model/provider/latency), Loki/Prom tails, slo dashboards.
5. **Multi-region & operational:** read-replicas note for Postgres, WAL → pg replication equivalent, rate limiter moves to Redis when REDIS_URL present, health includes redis.
6. **Billing & policy:** stripe/webhook emits for cost exceeding tier, usage exports (CSV), guardrails (PII redaction toggle, content filter interface).
7. **UI 3:** Teams page (org switcher, members, roles), SSO config, Billing exports, Region status.

**OUT (beyond 3 / future 4):** Vector DB prompt registry, fine-grained prompt templating marketplace, self-hosted model hosting — deliberately not in 3 to keep scope tight.

**Exit gates:**
- [ ] Two orgs: Alice sees only org A providers/keys/logs, Bob only org B (e2e header test)
- [ ] SSO login via mock OIDC → JWT contains org/role, AdminMiddleware enforces
- [ ] Postgres primary boots and passes same smoke.sh as sqlite
- [ ] RBAC matrix: readonly cannot POST /api/providers (403), member can, admin can delete org
- [ ] Webhook fires on cost overage and on audit event (captured by test sink)

---

## Dependency Graph (build order, no cycles)

```
Phase 1 (done) ──> Phase 1.5 (done) ──> Phase 1.6 (hardening buffer) ──> Phase 2 (resilience)
                                                        │                     │
                                                        └─> 2.5 (pre-enterprise scaffold) <─┘
                                                                        │
                                                                        v
                                                                     Phase 3 (enterprise)
```

*Parallel lanes inside Phase 2 (cache, resilience, observability) are safe to run in parallel once 1.6 completes because they touch disjoint packages (cache touches handler/proxy, resilience touches proxy/resolver, observability touches middleware/otel).*

**Smooth-build guarantee:** Following this order ensures no file is rewritten for a later migration type change, no schema is altered twice in same column, and no interface is defined twice.

---

## UI Design Direction (distinctive)
**Concept: "Signal Terminal"** — not generic dashboard. Dark graphite (#0F1311) with laboratory amber (#FFB84D) + signal teal (#2CF6B3) accents, off-white paper (#F8F6F1) cards, monospaced IBM Plex Mono for logs/keys, Fraunces or Inter for headings. Sidebar with large provider icons, key cards with typewriter reveal. Signature: live SSE stream visualization (amber dot pulsing, teal waveform) in Playground header.

**Page map by phase:**
- Phase 1: Dashboard (health, latency, provider dots), Providers (card grid + add drawer), Keys (reveal-once modal), Playground (dual-mode toggle, streaming chat), Logs (filterable table)
- Phase 1.5 adds: Models Explorer (search/filter, context/cost/reasoning badges + alias manager), Dashboard new cards, health dots, RPM editor, Playground catalog badge, Logs tokens/cost
- Phase 1.6 adds: Settings (thin system_config editor, env table) — no new theme
- Phase 2 adds: Analytics (tokens/cost over time, top models/keys, error rate histogram, p50/p95), budget editors on Keys, cache badges
- Phase 3 adds: Teams (org switcher, members, roles), SSO config, Billing/Exports, Region status — same Signal Terminal styling

---

## Security Notes
- Keys never logged, only prefix. Authorization header redacted in Logger.
- Provider keys: AES-256-GCM with MASTER_KEY (32B hex env). Derivation from ADMIN_PASSWORD is deterministic for dev only; prod must set MASTER_KEY (strength check in 1.6).
- JWT httpOnly for admin (cookie gw_token) + Bearer for API; HS256 with JWT_SECRET (≥32 chars, derived from ADMIN_PASSWORD if unset — 1.6 will warn on weak).
- CORS restricted (explicit allowlist in main.go), securityHeaders (nosniff/DENY/no-referrer), rate limit 429 with Retry-After, MaxBytesReader 10MB + per-endpoint tightening in 1.6.
- SSRF: validateBaseURL blocks metadata.google.internal/169.254.169.254/100.100.100.200, blocks userinfo, blocks newlines; allowlist note to be tightened via ALLOWED_PROVIDER_HOSTS in Phase 2.

---

## Gateway Key Auth (stable)
- Client sends Authorization: Bearer sk-gw-... or x-api-key: sk-gw-...
- Middleware hashes incoming (SHA256), looks up by hash, checks revoked, updates last_used, sets X-Gateway-Key-Prefix.
- Phase 2 adds BudgetCheck after auth, Phase 3 adds org scoping (key→org mapping).

---

## Success Criteria — Overall Done Definitions
- Phase 1 Done: § Phase 1 Exit gates (above) — ✅ all met
- Phase 1.5 Done: § Phase 1.5 Exit gates — ✅ all met
- Phase 1.6 Done: § Phase 1.6 Exit gates — MUST be met before Phase 2
- Phase 2 Done: § Phase 2 Exit gates — caching, retry, fallback, circuit, quotas, rollups, /metrics
- Phase 2.5 Done: § Phase 2.5 Exit gates — org scaffold with null-safe isolation
- Phase 3 Done: § Phase 3 Exit gates — multi-tenant + SSO + Postgres + RBAC matrix

---

## Operational Runbook (additions per phase)
- Phase 1.5: `MODELS_DEV_SYNC` via POST /api/models/sync; check GET /api/models/status for count/last_sync
- Phase 1.6: `GET /ready` vs `GET /health`; audit_logs retention query; openapi at /openapi.yaml
- Phase 2: REDIS_URL enables distributed cache/rate-limit; system_config `fallbacks.*` JSON for alias chains; usage_rollups cron enabled by default
- Phase 3: DATABASE_URL postgres:// enables Postgres; OIDC_* enables SSO; WEBHOOK_URL enables emits

---

## Risk Log & Why This Order De-risks
- **Cache invalidation wrong** → mitigated by 1.6 Cache interface with Noop first, then memory hit, then Redis — each gate requires header proof.
- **Retry amplifies upstream** → mitigated by unified error envelope (4xx never retried) + circuit breaker + 429 respect — defined in 1.6 before retry coded.
- **Budget enforcement races** → mitigated by adding limit columns in 1.6 while still no enforcement, then enforcing in 2 when enough data exists.
- **SQLite→Postgres big bang** → mitigated by 2.5 dialect abstraction + nullable org_id seam, so Phase 3 is toggle not rewrite.
- **RBAC without audit** → mitigated by 1.6 audit_logs so Phase 3 has provenance to filter by.

> **No other phases are needed.** If you want a “Phase 4” (Vector DB, prompt registry marketplace, hosted models), it sits after Phase 3 as a clean product extension — never before it.
