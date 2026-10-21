# Test coverage review — new protocol surfaces

Scope: the three new protocol surfaces (decision/System One, image generation,
video generation) plus their supporting infrastructure (migrations 026–029,
`db.BoolInt`, discovery capability flags, route registration).
Baseline: the test files listed in the brief, which already existed and were
left untouched (nothing weakened or deleted).

Commands used for every number below:

```
cd /home/karutoil/ai-gateway
go test ./internal/proxy/ ./internal/imagegen/ ./internal/videogen/ ./internal/decision/ ./internal/catalog/ ./internal/db/pgsmoke/ -cover
go test ./... -count=1
```

---

## 1. Per-package coverage

| package | before | after | note |
|---|---|---|---|
| `internal/proxy` | 69.5% | **72.6%** | +3.1 pts; new-surface functions went 0–45% → 68–95% |
| `internal/imagegen` | 72.6% | **87.4%** | +14.8 pts |
| `internal/videogen` | 75.0% | **89.9%** | +14.9 pts |
| `internal/decision` | 79.1% | **95.6%** | +16.5 pts |
| `internal/catalog` | 83.5% | **83.5%** | unchanged (new tests cover `SeedVideoModels`, fully covered already) |
| `internal/db/pgsmoke` | n/a | n/a | `[no statements]` — test-only package; **all Postgres tests SKIP** without `GATEWAY_PG_DSN` |
| `internal/db` | n/a (no test files) | **59.4%** | new; migration/dialect suite added |
| `internal/discovery` | 51.5% | **51.5%** | unchanged by % (the new video-seeding tests exercise already-covered helpers) |
| `cmd/gateway` | 3.4% | **3.4%** | new AST-based route-registration guard (no production statements covered) |

Per-function coverage of the three new proxy handler files after the work:

```
images.go:43   ImageGenerations         80.6%
images.go:103  ImageEdits               80.0%
images.go:186  imageUpstreamRequest     83.3%
images.go:209  imageEditUpstreamRequest 78.6%
images.go:310  parseEditMultipart       94.7%
images.go:580  rejectNonImageModelOnImages 93.8%
systemone.go:30  SystemOne              89.5%
systemone.go:108 decisionUpstream       88.9%
videos.go:90   VideoCreate              83.1%
videos.go:189  VideoGet                 84.0%
videos.go:226  VideoContent             71.4%
videos.go:292  VideoDelete              80.0%
videos.go:378  videoCreateRequest       89.5%
videos.go:542  normalizeVideoPoll       87.5%
videos.go:601  videoContentURL          88.9%
```

Remaining low spots are the *unreachable-by-design* branches of the shared
`proxyCandidates` machinery (`all provider attempts failed` tail paths) and
credential-decryption failures, which need a corrupted provider row.

---

## 2. Coverage map — endpoints

Legend: ✅ proven by a test named here (new tests marked **NEW**), ⚠️ proven only
for a subset, MISSING = no test.

### `POST /v1/systemone` (+ `/systemone`)

| invariant | test |
|---|---|
| happy path proxies a typed decision request | `TestSystemOneProxiesDecisionRequest` |
| provider-qualified id is stripped before upstream | `TestSystemOneQualifiedModelStripsProviderPrefix` |
| malformed body → 400 naming the field | `TestSystemOneRejectsMalformedBody`, `TestValidateRejectsMalformedRequests`, **NEW** `TestValidateRemainingBranches` |
| 1..64 questions bound, ids, instructions, noul/choice/score criteria | `TestValidateRejectsMalformedRequests`, **NEW** `TestValidateRemainingBranches` |
| deterministic error ordering across map iteration | **NEW** `TestValidateDeterministicQuestionOrder` |
| chat model on /v1/systemone → 400 naming the right endpoint | `TestWrongModelOnSystemOneNamesRightEndpoint` |
| **video model on /v1/systemone → 400 naming /v1/videos** | **NEW** `TestVideoModelRefusedOnSystemOne` |
| decision model on every text endpoint → 400 | `TestDecisionModelRefusedOnEveryTextEndpoint` |
| decision provider never serves chat | `TestChatEndpointDoesNotRouteToDecisionProvider` |
| Cloudflare dialect: model in path, short selector in body | **NEW** `TestSystemOneCloudflarePathAddressing` |
| generic systemone/kev/llama.cpp target building | `TestDecisionTargets`, `TestIsDecisionProvider` |
| unroutable: no decision provider / strict-routing 404 | **NEW** `TestSystemOneNoCandidatesBranches` |
| model alias resolved before routing | **NEW** `TestSystemOneModelAliasIsResolved` |
| unusual requests never reach upstream | `TestRealDecisionModelStillRoutes` + **NEW** bounds test |
| **oversize body → 413** | **NEW** `TestSystemOneQuestionBoundAndOversizeBody` |
| auth / allowlist / TPM / revocation | **NEW** `TestNewEndpointsRequireGatewayAuth`, `TestModelAllowlistEnforcedOnNewEndpoints`, `TestAllowlistedCapabilityModelIsServed`, `TestRevokedKeyCannotUseNewEndpoints`, `TestNewEndpointsEnforceTPM` |
| `/v1/models` lists decision models | `TestDecisionModelsAreListed` |
| catalog seeding + sync flags | `TestSeedDecisionModelsAndLookup`, `TestSyncMarksDecisionModelsByRegistry` |

