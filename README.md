# AI Gateway — Unified Go Gateway

> **One domain for every model.** Fast Go gateway + Signal Terminal UI. Bring OpenAI, Anthropic, Azure, Groq — expose OpenAI / Anthropic / Responses compat under one domain.

[![CI](https://github.com/karutoil/ai-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/karutoil/ai-gateway/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/Go-1.25-2CF6B3)

---

## Features

**Proxy**

✅ **Endpoints** — `POST /v1/chat/completions` (stream + non-stream), `POST /v1/completions`, `POST /v1/embeddings`, `GET /v1/models` (aggregated), `POST /v1/messages` (Anthropic compat), `POST /v1/responses`.  
✅ **Translation layer** — Anthropic `messages` ↔ OpenAI `chat`, Responses `input` → `messages`, tool-call schemas included.  
✅ **SSE streaming** — passthru flush, idle-timeout watchdog, usage injection option.

**Control plane**

✅ **Providers** — CRUD for `openai | anthropic | azure | openai_compatible`, AES-GCM encrypted keys, base URL per provider, auto-discovery of available models.  
✅ **Gateway Keys** — `sk-gw-…` (SHA256-hashed, prefix indexed, shown once), per-key RPM/RPH/RPD/TPM limits, daily/monthly token and cost quotas (`429 over_quota_error`).  
✅ **Auth** — dashboard password → JWT, **passkeys + recovery codes**, optional OIDC SSO, role-based access control scoped by org/team, session revocation on password/role change.  
✅ **Catalog** — 6k+ models synced on boot from `https://models.dev/api.json`; virtual aliases (`fast` → `openai/gpt-4o-mini`); rich `/v1/models` output (`provider/model` IDs).  
✅ **Cost tracking** — OpenAI + Anthropic usage parsed, catalog $/1M pricing, `GET /api/stats`, `GET /api/logs`.

**Resilience & routing**

✅ **Routing rules** — explicit curated provider groups per model with round-robin across requests and down-member skipping. **No silent failover by design**: qualified `provider/model` IDs and `X-Provider:` headers pin requests; retries hit the same provider only.  
✅ **Cache** — exact-match response cache (in-memory or Redis, `X-Cache: HIT|MISS`).  
✅ **Circuit breaker** — configurable fails/cooldown/half-open; tripped circuits short-circuit with `circuit_open`.  
✅ **Retries** — bounded exponential backoff on 5xx/429; streaming requests retry until the first byte is committed.

**Ops**

✅ **Observability** — Prometheus metrics (`/metrics`) plus an OpenTelemetry metrics API scaffold (OTLP export pending); structured request log with optional body capture and retention purge.  
✅ **Audit + webhooks** — audit trail plus optional webhook sink for billing export and over-quota events.  
✅ **Hardening** — refuses to boot with weak config when `ENV=production`; login/passkey/recovery endpoints are rate-limited per account (where identifiable) or IP; trusted-proxy allowlist.  
✅ **Health** — `GET /health` (liveness + db), `GET /ready`, `GET /openapi.yaml`.

---

## Quick Start

### Install a release (no Go needed)

Every push to `main` is versioned from commit history (conventional commits: `feat:` → minor, `fix:` → patch, `BREAKING CHANGE`/`!:` → major), built into a self-contained binary — web UI embedded, SQLite statically linked — and published as a [GitHub release](https://github.com/karutoil/ai-gateway/releases).

```bash
# interactive TUI: install (.env wizard) / update / uninstall / status
curl -fsSL https://raw.githubusercontent.com/karutoil/ai-gateway/main/install.sh | bash
# or: git clone && ./install.sh
```

The installer prompts for configuration with secure defaults (bare **Enter** accepts), can register a hardened systemd service, and `update` / `uninstall` never touch your `.env` and data unless you explicitly ask for a wipe. Scripted usage: `./install.sh install|update|uninstall|status` with optional `GATEWAY_VERSION`, `GATEWAY_INSTALL_DIR`, `GATEWAY_YES` overrides.

### Build from source

Requires Go 1.25+. `go build` needs **CGO with a C compiler (gcc/clang)** — the SQLite driver is `mattn/go-sqlite3`. On a bare container install `gcc` (Debian/Alpine: `apt-get install -y gcc` / `apk add gcc musl-dev`) or use the Dockerfile, which builds with CGO enabled. The repository ships prebuilt UI assets, so you don't need Node unless you want to modify the web UI.

```bash
git clone https://github.com/karutoil/ai-gateway && cd ai-gateway
go build -o bin/gateway ./cmd/gateway    # or: make build
ADMIN_PASSWORD=admin123 PORT=8080 ./bin/gateway
# -> http://localhost:8080  (UI)  +  http://localhost:8080/v1/* API
```

To rebuild the embedded UI after changing it: `make web` (runs `npm ci` + Vite build into `cmd/gateway/web`).

### Docker

```bash
cp .env.example .env                # then edit secrets
docker compose up --build           # data persisted to ./data
```

Or directly:

```bash
docker build -t ai-gateway .
docker run -p 8080:8080 -v gateway-data:/data --env-file .env ai-gateway
```

The image is Debian slim with CA certs, runs as non-root, and keeps SQLite at `/data/gateway.db` inside the volume. There is no in-image HEALTHCHECK — probe `GET /health` or `GET /ready` from your orchestrator.

Core env vars (full reference below):

- `PORT` (default `8080`)
- `ADMIN_PASSWORD` (default `admin123`; forbidden as-is when `ENV=production`)
- `DATABASE_URL` (default `./data/gateway.db`; `postgres://` enables **beta** Postgres — see below)

**Postgres (BETA):** `postgres://` DSNs activate the lib/pq dialect for core tables, but this path was only dialect-fixed in a recent changeset (budget counter backfills, some catalog upserts, migration typing). It is not yet soak-tested against long-lived Postgres instances — treat it as beta and prefer SQLite (`DATABASE_URL=./data/gateway.db`) as the supported default until then.

Full environment reference:

| Env var | Default | Meaning |
|---|---|---|
| `PORT` | `8080` | HTTP listen port |
| `DATABASE_URL` | `./data/gateway.db` | SQLite file path / `:memory:`; or `postgres://…` DSN (beta) |
| `ENV` | `` (dev) | Set to `production` to enforce hardening checks at boot |
| `ALLOW_INSECURE` | `false` | Explicit opt-out that overrides production checks (refuse-to-boot is the default) |
| `ADMIN_PASSWORD` | `admin123` (dev) | Bootstrap dashboard password; production requires ≥12 chars |
| `MASTER_KEY` | derived/persistent key file | 64-hex-char AES-GCM key encrypting provider credentials; required in prod |
| `JWT_SECRET` | derived/persistent seed file | Admin session signing secret (≥32 chars); required in prod |
| `REDIS_URL` | "" | `redis://…` shared cache + rate limiter; falls back to in-memory if unreachable |
| `PUBLIC_URL` | "" | Public origin (e.g. tunnel URL) used for CORS + logs |
| `CORS_ALLOWED_ORIGINS` | "" (= `*`) | Comma-separated origins overriding `PUBLIC_URL`; `*` allowed |
| `TRUSTED_PROXIES` | "" (= loopback) | Comma-separated IPs/CIDRs trusted for X-Forwarded-* / CF-Connecting-IP; `*` trusts all |
| `UPSTREAM_HEADER_TIMEOUT_SECONDS` | `120` | Dial+TLS+response-header deadline per upstream attempt |
| `REQUEST_TOTAL_TIMEOUT_SECONDS` | `0` | Overall request budget incl. retries (0 = disabled) |
| `STREAM_IDLE_TIMEOUT_SECONDS` | `300` | Max gap between stream chunks before watchdog aborts |
| `WRITE_HEADER_GRACE_SECONDS` | `60` | Write deadline for non-streaming responses |
| `SHUTDOWN_GRACE_SECONDS` | `90` | Graceful drain window for in-flight streams on SIGTERM |
| `CACHE_TTL_SECONDS` | `10` | Exact-match response cache TTL |
| `RETRY_MAX_RETRIES` | `2` | Retries on 5xx/429 for non-streaming requests |
| `RETRY_BASE_DELAY_MS` | `200` | Base backoff delay for retries (exponential) |
| `BREAKER_ALLOWED_FAILS` | `5` | Consecutive failures before a provider circuit opens |
| `BREAKER_COOLDOWN_SECONDS` | `30` | How long an open circuit waits before half-open probes |
| `BREAKER_HALF_OPEN_SUCCESSES` | `2` | Half-open successes required to close the circuit again |
| `LOG_BODIES` | `false` | Store request/response bodies in request logs (privacy-sensitive) |
| `BODY_LOG_MAX_BYTES` | `8192` | Cap on stored body bytes per request log row |
| `LOG_RETENTION_DAYS` | `0` (off) | Nightly purge of `request_logs` older than N days when >0 |
| `STREAM_USAGE_INJECT` | `true` | Inject `stream_options.include_usage` into OpenAI-compatible upstreams for billing accuracy; set `STREAM_USAGE_INJECT=0` to opt out |
| `METRICS_PROTECT` | `false` | Require admin auth on `/metrics` (enable behind hostile networks) |
| `WEBHOOK_URL` | "" | Audit/billing-export/over-quota webhook sink |
| `WEBHOOK_SECRET` | "" | HMAC-signs webhook deliveries as `X-Webhook-Signature: sha256=<hex HMAC-SHA256 of the raw body>` so consumers can verify authenticity |
| `OIDC_ISSUER` / `OIDC_CLIENT_ID` | "" | Optional SSO login via OIDC |
| `OIDC_ADMIN_SUBJECTS` | "" | Comma-separated OIDC subjects auto-granted the admin role on first login (bootstrap path); existing users keep their stored role |

---

## Using the API

```bash
# login — the dev boot bootstraps a dashboard user (admin / admin123),
# so the username must be included
TOKEN=$(curl -s -X POST http://localhost:8080/api/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"admin123"}' | jq -r .token)

# add provider
curl -X POST http://localhost:8080/api/providers \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"openai","type":"openai","api_key":"sk-...","base_url":"https://api.openai.com/v1"}'

# create gateway key (shown once!)
curl -X POST http://localhost:8080/api/keys \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"name":"my-key"}' | jq
# -> sk-gw-...

# use gateway as OpenAI
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-gw-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}]}'

# streaming
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-gw-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}],"stream":true}'

# Anthropic
curl http://localhost:8080/v1/messages \
  -H "Authorization: Bearer sk-gw-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-3-5-sonnet-20241022","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}'

# With SDKs — just change baseURL + use sk-gw-*
```

**SDK examples** in UI Dashboard & Playground.

---

## Model Routing & Load Balancing

Routing is **explicit and curated** — the gateway never silently switches providers on failure.

| Request form | Routing |
|---|---|
| Bare model (`"model": "gpt-4o-mini"`) | Follows the model's **Routing rule** if one exists; otherwise legacy resolution (health-aware round-robin among discovered owners → heuristic ownership → default provider). |
| Qualified ID (`"model": "openai/gpt-4o-mini"`) | **Pins** to that provider (by name, then by type). Always wins — even over a rule. |
| `X-Provider: <name\|id>` header | Hard pin, highest priority. |
| Alias (`"model": "fast"`) | Resolved via `model_aliases` first; rules may then match the resolved target. |

**Rules** map a model to an ordered provider group (Dashboard → *Routing*, or `GET/PUT/DELETE /api/lb/rules`):
- Each request selects **one** member via a rotating offset (round-robin across requests).
- Members marked `down` are skipped while others remain.
- **No automatic failover**: a failing member returns its own error after same-provider retries exhaust. Remove/reorder dead members in the UI.
- Members outside the group never receive traffic for that model.
- Same-provider retries are governed by `RETRY_MAX_RETRIES` / `RETRY_BASE_DELAY_MS`; a tripped circuit breaker still short-circuits with `circuit_open`.

## Production checklist
Set these **before** pointing real traffic at the gateway. With `ENV=production` the process refuses to boot on weak configuration (default `ADMIN_PASSWORD`, missing `MASTER_KEY`/`JWT_SECRET`, wildcard CORS) — `ALLOW_INSECURE=true` exists only as an explicit opt-out for throwaway environments.

**Required**

- [ ] `ADMIN_PASSWORD` — strong value, ≥12 characters (not `admin123`)
- [ ] `MASTER_KEY` — exactly 64 hex chars (32 bytes): `openssl rand -hex 32` (encrypts provider API keys)
- [ ] `JWT_SECRET` — ≥32 chars: `openssl rand -hex 32`
- [ ] `CORS_ALLOWED_ORIGINS` (explicit origin list) **or** `PUBLIC_URL` — wildcard CORS is rejected in production
- [ ] `DATABASE_URL` on a persistent volume (e.g. `/data/gateway.db` in Docker)

**Recommended**

- [ ] `METRICS_PROTECT=true` — gates `/metrics` behind dashboard auth whenever the port is reachable from hostile networks
- [ ] `TRUSTED_PROXIES` — defaults to trusting **loopback only**, which is correct for a `cloudflared` tunnel on the same host. Behind an external load balancer set your proxy IPs/CIDRs (comma-separated). `TRUSTED_PROXIES="*"` trusts every peer's forwarded headers — avoid unless network-isolated
- [ ] `LOG_RETENTION_DAYS` (e.g. `30`) — enables nightly purging of `request_logs`; logs grow fast with `LOG_BODIES=true`
- [ ] Redis (`REDIS_URL`) when running multiple replicas so cache/rate-limit state is shared
- [ ] Brute-force protection: `/api/auth/login`, `/api/auth/passkey/login/begin|finish` and `/api/auth/recovery/verify` are already rate-limited per account (where identifiable) or IP — don't front them with an LB that strips client IP

**Sessions after deploying:** all existing admin JWT sessions are invalidated on deploy by the token_version revocation upgrade — every user must log in again once. From then on, password changes, role changes, disabling and deleting a user revoke their outstanding sessions immediately.

---

## Operations

- **MASTER_KEY rotation:** the key encrypts every stored provider credential (AES-256-GCM). Rotating `MASTER_KEY` **without re-encrypting** the database makes all saved provider credentials undecryptable — proxy calls fail until each provider's key is re-entered. To rotate safely: re-enter provider keys (or re-encrypt the DB) under the new key before/with the change; never just swap the env var on a live data dir.
- **SQLite backups:** don't copy a live DB file alone. Either stop the gateway and copy `gateway.db` **including `-wal` and `-shm`**, or take an online snapshot: `sqlite3 data/gateway.db ".backup 'backups/gateway-$(date +%F).db'"`.
- **Upgrades / migrations:** at boot the gateway runs the versioned migrations in `internal/db/migrations/` (tracked in `schema_migrations`) automatically. A failed migration is flagged **dirty** and the process aborts on the next start — inspect the DB state, fix/roll back manually, then clear the `dirty` flag before restarting. There is no automatic down-migration.
- **Multi-replica caveats:** with `REDIS_URL` set, the response cache and rate-limit windows are shared across replicas. Everything else is per-instance/SQLite: the circuit-breaker state, in-memory limiter fallback, and the budgets/quota ledger live in the single SQLite DB — treat SQLite as the single writer (one gateway writing, others scaled read paths only) or move to Postgres (beta) before scaling out.
- **Metrics:** Prometheus exposition at `/metrics` (admin-gated when `METRICS_PROTECT=true`):
  - `gateway_requests_total{provider,model,endpoint,status}` — request counter with `2xx/4xx/5xx` status classes
  - `gateway_latency_ms{provider,endpoint}` — latency histogram
  - `gateway_cache_hits_total{result}` — cache hits vs misses

---

## Architecture

```
Client (OpenAI SDK / Anthropic SDK / curl)
   |  Authorization: Bearer sk-gw-*
   v
[ Chi Router :8080 ]
  ├─ /health, /ready, /metrics
  ├─ /api/*  (Admin JWT)
  │    ├─ POST /api/auth/login (+ passkeys, recovery, OIDC, logout)
  │    ├─ GET/POST/PUT/DELETE /api/providers (incl. discovery)
  │    ├─ GET/POST/PUT/DELETE /api/keys, /api/admin/users, /api/orgs
  │    ├─ GET/PUT/DELETE /api/lb/rules (routing rules)
  │    ├─ GET /api/models/catalog|aliases|settings, /sync (catalog)
  │    ├─ GET /api/stats, /api/logs, GET /api/audit
  │    └─ GET /*  (embedded UI)
  └─ /v1/*  (GatewayAuth: sk-gw-* + rate limits + budgets)
       ├─ POST /v1/chat/completions  ──┐
       ├─ POST /v1/completions        ─┤  translate? → resolve route/provider
       ├─ POST /v1/embeddings         ─┤  → decrypt provider key → proxy (SSE-aware,
       ├─ GET  /v1/models (aggregate) ─┤     retry, breaker, cache) → log usage/cost
       ├─ POST /v1/messages           ─┘
       └─ POST /v1/responses

Provider registry: SQLite + AES-GCM
```

See `ARCHITECTURE.md` for design details and history.

---

## Testing

```bash
make test         # go test ./... -v
make test-race    # full suite under the race detector
make lint         # golangci-lint (if installed)
make smoke        # live smoke: health/login/providers/keys/401 against a running gateway
make ci           # local parity with GitHub Actions: fmt-check + vet + build + race tests
```

Unit tests run entirely against mock upstreams (httptest) covering streaming, protocol translation, budgets, cache scoping, routing, and more. Live end-to-end tests against a real provider are gated behind the `live` build tag:

```bash
ADMIN_PASSWORD=admin123 PORT=8989 ./bin/gateway &
GATEWAY_URL=http://localhost:8989 make e2e-live
```

The target reads `GATEWAY_URL`, `GATEWAY_KEY`, `MODEL` and `ADMIN_PASSWORD` (`internal/e2e`); without `GATEWAY_KEY` it mints one via admin login.

There is also a tool-calling harness (`make harness-muse`, `cmd/harness-muse`) that exercises multi-turn + tool-calling behavior through the gateway with real upstreams — see `docs/muse-harness.md`.

---

## UI

- `web/` — Vite + React + Tailwind + React Router + Zustand
- Theme: Signal Terminal — graphite `#0F1311`, amber `#FFB84D`, teal `#2CF6B3`, paper `#F8F6F1`, IBM Plex Mono + Fraunces/Inter
- Pages: Dashboard, Providers, Models (catalog + aliases), Routing, Keys, Playground (SSE waveform, openai/anthropic/responses toggles), Logs, Analytics, Users, Teams, Profile, Settings
- Built with `make web`, output embedded into the Go binary via `embed.FS`

```bash
cd web && npm install && npm run dev   # dev on :5173 proxied to :8080
npm run build                          # production embeds
```

---

## Project Structure

```
ai-gateway/
  cmd/
    gateway/        # server binary + embedded web assets (embed.FS)
    harness-muse/   # live tool-calling harness
    harness/        # generic request harness
    test_translate/ # translation REPL/probe
  internal/
    config/         # env parsing, production hardening validation, persistence
    db/             # SQLite wiring, migrations (00x_*.sql)
    models/         # shared domain types
    provider/ apikey/ user/ budget/ catalog/ discovery/
    auth/           # JWT sessions, passwords, OAuth/OIDC bootstrap
    passkey/        # WebAuthn registration/login + recovery codes
    middleware/     # authn/z, rate limiting, roles, header hardening
    handler/        # admin control-plane routes + routing rules
    proxy/          # reverse proxy, SSE, caching, retries, breakers, LB routing
    translate/      # anthropic <-> openai <-> responses protocol translation
    httperr/ lb/ resilience/ webhook/ audit/ cache/ otel/
    e2e/            # live E2E (build tag: live)
  web/              # React dashboard source
  scripts/          # smoke.sh, dev tooling
  docs/             # operational guides
  ARCHITECTURE.md  openapi.yaml  Makefile  Dockerfile  docker-compose.yml
```

---

## Roadmap

- Postgres dialect soak-testing (works today but remains **beta** — prefer SQLite)
- Stripe billing, OTel trace export, mock-OIDC e2e suite
- Semantics-aware response caching

---

## Security Notes

- Gateway keys: `sk-gw-` + 32 bytes hex, SHA256 stored, prefix indexed, never logged.
- Provider keys: AES-256-GCM with `MASTER_KEY`.
- Set `ADMIN_PASSWORD` & `MASTER_KEY` & `JWT_SECRET` in prod; use `DATABASE_URL` persistent volume.
