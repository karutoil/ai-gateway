# Production Readiness Review — ai-gateway (2026-08-26)

Consolidated from five parallel review tracks (reliability/request-path, security/auth,
data-layer/handlers, LiteLLM feature-parity, tests/ops/contract) plus build verification.
Every finding cites a verified file:line. Baseline facts checked locally:

- `go build ./...`, `go vet ./...`, `go test ./...`, `go test -race ./...` all pass (race run is vacuous — no concurrency tests exist).
- 11 packages have zero tests: resilience, cache, db, discovery, catalog, otel, passkey, user, webhook, audit, models.
- `.gitignore` correctly excludes `data/*.db*`, `.master_key`, `.jwt_secret`, `.env`. Embedded UI assets are fresh vs `web/src`.

**Overall verdict: not production-ready.** The data plane (proxying, translation, keys,
rate limits, quotas, telemetry) is genuinely solid scaffolding, but there are critical
authentication-bypass chains, tenant-isolation gaps, a deterministic guillotine on any
stream longer than 120s, no failover for streaming requests, one process-crash race,
one single-connection SQLite deadlock, broken Postgres mode, and no CI to hold the line.

---

## P0 — BLOCKERS (fix before ANY production traffic)

### Auth & security
| # | Finding | Location |
|---|---------|----------|
| B1 | **Unauthenticated admin-JWT minting via `/api/auth/oidc`.** With `OIDC_ISSUER` unset (the default), the only gate is `if body.IDToken == "" && body.OrgID == ""`; caller chooses `role:"admin"`, arbitrary `org_id` (empty = global scope), arbitrary subject → signed 24h HS256 dashboard JWT. One curl = full control-plane compromise. | `internal/handler/admin.go:147–190` |
| B2 | Same endpoint when OIDC *is* configured: token is signature-verified but **claims are discarded** (`_ = VerifyOIDCToken(...)`); role/org still come from the request body → any legitimate IdP user self-escalates to admin of any org. | `internal/handler/admin.go:163–185` |
| B3 | Dashboard passwords hashed with **unsalted SHA-256**, min length 4, non-constant-time compare on login. Applies also to recovery codes. | `internal/auth/auth.go:31–33`, `internal/user/store.go:135–136,143,148` |
| B4 | **No brute-force protection** on `/api/auth/login`, `/api/auth/oidc`, `/api/auth/passkey/*`, `/api/auth/recovery/verify` (rate limiting wired only into `/v1`). | `cmd/gateway/main.go:203–263` |
| B5 | **Cross-org IDOR**: `DeleteProvider`, `DeleteKey`, `UpdateKey*`, `GET /api/logs/{id}` (which returns stored request/response bodies), `Stats` totals act on bare IDs without org scoping; member of org A can destroy/read org B's resources given UUIDs. | `internal/handler/admin.go:255–263,355–362,672–710,378–380` |
| B6 | **Recovery codes never consumed** — `ConsumeRecoveryCode` exists but has zero callers; one leaked code is a forever-replayable password+passkey bypass minting full-role JWTs. | `internal/passkey/handler.go:384–408`, `internal/user/store.go:216` |
| B7 | **Fail-open role defaults**: empty role → `"admin"` in `MakeTokenWithOrg`; `AdminMiddleware` initializes `role="admin", orgID=""`; passkey discoverable-login DB miss proceeds as `username="unknown", role="admin"`; registration falls back to binding attacker authenticators to user `admin`. | `internal/auth/auth.go:47–49,128–131,249–253`, `internal/passkey/handler.go:69–86,159–178,346–356` |
| B8 | `MASTER_KEY` / `JWT_SECRET` silently derived from `ADMIN_PASSWORD`; a leaked DB file + offline crack of one human password decrypts every provider API key. Rotating ADMIN_PASSWORD afterwards silently corrupts all stored ciphertexts. Must hard-fail in production instead of warn. | `internal/config/config.go:186–231` |
| B9 | **Prod boots fail-open**: `ENV=production` with weak secrets logs an error and continues; empty users table gets `admin/admin123` seeded regardless. No config problem ever prevents boot. | `internal/config/config.go:53–57,169–176`, `cmd/gateway/main.go:70–84` |