### `POST /v1/images/generations` (+ aliases)

| invariant | test |
|---|---|
| OpenAI-compatible passthrough (path, model prefix stripped, auth, billing) | `TestImageGenerationsOpenAIPassthrough` |
| Gemini `:generateContent` translate + response translate | `TestImageGenerationsGeminiTranslate`, `TestGeminiToShape`, `TestGeminiToURLFormat` |
| Imagen `:predict` translate | `TestImageGenerationsImagenTranslate`, `TestImagenToShape`, **NEW** `TestImagenRequestBodyFull` |
| dialect chosen by provider TYPE, not only registry | **NEW** `TestImageDialectSelectionByProviderType` |
| local validation (prompt, n) before upstream | `TestImageGenerationsValidatesLocally` |
| **prompt length bound (32000) + boundary accepted** | **NEW** `TestImageGenerationsRejectsOverlongPrompt` |
| reference-image shapes (data URL / base64 / object) + errors | **NEW** `TestParseRequestImageShapes`, `TestParseRequestImageErrors` |
| mask handling | **NEW** `TestParseRequestMask` |
| reference-image count bound (exactly 4) | **NEW** `TestParseRequestReferenceBoundary` |
| remote URLs rejected (no SSRF) | `TestParseRequestRejectsRemoteImageURL`, **NEW** `TestImageEditsRemoteImagePartIsRejectedInJSONPath` |
| per-image billing, n-scaled | `TestImageGenerationsOpenAIPassthrough`, `TestImageGenerationCountsMultipleImages` |
| **error mapping: Google 400/429/500, Imagen 400, [OI] 400** | **NEW** `TestImageGeminiErrorBodyIsRelayed`, `TestImageImagenErrorBodyIsRelayed`, `TestImageOpenAICompatibleErrorIsRelayed` |
| blocked Gemini 200 (no image) is not a fake success and is not billed | **NEW** `TestImageGeminiResponseWithoutCandidatesIsNotAFakeSuccess` |
| image-only model refused on all text endpoints | `TestImageOnlyModelRefusedOnTextEndpoints`, **NEW** `TestImageOnlyModelRefusedOnRemainingTextEndpoints` |
| image-CAPABLE model still chats | `TestImageCapableModelStillChats`, `TestImageCapableModelNotRefusedOnChat` |
| decision model refused on images | `TestDecisionModelRefusedOnImages`, **NEW** `TestNoCapabilityModelCrossesIntoAnotherEndpoint` |
| video model refused on images | **NEW** `TestNoCapabilityModelCrossesIntoAnotherEndpoint` |
| text model refused on images | **NEW** `TestChatModelRefusedOnNewCapabilityEndpoints` |
| **xAI video model must NOT be an image model** | **NEW** `TestGrokImagineVideoIsNotAnImageModel` (found a live bug — see §5) |
| `/v1/models` flags image / image_only / image_cost | `TestModelsListingFlagsImageModels`, `TestModelsListingExposesImageOnly` |

