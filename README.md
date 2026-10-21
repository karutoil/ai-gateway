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

### Image generation

The gateway also serves the OpenAI-compatible Images API through the same `sk-gw-*` key. Point any OpenAI Images SDK at the gateway by changing `baseURL`:

```bash
# Nano Banana (Google Gemini image model)
curl http://localhost:8080/v1/images/generations \
  -H "Authorization: Bearer sk-gw-..." -H "Content-Type: application/json" \
  -d '{"model":"gemini-2.5-flash-image","prompt":"a red panda astronaut","size":"1024x1024"}'

# OpenAI image model (gpt-image-1, dall-e-3, …)
curl http://localhost:8080/v1/images/generations \
  -H "Authorization: Bearer sk-gw-..." -H "Content-Type: application/json" \
  -d '{"model":"openai/gpt-image-1","prompt":"a red panda astronaut","n":1,"response_format":"b64_json"}'
```

Image edits use `multipart/form-data`: `POST /v1/images/edits` with one or more `image` files and an optional `mask`. Bare aliases `/images/generations` and `/images/edits` work too. Upstreams: OpenAI/Azure/openai_compatible, xAI Grok, Google Gemini (Nano Banana) and Imagen — Gemini/Imagen responses are normalized back to the OpenAI Images shape. `GET /v1/models` rows for image models carry `"image": true`, `"kind": "image"`, `image_cost` (USD per image) and `image_only`.

The multipart edit endpoint accepts the file part as either `image` (repeat it for multiple sources) or `images`, up to **4** source files, plus an optional `mask`. That differs from the OpenAI Images edit API, which keys on `image[]`. Note that `size` is validated against a fixed list (`auto`, `256x256`, `512x512`, `1024x1024`, `1536x1024`, `1024x1536`, `1792x1024`, `1024x1792`, `2048x2048`, `2048x1152`, `1152x2048`, `4096x4096`), while `quality`, `style`, `aspect_ratio` and `resolution` are **not** validated — they are passed through to the provider, so provider-specific tiers work.

Image responses are never cached (`X-Cache: BYPASS`) and are billed per generated image. Billable creates are also never retried or failed over, so one client request can never bill two assets. Slow image calls get a 300s client first-byte budget instead of the 85s chat budget (see `IMAGE_TIMEOUT_SECONDS`).

**`image` vs `image_only`** — the capability comes from models.dev's `modalities.output`:
- `image: true` means the model can generate images (served at `/v1/images/generations`).
- `image_only: true` means it generates *only* images (DALL·E, Imagen, `gpt-image-1`, Grok Imagine) and is therefore refused on the text endpoints with a pointer to the images API.
- `image: true, image_only: false` means it does both — Gemini image models (Nano Banana) and `gpt-image-1-mini`/`chatgpt-image-latest` narrate text alongside the image, so they remain fully usable for chat. They appear in the Playground's **images** mode *and* in the chat modes. Add a provider of type `google` (Nano Banana / Imagen) or `xai` (Grok images), or just use an existing OpenAI-compatible provider.

### Decision models (System One)

Decision models — TypeSafe **Jev**, Cloudflare **Clef** / **Clef-flash** / **Clef-omni**, the open-weight **Kev** family, and llama.cpp's `/v1/systemone` — do not generate text. They read a `state` plus a map of typed `questions` (`noul` yes/no, `choice`, `score`) and return a calibrated probability for every allowed answer. Add a provider of type `systemone` (Jev / Kev / llama.cpp) or `cloudflare` (Workers AI), then:

```bash
curl http://localhost:8080/v1/systemone \
  -H "Authorization: Bearer sk-gw-..." -H "Content-Type: application/json" \
  -d '{
    "model": "clef",
    "state": "Checkout has been failing for every customer for the last hour.",
    "questions": {
      "urgent":   { "type": "noul",   "instructions": "Is this support request urgent?" },
      "team":     { "type": "choice", "instructions": "Which team should handle it?",
                    "criteria": { "billing": "Payments and refunds", "technical": "Outages and errors" } },
      "severity": { "type": "score",  "instructions": "How severe is the impact?",
                    "criteria": ["None", "Minor", "Major", "Critical"] }
    }
  }'
```

