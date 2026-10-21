

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
    db/migrations/  # 001_initial … 029_video_models (embedded, run at boot; LatestMigrationVersion() = 29)
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
    proxy/      # reverse proxy, SSE, caching, retries, LB routing, stream usage inject,
                # systemone.go (System One / decision-model endpoint)
    translate/  # anthropic <-> openai <-> responses protocol translation
    imagegen/   # image generation: OpenAI Images protocol + Gemini/Imagen/xAI dialect translation
    videogen/   # video generation: OpenAI Videos async job protocol + Veo/xAI dialect translation
    decision/   # System One protocol: request validation + curated decision-model registry
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

Bare aliases (`/chat/completions`, `/images/generations`, `/images/edits`,
`/videos`, `/videos/{id}`, `/videos/{id}/content`, `/systemone`, …) are
registered on the **same** gateway group, so they carry the identical middleware
chain and auth. The one exception is `GET /models`: it is served by a private
sub-router with the same chain, because a browser refresh on the dashboard's
`/models` screen must get the SPA instead — `Accept: text/html` falls through to
`serveWeb`.

- `forwardedHeaders` honors `X-Forwarded-*` / `CF-Connecting-IP` **only** from loopback peers or `TRUSTED_PROXIES` CIDRs (`"*"` = trust all). Rate limiting and logging see the real client IP.
- Rate limiting: in-memory `middleware.RateLimiter` uses **fixed-window buckets** with atomic all-windows-pass admission; when `REDIS_URL` is set, `RedisRateLimiter` (INCR+TTL sliding-window variant) shares the same windows across replicas and falls back to memory on Redis errors.
- `audit.Middleware` writes an audit row for every mutating `/api/*` request (actor from the verified JWT subject or gateway-key prefix — `X-Actor` headers are never trusted) and fans out to the webhook sink.
- `securityHeaders` sets nosniff / DENY / no-referrer / baseline CSP.
- CORS: permissive `*` by default (tunnel/dev convenience); `PUBLIC_URL` or `CORS_ALLOWED_ORIGINS` lock it down, and production refuses wildcard.

---

## Data Model

Authoritative DDL lives in `internal/db/migrations/001_initial.sql … 034_request_client.sql` (embedded, applied at boot through `schema_migrations`; a failed migration marks the version **dirty** and boot aborts until an operator intervenes). The highest embedded version is reported by `db.LatestMigrationVersion()`. Highlights:

- `providers` (AES-GCM `api_key_enc`, `base_url`, `health_status`, optional `org_id`)
- `gateway_keys` (prefix + SHA256 hash, RPM/RPH/RPD/TPM, daily/monthly token & cost limits, `allowed_models`, optional `org_id`)
- `request_logs` (model/endpoint/status/latency/ttft, tokens, cost, stream flag, optional bodies, error, `cache_hit` for gateway response-cache hits, `cache_status`, plus the identity triple `end_user` / `app` / `client` from 024 and 034 — see *Attribution* below)
- `models_catalog`, `provider_models`, `model_aliases`, `system_config` — capability flags on both model tables: `decision` INT (026), `image` INT (027), `video` INT (029). Default 0 = text model; a model can carry several, and `GET /v1/models` reports each as a boolean plus a single `kind` label.
- `video_jobs` (028) — `id` / `provider_id` / `key_prefix` / `dialect` / `model` / `upstream_id` / `prompt` / `seconds` / `size` / `status` / `progress` / `error_code` / `error_message` / `video_url` / `created_at` / `updated_at` / `completed_at` / `expires_at`. The durable mapping from a gateway-issued `video_…` id to the provider's own handle plus the last observed state (indexed on `key_prefix`, `provider_id`, `created_at`). `expires_at` **is** enforced for clients — an expired job answers `410` and is marked `failed`/`expired` — but no row is ever deleted; see the Operations section of `README.md` for retention.
- `audit_logs` (actor, action, target, meta)
- `organizations`, `memberships` (org scaffold; org scope resolved from memberships)
- `lb_rules` (per-model ordered option groups: `provider_id` + `model_override` + `strategy` + `weight` + optional consumed-quota threshold; a provider may repeat with distinct models)

SQLite (WAL, single-conn writes) is the default dialect; `postgres://` DSNs switch to lib/pq — functional but **beta** (not yet soak-tested).