### `POST /v1/images/edits` (+ aliases)

| invariant | test |
|---|---|
| multipart passthrough (prefix stripped, image part intact) | `TestImageEditsMultipartPassthrough` |
| multipart boundary preserved upstream | `TestEditMultipartContentTypeReachesUpstream` |
| Google edits inlined into `inlineData` | `TestImageEditsGoogleInlinesImages` |
| requires at least one image | `TestImageEditsRequiresImage` |
| non-multipart body rejected | `TestImageEditsRejectsNonMultipart` |
| **missing boundary / malformed body / missing model / missing prompt / too many images** | **NEW** `TestImageEditsMultipartValidation` |
| **options (n/size/quality/style/response_format/aspect_ratio/resolution) + mask forwarded** | **NEW** `TestImageEditsForwardsOptionsAndMask` |
| **non-numeric `n` falls back to the default** | **NEW** `TestImageEditsNonNumericNIsIgnored` |
| `images` vs `image` field tolerance | `TestParseEditMultipartAcceptsImagesField` |

### `POST /v1/videos` and job routes (+ aliases)

| invariant | test |
|---|---|
| create is async, returns `queued`, persists upstream handle + billing | `TestVideoCreateReturnsQueuedJob` |
| poll → content, terminal jobs served from storage (no re-poll) | `TestVideoPollThenContent` |
| content before ready → 409 | `TestVideoContentBeforeReady` |
| failed job surfaces the provider message (poll + content) | `TestVideoFailedJobReportsError` |
| local validation (model/prompt/seconds/size) | `TestVideoValidatesLocally` |
| **prompt length bound (32000) + body cap 413** | **NEW** `TestVideoPromptBoundAndOversizeBody` |
| list is key-scoped; delete forgets the job | `TestVideoListAndDelete` |
| **ownership matrix: GET / content / DELETE cross-key all 404** | **NEW** `TestVideoCrossKeyOwnershipOnEveryRoute` |
| cross-key list is empty | `TestVideoJobScopedToKey` |
| **empty-prefix visibility rule (gap pinned)** | **NEW** `TestVideoEmptyPrefixCannotDeleteForeignJob`, `TestVideoJobWithoutKeyPrefixIsVisibleToEveryone` |
| **expiry (`expires_at`) enforced?** | **NEW** `TestVideoExpiredJobIsStillServedFromStorage` — NOT enforced (P2) |
| **provider content errors mapped (5xx/403 keep status, envelope used)** | **NEW** `TestVideoContentUpstreamErrorsAreMapped` |
| **transient poll failure serves last-known state, then recovers** | **NEW** `TestVideoPollUpstreamFailureServesLastKnownState` |
| **status vocabulary normalization (succeeded/processing/pending/error/unknown)** | **NEW** `TestVideoStatusNormalizedFromDialectVocabularies` |
| **upstream create refusal relayed; handle-less 2xx → 502 and no job row** | **NEW** `TestVideoCreateUpstreamErrorIsRelayed` |
| **xAI dialect end-to-end (create path, request_id, done→completed, CDN download without key)** | **NEW** `TestVideoXAIDialectCreatePollContent` |
| **xAI moderation rejection → failed job** | **NEW** `TestVideoXAIModerationRejectionIsFailed` |
| **Veo dialect end-to-end (predictLongRunning, operation poll, x-goog-api-key)** | **NEW** `TestVideoVeoDialectEndToEnd` |
| **Veo URL builders (4 base shapes × create + operation)** | **NEW** `TestVideoVeoOperationTargets` |
| **dialect selection precedence (provider type vs registry)** | **NEW** `TestVideoDialectFor` |
| **create-request body shapes per dialect incl. input_reference and Azure deployment path** | **NEW** `TestVideoCreateRequestShapes` |
| **upstream handle extraction per dialect** | **NEW** `TestVideoUpstreamIDPerDialect` |
| **content URL resolution (stored URL / OpenAI content route / Veo re-poll)** | **NEW** `TestVideoContentURLResolution` |
| **same-origin key scoping (never send the provider key to a CDN)** | **NEW** `TestSameOriginCredentialScope` |
| **per-second pricing + unknown model = 0** | **NEW** `TestVideoCostPricing`, `TestCostPerSecond` (lib) |
| **unroutable branches (rule quorum / strict-routing 404 / no video provider)** | **NEW** `TestVideoNoRouteBranches` |
| video model refused on every text endpoint | `TestVideoModelRefusedOnTextEndpoints` (chat+embeddings), **NEW** `TestVideoModelRefusedOnEveryTextEndpoint` (all 5) |
| image model refused on videos | `TestVideoModelRefusedOnTextEndpoints`, **NEW** `TestImageOnlyModelOnVideosNamesImagesEndpoint` |
| **text model refused on videos** | **NEW** `TestChatModelRefusedOnNewCapabilityEndpoints` |
| **video-only catalog modality refused** | **NEW** `TestCatalogModalitiesDriveVideoOnlyRefusal` |
| video flag persists on provider models; discovery seeds it | `TestVideoModelsListed`, **NEW** `TestDiscoverGoogleSeedsVideoModels`, `TestDiscoverXAISeedsVideoModels`, `TestEnrichFlagsVideoModel` |
| auth / allowlist / TPM / revocation | **NEW** `TestNewEndpointsRequireGatewayAuth`, `TestModelAllowlistEnforcedOnNewEndpoints`, `TestNewEndpointsEnforceTPM`, `TestRevokedKeyCannotUseNewEndpoints` |

