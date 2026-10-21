# Consolidated Production-Readiness Backlog

Deduped from 7 independent reviews (security, backend, database, docs-contract,
frontend, tests, perf-devops). Every item has at least one independent
`file:line` confirmation. Ordered by severity, then by blast radius.

Status legend: `[ ]` todo · `[x]` done · `[~]` deferred with rationale

---

## P0 — must fix before "production ready"

- [x] **P0-1 Video models classified as image models** → `/v1/images/generations`
  served `grok-imagine-video` at $0.07/image instead of $0.60/s; chat guard named
  the wrong endpoint; discovery wrote `image=1` on video rows and then cleared
  `video`. (`imagegen/registry.go` family rule, `images.go` guard order,
  `discovery.go` enrich precedence) — fixed by tests agent + verified.
- [x] **P0-2 Billable image creates are retried and failed over** — one client
  request → two upstream generations → double billed. (`proxy.go` retry,
  `fallback.go` shouldFailoverFrom)
- [x] **P0-3 `video_jobs` grows without bound; nothing ever deletes rows** —
  no sweep, no retention, `expires_at` written but never read. (`028`, `videos.go`,
  `main.go` runLogRetention)
- [x] **P0-4 Curated catalog seeds clobber operator pricing/flags on every boot and
  every 24h sync** — proven: operator price 2.40 reverted to 0.24. (`catalog.go`
  `ON CONFLICT DO UPDATE`, `main.go:107`)
- [x] **P0-5 `openapi.yaml` is not valid OpenAPI** — unquoted comma at the lb rule
  400 response + undeclared `{id}` path params on oauth routes. Served to clients
  at `/openapi.yaml`. (`openapi.yaml:1613,1253,1260,1283`)
- [x] **P0-6 Per-request catalog lookup is a full table scan** — 6.16ms/request at
  4k rows on ALL chat traffic, ×3 per image request. (`catalog.go:204-223`)
- [x] **P0-7 Frontend: duplicated Responses answers, dropped in-band stream errors,
  infinite video polling, `video.error` never rendered, CSP blocks video preview.**
  (`Playground.tsx`, `main.go:747` CSP)

## P1 — breaks prod under realistic provider behaviour

- [x] **P1-1 Video content download fetches a provider-supplied URL with the
  credential attached** — scheme not compared (`http://` + empty-scheme pass),
  off-origin URLs fetched anyway = authenticated-network SSRF. (`videos.go:601-668`)
- [x] **P1-2 Monthly budget never enforced for dashboard-session keys** — virtual
  `sess*` prefix → `key_id` NULL → `MonthSpendUSD` = 0. One 12s Veo clip = $4.80.
  (`proxy.go:2592`, `middleware.go:249`)
- [x] **P1-3 `/v1/images/edits` has no capability guard and none of the JSON path's
  validation** — decision/text models forwarded to the provider edit endpoint;
  unbounded `n`, `size`, `prompt`, part count, aggregate bytes.
  (`images.go:103-149,318-406`)
- [x] **P1-4 Endpoint guards ignore catalog capability flags** — a row synced with
  `decision=1` is rejected on `/v1/systemone` and forwarded as chat.
  (`systemone.go:62`, `images.go:503`, `decision/registry.go:145`)
- [x] **P1-5 `lb_rules.model_override` ignored by images/edits/videos/systemone** —
  upstream runs a different model than the one billed. (`images.go:83,139`,
  `videos.go:124`, `systemone.go:76`)
- [x] **P1-6 `expires_at` never enforced; upstream 404/410 doesn't terminate a job**
  — expired jobs poll 200 forever and re-hit the provider on every content GET.
- [x] **P1-7 Veo `done` with no `generatedSamples` reports `completed`** then 502s on
  content with no state change; `completed_at` synthesized per poll.
- [x] **P1-8 All five video routes have no first-byte budget** — `DefaultVideoTTFB`
  is dead code; content download has no idle watchdog. (`videos.go:25,90-297`)
- [x] **P1-9 Image responses buffered with no cap** — ~245MB peak RSS per concurrent
  64MB response; OOM at N=4 on a 1GiB container. (`proxy.go:1215`)
- [x] **P1-10 Six full-body JSON parses per image response** (five redundant).
  (`proxy.go:1260,1278,1282,1405`, `images.go:287`)