**Dialect trap — never write a Go `bool` into an INTEGER column.** lib/pq
refuses the coercion (`pq: invalid input syntax for type integer: "false"`)
while SQLite silently accepts it, so a bool-into-int write passes every SQLite
test and fails only on Postgres. The capability flags `decision` / `image` /
`video` are INTEGER (added by later migrations; every read path scans them as
`sql.NullInt64`), so their write sites must normalize through `db.BoolInt`.
The sibling capability columns (`reasoning`, `tool_call`, `structured_output`,
`attachment`, `open_weights`) are genuine BOOLEANs and take bools directly.
`internal/db/pgsmoke` is the guard: it exercises every flag write path against
a real Postgres, because SQLite cannot catch this class of bug. It **skips
itself when `GATEWAY_PG_DSN` is unset**, and CI currently does not set it — so
today it only protects a developer who runs it by hand:

```bash
GATEWAY_PG_DSN=postgres://gateway:gateway@127.0.0.1:5433/gateway?sslmode=disable \
  go test ./internal/db/pgsmoke
```

Wire a `postgres:16` service into `ci.yml` before relying on it as a regression
gate.

---

## Translation Spec (stable since Phase 1)
| Inbound | Upstream OpenAI | Upstream Anthropic | Notes |
|---|---|---|---|
| OpenAI chat | passthrough | Anthropic translate: system+messages, tools->tool_use | stream: `data: {...}` |
| Anthropic messages | OpenAI translate: messages flatten, max_tokens default 1024 | passthrough (`anthropic-version: 2023-06-01`, `x-api-key`) | tool_call preserved |
| Responses API | Chat translate: `input`->messages, `instructions`->system, `reasoning.effort`->`reasoning_effort` | Anthropic translate | streaming supported |
| Models | aggregate from all providers + catalog enrichment + aliases | — | dedupe by id, alias as gateway-alias; image rows carry `image: true`, `kind: image`, `image_cost` |
| Images `/v1/images/generations` | passthrough (OpenAI / azure / openai_compatible); xAI Grok speaks the same shape | Gemini `generateContent` (`responseModalities: ["IMAGE"]`) and Imagen `:predict` are translated to/from the OpenAI Images shape | `b64_json` or `url`; optional `image` / `images` reference inputs |
| Images `/v1/images/edits` | multipart passthrough to OpenAI-style edit endpoints | Gemini: multipart source images are inlined into `generateContent` | multipart: `image` file(s) + optional `mask` |
| Videos `/v1/videos` | passthrough to OpenAI-compatible `/videos` (async job) | xAI Grok Imagine (`/videos/generations` → `request_id`) and Google Veo (`:predictLongRunning` → operation) are translated to/from the OpenAI Videos job shape | async: create → poll → `GET /v1/videos/{id}/content` (`video/mp4`); `seconds`/`size`; optional `input_reference` |
| Videos poll/content | reuses the stored job row; terminal jobs are served without re-polling upstream | xAI's `done` normalized to `completed`; Veo's done/not-done synthesized into `progress` | job rows persist in `video_jobs` (migration 028) |
| System One (`/v1/systemone`) | passthrough to decision providers (`systemone` / `cloudflare`); model-in-path for Workers AI | — | no streaming; typed noul/choice/score answers; input-only billing |

Multi-protocol providers (OpenCode Go/Zen): one entry serves chat, responses,
and messages models on one base URL + key. Each model speaks exactly one
upstream endpoint (Go: glm/kimi via chat, grok/gpt/muse-spark via responses,
qwen/minimax via messages; Zen: minimax via chat, claude via messages).
`/v1/messages` accepts `openai_compatible` multi entries natively;
`/v1/responses` routes messages-models via Responses→Anthropic; wrong-endpoint
calls fail fast with the correct POST path. Discovery merges both dialects;
health is up when either probe succeeds.