### Infrastructure invariants

| invariant | test |
|---|---|
| fresh DB migrates to version 29, all clean | **NEW** `TestFreshDatabaseMigratesToLatest` |
| migration ledger is contiguous + `LatestMigrationVersion()` correct | **NEW** `TestLatestMigrationVersionIsMonotonic` |
| capability columns are INTEGER with DEFAULT 0 on both tables; legacy rows stay text | **NEW** `TestCapabilityColumnsExistWithIntegerAffinity` (SQLite), **NEW** `TestCapabilityColumnsAreIntegerOnPostgres` (PG, skips) |
| `video_jobs` schema + index + TIMESTAMPTZ round-trip | **NEW** `TestCapabilityColumnsExistWithIntegerAffinity` (SQLite), **NEW** `TestVideoJobsSchemaOnPostgres` (PG, skips) |
| re-running migrations at boot is a no-op | **NEW** `TestMigrateIsIdempotent` |
| a dirty migration aborts boot, naming the version | **NEW** `TestDirtyMigrationAbortsBoot` |
| an older DB (029 removed) replays only 029 | **NEW** `TestPartialReplayAppliesPendingMigration` |
| `BoolInt` 0/1 round-trip + capability predicates | **NEW** `TestBoolIntRoundTrip` |
| `pgTranslateDDL` keeps INTEGER affinity + TIMESTAMPTZ + indexes | **NEW** `TestPostgresCapabilityDDLTranslates` |
| Postgres writes never pass a Go bool into the INTEGER columns | `TestCapabilityFlagWritesAgainstPostgres`, `TestCapabilityFlagsRoundTripFromCatalog` (both **skip** without DSN) |
| curated seeds are disjoint per capability | **NEW** `TestAllThreeCapabilitySeedsCoexist` |
| `SeedVideoModels` seeds, flags and prices correctly | **NEW** `TestSeedVideoModelsAndLookup` |
| **new routes live inside the auth+budget+rate-limit group, with bare aliases** | **NEW** `TestNewEndpointsRegisteredInsideAuthGroup`, `TestNewEndpointsHaveBareAliases` |
| `IMAGE_TIMEOUT_SECONDS` sentinel: 0→300s default, explicit wins, default > chat TTFB | **NEW** `TestImageTTFPSentinelPlumbing` |

---

## 3. Tests added

93 test functions across 17 new files (many table-driven, so ~180 cases).
No existing test was modified or deleted; the only changes to pre-existing
files are the two additive items noted at the end.

