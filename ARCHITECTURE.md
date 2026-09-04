

# Unified AI Gateway — Architecture

> One domain to rule all AI APIs. Fast Go gateway + control plane.

## Vision
Single gateway (`https://ai.yourdomain.com`) that speaks **OpenAI**, **OpenAI Responses**, and **Anthropic** natively. Any client (OpenAI SDK, Anthropic SDK, Vercel AI SDK, LangChain, curl) just changes `baseURL` + uses a gateway `sk-gw-*` key. Gateway routes to configured upstream providers.

```
[Client: OpenAI SDK] ─┐
[Client: Anthropic SDK] ├─> [ AI Gateway :8080 ] ──> [ Provider: OpenAI ]
[Client: curl ] ───────┘       │  ├─ auth (gateway keys)         ├─> [ Provider: Anthropic ]
                               │  ├─ provider registry            ├─> [ Provider: Azure OpenAI ]
                               │  ├─ translation layer            └─> [ Provider: OpenAI-compatible (Groq, Ollama, etc.) ]
                               │  ├─ streaming proxy (SSE)
                               │  ├─ cache / retry / budgets (done)
                               │  ├─ metrics, audit, webhooks, RBAC (done)
                               │  └─ admin API + UI (embedded)
                               └─ SQLite (+AES-GCM at rest) or Postgres (beta)
```

---

## Snapshot — Where We Actually Are (v1.7.0)

| Phase | State | Evidence |
|-------|-------|----------|
| **Phase 1 — Foundation** | ✅ DONE | proxy + translate httptest suites, provider/keys CRUD, embedded UI |
| **Phase 1.5 — Model Intelligence** | ✅ DONE | models.dev catalog sync, aliases, enriched /v1/models, token/cost logging, health checker |
| **Phase 1.6 — Hardening** | ✅ DONE | versioned migrations (`db/migrations/001–009`), unified `httperr` envelope, audit_logs, config fail-closed in prod, openapi.yaml |
| **Phase 2 — Resilience & Control** | ✅ DONE | response cache (memory + Redis), retry/failover pre-commit, budgets + ledger enforcement, `/metrics`, `/ready` |
| **Phase 2.5 — Pre-Enterprise** | ✅ DONE | orgs + memberships, `RequireRole` enforcement, webhook dispatcher (HMAC-signed), passkeys + recovery, dashboard users/RBAC |
| **Future — Phase 3** | ⏳ PLANNED | OTel trace/OTLP export, Stripe billing, semantics-aware cache, Postgres soak-testing (dialect works today, labeled beta) |

The once-stubbed packages are real and wired in `cmd/gateway/main.go`: `cache`, `budget`, `audit`, `otel`, `lb`, `webhook`, `passkey`, `user`, `resilience`, `httperr`.

---

## Folder Structure

### Current (matches `internal/`)
```
ai-gateway/
  cmd/gateway/main.go          # entry: chi router, middleware chain, embedded UI + openapi.yaml
  internal/
    config/     # env loading (.env), production hardening, MASTER_KEY/JWT_SECRET derivation + persistent key files
    db/         # sqlite/postgres open, versioned migrations via schema_migrations (dirty-flag abort)
    db/migrations/  # 001_initial … 011_routing_strategies (embedded, run at boot)
    models/     # shared domain types (Provider, GatewayKey, RequestLog, CatalogModel, …)
    provider/   # Store CRUD + AES-GCM Encrypt/Decrypt + Resolve() + health.StartHealthChecker
    apikey/     # Generate (sk-gw- + 32B hex), Hash (SHA256), Verify, Prefix
    user/       # dashboard_users store: bcrypt hashes, roles, token_version revocation
    auth/       # JWT mint/verify, AdminMiddleware(+revocation), OIDC verification, passwords
    passkey/    # WebAuthn registration/login + recovery codes (handler/store/webauthn)
    budget/     # per-key quota limiter + middleware + UsageSink ledger
    cache/      # Cache interface: MemoryCache + RedisCache (REDIS_URL)
    resilience/ # RetryPolicy (5xx/429, pre-commit only)
    lb/         # load-balancer routing rules store (ordered provider groups)
    audit/      # audit_logs Recorder + request-auditing Middleware
    webhook/    # async HTTP dispatcher, optional HMAC signing (WEBHOOK_URL/WEBHOOK_SECRET)
    otel/       # Prometheus metrics (requests/latency/cache) + OTel metrics API scaffold
    httperr/    # unified error envelope (authentication/invalid/rate_limit/proxy/not_found)
    middleware/ # RequestID, Recovery, Logger, CSRF, GatewayAuth, rate limiters, RequireRole
    handler/    # admin.go (providers/keys/stats/logs/billing), catalog.go, discovery.go,
                # routing.go (lb rules), org.go, users.go, profile.go, session.go (cookies)
    proxy/      # reverse proxy, SSE, caching, retries, LB routing, stream usage inject
    translate/  # anthropic <-> openai <-> responses protocol translation
    discovery/  # provider model discovery (OpenAI/Anthropic /models, models.dev enrich)
    e2e/        # live E2E (build tag: live; GATEWAY_URL/GATEWAY_KEY/MODEL/ADMIN_PASSWORD)
  web/          # Vite + React + Tailwind dashboard source (built into embed.FS)
  scripts/      # smoke.sh, muse-harness.sh, mock upstreams
  ARCHITECTURE.md  README.md  openapi.yaml  Makefile  Dockerfile  docker-compose.yml
```