Decision models (the **System One API**) are a separate protocol, not a chat
variant: `/v1/systemone` accepts a `state` plus a map of typed `questions`
(`noul` yes/no, `choice`, `score`) and returns calibrated probabilities for
every allowed answer — never text, never a stream. Providers typed `systemone`
(TypeSafe Jev, a local Kev server, llama.cpp's `/v1/systemone`) or `cloudflare`
(Workers AI, where the model is addressed in the path as
`…/ai/run/@cf/cloudflare/<model>`) serve them; any other entry whose base URL
contains `systemone` or `/ai/run` qualifies too. Decision providers are
excluded from chat/completions/embeddings and text providers from
`/v1/systemone`, so a decision model can never be silently sent a chat body.
The curated registry (`internal/decision`) seeds Jev / Clef / Clef-flash /
Clef-omni / Kev catalog rows with input-only pricing and `decision=1`, so they
list (`GET /v1/models` includes `"decision": true`, `"kind": "decision"`) and
bill like any other model. The route is always registered — it is unroutable
until a decision-capable provider exists.

Image models join the same multi-protocol picture: OpenAI `gpt-image-*` /
`dall-e-*` speak the Images API, xAI Grok is OpenAI-compatible on
`/v1/images/generations`, Google Gemini Nano Banana uses
`v1beta/models/{model}:generateContent` with `responseModalities: ["IMAGE"]`,
and Imagen uses `:predict` — `internal/imagegen/` normalizes all of them to the
OpenAI Images shape (`created`, `model`, `data[]`, optional `usage`). Image models
bill per generated image (curated per-image prices). Provider types `google`
(base `https://generativelanguage.googleapis.com`, API key required, image-only)
and `xai` (base `https://api.x.ai/v1`, API key required) are first-class. The
same two types serve video (Veo on `google`, Grok Imagine on `xai`).

**Image capability comes from models.dev `modalities.output`** — models.dev has
no dedicated image flag (its `type` field only carries
embedding/reranking/video), so `output` containing `image` IS the tag. That
output list is authoritative whenever present; the curated
`internal/imagegen` registry is consulted only for rows models.dev has not
caught up with, so a row can never claim image output while advertising
text-only modalities. The same list yields the `image_only` distinction:
`output: ["image"]` is a pure generator (DALL·E, Imagen, gpt-image-1, Grok
Imagine) and is refused on the text endpoints with a pointer to the images API,
while `output: ["text","image"]` (Gemini image models, gpt-image-1-mini,
chatgpt-image-latest) also chats and is allowed through. `/v1/models` exposes
`image`, `image_only`, `modalities` and `image_cost` so clients pick the right
models per mode.

**Endpoint-vs-model validity is checked before routing, in both directions.**
`rejectImageModelOnText` / `rejectDecisionModelOnText` (text endpoints) and the
System One handler's decision check all run *before* candidate resolution, so a
model sent to the wrong protocol gets an actionable 400 naming the right
endpoint. These checks used to sit inside the "no candidates" branch behind
`unroutedModel()`, which in the default configuration returned a generic
404 `model_not_routed` and made the actionable message unreachable — the
decision-model guard on `/v1/systemone` was dead code for exactly that reason.
`noRouteFor` is likewise capability-aware: an unrouted image or decision model
is told which endpoint serves it, while an unknown id keeps the generic routing
advice. A decision model is refused on the images endpoint, while an unknown id
is forwarded (resellers expose image models the gateway cannot enumerate).

**Video generation is a job, not a request.** A clip takes tens of seconds to
minutes — beyond any HTTP budget and far beyond the chat first-byte window — so
`POST /v1/videos` enqueues work upstream and returns a job in state `queued`
immediately; the client polls `GET /v1/videos/{id}` until `completed`/`failed`
and then downloads the bytes from `GET /v1/videos/{id}/content` as `video/mp4`.
Returning the clip inline is equally impossible: it is megabytes, so base64 in a
JSON body is impractical. The gateway runs **no background poller** — a client
poll becomes a single upstream poll — which keeps it stateless and avoids
burning provider quota for clients that stopped caring. The `video_jobs` table
(migration 028) is the durable mapping from the gateway-issued id to the
provider's own handle plus the last observed state, so a finished job is served
from storage without re-polling and survives a restart. Video models bill per
generated second (curated per-second prices), metered at enqueue time since the
duration is known from the request. The capability tag is again
models.dev `modalities.output` containing `video`; `video_only` (video with no
text output) bars a model from the text endpoints, exactly like `image_only`.

The lifecycle contract for clients:

1. `POST /v1/videos` — enqueue; returns `queued` immediately. Priced and metered
   here, so an abandoned or later-failed job is still billed.
2. `GET /v1/videos/{id}` — poll. One upstream poll per call while the job is live
   (bounded by the normal first-byte budget); a transient upstream failure serves
   the last known state as `200`. A terminal job is answered from the row without
   touching the provider.
3. `GET /v1/videos/{id}/content` — stream the bytes. `409` until the job is
   terminal (and `409` with the provider's message for a `failed` job).

**Expiry is terminal and client-visible.** `videoJob.expired()` fires when the
provider's advertised `expires_at` has passed for a `completed` job, or when the
job is already marked `expired`. `VideoGet` and `VideoContent` check it first and
answer **`410 Gone`** after rewriting the row to `failed` with
`error_code="expired"`; a provider `404`/`410` on poll or content does the same
via `failVideoJob`. So the gateway stops re-hitting the provider for an artefact
it no longer retains, and every later request is answered from the row.

**Nothing is cached on this surface.** The content route re-fetches from the
provider on every call — no gateway-side media store, no response-cache entry —
so the provider's retention window is the only guarantee of availability. Two
consequences worth knowing: a content URL that is **not** on the provider's own
origin is never fetched server-side (the credential must not travel to a
third-party host) and is answered `302` with `Location` so the client fetches it
directly; and in-flight transfers are bounded by a first-byte budget plus a
per-chunk idle watchdog.

`video_jobs` rows are **never reaped** — no sweep, no retention job, so the table
(including plaintext `prompt` and any signed `video_url`) grows with every create.
Operators must prune it themselves (`README.md` → Operations).

Reasoning mapping: OpenAI `reasoning_effort` (low/medium/high/max) <-> Anthropic `thinking.effort` or legacy `budget_tokens` via budgetToEffort heuristic; validated against catalog `reasoning_levels/limits`.

---

## Routing, Resilience & Metering (all wired)

- **Routing rules** (`lb`): per-model ordered groups of **(provider, model) options** with a **strategy** — `round_robin` (default; rotate evenly across requests), `random`, `weighted` (per-member weight 1–100, proportional pick), or `failover` (first healthy member in position order; later members only on retriable failure — the only strategy with cross-provider failover). A provider may appear multiple times in one rule with different `model_override` values (e.g. `openai+gpt-4o` and `openai+gpt-4o-mini` as separate options); exact duplicate (provider, model) pairs are rejected. OAuth subscriptions are separate provider rows, so multiple Gemini accounts can be members of one rule. A rule's optional consumed-quota threshold (default 90%) skips Antigravity/Gemini members at or above that usage; quota snapshots are cached briefly and quota-service failures fail open. Down members and open circuits are skipped where a healthy sibling exists; non-failover strategies serve each request with ONE member (a failing member returns its own error after same-provider retries). Each member may carry a `model_override` — the model id rewritten onto the outbound request for that member. Unrouted bare model names are rejected with **404 `model_not_routed`**; `ROUTING_LEGACY_FALLBACK=true` restores the old heuristic resolution (provider-models ownership, name heuristics, default provider) as a migration escape hatch. Qualified `provider/model` IDs and `X-Provider:` pin requests bypass rules; aliases resolve first. Model lists for rules come from per-provider discovery (`POST /api/providers/{id}/discover` fetches the provider's `/models` API).
- **Retries**: `RETRY_MAX_RETRIES` (default 2) with exponential backoff on 5xx/429 — only while **nothing is committed**; streaming requests retry until the first byte is committed, then fail honestly.
- **Cache**: exact-match response cache (`X-Cache: HIT|MISS|BYPASS`), in-memory or Redis; `CACHE_TTL_SECONDS`. `CACHE_STREAMS=true` (opt-in) extends it to streaming requests: a clean stream's exact client-bound SSE bytes are stored (bounded to 1 MiB) and replayed verbatim for identical requests — the classic cache only ever serves non-streaming traffic. Every request logs its cache disposition (`request_logs.cache_status`: `hit` / `miss` / `bypass`, mirrored by the `X-Cache` header; the models-list endpoint logs rows too), cached responses carry `cache_hit=1` (zero tokens — not re-billed), and the dashboard shows the state per request (Requests list, detail, Analytics hit/miss/bypass breakdown with the rate over eligible requests) plus Prometheus `gateway_cache_hits_total`. Request bodies up to 4 MiB are cache-eligible (agentic prompts run large); larger bodies bypass.
- **Cache/retry exceptions on the new surface**: the image endpoints set `noCache`, `noRetry` and `noFailover` on their proxy call — a generated image is stale the moment it is served (a re-roll must not return yesterday's picture), a cached replay would log `cost_usd=0`, and a retry or a failover of a billable create bills a second asset for one client request. They answer `X-Cache: BYPASS`. The video create path does not use the shared candidate chain at all for the same reason. Verified behaviour: two identical `POST /v1/images/generations` calls reach the provider twice with `X-Cache: BYPASS`.
- **Image first-byte budget**: `/v1/images/*` uses its own budget (`IMAGE_TIMEOUT_SECONDS`) instead of the 85s chat `TTFB_TIMEOUT_SECONDS`, because image generation legitimately runs tens of seconds. Tri-state: unset or `-1` = built-in 300s default, `0` = disabled, `N>0` = N seconds. The boot log names the resolved state.
- **Budgets**: per-key daily/monthly token + cost quotas enforced in `budget.Middleware` with a ledger (`429 over_quota_error`).
- **Usage metering**: `STREAM_USAGE_INJECT` (default **true**) injects `include_usage` so OpenAI-compatible streams are metered; opt out with `STREAM_USAGE_INJECT=0`.
- **Webhooks**: `webhook.Global` delivers audit + billing-export + over-quota events asynchronously (bounded queue, 2 attempts); `WEBHOOK_SECRET` signs deliveries as `X-Webhook-Signature: sha256=<hex HMAC>`.
- **Metrics**: Prometheus at `/metrics` — `gateway_requests_total{provider,model,endpoint,status}`, `gateway_latency_ms{provider,endpoint}`, `gateway_cache_hits_total{result}`; `METRICS_PROTECT=true` gates it behind admin auth. OpenTelemetry metrics API is scaffolded but OTLP export is pending.

---

## Attribution (who used the gateway)

Three identity columns on `request_logs`, in decreasing order of how much the caller has to do:

| column | source (precedence) | populated by default? |
| --- | --- | --- |
| `end_user` | `X-End-User` → body `user` ([OI]) → body `metadata.user_id` (Anthropic) | no — the application must pass a user id |
| `app` | `X-App-Id` → `X-Title` / `X-OpenRouter-Title` → origin of `HTTP-Referer` / `Referer` | no — the caller must declare itself |
| `client` | `X-Client` → `User-Agent` product token (lowercased, version dropped) | **yes** — every HTTP client sends a UA |

`client` exists because the first two do not work in practice. Harnesses only send `X-OpenRouter-Title` / `HTTP-Referer` when the upstream *is* OpenRouter (pi gates them on the host), and only send a body `user` when the application passes one, so a coding-agent deployment landed 100% NULL in both columns and the Attribution page was structurally empty. The UA product token is the one identity every caller supplies without knowing the gateway exists. It is kept in its own column rather than being folded into `app`: `app` means "the caller declared itself", `client` means "what the wire says called us", and reports must not blur a declaration with an inference.

Both `app` and `client` are groupable (`GET /api/logs/group?group_by=app|client`) and filterable (`?app=`, `?client=`, exact match so a report drilldown selects exactly the bucket it counted). `end_user` stays filter/rank only — it is unbounded by construction, so grouping by it is the rollup explosion Langfuse explicitly forbids. `GET /api/analytics/attribution` returns `apps`, `end_users` and `clients` rankings plus a `coverage` block (`total_requests`, `with_app`, `with_end_user`, `with_client`, `identified`, `unattributed`), where `unattributed` counts rows with **no** identity of any kind.

Every value is attacker-influenced: each is trimmed, stripped of control characters, capped at 200 bytes on a rune boundary, and rejected outright if it is a known sentinel (`undefined`, `null`, `NaN`, `nil`, `(null)`, `[object Object]`). The sentinel guard exists because a client that interpolates an unset variable into a header otherwise creates an identity literally named `undefined`, which is worse than no attribution at all — it looks like data.

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