| file | tests | what it proves |
|---|---|---|
| `internal/proxy/capability_isolation_test.go` | 9 | Full endpoint ↔ model-capability matrix across 5 text protocols + systemone + images + videos, asserting 400s name the right endpoint and never touch an upstream; catalog-modality authority; documented gaps |
| `internal/proxy/videos_lifecycle_test.go` | 8 | Ownership matrix incl. destructive DELETE, empty-prefix escape hatch, expiry behaviour, content error mapping, poll resilience, status-vocabulary normalization, create failure contracts |
| `internal/proxy/images_validation_test.go` | 5 | Prompt bound, multipart validation matrix, option/mask forwarding, non-numeric `n`, remote-URL refusal |
| `internal/proxy/capability_chain_test.go` | 6 | Auth / allowlist / revocation / TPM / 413 enforcement on all three new surfaces |
| `internal/proxy/images_dialect_test.go` | 5 | Upstream error mapping per dialect, no-fake-success on a blocked Gemini response, dialect selection by provider type |
| `internal/proxy/videos_dialects_test.go` | 4 | xAI + Veo dialects end-to-end, key placement / no-leak-to-CDN, Veo URL builders, moderation rejection |
| `internal/proxy/videos_routing_test.go` | 8 | `imageNoRoute`/`videoNoRoute` branches, dialect precedence, create-body shapes, handle extraction, content-URL resolution, same-origin scoping, pricing |
| `internal/proxy/systemone_routing_test.go` | 5 | System One unroutable branches, alias resolution, Cloudflare path addressing, TPM on new endpoints, image-TTFB sentinel |
| `internal/proxy/grok_capability_regression_test.go` | 2 | **P1 regression guard** for grok-imagine video models being classified as image models, plus the positive half |
| `internal/db/migrations_capability_test.go` | 8 | Fresh DB → v29, column affinity/defaults, idempotency, dirty abort, partial replay, `BoolInt`, Postgres DDL translation |
| `internal/db/pgsmoke/video_schema_test.go` | 2 | Postgres column types for capability flags and `video_jobs` (skips without DSN) |
| `internal/catalog/video_seed_test.go` | 2 | `SeedVideoModels` correctness + disjoint capability flags across all three seeds |
| `internal/discovery/video_discovery_test.go` | 3 | Google/xAI discovery seeds video models; enrichment flags them without polluting the image flag |
| `internal/imagegen/reference_images_test.go` | 10 | Reference-image shapes/errors, mask, count boundary, `Validate`, Imagen body, dialect fallback, registry projections |
| `internal/videogen/translate_branches_test.go` | 7 | `Validate`, xAI/Veo body reference branches, `DecodeInputReference` gaps, namespace/catalog projections, `SecondsToInt` |
| `internal/decision/registry_branches_test.go` | 7 | Registry projections, remaining validation branches, determinism, `BaseName`, family-prefix over-match |
| `cmd/gateway/route_registration_test.go` | 2 | AST guard: new routes are registered inside the GatewayAuth + budget + rate-limit group, and bare aliases exist |

Additive only:
* `internal/imagegen/capability_test.go` — appended `TestGrokVideoModelsAreNotImageModels`
  (root-cause guard for the bug fixed in `internal/imagegen/registry.go`).
* `internal/imagegen/registry.go` — **implementation fix**: the `grok-imagine`
  family rule now excludes video ids (see §5, P1).

---

## 4. Command + result

```
$ cd /home/karutoil/ai-gateway && go test ./... -count=1
ok  	ai-gateway/cmd/gateway	0.019s
ok  	ai-gateway/internal/antigravity	0.029s
ok  	ai-gateway/internal/apikey	0.151s
ok  	ai-gateway/internal/auth	0.039s
ok  	ai-gateway/internal/budget	0.229s
ok  	ai-gateway/internal/catalog	0.413s
ok  	ai-gateway/internal/config	0.026s
ok  	ai-gateway/internal/db	0.224s
ok  	ai-gateway/internal/db/pgsmoke	0.007s
ok  	ai-gateway/internal/decision	0.017s
ok  	ai-gateway/internal/devin	0.061s
ok  	ai-gateway/internal/discovery	0.339s
ok  	ai-gateway/internal/handler	1.530s
ok  	ai-gateway/internal/httperr	0.008s
ok  	ai-gateway/internal/imagegen	0.008s
ok  	ai-gateway/internal/lb	0.006s
ok  	ai-gateway/internal/middleware	0.113s
ok  	ai-gateway/internal/oauth	0.006s
ok  	ai-gateway/internal/oauth/providers	0.011s
ok  	ai-gateway/internal/otel	0.057s
ok  	ai-gateway/internal/pat	0.056s
ok  	ai-gateway/internal/provider	0.137s
ok  	ai-gateway/internal/proxy	21.947s
ok  	ai-gateway/internal/rbac	0.003s
ok  	ai-gateway/internal/resilience	0.002s
ok  	ai-gateway/internal/translate	0.004s
ok  	ai-gateway/internal/user	0.360s
ok  	ai-gateway/internal/videogen	0.004s
ok  	ai-gateway/internal/webhook	2.263s
```

