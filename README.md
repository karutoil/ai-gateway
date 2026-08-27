# AI Gateway — Unified Go Gateway

> **One domain for every model.** Fast Go gateway + Signal Terminal UI. Bring OpenAI, Anthropic, Azure, Groq — expose OpenAI / Anthropic / Responses compat under one domain.

![Phase](https://img.shields.io/badge/Phase-2_Resilience-2CF6B3) ![Go](https://img.shields.io/badge/Go-1.25-2CF6B3) ![license](https://img.shields.io/badge/license-MIT-lightgrey)

---

## What's working

✅ **Models.dev sync** — 6k+ models auto-synced on boot from `https://models.dev/api.json`; `GET /api/models/catalog?q=gpt&provider=openai&reasoning=true`, `POST /api/models/sync`, `GET /api/models/status`.  
✅ **Virtual Aliases** — `POST /api/models/aliases {"alias":"fast","target":"openai/gpt-4o-mini"}`.  
✅ **Rich /v1/models** — `id` is `provider/model` (plus `model_id` short name) so harnesses can parse owner vs model. Aliases stay bare.  
✅ **Token & Cost tracking** — OpenAI + Anthropic usage, catalog $/1M, `GET /api/stats`, `GET /api/logs`.  
✅ **Rate limiting + budgets** — per-key RPM/RPH/RPD/TPM; daily/monthly token and cost quotas (`429 over_quota_error`).  
✅ **Resilience** — in-memory/Redis cache (`X-Cache: HIT|MISS`), 2 retries on 5xx/429 (non-stream), circuit breaker (configurable; `health_status=circuit_open`). **Provider failover is disabled by design** — each request is served by exactly one provider (retries hit the same provider only). Load distribution across same-model providers happens via explicit **Routing rules** instead.  
✅ **Playground** — session JWT can call `/v1/*`; reasoning/advanced options; cached vs live badge.  
✅ **Control plane** — Providers, keys, users, passkeys, teams/orgs, analytics, settings, admin JWT.  
✅ **Ops** — `GET /health` (liveness + `db`/`version`), `GET /ready`, `GET /metrics`, `GET /openapi.yaml`.

---

## What's working (Phase 1)

✅ **Providers** — CRUD for `openai | anthropic | azure | openai_compatible`, AES-GCM encrypted keys, base URL per provider.  
✅ **Gateway Keys** — `POST /api/keys` → `sk-gw-...` (hashed SHA256, prefix indexed), list/revoke, `Authorization: Bearer sk-gw-*` for all `/v1/*`.  
✅ **Proxy + Streaming** — SSE passthru flush, non-stream JSON.  
✅ **Endpoints:**
- `POST /v1/chat/completions` (stream + non-stream)
- `POST /v1/completions`
- `POST /v1/embeddings`
- `GET  /v1/models` (aggregated)
- `POST /v1/messages` (Anthropic compat, translates ↔ OpenAI as needed)
- `POST /v1/responses` (OpenAI Responses, native or translated to chat)
✅ **Translation layer** — Anthropic `messages` ↔ OpenAI `chat`, Responses `input` → `messages`.  
✅ **Admin auth** — `ADMIN_PASSWORD` → `POST /api/auth/login` → JWT, protected `/api/*`, cookie + Bearer.  
✅ **SQLite** — WAL, migrations, `data/gateway.db` (or `DATABASE_URL=:memory:`), request logs.  
✅ **Beautiful UI** — React + Vite + Tailwind, Signal Terminal theme (graphite/amber/teal), Dashboard / Providers / Keys / Playground (SSE waveform) / Logs. Embedded into Go binary.

---

## Quick Start

### 1. Build

```bash
make web      # builds UI into cmd/gateway/web
make build    # go build -o bin/gateway ./cmd/gateway
# or
go build -o /tmp/gateway ./cmd/gateway
```

### 2. Run

```bash
ADMIN_PASSWORD=admin123 PORT=8787 ./bin/gateway
# or
ADMIN_PASSWORD=admin123 PORT=8787 go run ./cmd/gateway
# -> http://localhost:8787  (UI)  +  http://localhost:8787/health
```

Env:
- `PORT` (default 8080)
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
| `STREAM_USAGE_INJECT` | `false` | Inject `stream_options.include_usage` into OpenAI-compatible upstreams for billing accuracy |
| `METRICS_PROTECT` | `false` | Require admin auth on `/metrics` (enable behind hostile networks) |
| `WEBHOOK_URL` | "" | Audit/billing-export/over-quota webhook sink |
| `OIDC_ISSUER` / `OIDC_CLIENT_ID` | "" | Optional SSO login via OIDC |

### 3. E2E Use

```bash
# login
TOKEN=$(curl -s -X POST http://localhost:8787/api/auth/login \
  -H "Content-Type: application/json" -d '{"password":"admin123"}' | jq -r .token)

# add provider
curl -X POST http://localhost:8787/api/providers \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"openai","type":"openai","api_key":"sk-...","base_url":"https://api.openai.com/v1"}'

# create gateway key (shown once!)
curl -X POST http://localhost:8787/api/keys \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"name":"my-key"}' | jq
# -> sk-gw-...

# use gateway as OpenAI
curl http://localhost:8787/v1/chat/completions \
  -H "Authorization: Bearer sk-gw-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}]}'

# streaming
curl http://localhost:8787/v1/chat/completions \
  -H "Authorization: Bearer sk-gw-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}],"stream":true}'

# Anthropic
curl http://localhost:8787/v1/messages \
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
- [ ] Brute-force protection: `/api/auth/login`, `/api/auth/passkey/login/begin|finish` and `/api/auth/recovery/verify` are already rate-limited per account/IP — don't front them with an LB that strips client IP

**Sessions after deploying:** all existing admin JWT sessions are invalidated on deploy by the token_version revocation upgrade — every user must log in again once. From then on, password changes, role changes, disabling and deleting a user revoke their outstanding sessions immediately.

---

## Architecture

```
Client (OpenAI SDK / Anthropic SDK / curl)
   |  Authorization: Bearer sk-gw-*
   v
[ Chi Router :8787 ]
  ├─ /health
  ├─ /api/*  (Admin JWT)
  │    ├─ POST /api/auth/login
  │    ├─ GET/POST/DELETE /api/providers
  │    ├─ GET/POST/DELETE /api/keys
  │    ├─ GET /api/stats, /api/logs
  │    └─ GET /*  (embedded UI)
  └─ /v1/*  (GatewayAuth: sk-gw-*)
       ├─ POST /v1/chat/completions  ──┐
       ├─ POST /v1/completions        ─┤  translate? → resolve provider by model/prefix
       ├─ POST /v1/embeddings         ─┤  → decrypt provider key → proxyRequest (SSE-aware)
       ├─ GET  /v1/models (aggregate) ─┤  → log to request_logs
       ├─ POST /v1/messages           ─┘
       └─ POST /v1/responses

Provider registry: SQLite + AES-GCM
```

See `ARCHITECTURE.md` for full design + Phase 2/3 roadmap.

---

## Testing — Phase 1 Proof

```bash
go test ./... -v
# proxy + translate httptest (mock upstream)
./scripts/smoke.sh        # live health/login/providers/keys/401 check
```

Live E2E against mock upstream (8788) validated:
- `chat/completions` non-stream & stream ✅
- `completions` ✅
- `embeddings` ✅
- `messages` (Anthropic) ✅
- `models` ✅
- `responses` ✅
- `logs` + `stats` ✅
- 401 without key ✅

---

## UI

- `web/` — Vite + React + Tailwind + React Router + Zustand
- Theme: Signal Terminal — graphite `#0F1311`, amber `#FFB84D`, teal `#2CF6B3`, paper `#F8F6F1`, IBM Plex Mono + Fraunces/Inter
- Pages: Dashboard, Providers, Keys, Playground (SSE waveform, model toggle openai/anthropic/responses), Logs
- Build outputs to `cmd/gateway/web` and is `embed.FS`ed

```bash
cd web && npm install && npm run dev   # dev on :5173 proxied to :8787
npm run build                          # production embeds
```

---

## Project Structure

```
ai-gateway/
  cmd/gateway/main.go
  internal/
    config/ db/ models/ provider/ apikey/ auth/ proxy/ translate/ middleware/ handler/
  web/ (React)
  ARCHITECTURE.md  Makefile  scripts/smoke.sh
```

---

## Remaining

- **Phase 3 polish:** mock-OIDC e2e, Postgres soak-testing (dialect works but remains **beta** — see Quick Start), Stripe billing, OTel traces, region status.
- Docs in `ARCHITECTURE.md` still describe the original phase buffers; the binary is ahead of that snapshot.

---

## Security Notes

- Gateway keys: `sk-gw-` + 32 bytes hex, SHA256 stored, prefix indexed, never logged.
- Provider keys: AES-256-GCM with `MASTER_KEY`.
- Set `ADMIN_PASSWORD` & `MASTER_KEY` & `JWT_SECRET` in prod; use `DATABASE_URL` persistent volume.