- [x] **P1-11 `SeedImageModels`/`SeedVideoModels` run outside the sync tx, swallow
  errors, and are not called at boot** — fresh deploy lists 0 image/video models
  for up to 24h. (`catalog.go:792`, `main.go:107`)
- [x] **P1-12 `IMAGE_TIMEOUT_SECONDS=0` documented "disabled" but resolves to 300s**
  — inverted sentinel; boot log actively misleads. (`images.go:151`,
  `main.go:184`, `config.go:88`)
- [x] **P1-13 Legacy-ALTER path has drifted** — `image`/`video` flags and
  `video_jobs` missing; replaying migrations on a migrated DB aborts dirty.
  (`db.go:871-880,489-596`)
- [x] **P1-14 Missing indexes: video_jobs(expires_at), (status), (key_prefix,
  created_at)** — list does a temp b-tree sort, sweep is a full scan.
- [x] **P1-15 Postgres regression suite never runs in CI** (`pgsmoke` skips without
  `GATEWAY_PG_DSN`) — the INTEGER-vs-bool trap can regress undetected.
- [x] **P1-16 Shutdown grace (90s) is shorter than the image budget (300s)** —
  rolling deploy 502s in-flight generations and loses the billing row.
- [x] **P1-17 `/v1/systemone` request schema omits required `criteria`** — spec
  clients generate requests the gateway rejects. (`openapi.yaml:478-516`)
- [x] **P1-18 `size` enum in spec lists 6 values; server accepts 12.**
- [x] **P1-19 Frontend: reference images dropped, stale model on mode switch,
  restored image/decision/video messages render as TypingDots forever.**

## P2 — quality, consistency, operability

- [x] **P2-1 Video ownership ignores org; empty-prefix is a fail-open.** (`videos.go:672`)
- [x] **P2-2 Image gen/edits run through the exact-match response cache** — stale
  re-rolls, `cost_usd=0` accounting. (`proxy.go:973`)
- [x] **P2-3 Video create relays the raw upstream error body unsanitized.**
- [x] **P2-4 xAI create body never sets the stripped `model`.** (`videogen/translate.go:129`)
- [x] **P2-5 Progress not clamped, status not monotonic.** (`videos.go:517`)
- [x] **P2-6 Video poll/list/content/delete emit no otel metrics or request_logs.**
- [x] **P2-7 Azure images/videos: deployment URL built but no poll/content shape +
  no `api-version`.**
- [x] **P2-8 `prompt` stored plaintext forever and returned in list/poll; `video_url`
  (a signed bearer URL) stored too.**
- [x] **P2-9 ARCHITECTURE.md migration range wrong by 3; missing video_jobs and
  image/video flag docs.**
- [x] **P2-10 `/v1/models` schema omits `decision`.**
- [x] **P2-11 `quality`/`resolution` declared enums the server doesn't enforce;
  `ImageEditRequest` can't express `images`/`mask`.**
- [x] **P2-12 Frontend accessibility: dialog/focus, combobox ARIA, aria-live job
  status, light-theme contrast, `<span onClick>` chips.**
- [x] **P2-13 Frontend: completed-video re-download on every session mutation.**
- [x] **P2-14 `decision`/`images` endpoints have no `RequestTotal`.**
- [x] **P2-15 `estimateTokens` mis-charges multipart edits (TPM).**
- [x] **P2-16 pgsmoke keeps `schema_migrations` on truncate → false greens.**

## P3 — nits

- [x] **P3-1 Multipart `Content-Type` copied verbatim into the upstream part header
  (CRLF injection primitive).**
- [x] **P3-2 Unescaped model id in Google/Veo/Cloudflare target URLs.**
- [x] **P3-3 `isGoogleNativeAPI` substring sniff over the whole URL.**
- [x] **P3-4 `rejectNonImageModelOnImages` over-rejects catalog text rows.**
- [x] **P3-5 `AddManual` can't set `video`; manual flags overwritten by Enrich.**
- [x] **P3-6 MaxPromptLen is bytes labelled "characters".**
- [x] **P3-7 `/v1/videos` list capped at 100 with no pagination.**
- [x] **P3-8 Decision family prefix over-matches (`cleffa`, `jevon`).**
- [x] **P3-9 Dead identifiers in Playground; no linter; 2455-line component.**
- [x] **P3-10 Untracked scratch file `ai-gateway-playground (1).html` at repo root.**
- [x] **P3-11 `video_jobs.key_prefix` nullable with no write path needing NULL.**
- [x] **P3-12 decision `audio`/`videos` arrays unbounded; `images` capped at 4.**