`go build ./...` and `go vet ./...` are also clean.
Flakiness check: the ~50 new tests were run `-count=3` (pass) and under `-race`
(pass). No sleeps, no live network — every upstream is an `httptest.Server` and
every retry-heavy assertion disables the retry policy so wall clock stays flat.

---

## 5. Findings produced by the testing work

**FIXED (P1) — a video model was served by the images API.**
`internal/imagegen/registry.go` had the family rule
`strings.HasPrefix(s, "grok-imagine")`, which also matches
`grok-imagine-video*`. Consequences, both reproduced end-to-end against a mock
upstream:
1. `POST /v1/images/generations` with `model=grok-imagine-video` returned **200**
   and forwarded the request to the provider's `/images/generations` — the
   "a video model must never be served by /v1/images/*" invariant broken.
2. `POST /v1/chat/completions` with the same model was refused as
   *"is an image-only model; use POST /v1/images/generations"*, i.e. pointing
   video traffic at the images API.
Fix: the rule now returns false for ids containing `video` (owned by
`videogen`). Guarded by `TestGrokImagineVideoIsNotAnImageModel` (proxy level)
and `TestGrokVideoModelsAreNotImageModels` (registry level). Note this bug is
**not visible on Postgres or SQLite** — it is pure registry logic — so only a
capability-matrix test can catch it.

### Gaps that remain (blocking list)

1. **P1 — Postgres is never exercised in CI.** `internal/db/pgsmoke` skips every
   test unless `GATEWAY_PG_DSN` is set, and `.github/workflows/ci.yml` runs only
   `go test ./... -race -count=1` with no Postgres service. The dialect trap the
   brief calls out (lib/pq rejecting a Go bool for an INTEGER column) shipped
   once already precisely because SQLite accepts both. The test that would catch
   it exists (`TestCapabilityFlagWritesAgainstPostgres`, plus the two new schema
   tests) but never runs. Fix: add a `postgres:16` service container to CI and
   export `GATEWAY_PG_DSN`.
2. **P2 — `expires_at` on `video_jobs` is never enforced.** Migration 028 stores
   it, `videos.go` surfaces it, and nothing compares it to `time.Now()`: a job
   that expired an hour ago still polls 200 (and the content route still proxies
   to the provider). Confirmed by
   `TestVideoExpiredJobIsStillServedFromStorage`. No reaper exists either, so
   rows accumulate forever. Fix: 404/410 on a content request whose
   `expires_at` is past, and prune terminal jobs in `runLogRetention`.
3. **P2 — an empty `X-Gateway-Key-Prefix` sees and DELETES every video job.**
   `videoJobVisible` returns true when the request prefix *or* the row prefix is
   empty, and DELETE is destructive. Confirmed by
   `TestVideoEmptyPrefixCannotDeleteForeignJob`. Acceptable for dashboard
   sessions, unsafe if any code path reaches the gateway group with an empty
   prefix. Fix: distinguish dashboard sessions from key traffic explicitly
   (e.g. require a `key_prefix` match for DELETE) instead of a magic empty value.
4. **P2 — decision models on `/v1/videos` get an opaque 503.**
   `rejectNonVideoModelOnVideos` has image and catalog branches but no decision
   branch, so `model=clef` on `POST /v1/videos` answers
   `503 no video-capable provider configured for this model` instead of the
   actionable `400 … use POST /v1/systemone` every other endpoint produces.
   No upstream leak and no job row (both asserted), so this is quality, not
   safety. Confirmed by `TestDecisionModelOnVideosDoesNotLeakUpstream`.