---

## Middleware Chain (actual, `cmd/gateway/main.go`)

```
RequestID -> Recovery -> Logger -> audit.Middleware -> forwardedHeaders -> securityHeaders -> CORS
  /health, /ready            (public; /ready checks db.Ping)
  /metrics                   (public; behind AdminMiddleware when METRICS_PROTECT=true)
  /api/*  -> CSRFProtection (cookie mutations)
             login/oidc     -> AuthRateLimiter (per account where identifiable, else IP)
             protected      -> AdminMiddlewareWithRevocation(JWT via Bearer or gw_token cookie)
             mutations      -> RequireRole(...) strict allowlist (see RBAC below)
  /v1/*   -> GatewayAuthWithJWTRevocation (sk-gw-* Bearer/x-api-key, or gw_token for Playground)
             budget.Middleware (daily/monthly token+cost quotas -> 429 over_quota_error)
             GatewayRateLimitWithLimits (fixed-window buckets per key: RPM/RPH/RPD + TPM)
  /*      -> NotFound -> serveWeb (embed.FS, SPA fallback to index.html)
```

- `forwardedHeaders` honors `X-Forwarded-*` / `CF-Connecting-IP` **only** from loopback peers or `TRUSTED_PROXIES` CIDRs (`"*"` = trust all). Rate limiting and logging see the real client IP.
- Rate limiting: in-memory `middleware.RateLimiter` uses **fixed-window buckets** with atomic all-windows-pass admission; when `REDIS_URL` is set, `RedisRateLimiter` (INCR+TTL sliding-window variant) shares the same windows across replicas and falls back to memory on Redis errors.
- `audit.Middleware` writes an audit row for every mutating `/api/*` request (actor from the verified JWT subject or gateway-key prefix — `X-Actor` headers are never trusted) and fans out to the webhook sink.
- `securityHeaders` sets nosniff / DENY / no-referrer / baseline CSP.
- CORS: permissive `*` by default (tunnel/dev convenience); `PUBLIC_URL` or `CORS_ALLOWED_ORIGINS` lock it down, and production refuses wildcard.

---

## Data Model

Authoritative DDL lives in `internal/db/migrations/001_initial.sql … 011_routing_strategies.sql` (embedded, applied at boot through `schema_migrations`; a failed migration marks the version **dirty** and boot aborts until an operator intervenes). Highlights:

- `providers` (AES-GCM `api_key_enc`, `base_url`, `health_status`, optional `org_id`)
- `gateway_keys` (prefix + SHA256 hash, RPM/RPH/RPD/TPM, daily/monthly token & cost limits, `allowed_models`, optional `org_id`)
- `request_logs` (model/endpoint/status/latency/ttft, tokens, cost, stream flag, optional bodies, error)
- `models_catalog`, `provider_models`, `model_aliases`, `system_config`
- `audit_logs` (actor, action, target, meta)
- `organizations`, `memberships` (org scaffold; org scope resolved from memberships)
- `lb_rules` (per-model provider groups: ordered members with `strategy`, `model_override`, `weight`)

SQLite (WAL, single-conn writes) is the default dialect; `postgres://` DSNs switch to lib/pq — functional but **beta** (not yet soak-tested).

---

## Translation Spec (stable since Phase 1)
| Inbound | Upstream OpenAI | Upstream Anthropic | Notes |
|---|---|---|---|
| OpenAI chat | passthrough | Anthropic translate: system+messages, tools->tool_use | stream: `data: {...}` |
| Anthropic messages | OpenAI translate: messages flatten, max_tokens default 1024 | passthrough (`anthropic-version: 2023-06-01`, `x-api-key`) | tool_call preserved |
| Responses API | Chat translate: `input`->messages, `instructions`->system, `reasoning.effort`->`reasoning_effort` | Anthropic translate | streaming supported |
| Models | aggregate from all providers + catalog enrichment + aliases | — | dedupe by id, alias as gateway-alias |