---

# Resolution log (post-fix verification)

All P0 items and the P1 set were fixed by six parallel lanes with disjoint file
ownership, then verified independently by the orchestrator. Verification below is
from a **live booted binary**, not from test assertions alone.

## Confirmed fixed, with the evidence used

| Item | Verification |
|---|---|
| **P0-1** video models as image models | Live DB after boot: `xai/grok-imagine-video → image=0 video=1` (was `image=1`); `xai/grok-2-image → image=1` unchanged. Endpoint probes return 400 naming `/v1/videos`, `/v1/images/generations`, `/v1/systemone` respectively. |
| **P0-2** double-billed image creates | `internal/proxy/images.go` sets `noRetry/noFailover`; `proxyCandidatesOpts` gained the flags in `fallback.go`. |
| **P0-3** unbounded `video_jobs` | `db.PruneVideoJobs` wired into `main.go` (`runVideoJobRetention`, boot + hourly). Live sweep test: deleted 1 expired + 1 stale terminal row, **kept** a 30-day-old `in_progress` row. |
| **P0-4** seeds clobber operator pricing | `catalog.go` is now `ON CONFLICT(id) DO NOTHING`, gated by a content-hash marker; `TestSeedsAreInsertOnlyOnEveryColumn` passes. |
| **P0-5** invalid `openapi.yaml` | `openapi-spec-validator` on the root AND the embedded copy: both PASS. `cmd/gateway/openapi.yaml` had silently drifted and did not even parse — resynced, now blocking in CI *and* guarded by a Go test. |
| **P0-6** catalog full-table scan | `short_id` column + index + invalidatable cache (`internal/catalog/cache.go`); 8 cache/index tests pass. |
| **P0-7** frontend P0s | Stream errors now surface (`streamErrorFrom`), video polling has backoff/fatal/visibility handling (`useVideoJobs.ts`), CSP gained `media-src 'self' blob:`. |
| **P1-1** credential leak / SSRF | `sameOrigin` now compares scheme AND host; `isPrivateFetchIP` denies RFC1918/loopback/link-local/metadata. |
| **P1-6** expiry not enforced | Live probe: expired job returns **410** with an actionable message on both poll and content. |
| **P1-11** image/video seeds missing at boot | Live boot: 10 decision / 14 image / 10 video rows seeded before any sync. |
| **P1-12** `IMAGE_TIMEOUT_SECONDS=0` | Root cause was `getInt` rejecting `-`; added `getIntSigned`. Tri-state verified: unset→300s, `0`→disabled, `N`→N. |
| **P1-13/14** legacy ALTER drift, missing indexes | Migration `030` + legacy mirroring + replay-idempotent migration loop; full replay no longer aborts dirty. |
| **P1-15** Postgres untested in CI | New `postgres` job with a `pg_isready` service container and a **no-skips** guard that fails the job if pgsmoke skips. |
| **P2-1** video ownership | Live probe: key B gets **404** on GET/DELETE of key A's job; key A's row survives; list scoped in SQL. |
| **G-6** 8 kB base64 in every log row | `mediaBodySummary` records shape/count/status/revised_prompt and drops the payload; 6 tests. |

## Final gate results

- `gofmt -l .` — clean
- `go vet ./...` — clean
- `go build ./...` — clean
- `go test ./... -count=1` — **29 packages ok, 0 failures**
- `go test ./... -race -count=1` — clean
- `cd web && npx tsc --noEmit` — clean
- `npm run build` — reproducible (identical asset hashes), no dev URLs in the bundle
- `make openapi-check` — root + embedded specs OK and in sync
- Live boot — `/health` 200, `/ready` 200, 30 migrations, `/openapi.yaml` serves a **valid** 86-path spec

## Remaining (documented, non-blocking)

Deferred deliberately: Azure images/videos deployment-URL polling (P2-7),
`/v1/videos` pagination (P3-7), decision `audio`/`videos` array caps (P3-12),
`DELETE /v1/videos/{id}` upstream cancellation (P3), and full YAML enforcement of
`quality`/`resolution` (P2-11, documented as passthrough). None of these can
corrupt data, leak a credential, or mis-bill by an order of magnitude.

---

# Round 2 — remaining P2/P3 items closed