### Reliability (request path)
| # | Finding | Location |
|---|---------|----------|
| B10 | **120-second ceiling kills all long streams**, twice over: `http.Client{Timeout:120s}` includes reading the response body; server `WriteTimeout:120s` kills the client side mid-SSE. Long reasoning-model generations deterministically truncate. | `internal/proxy/proxy.go:47,56`, `cmd/gateway/main.go:310–315` |
| B11 | **Streaming requests never retry or fall back** — even for pre-first-byte upstream 500/429, headers/status are committed straight to the client; failover candidates unreachable for streams. `TestStreamDoesNotRetry` codifies the gap. LiteLLM retries streams until first chunk. | `internal/proxy/proxy.go:385–391`, `internal/proxy/fallback.go:184–188` |
| B12 | **Mid-stream upstream death = silent truncation**: no `[DONE]` / Anthropic `error` terminal event, row logged as success 200 with zeros usage, breaker records success → mid-body provider failures are invisible to reliability machinery AND billing. | `internal/proxy/proxy.go:396–441` |
| B13 | **Redis→memory rate-limiter fallback mutates the shared bucket map without its mutex** (`allowWindowLocked` called un-Locked) → Go map race, potential process crash exactly during Redis incidents. | `internal/middleware/ratelimit.go:189,212` vs `61–102` |
| B14 | **Single SQLite conn self-deadlock**: `DiscoverAll` iterates open `rows` while calling `Discover()` which issues new queries → wedges the only connection; entire gateway stalls until restart. One authenticated POST triggers it. | `internal/discovery/discovery.go:384–399` (cf. correct pattern comment in `user/store.go:81–83`) |
| B15 | **Response cache not scoped per key/org** — completions cache key is `endpoint+model+body`; cross-tenant cache HITs serve another key's completion and skew cost attribution. (`modelsCacheKey` already does this right.) | `internal/proxy/proxy.go:229–235,338` |

### Data & platform
| # | Finding | Location |
|---|---------|----------|
| B16 | **Budget enforcement fails open + double-spend window**: admission check is post-hoc `SUM(cost_usd)` per request (O(all month rows) on every call); N concurrent requests share one snapshot → unbounded overshoot under burst; every query error returns "allow"; `RecordUsage` is a stub. Compare LiteLLM's atomic reserve guarantee. | `internal/budget/budget.go:97–145` |
| B17 | **Synchronous webhook POST (5s timeout) inside the request path** — every over-quota 429 and audited mutation can stall 5s against a slow sink; goroutines pile onto the single conn. Needs queue + worker. | `internal/webhook/dispatcher.go:55–80` |
| B18 | **DATETIME scanned into string ships dead features**: go-sqlite3 converts timestamp-declared columns to `time.Time`; scanning into `string` errors and the handler swallows it → **BillingExport CSV emits header-only CSVs**, Profile Activity/Logins permanently empty. Also broken under lib/pq. | `internal/handler/admin.go:899–912`, `internal/handler/profile.go:143–158,193–199` |
| B19 | **Postgres mode is broken on first boot**: `INSERT OR REPLACE` (SQLite-only) ×4 sites, `reasoning = 1` against PG BOOLEAN, migrations use SQLite types verbatim, some queries skip `db.Q()` rebind. README advertises it ("Phase 3") — overstated. Decide: fix or drop the claim. | `internal/catalog/catalog.go:88,194,224`, `internal/db/db.go:160`, `handler/catalog.go:174,219` |
| B20 | **No CI, no linter, no Dockerfile/deploy artifacts.** Nothing automated runs tests/lint/build/docker-smoke anywhere; every green claim is local discipline. Makefile lacks lint/race targets; `npm install` used instead of `npm ci`. | repo-wide |

---

## P1 — HIGH (first week of operation)

