# AI Gateway — Unified Go Gateway

> **One domain for every model.** Fast Go gateway + Signal Terminal UI. Bring OpenAI, Anthropic, Azure, Groq — expose OpenAI / Anthropic / Responses compat under one domain.

[![CI](https://github.com/karutoil/ai-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/karutoil/ai-gateway/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/Go-1.25-2CF6B3)

---

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/karutoil/ai-gateway/main/install.sh | bash
```

The installer downloads the latest self-contained release binary (web UI embedded, SQLite statically linked — no Go, Node or system packages needed), walks you through `.env` setup with secure defaults (**Enter accepts everything**), and can register a systemd service.

<details>
<summary>More installer commands</summary>

```bash
bash install.sh update      # update to the latest release — keeps .env and data
bash install.sh uninstall   # remove the gateway; wiping data is your choice
bash install.sh status      # installed version vs latest, service state
```

(From a repo clone the commands are identical — the script locates the installation automatically.)

Non-interactive: `GATEWAY_YES=1` accepts all defaults, `GATEWAY_VERSION=v1.7.1` pins a release, `GATEWAY_INSTALL_DIR=/opt/ai-gateway` overrides the location.

</details>

Every push to `main` is versioned from commit history (`feat:` → minor, `fix:` → patch, `BREAKING CHANGE` → major), built for **linux/amd64 + arm64** (fully static), and published with checksums to [Releases](https://github.com/karutoil/ai-gateway/releases).

## First run

```bash
cd /opt/ai-gateway && ./gateway        # or: systemctl start ai-gateway
# -> http://localhost:8080  (UI)  +  http://localhost:8080/v1/* API
```

The installer generates a strong `ADMIN_PASSWORD` and prints it once — sign in at `/` with username `admin`. In dev (no `.env`) the default is `admin / admin123`.

---

## Using the API

```bash
TOKEN=$(curl -s -X POST http://localhost:8080/api/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"admin123"}' | jq -r .token)

# add provider
curl -X POST http://localhost:8080/api/providers \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"openai","type":"openai","api_key":"sk-...","base_url":"https://api.openai.com/v1"}'

# create gateway key (shown once!)
curl -X POST http://localhost:8080/api/keys \
  -H "Authorization: Bearer $TOKEN" -d '{"name":"my-key"}' | jq
# -> sk-gw-...

# use the gateway like OpenAI — or point any SDK at it
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-gw-..." -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}]}'
```

SDKs only need a `baseURL` change plus your `sk-gw-*` key. OpenAI, Anthropic (`/v1/messages`) and Responses (`/v1/responses`) protocols are all served — streaming included — with automatic protocol translation between them.

## Features

- **Proxy** — chat/completions, completions, embeddings, models, messages, responses; SSE streaming with usage injection
- **Providers** — OpenAI / Anthropic / Azure / any OpenAI-compatible; AES-GCM encrypted keys, model auto-discovery
- **Gateway keys** — `sk-gw-*`, per-key rate limits and token/cost quotas
- **Auth** — dashboard JWT, passkeys + recovery codes, optional OIDC SSO, org/team RBAC
- **Catalog** — 6k+ models with $/1M pricing; virtual aliases (`fast` → `openai/gpt-4o-mini`)
- **Routing** — explicit curated provider groups per model, round-robin, down-member skipping, **no silent failover by design**
- **Resilience** — circuit breaker, bounded retries, exact-match response cache (in-memory or Redis)
- **Ops** — Prometheus `/metrics`, OpenTelemetry scaffold, request logs with retention purge, audit trail + webhooks, production hardening that refuses weak configs

Routing in one line: bare model names follow routing rules (Dashboard → *Routing*); `openai/gpt-4o-mini` **pins** the provider; `X-Provider:` header pins hardest; a failing member returns its own error — remove it from the group instead of hoping for failover.

---

## Configuration

Everything is env-driven (`.env` next to the binary is loaded automatically — the installer generates one). The essentials:

| Env var | Default | Meaning |
|---|---|---|
| `ENV` | dev | `production` enforces the hardening checks below |
| `PORT` | `8080` | HTTP listen port |
| `ADMIN_PASSWORD` | `admin123` (dev) | Dashboard login; production requires ≥12 chars |
| `MASTER_KEY` | derived/persistent | 64 hex chars — encrypts provider credentials; required in prod |
| `JWT_SECRET` | derived/persistent | ≥32 chars session signing secret; required in prod |
| `PUBLIC_URL` | "" | Public origin for CORS (tunnel/reverse proxy) |
| `CORS_ALLOWED_ORIGINS` | `*` (dev) | Comma-separated origins; wildcard rejected in production |
| `DATABASE_URL` | `./data/gateway.db` | SQLite path, or `postgres://…` DSN (beta) |
| `REDIS_URL` | "" | Shared cache + rate limiting across replicas |
| `LOG_BODIES` | `false` | Store request/response bodies (privacy-sensitive) |
| `LOG_RETENTION_DAYS` | `0` | Nightly purge of request logs older than N days |
| `METRICS_PROTECT` | `false` | Require admin auth on `/metrics` |

Full reference: `.env.example` (covers timeouts, retries, breaker tuning, webhooks, OIDC).

**Production checklist** (`ENV=production` refuses to boot otherwise): strong `ADMIN_PASSWORD`, explicit `MASTER_KEY` + `JWT_SECRET` (`openssl rand -hex 32`), non-wildcard CORS via `PUBLIC_URL`/`CORS_ALLOWED_ORIGINS`, persistent `DATABASE_URL`. Recommended: `METRICS_PROTECT=true`, `TRUSTED_PROXIES` set to your proxy CIDRs (loopback default suits a same-host tunnel), `LOG_RETENTION_DAYS=30`, Redis when running multiple replicas.

---

## Operations

- **Upgrades** — `install.sh update` or re-run the curl one-liner: binary swap with rollback, `.env` and data untouched. Migrations run automatically at boot; a failed migration is flagged dirty and blocks the next start until resolved.
- **`MASTER_KEY` rotation** — re-encrypt or re-enter provider keys alongside the change; swapping the key on a live data dir makes saved credentials undecryptable.
- **Backups** — stop the gateway or use `sqlite3 data/gateway.db ".backup 'backups/gw.db'"`; never copy a live `gateway.db` without its `-wal`/`-shm`.
- **Scaling** — SQLite is the single writer; share cache/rate limits via Redis or move to Postgres (beta) before scaling writes.

### Docker

```bash
cp .env.example .env          # then edit secrets
docker compose up --build     # data persisted to ./data
```

---

## Development

```bash
git clone https://github.com/karutoil/ai-gateway && cd ai-gateway
make build          # Go 1.25+, CGO (gcc/clang) for sqlite3; UI assets are pre-built
ADMIN_PASSWORD=admin123 PORT=8080 ./bin/gateway

make web            # rebuild embedded UI (npm ci + vite -> cmd/gateway/web)
make test           # unit suite (mock upstreams; streaming, translation, budgets…)
make test-race      # …under the race detector
make ci             # local parity with GitHub Actions
```

Architecture overview:

```
Client (OpenAI/Anthropic SDK, curl)
   |  Authorization: Bearer sk-gw-*
   v
[ Chi Router :8080 ]
  ├─ /health /ready /metrics /openapi.yaml
  ├─ /api/*   admin control plane (JWT): providers, keys, users,
  │           orgs, routing rules, catalog, stats, logs, audit, UI
  └─ /v1/*    gateway API (sk-gw-*): rate limits + budgets →
              translate? → resolve route → decrypt key → proxy
              (SSE-aware, retry, breaker, cache) → log usage/cost
```

See `ARCHITECTURE.md` for design details, `openapi.yaml` for the API surface, and `docs/muse-harness.md` for the live tool-calling harness.

---

## Security Notes

- Gateway keys: SHA256-hashed, prefix-indexed, shown once, never logged.
- Provider keys: AES-256-GCM under `MASTER_KEY`.
- Production boots fail closed on weak configuration (`ALLOW_INSECURE=true` is the explicit escape hatch).
- Login/passkey/recovery endpoints are rate-limited; password/role changes revoke outstanding sessions.