Answers come back keyed by the same question ids (`answers.urgent.noul`, `answers.team.choice`, `answers.severity.score`). Decision models bill **input tokens only** — output tokens are free — and the gateway prices them from the catalog like any other model. The endpoint is always available (like the other protocol endpoints); it is unroutable until a decision-capable provider exists, and decision providers are never selected for chat/completions/embeddings while text providers are never selected for `/v1/systemone`.

> `/v1/systemone` is a *gateway-defined* endpoint speaking the System One wire protocol
> (TypeSafe Jev / Cloudflare Workers AI). It is not an OpenAI API; there is no OpenAI SDK
> mode for it.

Request rules the gateway enforces locally (a violation is a `400` naming the field):

- `model` must be a decision model; `state` must be present and non-null (any other JSON value is fine).
- `questions` holds 1–64 entries. Each id must be 1–100 characters of `[A-Za-z0-9_.-]` — no spaces.
- `instructions` is required and must be a **non-empty** string, object or array (`""`, `{}` and `[]` are rejected).
- `criteria` is **required** for `choice` (object of 2–255 option labels → description) and `score` (ordered array of 2–10 levels). It is optional for `noul`, where the only permitted keys are the literal strings `"true"` and `"false"`.
- `images` takes at most 4 inline entries (data URL, base64 or object); remote `http(s)` URLs are rejected. `audio` / `videos` (Clef-omni) are forwarded unchecked.

### Video generation

Video models are served through the OpenAI-compatible Videos API on the same `sk-gw-*` key. Video generation takes minutes, so the API is **job-based**: `POST /v1/videos` returns immediately with a job in `queued` status, you poll `GET /v1/videos/{id}` until it is `completed` (or `failed`), then download the bytes from `GET /v1/videos/{id}/content` as `video/mp4`. The media is delivered as a binary content endpoint rather than inline base64, mirroring the OpenAI Videos API exactly — official SDKs work by changing `baseURL`.

```bash
# Google Veo — create -> poll -> download
ID=$(curl -s http://localhost:8080/v1/videos \
  -H "Authorization: Bearer sk-gw-..." -H "Content-Type: application/json" \
  -d '{"model":"veo-3.1-generate-preview","prompt":"a red panda astronaut waving","seconds":8,"size":"1280x720"}' | jq -r .id)

# poll until status is completed or failed
curl -s http://localhost:8080/v1/videos/$ID \
  -H "Authorization: Bearer sk-gw-..." | jq '{status, progress}'

curl -s http://localhost:8080/v1/videos/$ID/content \
  -H "Authorization: Bearer sk-gw-..." -o veo.mp4

# xAI Grok Imagine — same create -> poll -> download, dialect translated
ID=$(curl -s http://localhost:8080/v1/videos \
  -H "Authorization: Bearer sk-gw-..." -H "Content-Type: application/json" \
  -d '{"model":"grok-imagine-video","prompt":"a red panda astronaut waving","seconds":8,"size":"1280x720"}' | jq -r .id)

curl -s http://localhost:8080/v1/videos/$ID \
  -H "Authorization: Bearer sk-gw-..." | jq '{status, progress}'

curl -s http://localhost:8080/v1/videos/$ID/content \
  -H "Authorization: Bearer sk-gw-..." -o grok.mp4
```