- **SSF… SSRF decorative, redirects leak upstream keys**: `validateBaseURL` blocks 3 hostname literals only; no private-range/DNS-rebinding defense; proxy follows up to 10 redirects re-sending custom headers (`x-api-key`) to whatever host redirects point at. Member-role users can create providers. — `provider/store.go:165–192`, `proxy.go:303–330`
- **No session revocation + XSS-readable tokens**: stateless 24h JWTs survive password change/disable/delete; `gw_token` cookie is `HttpOnly:false`, no `Secure`; SPA duplicates JWT into localStorage. — `auth.go:76–99`, `admin.go:97–113`, `web/src/App.tsx:28–36`
- **CSRF**: cookie-authenticated mutating `/api` (and spend-capable `/v1`) routes have no CSRF defense beyond SameSite=Lax. — repo-wide
- **RBAC gaps**: `readonly` users can mutate catalog/aliases/settings and fire `discover-all` across all orgs (those routes sit behind bare AdminMiddleware, unlike providers/keys/orgs). — `main.go:244–257`
- **Fallback stops on any 4xx incl. 429** — rotated credentials or quota-limited primary ends routing despite healthy alternates. — `fallback.go:198–206`
- **Circuit breaker v2 needed**: no half-open single-probe storm protection; one success wipes history and clears open state; 429 (one tenant's quota) trips the global breaker; in-memory only (meaningless multi-replica); params hardcoded in main.go, not configurable (LiteLLM: per-deployment `allowed_fails`/`cooldown_time`). — `resilience/circuit.go`, `main.go:103`
- **`/v1/responses` wrong-protocol fallback**: when native path fails, OpenAI Responses clients receive raw Anthropic/chat-shaped bodies; stream event shapes don't match either. — `proxy.go:1245–1300`, shape gate at `proxy.go:466`
- **Audit actor spoofable & coverage narrow**: `X-Actor` header trusted for attribution; logins/user changes not audited. — `audit/audit.go:50–57`
- **Missing schema hardening**: no index on `request_logs(key_prefix[, created_at])` hot paths; `gateway_keys.prefix` indexed but not UNIQUE (2³² space collides at ~65k keys → coalesced budget buckets). — `migrations/001_initial.sql:20`
- **Multi-statement ops not transactional**: DeleteOrg interleaves 4 autocommits; `CreateKey` swallows limits-update failure → enabled unlimited key differs from what operator asked. — `handler/org.go:103–113`, `admin.go:329–350`
- **Raw error strings + hand-built JSON to clients** (breaks on quotes in err.Error()); several decode sites lack MaxBytesReader. — `org.go:63`, `admin.go:241,767,821`
- **Translation correctness**: unchecked type assertion panics client-triggerable on `input:{content}` without role (`translate.go:1086`); OpenAI→Anthropic silently forces `max_tokens:1024` truncating generations (`translate.go:717–721`).
- **Usage/accounting accuracy**: SSE usage parsed only if it lands within one 8KB read buffer → intermittent cost=0 rows; TPM estimate counts base64 image bytes as tokens; actual usage never reconciled back into limiter buckets. — `proxy.go:94–129,398–434`
- **TRUSTED_PROXIES parsed and never used** while XFF/CF headers trusted unconditionally (known). Rate-limit/audit identity spoofable from direct connections. — `main.go:342–376`
- **Graceful shutdown gives streams 10s** then exits mid-generation on every deploy. — `main.go:330–337`
- **openapi.yaml drift**: ~35 live routes undocumented (passkeys, profile, org writes, provider-models, stats/logs/billing, no-prefix aliases), 1 documented route missing (`GET /api/users` vs real `/api/admin/users`), spec version 2.0.0 vs code 1.6.0; spec served from filesystem with hardcoded absolute path baked into binary → 404 outside original checkout; should be `go:embed`. — `openapi.yaml`, `main.go:294–303`

## P2 — MEDIUM (hardening backlog)

- Float64 money with truncating cents conversion biases budgets/billing (`budget.go:124–140`) → integer micro-cents end-to-end.
- Quota windows hardcoded UTC day/month; MemoryLimiter uses different semantics than DBLimiter; no rollover tests (`budget.go:48–51,104–133`).
- Fixed-window rate limiter allows ~2× limit bursts at boundaries; partial window consumption on multi-window denial (RPM slot burned when RPH denies); atomicity needs one Lua script (`ratelimit.go:63–102`).
- Key tables lack `expires_at`/`metadata`/rotation (LiteLLM virtual-key lifecycle parity gap); keys live forever until manual revoke (`models.go:27–45`).
- Payload logging columns exist but nothing writes them (`request_body/response_body` permanently NULL); once wired, needs redaction gating + retention (LiteLLM `log_request_and_response` parity). Combined with absent retention/purge job → unbounded growth (`db.go:349–350`, `proxy.go:171–175`).
- Cache TTL hardcoded 10s, eviction pseudorandom-not-LRU, expired entries squat capacity; Redis mode double-writes a local mirror producing divergent stale answers after flaps (`cache/memory.go:42–51`, `redis.go:76–91`).
- Retry/backoff sleeps ignore context cancellation (`proxy.go:378,448`).
- Header residue across providers on fallback (invert skip-list to allowlist) (`proxy.go:306–315`).
- `Verify()` does a write per authenticated call (`last_used_at`) amplifying WAL churn (`apikey.go:254–256`).
- Stats materializes all latency rows + ~10 sequential aggregates per dashboard load (`admin.go:369–520`).
- Username enumeration signals + weak password policy residue (`admin.go:105–108`, `profile.go:111–115`).
- WebAuthn RP origins always include localhost even in prod; CORS entries appended to RPOrigins unvalidated (`passkey/webauthn.go:20–45`).
- Migration runner: `dirty` flag written never read; ALTER errors swallowed wholesale; no per-migration tx on PG path (`db/db.go:146–176,296–355`).
- Mixed timestamp dialects written by two update statements (CURRENT_TIMESTAMP vs time.Time binds) (`users.go:133–141`).
- DSN param sniffing substring test can silently drop WAL/busy_timeout pragmas (`db/db.go:105–110`).

## Test gaps (ranked)

1. Streaming fallback/retry: unimplemented *and* untested — add test asserting stream 502 pre-commit falls through to candidate B.
2. `internal/resilience` (prod-path breaker/retry): zero unit tests; documented half_open state doesn't exist in implementation.
3. Zero concurrency/contention tests: ratelimiter (no test file at all), budget limiter parallel Check/Record — current `-race` green proves nothing; add goroutine-spray `-race` tests.
4. Mid-stream failure injection: no test that errors surface or that usage/status are recorded.
5. Redis cache/limiter paths (incl. race B13): untested — miniredis-based suite.
6. Passkey flows + WebAuthn origin derivation: untested; OIDC handler untested (which is how B1 survived) — regression test must assert refusal without issuer config.
7. Postgres migrations/queries never exercised; PG job blocked until P0-B19 resolved.
8. Webhook dispatcher sync-timeout behavior untested.

## LiteLLM feature-parity gaps

Already strong vs LiteLLM: models.dev catalog w/ costs+reasoning metadata, native Anthropic
`/v1/messages` + Responses bridge, TTFT/TPS analytics, per-key model allowlists w/ wildcards,
per-key RPM/RPH/RPD/TPM + daily/monthly budgets, exact cache, RBAC/org scaffold, passkeys.

Still missing for true LiteLLM-class parity:

- **P0-parity**: configurable upstream/stream timeouts per deployment; per-provider `allowed_fails`/`cooldown_time`; team-level budgets with `budget_duration` resets; payload logging w/ redaction + retention; streaming failover (see B11).
- **P1-parity**: routing strategies (weighted/least-busy/latency-based) + weights; retry policy per model/status-class; cache TTL + per-request bypass (+semantic later); observability integrations beyond one webhook (Langfuse/Slack/Sentry/OTel traces); pass-through endpoints; guardrails/moderation/PII masking; TPM reconciliation with real usage; model groups/tags (aliases currently single-target).
- **P2-parity**: prompt management/templates, semantic caching, mock/load-test mode, health-gated routing, SSO auto-provisioning.

## Suggested sequencing

1. **Wave 1 — security stop-the-bleed** (small diffs, days): B1/B2 (gate OIDC, trust claims only), B6 (consume codes), B7 (fail-closed defaults), B3 (bcrypt migration — keep SHA256 verifier during cutover), B4 (login rate limits), B5 (org scoping on id-addressed routes), B9 (fail closed in prod), B14 (materialize IDs before Discover).
2. **Wave 2 — reliability core** : B10 (timeout architecture: transport timeouts + Client.Timeout=0 + idle watchdog + per-route WriteDeadline), B11/B12 (stream pre-commit retry/fallback + terminal SSE events + breaker feed), B13 (lock fix), B15 (cache scoping), B16 (atomic budget ledger + rollups + fail-closed), B17 (async webhook queue), B18 (datetime scans).
3. **Wave 3 — platform**: B20 CI bootstrap (vet+gofmt+race+docker+smoke; golangci config), Dockerfile/compose, embed openapi.yaml + spec regeneration, session revocation/cookie hygiene, PG dialect decision, breaker v2 + configurable resilience params, schema indexes/UNIQUE prefix.
4. **Then**: P2 backlog and LiteLLM P1-parity features per demand.