Multi-protocol providers (OpenCode Go/Zen): one entry serves chat, responses,
and messages models on one base URL + key. Each model speaks exactly one
upstream endpoint (Go: glm/kimi via chat, grok/gpt/muse-spark via responses,
qwen/minimax via messages; Zen: minimax via chat, claude via messages).
`/v1/messages` accepts `openai_compatible` multi entries natively;
`/v1/responses` routes messages-models via Responses→Anthropic; wrong-endpoint
calls fail fast with the correct POST path. Discovery merges both dialects;
health is up when either probe succeeds.

Reasoning mapping: OpenAI `reasoning_effort` (low/medium/high/max) <-> Anthropic `thinking.effort` or legacy `budget_tokens` via budgetToEffort heuristic; validated against catalog `reasoning_levels/limits`.

---

## Routing, Resilience & Metering (all wired)

- **Routing rules** (`lb`): per-model provider group with a **strategy** — `round_robin` (default; rotate evenly across requests), `random`, `weighted` (per-member weight 1–100, proportional pick), or `failover` (first healthy member in position order; later members only on retriable failure — the only strategy with cross-provider failover). Down members and open circuits are skipped where a healthy sibling exists; non-failover strategies serve each request with ONE member (a failing member returns its own error after same-provider retries). Members may carry a `model_override` — the model id rewritten onto the outbound request for that member. Unrouted bare model names are rejected with **404 `model_not_routed`**; `ROUTING_LEGACY_FALLBACK=true` restores the old heuristic resolution (provider-models ownership, name heuristics, default provider) as a migration escape hatch. Qualified `provider/model` IDs and `X-Provider:` pin requests bypass rules; aliases resolve first. Model lists for rules come from per-provider discovery (`POST /api/providers/{id}/discover` fetches the provider's `/models` API).
- **Retries**: `RETRY_MAX_RETRIES` (default 2) with exponential backoff on 5xx/429 — only while **nothing is committed**; streaming requests retry until the first byte is committed, then fail honestly.
- **Cache**: exact-match response cache (`X-Cache: HIT|MISS`), in-memory or Redis; `CACHE_TTL_SECONDS`.
- **Budgets**: per-key daily/monthly token + cost quotas enforced in `budget.Middleware` with a ledger (`429 over_quota_error`).
- **Usage metering**: `STREAM_USAGE_INJECT` (default **true**) injects `include_usage` so OpenAI-compatible streams are metered; opt out with `STREAM_USAGE_INJECT=0`.
- **Webhooks**: `webhook.Global` delivers audit + billing-export + over-quota events asynchronously (bounded queue, 2 attempts); `WEBHOOK_SECRET` signs deliveries as `X-Webhook-Signature: sha256=<hex HMAC>`.
- **Metrics**: Prometheus at `/metrics` — `gateway_requests_total{provider,model,endpoint,status}`, `gateway_latency_ms{provider,endpoint}`, `gateway_cache_hits_total{result}`; `METRICS_PROTECT=true` gates it behind admin auth. OpenTelemetry metrics API is scaffolded but OTLP export is pending.

---

## RBAC Reality (current)

- Roles: `admin | support | member | readonly` (`internal/user`). Unknown/empty roles normalize to least privilege.
- `RequireRole(roles...)` is a **strict allowlist**: a non-admin role passes only if explicitly listed — there is no member bypass. `readonly` can never mutate, even if somehow listed.
- Providers/keys writes are open to `admin|member|support` (support manages providers/keys); catalog mutations (`/api/models/sync`, aliases, settings), routing rules, discovery mutations, org writes and `/api/audit` are **admin-only**.
- Org claims in JWTs are resolved server-side from `memberships` (`orgClaimFor` → first membership). Client-supplied `X-Org`/`X-Organization-Id` headers are **ignored** — scope derives exclusively from the verified token.
- OIDC roles are also server-side only: existing users keep their stored role; new subjects provision as `member` unless listed in `OIDC_ADMIN_SUBJECTS`.
- Session revocation: password/role/disable/delete bumps `token_version`; logout revokes the caller's session; admin login is rate-limited per account (where identifiable) or IP.

---

## Phase Roadmap

### Phase 1 — Foundation — ✅ DONE
Providers (openai|anthropic|azure|openai_compatible) + AES-GCM + SSRF-guarded base URLs, gateway keys (shown once, hashed), chat/completions/embeddings/models/messages/responses proxy with SSE, translation layer, JWT admin auth + gw_token cookie, SQLite WAL, UI (Dashboard/Providers/Keys/Playground/Logs), /health.

### Phase 1.5 — Model Intelligence — ✅ DONE
models.dev ingestion + sync API, catalog search, virtual aliases, enriched `/v1/models`, token & cost tracking, per-key rate limits, provider health checker + discovery, Models Explorer UI.

### Phase 1.6 — Hardening — ✅ DONE
Versioned embedded migrations (`schema_migrations`, dirty-flag abort), unified `httperr` error envelope (4xx never retried), audit_logs + read API, config fail-closed for `ENV=production` (strong ADMIN_PASSWORD, explicit MASTER_KEY/JWT_SECRET, no wildcard CORS), request validation caps, `/ready` split from `/health`, openapi.yaml served at `/openapi.yaml`.

### Phase 2 — Resilience & Control — ✅ DONE
Response cache (memory + Redis adapter), retry with exponential backoff (pre-commit only), budget quotas + ledger, Prometheus `/metrics`, `/ready`, analytics rollups (`/api/stats?range=`), billing CSV export.

### Phase 2.5 — Pre-Enterprise — ✅ DONE
Organizations + memberships, dashboard users with roles + token_version revocation, `RequireRole` enforcement, passkeys (WebAuthn) + single-use recovery codes, HMAC-signed webhook dispatcher, `/api/admin/users` management API, profile activity/logins feeds.

### Phase 3 — Enterprise — ⏳ FUTURE (not started)
- **OTel trace export / OTLP** (metrics API scaffold exists; exporter pending)
- **Stripe billing** + invoice surfaces
- **Semantics-aware response caching** (beyond exact-match)
- **Postgres soak-testing** — the dialect toggle works today (budget backfills, catalog upserts, migration typing all dialect-fixed) but is labeled **beta**; SQLite remains the supported default
- Row-level org isolation tightening, SSO group→role mapping, multi-region notes

---

## Security Notes
- Gateway keys: `sk-gw-` + 32 bytes hex, SHA256 stored, prefix indexed, shown once, never logged. Accepted as `Authorization: Bearer` **or** `x-api-key`.
- Provider keys: AES-256-GCM with `MASTER_KEY` (64 hex chars). In dev an unset key is derived or persisted to a key file (`MASTER_KEY_FILE`, default `<db-dir>/.master_key`); **production requires an explicit MASTER_KEY** — rotating it without re-encrypting makes stored credentials undecryptable (see README → Operations).
- JWT: HS256 with `JWT_SECRET` — **required in production** (≥32 chars). In dev it is derived deterministically and persisted via the master-key/JWT-seed files so sessions survive restarts. Dashboard gets an HttpOnly `gw_token` cookie (Secure on https, SameSite=Lax); API clients use Bearer. Logout bumps `token_version`.
- CSRF: cookie-authenticated mutations must pass Origin/Referer host checks (`CSRFProtection`); Bearer-token API clients are unaffected.
- CORS defaults to permissive `*` (overridable via `PUBLIC_URL` / `CORS_ALLOWED_ORIGINS`); wildcard is rejected outright when `ENV=production` unless `ALLOW_INSECURE=true`.
- SSRF: `validateBaseURL` blocks cloud-metadata hosts (169.254.169.254, metadata.google.internal, 100.100.100.200), userinfo and newline injection on provider base URLs. (There is deliberately **no host allowlist env**; pinned outbound hosts are not configurable.)
- Brute force: login/passkey/recovery endpoints are rate-limited per account (where identifiable) or IP; disabled accounts are indistinguishable from wrong passwords.
- Audit: every mutating `/api/*` call is recorded with the verified actor; spoofable actor headers are ignored. Optional webhook fan-out is HMAC-signed.

---

## Gateway Key Auth (stable)
- Client sends `Authorization: Bearer sk-gw-...` or `x-api-key: sk-gw-...`.
- Middleware hashes incoming (SHA256), looks up by hash, checks revoked + token_version, updates last_used, sets `X-Gateway-Key-Prefix` (and `X-Gateway-Org`) in context.
- Budget check and rate limiting run right after auth on `/v1/*` and the bare convenience aliases (`/chat/completions`, `/completions`, `/embeddings`, `/messages`, `/responses`, `/models`).

---

## Success Criteria
- Phase 1 / 1.5 / 1.6 / 2 / 2.5 exit gates: **met** (see git history and `go test ./...`; live E2E gated behind the `live` build tag).
- Phase 3 will be done when: OTel traces export end-to-end, Stripe subscriptions meter usage, semantic cache shows measurable hit-rate gains, and Postgres passes a soak test with the same smoke suite as SQLite.

---

## Operational Runbook (pointers)
- `GET /health` (liveness + db + config_ok) vs `GET /ready` (503 when DB down); openapi at `/openapi.yaml`.
- Migrations run automatically at boot; a dirty flag aborts startup until resolved manually (see README → Operations).
- `REDIS_URL` shares cache + rate-limit state across replicas; the budget ledger remains per-instance/SQLite (single writer).
- `WEBHOOK_URL` (+ `WEBHOOK_SECRET`) fans out audit/billing/over-quota events; `OIDC_*` enables SSO; `LOG_RETENTION_DAYS` purges request logs nightly.