`GET /v1/videos` lists jobs and `DELETE /v1/videos/{id}` removes one; bare aliases `/videos`, `/videos/{id}` and `/videos/{id}/content` work too. The gateway detects the upstream dialect — OpenAI-compatible `/v1/videos`, xAI Grok Imagine (`/v1/videos/generations` -> `request_id`, with xAI's `done` normalized to `completed`), and Google Veo (`:predictLongRunning` -> operation polling, then the video URI). `GET /v1/models` rows for video models carry `"video": true`, `"kind": "video"` and `video_cost` (USD per second); models that only serve the Videos API also carry `"video_only": true`.

`seconds` may be sent as `8` or `"8"` on create (both are validated against 4 / 8 / 12); the job object always reports it as a string (`"seconds": "8"`), matching the OpenAI Videos schema. `size` is limited to `1280x720`, `720x1280`, `1024x1792`, `1792x1024`.

Lifecycle details worth knowing before you build against it:

- **Polling is what advances a job.** The gateway runs no background poller — each `GET /v1/videos/{id}` on a live job is exactly one upstream poll, and a transient upstream failure serves the last known state as `200` rather than an error. A `completed`/`failed` job is answered from the `video_jobs` table without touching the provider.
- **Downloading requires a terminal job.** `/content` answers `409` while the job is `queued`/`in_progress`, and `409` carrying the provider's error message for a `failed` job. Poll first.
- **Expiry is a `410 Gone`, and it sticks.** The gateway enforces the provider's `expires_at` for a completed job, and also treats a provider `404`/`410` for the bytes as a terminal expiry. Either way the row is marked `failed` with `error.code: "expired"`, and every later poll **and** content request answers `410` with the same message — the gateway stops re-contacting the provider for it.
- **Content is not cached and not stored gateway-side.** The `/content` route streams from the provider on every call, so the provider's own retention window is the real limit. If the provider hands back a URL on a different origin, the gateway refuses to fetch it (it will not attach your provider credential to a third-party host) and answers **`302`** with `Location` set — fetch it yourself.
- **Jobs are never deleted, only marked.** Rows accumulate forever; an expired job stays in `GET /v1/videos` as `failed`/`expired`. Add your own retention if you run video at volume (see Operations).
- **Billing happens at enqueue**, priced per generated second from the requested duration — a job you never poll, or one that later fails or expires, is still billed because the provider was already told to render it. `DELETE` forgets the job locally only; it is not a cancel and does not refund.
- **Upstream create errors come back sanitized** in the standard envelope (`{error:{message,type:"upstream_error",code}}`): the provider's message is preserved but bounded and credential-scrubbed.

A cURL loop to completion:

```bash
while :; do
  STATUS=$(curl -s http://localhost:8080/v1/videos/$ID \
    -H "Authorization: Bearer sk-gw-..." | jq -r .status)
  case "$STATUS" in completed|failed) break ;; esac
  sleep 5
done
```

## Features

- **Proxy** — chat/completions, completions, embeddings, models, messages, responses, **systemone** (decision models), **images**; SSE streaming with usage injection
- **Images** — text-to-image generation and edits at `/v1/images/generations` + `/v1/images/edits` (OpenAI Images compatible); Gemini Nano Banana, Imagen, xAI Grok and OpenAI GPT Image / DALL·E models, returning `b64_json` or `url`; per-image billing
- **Videos** — async text-to-video at `/v1/videos` (OpenAI Videos compatible): create → poll → download `video/mp4`, with Google Veo, xAI Grok Imagine and OpenAI Sora models and optional `input_reference` image-to-video
- **MCP servers** — register MCP servers once and expose them as a single authenticated `/mcp` endpoint that any harness (Pi, OpenCode, Claude Code, Cursor) points at; namespaced tools, per-key grants, Streamable HTTP / SSE / stdio upstreams, encrypted credentials, health probing with automatic reconnect. LiteLLM-compatible `{"type":"mcp","server_url":"litellm_proxy"}` references on chat/completions, messages and responses expand into real function tools, and `require_approval:"never"` executes them gateway-side in a bounded loop. See [docs/mcp.md](docs/mcp.md)
- **Decision models** — System One API (`/v1/systemone`) for Jev / Clef / Clef-flash / Clef-omni / Kev: typed `noul`/`choice`/`score` questions in, calibrated probabilities out; input-only pricing; `decision` flag in model listings
- **Providers** — OpenAI / Anthropic / Azure / any OpenAI-compatible; AES-GCM encrypted keys, model auto-discovery
- **Gateway keys** — `sk-gw-*`, per-key rate limits and token/cost quotas
- **Auth** — dashboard JWT, passkeys + recovery codes, optional OIDC SSO, org/team RBAC
- **Catalog** — 6k+ models with $/1M pricing; virtual aliases (`fast` → `openai/gpt-4o-mini`)
- **Routing** — curated provider groups per model with selectable strategies: round-robin, random, weighted (per-member 1–100), or failover (priority order); per-member model overrides; down-member skipping; multiple OAuth subscriptions can be separate provider members; optional per-rule quota threshold (default 90% consumed) skips near-exhausted Gemini subscriptions; unrouted bare model names are rejected (`model_not_routed`) unless `ROUTING_LEGACY_FALLBACK=true`
- **Resilience** — bounded retries with exponential backoff, cross-provider failover, exact-match response cache (in-memory or Redis)
- **Ops** — Prometheus `/metrics`, OpenTelemetry scaffold, request logs with retention purge, audit trail + webhooks, production hardening that refuses weak configs

Routing in one line: bare model names follow routing rules (Dashboard → *Routing*, which fetches each provider's model list via its API); `openai/gpt-4o-mini` **pins** the provider; `X-Provider:` header pins hardest; non-failover strategies serve each request with one member — a failing member returns its own error (use the `failover` strategy to walk members in order).

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
| `LOG_BODIES` | `true` | Store request/response bodies in the usage log — assembled stream text included, truncated to `BODY_LOG_MAX_BYTES` and credential-scrubbed. Opt out (`LOG_BODIES=0`) for metadata-only logging (privacy-sensitive) |
| `BODY_LOG_MAX_BYTES` | `8192` | Per-body capture cap in bytes for the usage log |
| `LOG_RETENTION_DAYS` | `0` | Nightly purge of request logs older than N days |
| `METRICS_PROTECT` | `false` | Require admin auth on `/metrics` |
| `ROUTING_LEGACY_FALLBACK` | `false` | Restore pre-strategy heuristic resolution (provider-models ownership, name heuristics, default provider) for bare model names with no routing rule. Default off: unrouted bare models get `404 model_not_routed` |
| `MCP_MAX_ROUNDS` | `25` | Tool-execution rounds per request for LiteLLM-compatible MCP references with `require_approval:"never"` |
| `MCP_CALL_TIMEOUT_SECONDS` | `90` | Timeout for one MCP connect/list/call and one LLM round inside the tool loop |
| `MCP_LOOP_TIMEOUT_SECONDS` | `600` | Total budget for the gateway-side MCP tool loop |
| `MCP_HEALTH_INTERVAL_SECONDS` | `60` | Background MCP server probe/reconnect interval |
| `IMAGE_TIMEOUT_SECONDS` | unset (= built-in `300`) | Client first-byte budget for `/v1/images/*`. Image generation is much slower than chat, so it does not use `TTFB_TIMEOUT_SECONDS`. Tri-state: **unset or `-1`** = built-in 300s default, **`0`** = budget disabled, **`N>0`** = N seconds |

Full reference: `.env.example` (covers timeouts, retries, webhooks, OIDC).

**Production checklist** (`ENV=production` refuses to boot otherwise): strong `ADMIN_PASSWORD`, explicit `MASTER_KEY` + `JWT_SECRET` (`openssl rand -hex 32`), non-wildcard CORS via `PUBLIC_URL`/`CORS_ALLOWED_ORIGINS`, persistent `DATABASE_URL`. Recommended: `METRICS_PROTECT=true`, `TRUSTED_PROXIES` set to your proxy CIDRs (loopback default suits a same-host tunnel), `LOG_RETENTION_DAYS=30`, Redis when running multiple replicas.

---

## Operations

- **Upgrades** — `install.sh update` or re-run the curl one-liner: binary swap with rollback, `.env` and data untouched. Migrations run automatically at boot; a failed migration is flagged dirty and blocks the next start until resolved.
- **Migration 026–029** — adds the `decision` / `image` / `video` capability columns and the `video_jobs` table. All four are additive and safe to run on a live database, but the schema version is left at **29**: rolling *back* to a pre-026 binary against a 029 schema is not supported (`schema_migrations` keeps the higher version and the older binary will not undo it).
- **Video job retention** — `expires_at` **is** enforced for clients (an expired job answers `410` and is marked `failed`/`expired`), but the row itself is **never deleted by the gateway**. Every `POST /v1/videos` adds a row forever, and rows also store `prompt` and possibly a signed `video_url` in plaintext. If you run the videos API at volume, schedule your own prune, e.g. `DELETE FROM video_jobs WHERE created_at < datetime('now','-30 days')` on SQLite (use `now() - interval '30 days'` on Postgres).
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
              (SSE-aware, retry, cache) → log usage/cost
              chat/completions · embeddings · messages · responses
              images/{generations,edits} · videos (async jobs) · systemone
```

See `ARCHITECTURE.md` for design details, `openapi.yaml` for the API surface, `docs/images-videos-systemone.md` for the images/videos/System One operator and client notes, and `docs/muse-harness.md` for the live tool-calling harness.

---

## Security Notes

- Gateway keys: SHA256-hashed, prefix-indexed, shown once, never logged.
- Provider keys: AES-256-GCM under `MASTER_KEY`.
- Production boots fail closed on weak configuration (`ALLOW_INSECURE=true` is the explicit escape hatch).
- Login/passkey/recovery endpoints are rate-limited; password/role changes revoke outstanding sessions.