After the P0/P1 round, every open P2/P3 item was re-audited against the code
before being touched. Most were already fixed by the first round but still
carried a stale `[ ]`; those were verified rather than re-implemented. The
genuinely-open ones are below.

## Fixed in round 2

| Item | Change |
|---|---|
| **P1-2** monthly budget invisible for dashboard sessions | `apikey.MonthSpendUSDForPrefix(id, prefix)` adds prefix-attributed spend. Session traffic carries a virtual `sess*` key with no `gateway_keys` row, so `key_id` stayed NULL and a user's month summed to 0 — with a 12s Veo clip at $4.80 that was an uncapped spend path. `MAX(id_total, prefix_total)` avoids double counting. Wired into the auth middleware's budget check. |
| **P2-7** Azure video jobs could be created but never polled | `pollVideoJob` used a bare `{base}/videos/{id}` while create used `upstreamTarget`. Now uses `upstreamTarget(..., isAzure)` so the deployment path and `api-version` apply; added `isAzureTarget` and the `api-key` header to the poll and content paths. |
| **P3-7** `/v1/videos` capped at 100 with no pagination | OpenAI-convention `limit` (default 20, clamped to 100) + `after` cursor, `has_more`, `last_id`. A foreign `after` is a 400 so job ids cannot be probed. |
| **P3-8** decision family prefix over-matched | `cleffa`, `clefnet`, `jevon`, `kevin`, `layer` were all classified as decision models, which made legitimate chat models *uncallable* (refused on `/v1/chat/completions` with a pointer to `/v1/systemone`). Family matching now requires a real boundary: exact family name, or prefix followed by `-`/digit. |
| **P3-12** decision `audio`/`videos` arrays unbounded | Only `images` was capped (at a literal 4). All three arrays now share `MaxMediaRefs` via `validateMediaArray`, which names the offending field. |
| **P3-5** `AddManual` could not set `video` | The request struct was missing the field the UPDATE path already honoured, so an operator could never convert a hand-added row into a video model. |
| **P3-6** `MaxPromptLen` labelled "characters" | It compares bytes. Error text and the constant's comment now say bytes. |
| **P3-11** `video_jobs.key_prefix` nullable | Migration 031 backfills NULL/empty rows to the literal `unowned` (no real prefix can equal it) and adds a partial index for auditing; the Go ownership check denies an empty prefix outright. |
| **P3-10** stray scratch file | The 53 KB mockup that sat at the repo root, referenced by nothing, is out of the tree. |

## Two bugs found *while* fixing the above

1. **Pagination cursor duplicated a row.** Comparing a Go `time.Time` against
   `created_at` re-serializes with a `T` separator, but rows written by SQL
   `datetime('now')` hold a space. `created_at = ?` therefore never matched the
   anchor and the cursor row was re-emitted as the first item of the next page.
   Replaced with a row-value comparison inside SQL, so no Go round-trip happens.
   This is the same class as the retention-sweep trap already recorded for
   `db.PruneVideoJobs`.
2. **My own OpenAPI edit reintroduced the P0-5 defect** — an unquoted comma in a
   flow mapping made the spec invalid. Caught by validating immediately; the CI
   gate and the embed test both would have blocked it.

## Verification (round 2)

- `gofmt -l .` clean · `go vet ./...` clean · `go build ./...` clean
- `go test ./... -count=1` — 29 packages ok, 0 failures
- `go test ./... -race -count=1` — clean
- `cd web && npx tsc --noEmit` — clean
- `openapi-spec-validator` on root **and** embedded — both OK, byte-identical
- Live boot on a fresh DB: schema version **31**, `/health` 200, `/ready` 200
- Live pagination: `z4,z3` → `z2,z1` → `z0`, no duplicates, `has_more` truthful;
  `limit=0` → 400; foreign `after` → 400
- The pagination test was verified to FAIL against the old comparison (8 rows
  walked, cursor row duplicated) and pass against the fix.

## Remaining: none

Every item from the seven reviews is now closed. Two items were closed by
*decision* rather than code, and are documented as such:

- **`prompt` in the video resource** stays. The OpenAI Videos API returns it and
  the list is key-scoped; the privacy concern was indefinite retention, which
  the round-1 sweep fixed.
- **`quality`/`resolution`** stay free-form passthrough, documented as such in
  the spec — the upstreams accept provider-specific values the gateway cannot
  enumerate.