5. **P2 — a video-only catalog row is mis-described on the images endpoint.**
   `POST /v1/images/generations` with a `video`-modality row says
   *"is a text model, not an image model"* because
   `rejectNonImageModelOnImages` checks image/decision before video modalities.
   Confirmed by `TestCatalogModalitiesDriveVideoOnlyRefusal`.
6. **P2 — dialect selection ignores the provider type for resellers.**
   `videoDialectFor` only special-cases `ProviderGoogle`/`ProviderXAI`; any other
   type falls through to the curated registry. An `openai_compatible` reseller
   proxying a Veo model is therefore called with Google's
   `:predictLongRunning` dialect instead of the OpenAI Videos shape, and a
   `ProviderOpenAI` entry serving a Veo id does the same. Pinned by
   `TestVideoDialectFor`. Fix: gate the registry fallback on the registry
   namespace matching the provider, or make the dialect an explicit provider
   setting.
7. **P3 — decision family prefixes over-match.** `familyPrefixes` uses
   `strings.HasPrefix`, so `cleffa`, `jevon`, `laya-chat` and any `clef-*` id
   resolve as decision models and would be refused on the text endpoints.
   Pinned by `TestFamilyPrefixOverMatch`.
8. **P3 — video "not ready" semantics.** `VideoContent` answers 409 for a
   non-terminal job (pinned by `TestVideoContentBeforeReady`), while the OpenAI
   Videos API answers 404 until content exists. Documented, not changed: the
   409 is friendlier, but an unmodified OpenAI SDK will surface it as an error
   rather than retry.
9. **P3 — `VideoList` has no pagination.** `LIMIT 100` with no `after`/`limit`
   parameters and no `has_more`, so a heavy video user silently loses history.
   No test asserts the truncation boundary.
10. **P3 — unbounded `input_reference` on video create.** The prompt is capped at
    32 000 chars and the whole body at `MAX_PROXY_BODY_MB`, but a single inline
    reference image is only bounded by the body cap, and it is re-base64'd into
    the upstream body (inflating ~1.33×) after being read fully into memory. No
    dedicated limit; covered only incidentally by
    `TestVideoPromptBoundAndOversizeBody`.
11. **P3 — `VideoDelete` leaves the provider's job running.** The handler only
    forgets the local row (documented intent), so upstream quota keeps burning
    for a cancel xAI/Veo may support. Not tested because the providers' cancel
    APIs are not implemented.

---

## Verdict

**NEEDS WORK**

The new surfaces are now well covered on the happy path, the capability matrix,
the multipart/validation edges, all three upstream dialects, ownership and the
billing maths — and the work turned up one real P1 correctness bug (a video
model served by the images API), which is fixed and regression-guarded. What
keeps this off "production ready" is that the Postgres half of the dialect story
is written but never executed in CI (P1), and three ownership/retention
invariants (`expires_at` unenforced, empty-prefix destructive access, no job
reaping) are confirmed rather than merely suspected (P2).

### Blocking list (P0–P3)

1. **P1** — Postgres is untested in CI: `pgsmoke` skips without
   `GATEWAY_PG_DSN` and no workflow supplies one, so the INTEGER-vs-bool dialect
   trap can regress undetected.
2. **P2** — `expires_at` is stored but never enforced; expired jobs still poll
   200 and still proxy content, and no reaper removes them.
3. **P2** — an empty `X-Gateway-Key-Prefix` can read *and delete* another key's
   video job.
4. **P2** — a decision model on `/v1/videos` answers an opaque 503 instead of a
   400 naming `/v1/systemone`.
5. **P2** — a video-only catalog model on `/v1/images/generations` is mislabelled
   a "text model" instead of being pointed at `/v1/videos`.
6. **P2** — `videoDialectFor` ignores the provider type for non-Google/xAI
   providers and can speak the wrong dialect to a reseller.
7. **P3** — decision family-prefix matching over-matches (`cleffa`, `jevon`).
8. **P3** — content-before-ready returns 409 rather than the OpenAI 404.
9. **P3** — `/v1/videos` list is capped at 100 with no pagination parameters.
10. **P3** — `input_reference` has no dedicated size bound.
11. **P3** — `DELETE /v1/videos/{id}` cannot cancel the upstream job.
