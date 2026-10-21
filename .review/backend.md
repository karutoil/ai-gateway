# Backend correctness review — decision / images / videos

Scope: `internal/proxy/{images,videos,systemone}.go`, `internal/imagegen/`,
`internal/videogen/`, `internal/decision/`, and the diffs in
`internal/proxy/{proxy,fallback,upstream_routing,responses_stream}.go`,
`internal/discovery/discovery.go`, `internal/catalog/catalog.go`,
`internal/models/models.go`, `cmd/gateway/main.go`, `internal/config/config.go`.

Baseline: `go build ./...` clean, `go vet ./...` clean, `go test ./...` green.
Every finding below was reproduced with a throwaway test/probe (deleted
afterwards; **no source was modified**).

---

## 1. Capability isolation invariants

### 1.1 What holds

* The text-endpoint guards are wired on all five text surfaces and run
  *before* routing: chat `proxy.go:3190-3198`, completions `3324-3332`,
  embeddings `3522-3530`, messages `4042-4050`, responses `4188-4196`.
* `noRouteFor` is capability-aware (`fallback.go:518-533`) and tests exist
  (`endpoint_guards_test.go:192`).
* Decision models are refused on `/v1/images/generations`
  (`images.go:62`, `rejectNonImageModelOnImages` at `images.go:580-604`) and
  chat can never select a decision provider (`proxy.go:3217-3219`,
  `systemone.go:88-101`, test `systemone_test.go:158`).
* `X-Provider` pins and qualified-ID pins go through the same `pred`
  (`fallback.go:185-205`), so a pin cannot widen capability. Verified by
  probe: `X-Provider: <openai>` + `model: sora-2` still 400s.
* Alias resolution happens before every guard (`resolveAlias`, `proxy.go:125`),
  so `alias -> dall-e-3` is refused like `dall-e-3`.

### P0-1 — Video models are classified as **image** models; `/v1/images/generations` serves them

`videogen` model ids leak into `imagegen` through the `familyRules` prefix
matchers:

* `internal/imagegen/registry.go:156-158`
  `{match: strings.HasPrefix(s, "grok-imagine") ...}` — this matches
  `grok-imagine-video`, `grok-imagine-video-1.5`, `grok-imagine-video-1.5-lite`.

Measured (`imagegen.IsImageModel` / `IsImageOnlyModel`):

```
grok-imagine-video            image=true  imageOnly=true  video=true
grok-imagine-video-1.5        image=true  imageOnly=true  video=true
grok-imagine-video-1.5-lite   image=true  imageOnly=true  video=true
```

Consequences (all reproduced end to end):

1. **A video model is accepted and forwarded by the images endpoint.**
   `rejectNonImageModelOnImages` returns early at `images.go:581-583` because
   `imagegen.IsImageModel(model)` is true, so the video check at
   `images.go:591-594` is dead for every Grok video model:

   ```
   POST /v1/images/generations {"model":"xaip/grok-imagine-video","prompt":"a cat"}
   -> 200 (upstream called, model "grok-imagine-video" forwarded)
   POST /v1/images/generations {"model":"grok-imagine-video", ...}  -> 200
   ```

   This is precisely the invariant the brief calls out
   (“a video model must never be served by `/v1/images/*`”).

2. **The wrong billing rate is applied.** `imageCost` (`images.go:287-300`)
   resolves `imagegen.CostPerImage("grok-imagine-video")` through the family
   rule → `$0.07/image`, so a 12-second video is metered at 7 cents instead of
   `$0.60` (12 s × `$0.05/s`). Budgets under-count by ~9×.

3. **The text-endpoint refusal names the wrong endpoint.** Because
   `rejectImageModelOnText` runs first (`proxy.go:3190-3192`):

   ```
   POST /v1/chat/completions {"model":"xaip/grok-imagine-video",...}
   -> 400 "model 'xaip/grok-imagine-video' is an image-only model;
           use POST /v1/images/generations instead of /v1/chat/completions"
   ```

   The `rejectVideoModelOnText` guard behind it can never fire, so the generic
   test `videos_test.go:340` (which uses `sora-2`, not a Grok id) never catches
   this.

4. **The catalog row is wrong.** `discovery.enrichFor`
   (`discovery.go:488-497`) runs the image fallback first, so a seeded video
   row gets `image=1` **and** `modalities={"output":["image"]}`:

   ```
   row grok-imagine-video  image=1 video=1 mods={"input":["text"],"output":["image"]}
   ```

   `/v1/models` then advertises the video model as `kind:"image"`,
   `image_only:true` AND `kind:"video"` (the later `if cm.Video` block
   overwrites `kind`), and `Enrich()` on the row *flips video back to 0*:

   ```
   after seed:   grok-imagine-video  image=1 video=1 src=image
   after enrich: grok-imagine-video  image=1 video=0 src=image
   ```

   because `enrichFor` derives `e.video` from the *image* modalities blob the
   image fallback just installed (`discovery.go:501-509`). Video disappears from
   the listing on the next enrich.

**Fix (do all four):**
* Narrow the image family rule to exclude video ids, e.g.
  `strings.HasPrefix(s, "grok-imagine") && !strings.Contains(s, "video")`, and
  add a hard exclusion at the top of `imagegen.Lookup`:
  `if videogen-lookalike { return Model{}, false }` — but `imagegen` must not
  import `videogen` (cycle-free today). The cleanest form is a shared
  `capability` predicate or a literal skip list in `imagegen.Lookup` for the
  three `grok-imagine-video*` ids, mirrored by a `imagegen` test that asserts
  every `videogen.All()` id is **not** `IsImageModel`.
* Make the guards capability-exclusive rather than order-dependent:
  in `rejectNonImageModelOnImages`, check `videogen.IsVideoModel` **first**
  (`images.go:580`), and in the text guards check video before image
  (`proxy.go:3190-3198` and the four siblings).
* In `discovery.enrichFor`, run the **video** fallback before the image
  fallback and make the image fallback skip when `e.video` is already set
  (`discovery.go:488` gate on `!e.video`), and never let the image branch
  overwrite `e.modalities` once a video modality is present.
* In `Models` (`proxy.go` ~3702-3743 / 3792-3830), emit `kind` with a precedence
  (`video` > `image`) instead of two independent `entry["kind"] =` writes, and
  set `image_only=false` for video rows.

### P1-2 — `/v1/images/edits` has **no** capability guard at all

`ImageEdits` (`images.go:103-149`) never calls `rejectNonImageModelOnImages`.
Reproduced:

```
POST /v1/images/edits (multipart, model="clef")   -> 200, upstream saw model="clef"
POST /v1/images/edits (multipart, model="gpt-4o") -> 200, upstream saw model="gpt-4o"
```

A decision model and a text model are both forwarded to the provider's edit
endpoint, burning an upstream round trip (and, on metered providers, a failed
billable call). `images_test.go:381` covers the generation guard only, so there
is no test asserting the edits guard.

**Fix:** call `if h.rejectNonImageModelOnImages(w, model) { return }` right
after allowlist/TPM in `ImageEdits` (before `parseEditMultipart` consumes 64 MiB
of body), and add the edits case to `endpoint_guards_test.go`.

### P1-3 — `lb_rules` `model_override` is silently ignored by all three new endpoints

`ProxyCandidatesWithOpts` computes `candBody`/`candModel` from the rule member's
override (`fallback.go:382-386`) and passes `candModel` to `prepare`, but the new
`prepare` closures ignore the model argument and re-derive it from the *outer*
handler variable:

* images generation `images.go:83-89` → `h.imageUpstreamRequest(p, n, in, model)` (captured outer `model`)
* images edits `images.go:139-146` → same
* video create `videos.go:124-127` → `upstream := stripProviderPrefix(model, p)` (outer `model`), and `videoCreateRequest(..., upstream)`
* systemone `systemone.go:76-86` → `decisionUpstream(p, body, model)` (outer `model`)

Reproduced for images:

```
rule "img-alias" -> provider xaip, model_override "grok-imagine-image-2.0"
POST /v1/images/generations {"model":"img-alias",...}
-> upstream saw model="img-alias"   (override lost)
```

Chat does this correctly (`proxy.go:3284-3291` derives `bodyModel :=
translate.ExtractModel(body)`), so this is a regression in the new code paths.
Because `candModel` *is* used for cost/logging, the request is billed as the
override model while the upstream actually ran the alias id (or vice versa),
which also breaks the quota-eligibility keying done with the override
(`fallback.go:217-221`).

**Fix:** derive the upstream id inside each prepare closure exactly like chat:
`bodyModel := translate.ExtractModel(body)` (or `n.Model`), pass *that* to
`imageUpstreamRequest` / `videoCreateRequest` / `decisionUpstream`, and delete
the outer-`model` capture.

### P2-4 — `rejectNonImageModelOnImages` over-rejects catalog text rows

`images.go:596-603` refuses any exact catalog row where `!cm.Image &&
!cm.Decision`. A row seeded by discovery for an **unknown reseller image model**
(`flux-1.1-pro`, `seedream-4`) has `image=0`, so the request gets a 400 “is a
text model”, contradicting the function's own doc comment (“Unknown ids are
deliberately allowed through”, `images.go:574-579`) and the passthrough test
`images_test.go` (`TestChatRejectsImageModel` only exercises an id with **no**
catalog row). Same shape for `rejectNonVideoModelOnVideos`
(`videos.go:332-357`) — there the extra `cm.Modalities != ""` guard already
prevents it, which is the correct pattern.

**Fix:** only refuse when the catalog row positively says “text” —
`cm.Modalities != "" && len(imagegen.OutputModalities(cm.Modalities)) > 0 &&
!imagegen.ImageModality(...)` — or drop the catalog branch entirely and rely on
the curated registry (the passthrough behaviour the docs promise).

### P1-5 — The guards trust only the **curated registry**; catalog-declared
capabilities are ignored on the request path

* `rejectDecisionModelOnText` (`images.go:503-513`) and the `/v1/systemone`
  model check (`systemone.go:62-65`) both call `decision.IsDecisionModel`
  (`decision/registry.go:145-148`), which only knows the hand-maintained
  registry. The catalog *does* carry the flag — `RawModel.Decision`
  (`catalog.go:43-44`), `scanCatalogModel`’s `m.Decision =
  decision.Int64 != 0 || decisionModality(...)` (`catalog.go:134`), and
  `SeedDecisionModels` — and `/v1/models` advertises it.
* Consequence, reproduced with a catalog row `decision=1,
  modalities={"output":["decision"]}`:

  ```
  GET  /v1/models           -> "decision": true
  POST /v1/systemone        -> 400 "model 'acme-decide' is not a decision model"
  POST /v1/chat/completions -> routed as ordinary chat (not refused)
  ```

  So an operator (or a models.dev row the gateway itself synced) can be told by
  the listing that a model is a decision model while the endpoint refuses it and
  the text endpoints happily forward a chat body to it. The same asymmetry
  exists for images in the other direction: `rejectImageModelOnText` uses
  `h.modelIsImageOnly`, which reads the catalog **first**
  (`images.go:551-568`), while `rejectNonImageModelOnImages` consults the
  curated registry **first** (`images.go:581`).

**Fix:** add a catalog-aware helper next to `modelIsImageOnly`/`modelIsVideoOnly`
— `modelIsDecisionModel(model)` that checks `h.CatalogStore.Get(...).Decision ||
decision.IsDecisionModel(model)` — and use it in both the text guards and the
`/v1/systemone` model check. Add a test that a catalog-only decision row is
refused on chat and accepted on `/v1/systemone`.

### P3-6 — Guard coverage gaps

`endpoint_guards_test.go` covers decision-on-text, image-on-text, wrong-model on
systemone, and (in `videos_test.go`) video-on-text with `sora-2`. Untested:
video model on **images** (would have caught P0-1), any model on **edits**
(P1-2), `X-Provider` pin bypass, alias bypass, `lb_rules` override bypass
(P1-3) and P1-5, and “decision provider never selected for images/videos” when the pin
header names it.

---

## 2. Provider dialect translation

### 2.1 Verified correct

* Gemini `:generateContent` body shape (`imagegen/translate.go:20-47`):
  `contents[].parts[].inlineData{mimeType,data}`, `generationConfig.
  responseModalities`, `imageConfig.aspectRatio`, 4096px-bounded sizes — matches
  the real API. `candidateCount` for `n>1` matches the schema.
* Imagen `:predict` (`translate.go:50-72`): `instances[].prompt`,
  `parameters.sampleCount/aspectRatio` — correct.
* Gemini → OpenAI response mapping (`translate.go:88-140`): `inlineData` →
  `b64_json`, concatenated text → `revised_prompt`, `usageMetadata` →
  `usage` — correct field names.
* Imagen → OpenAI (`translate.go:152-177`): `predictions[].bytesBase64Encoded`
  / `mimeType` — correct.
* Veo `predictLongRunning` (`videogen/translate.go:170-200`):
  `instances[].prompt`, `parameters.durationSeconds/aspectRatio` — correct.
  Operation poll URL (`videos.go:419-426`) appends the returned
  `name` (`models/.../operations/...`) to `{base}/v1beta/` — correct.
* Veo operation parsing (`translate.go:290-345`):
  `response.generateVideoResponse.generatedSamples[].video.uri` and top-level
  `error{code,message,status}` — correct.
* Cloudflare Workers AI path (`systemone.go:144-156`):
  `<base>/ai/run/@cf/cloudflare/<selector>` — correct, and idempotent when the
  operator pasted the full run URL.
* Google auth (`proxy.go:751-756`, `isGoogleNativeAPI`): `x-goog-api-key` and
  **never** `Authorization: Bearer` — verified by
  `images_test.go:TestImageGenerationsGeminiTranslate`.

### P1-7 — Azure images / videos build a deployment URL but never send `api-key`, and are never chosen as Azure

`upstreamTarget(..., isAzure=true)` (`proxy.go:707-717`) produces
`/openai/deployments/<model>/{images/generations,videos}`; the auth branch keys
off `strings.Contains(targetURL, "/openai/deployments/")`
(`proxy.go:757-761`), so that part works **if** the URL is built. Two gaps:

1. `imageUpstreamRequest`/`imageEditUpstreamRequest` pass
   `p.Type == models.ProviderAzure` (`images.go:199, 226`) — fine — but
   `videoCreateRequest` does the same at `videos.go:406`, and
   `pollVideoJob` builds `{base}/videos/{id}` **without** the Azure deployment
   shape or `api-version` (`videos.go:487-494`), so an Azure video job can be
   created but never polled. Same for `videoContentURL` (`videos.go:604-609`).
2. Azure's real Images surface is `/openai/deployments/<d>/images/generations?
   api-version=…` **and** Azure's video API was retired/renamed repeatedly; the
   code claims Azure support with no test. `azureAPIVersion` is pinned to
   `2024-06-01`, which predates the images/videos GA.

**Fix:** either implement the Azure poll/content URL with the deployment shape +
`api-version` (mirroring `upstreamTarget`), or refuse Azure on `/v1/videos`
explicitly with an actionable 400. Add a unit test for the Azure URL shapes.

### P2-8 — xAI video `model` field is never set in the create body

`XAICreateBody` (`videogen/translate.go:129-135`) writes
`{"model","prompt","seconds",...}` — but `Normalized.Model` is the **client's**
model id (`videogen.go:ParseRequest`), which may be `xaip/grok-imagine-video` when
the client used a bare name that didn't hit `stripProviderPrefix` (e.g. a
reseller). The other dialects get the stripped id via `videoCreateRequest`'s
`model` parameter (`videos.go:378-390`); xAI uses `n.Model`. Verified by reading:
`videoCreateRequest` passes `n` through untouched, so `XAICreateBody` is the only
place `n.Model` survives. Same issue in `imageUpstreamRequest`: the Google branch
gets `upstream` (`images.go:194`) but the OpenAI branch relies on
`replaceModelInBody(rawBody, upstream)` (`images.go:203`), which is correct.

**Fix:** pass `model` into `XAICreateBody(n, model)` and set `req["model"] =
model`; add a test asserting the upstream body carries the stripped id.

### P2-9 — xAI poll: `progress` and `status` vocabulary

`XAIPollToVideo` (`translate.go:222-273`) reads `status`, `progress`, `video.url`,
`error{code,message}` and special-cases “completed without video ⇒
moderation_rejected”. Reasonable. Two gaps:

* xAI's documented poll values are `queued`/`pending`/`processing`/`done`/
  `failed`; `NormalizeStatus` (`videogen.go:253-269`) maps `pending`→`queued`
  and `done`→`completed`, good, but the **first poll** for a just-created job is
  `queued`, and `pollVideoJob` overwrites the stored status with the poll result
  — no monotonicity guard, so a provider that answers `queued` after
  `in_progress` (observed on resellers) regresses the client-visible status.
* `progress` is copied verbatim with no clamp; a provider returning `-1` or
  `1000` is surfaced as-is and stored (`videos.go:517`).

**Fix:** clamp progress to `[0,100]` in `normalizeVideoPoll`, and make status
updates monotonic (`queued < in_progress < completed|failed`) in
`pollVideoJob` — never move backwards, never leave a terminal state.

### P2-10 — `imageDatum` with `response_format:"url"` returns a `data:` URL

`translate.go:177-186` sets `d.URL = "data:"+mime+";base64,"+b64`. OpenAI clients
that receive `url` and hand it to an HTTP fetcher will fail; and a 4 MB
base64 blob in a `url` field is a footgun. This is documented in the comment but
undocumented for clients (`openapi.yaml` says nothing).

**Fix:** either return `b64_json` regardless of `response_format` for Google
dialects (and document it), or upload to a gateway-hosted blob route. At minimum
add `x_gateway_data_url: true` to the response media-type docs.

### P3-11 — `aspect_ratio` in the Gemini body is dropped when `auto`

`GeminiRequestBody` (`translate.go:39-41`) skips `imageConfig` when
`AspectRatio == "auto"`, but `Normalized.AspectRatio` is only the *explicit*
client field (`imagegen.go:222-227`) — `size` is never folded in for Gemini
(deliberate, per the comment). Meanwhile for Imagen the same field **is** folded
via `AspectRatioFor()` (`translate.go:61`). So `size: "1792x1024"` changes Imagen
output but silently does nothing for Gemini. Inconsistent, not incorrect.

**Fix:** document, or honor `size` for Gemini when the model is known to support
`imageConfig` (the curated registry already carries `AspectRatios`).

### P3-12 — Dead/unused protocol data

* `videogen.Model.SupportedSeconds` / `SupportedSizes` are populated
  (`registry.go:54-113`) and **never read**; `videogen.ParseRequest` validates
  against the global `validSeconds`/`validSizes` (`videogen.go:120-133`), so a
  Veo model accepts `seconds:"12"` locally and 400s upstream.
* `imagegen.Model.MaxN` is set for Grok (`registry.go:130`) and never read;
  `ParseRequest` uses the global `MaxImagesPerRequest = 10`.
* `UpstreamSystemOne` (`upstream_routing.go:18`) is added but no call site can
  produce it (`UpstreamAPIForModel` never returns it), so
  `correctInboundFor(UpstreamSystemOne)` at `upstream_routing.go:115` is dead.

**Fix:** either enforce the per-model limits (best: fold
`SupportedSeconds`/`MaxN` into validation once the router has chosen a provider —
i.e. validate in the prepare closure) or delete the fields.

### P3-13 — `isGoogleNativeAPI` is a substring sniff over the whole URL

`proxy.go:783-789` returns true for any URL containing `:generateContent`,
`:predict`, or the Google host. A **reseller** whose base URL is
`https://gateway.example.com/proxy/generativelanguage.googleapis.com/v1beta`
would receive `x-goog-api-key` instead of `Authorization`. Low risk (operator
controlled) but the comment's claim (“the gateway only ever builds these for
google providers”) is not enforced by a type check.

**Fix:** pass the provider type into `newUpstreamRequest` (it already receives
`isAnthropicUpstream`; add `isGoogleNative bool` from `p.Type ==
models.ProviderGoogle`).

---

## 3. Async video lifecycle

### 3.1 What holds

* No goroutines: create persists the row and returns; a client poll becomes a
  single upstream poll (`videos.go:189-224`). The migration comment's
  “deliberately stateless” claim is accurate.
* Terminal jobs are served from the row without an upstream call
  (`videos.go:198-201`) — test `videos_test.go:TestVideoPollThenContent`.
* Restart mid-job: the row survives, `ProviderID`/`UpstreamID`/`Dialect` are
  persisted (`videos.go:699-705`), so a poll after restart rebuilds the upstream
  URL from `p.BaseURL`. Correct.
* Key scoping is checked on GET, content and DELETE
  (`videos.go:196, 231, 297`); test `TestVideoJobScopedToKey`.

### P1-14 — `expires_at` is stored and returned but **never enforced**

`videos.go` reads `expires_at` into the row (`videos.go:739-753`), echoes it
(`videos.go:71-76`) and stores it (`videos.go:707-719`) — and never looks at it.
Reproduced with an already-expired completed job:

```
GET /v1/videos/{id}         -> 200 {"status":"completed","expires_at":1577836800,...}
GET /v1/videos/{id}/content -> 200 (bytes served)
```

A job whose provider-side artefact expired a year ago is reported as completed
and the content route attempts the fetch anyway. When the provider does answer
404/410, `streamVideoContent` maps it through `outboundStatus` to 404/410
(`videos.go:637-641`) — the mapping is right, but the **stored state is not
updated**, so every subsequent poll keeps reporting `completed` and every
content request re-hits the provider.

**Fix:** in `VideoGet`/`VideoContent`, if `job.Expires != nil && time.Now().
After(*job.Expires)` (and status is completed), return a distinct
`410 Gone` (“the generated video expired at …; provider artefacts are retained
per the provider's policy”) and mark the row (`status='expired'` or
`video_url=''` + `error_code='expired'`). Also on an upstream 404/410 in
`streamVideoContent`, persist the terminal state.

### P1-15 — Veo “done” with no sample is reported as `completed`, and
`completed_at` is local wall-clock

`VeoPollToVideo` (`translate.go:322-341`):

```go
case op.Done:
    v.Status = StatusCompleted
    v.Progress = 100
    now := time.Now().Unix()
    v.CompletedAt = &now
```

If `done:true` but `generatedSamples` is empty (content-policy refusal, which
Veo signals as `done` + `raiFilteredReason`/empty samples), the job is reported
as **completed with nothing to download**, and
`VeoVideoURI` returns `""`. `videoContentURL` then takes the
`DialectVeo` branch (`videos.go:611-618`) and *polls again*; if the URI is still
empty it returns `("", nil)` and the handler 502s “could not resolve the
generated video” (`videos.go:257-261`) — a permanent stuck job that reports
success on every poll. xAI has the equivalent guard
(`translate.go:265-270`, moderation_rejected); Veo does not.

Also: `CompletedAt`/`Expires` for xAI and Veo are synthesized from
`time.Now()` (`translate.go:254-257`, `330-331`), so `completed_at` drifts on
every poll (`videos.go:517-520` rewrites the row each time) and `created_at` in
the synthesized resource is `time.Now()` (`translate.go:245`, `333`) — the
`videoJob.toVideo` path uses the stored `CreatedAt`, so only the *dialect*
constructors are wrong, but they are the ones `progress`/`completed_at` flow
through.

**Fix:** treat `done && uri == ""` as `failed` with
`code="no_output"`/`moderation_rejected` (mirroring xAI), and only set
`CompletedAt` on the *transition* to completed (compare with the stored status
before persisting).

### P2-16 — `videoJobVisible` fails open on an empty prefix

`videos.go:672-678`:

```go
if prefix == "" || job.KeyPrefix == "" { return true }
```

`middleware.attachGatewayKey` always sets the header for both real keys
(`middleware.go:277`) and dashboard sessions (`sessionPrefix`, `middleware.go:288`),
so `prefix == ""` means “no gateway auth in front of this route”. The video
routes **are** inside the auth group (`main.go:474-502`), so this is not
currently exploitable, but it is a fail-open default in the exact function that
owns the tenancy check, and `key_prefix` is nullable (`028:15`) with no write
path that needs NULL. The database reviewer flagged the same thing.

**Fix:** `if job.KeyPrefix == "" { return false }` and add `NOT NULL` (or a boot
backfill) for `key_prefix`; add a test that an empty-prefix request cannot read a
prefixed job.

### P2-17 — `DELETE /v1/videos/{id}` leaves the upstream job (and its bill) running

Documented as intentional (`videos.go:290-292`) and acceptable, but combined with
P1-14 it means a client that deletes a job has no way to discover it is gone and
the DB keeps no tombstone. Consider `status='deleted'` instead of `DELETE` so
audit/`request_logs` correlation still resolves, and note it in the OpenAPI
description.

### P3-18 — No `video_jobs` retention

Nothing prunes `video_jobs`; `runLogRetention` (`main.go:702`) only touches
`request_logs`. Every create adds a permanent row.

---

## 4. Error handling, retries and idempotency

### P0-19 — Non-idempotent image/video creates are retried and failed over — double billing

`proxyWithMetricsOpts` retries any 429/5xx up to `MaxRetries` with backoff
(`proxy.go:1148-1170`, `resilience/retry.go:26-38`) and `shouldFailoverFrom`
walks the next candidate on 429/5xx/0 (`fallback.go:299-305`). The new image and
systemone endpoints inherit that machinery **unchanged** (`proxyCandidatesWithOpts`
→ `proxyWithMetricsOpts`), and requests are *not* cache-eligible for retry
dedupe. Reproduced:

```
upstream: first call -> 500, second call -> 200
POST /v1/images/generations {"model":"xaip/grok-2-image"}
-> status=200, upstreamCalls=2
   request_logs rows=1 total cost=0.07
```

The client sees one image; the provider generated (and billed) two. With a
ledger-based provider this is a direct double charge; with failover enabled it is
worse (two *different* providers bill for one client request). Video create has
the same exposure on its own path?? — no: `VideoCreate` calls
`doVideoUpstream` exactly once (`videos.go:131`) and does **not** use
`proxyCandidates`, so it neither retries nor fails over. That is the correct
behaviour for a non-idempotent POST, and it is worth stating explicitly in the
comment as the *intended* contrast with images.

**Fix (pick one, but pick now):**
1. **Preferred:** pass `proxyCandidatesOpts{noRetry: true, noFailover: true}` for
   `/v1/images/*` and `/v1/systemone` (systemone is a read-only evaluation, so
   retry is safe there — the double-bill case is images only), i.e. add a flag
   that makes `proxyWithMetricsOpts` skip `retry.ShouldRetry` and makes
   `shouldFailoverFrom` return false for that endpoint.
2. Or make the retry safe with an idempotency key: generate
   `Idempotency-Key` per client request, forward it, and only retry when the
   provider echoes support.
3. Minimum viable: keep one retry but only when `status == 429` (a request that
   was rejected before generation cannot have billed) and never fail over.

Also add a metrics counter for retried billable creates so the over-billing is
observable.

### P1-20 — Video create relays the upstream error body verbatim, unsanitized

`videos.go:136-144`:

```go
w.Header().Set("Content-Type", "application/json")
w.WriteHeader(outboundStatus(status))
_, _ = w.Write(respBody)
```

`respBody` is the provider's raw body (up to 1 MiB, `videos.go:481`). Chat's
equivalent path goes through `isClientCaused5xx` classification and
`truncateDetail` (`fallback.go:492-500`), and the streaming 4xx relay uses the
same idea. Here there is no size bound on the client-visible body beyond the read
limit, no `X-Fallback-Used`/`X-Cache` header consistency, and no
`httperr` envelope — a provider that returns an HTML error page or a body
containing the provider's internal account id is relayed to the gateway client
verbatim. Also `w.Write` return value is ignored (fine) but the response is not
logged at all on this path (no `logRequestExtendedBodies`), so failed video
creates are invisible in `request_logs`/audit — verified: `request_logs=0` after
a 500 (`P0-19` probe output).

**Fix:** bound the relayed body (e.g. 8 KiB), scrub it with
`ScrubSecrets`, wrap non-JSON bodies in the `httperr` envelope, and always write
a `request_logs` row with the observed upstream status.

### P2-21 — `VideoGet` swallows poll failures and returns a stale success

`videos.go:214-221` logs a warning and returns the last known state with 200.
That is a defensible UX choice, but it makes a *permanently* broken job (deleted
provider, revoked key, provider 404) indistinguishable from a slow one, and there
is no `Retry-After` or staleness signal. At minimum add
`?X-Video-Stale: true` (or a `updated_at` age check) so clients can escalate, and
count the poll failures in metrics.

### P3-22 — `errSnippet`/error text leaks into logs only, which is fine — but `DecryptKey` failures are 502 without a log

`videos.go:110-114`, `videos.go:206-210`, `videos.go:243-247` answer
“provider credential unavailable” with no server-side log line, so a rotated
master key looks like a provider outage. Add `log.Error().Err(derr)`.

---

## 5. Context, timeouts, leaks

### P1-23 — `/v1/videos` create/poll/content has **no** first-byte budget and no
upstream timeout beyond the shared transport

`VideoCreate` calls `doVideoUpstream` directly (`videos.go:131`), which uses
`http.NewRequestWithContext(r.Context(), ...)` and `h.Client` — no
`ttfbController`, no `DefaultVideoTTFB` (declared at `videos.go:31` and **never
used anywhere**; `grep` finds only the declaration). `RequestTotal` is `0` by
default (`upstream.go:40`), so a hung provider leaves the create request open
until `ResponseHeaderTimeout` (120 s, `config.go:365`) fires — longer than the
~100 s Cloudflare window, producing exactly the 524 signature the ttfb machinery
was built to avoid. `pollVideoJob` and `streamVideoContent` have the same gap
(`videos.go:483-500`, `620-668`); the content route can additionally sit in
`io.Copy` for the full 512 MiB with no idle deadline.

**Fix:** wrap the three video upstream calls in
`context.WithTimeout(r.Context(), h.videoTTFB())` (wire `DefaultVideoTTFB`,
make it configurable next to `IMAGE_TIMEOUT_SECONDS`), and for the content copy
use a `time.AfterFunc` watchdog on `resp.Body` read gaps (the same pattern as
`StreamIdle`).

### P2-24 — Client disconnect *is* honoured everywhere (good), response bodies are
closed (good) — except one

Every new upstream call uses `r.Context()` and `defer resp.Body.Close()`
(`videos.go:479, 498, 635`; `proxy.go` path already did). No `go func` in any of
the new files. The one gap: `streamVideoContent` does not check the `io.Copy`
error nor `r.Context().Err()` and always flushes a 200 already written — that is
correct HTTP-wise (headers were sent), but the failure is invisible; log it.

### P2-25 — `readProxyBody` buffers the whole multipart edit in memory, twice

`ImageEdits` reads the full body (up to `MAX_PROXY_BODY_MB`, default 64 MiB,
`proxy.go:510`) into `body`, then `parseEditMultipart` copies every part's bytes
into `files` **and** `norm.Images` (`images.go:340-350`) — so a 4-reference-image
edit peaks at ~3× the body size, then `buildEditMultipart` copies again
(`images.go:408-447`). Two clients with 64 MiB edits can push a small gateway
into GC pressure. Also `parseEditMultipart` is called **twice** per request
(once at `images.go:111`, again inside the prepare closure at
`images.go:141`) — the second parse is only needed because the outer parse
produced `norm` but the closure re-reads `in`.

**Fix:** parse once and reuse (`norm` is already available); stream the parts to
the upstream (or at least drop the duplicate `files` copy for the Google branch,
which never uses it); lower the edits body cap to a `MAX_EDIT_BODY_MB` default
(8 MiB covers 4 × 2 MiB references).

### P2-26 — `estimateTokens` mis-charges multipart edits (TPM)

`ImageEdits` calls `h.checkTPM(w, r, body)` (`images.go:120`) on the raw
multipart body. `estimateTokens` strips base64 blobs (`proxy.go:191-...)` but a
multipart binary part is not base64 — it is opaque bytes. Measured:

```
multipart body 2 097 643 bytes -> estimateTokens=524 411
```

i.e. a 2 MiB reference image consumes **524 411 TPM units**, so any key with a
sane TPM cap gets a spurious 429 on its first edit, while the equivalent JSON
`data:` URL path costs 21 tokens. This is the exact bug `stripBase64Blobs` was
added to fix for JSON clients.

**Fix:** pass `norm.Prompt` + `norm.Size` etc. (a synthesized small JSON) to
`checkTPM`, or make `estimateTokens` multipart-aware (skip
`Content-Type: image/*`/`application/octet-stream` part payloads).

### P3-27 — `IMAGE_TIMEOUT_SECONDS` sentinel plumbing is incoherent

`config.go:400-404` defaults to `-1`, `main.go:180-189` maps
`0 → ImageTTFB = 0` (disabled) / `<0 → DefaultImageTTFB` (300 s), and
`imageTTFB()` (`images.go:151-156`) maps `<=0 → DefaultImageTTFB`. So
`IMAGE_TIMEOUT_SECONDS=0`, which `main.go` logs as “first-byte budget disabled”,
actually re-enables the 300 s default because the handler field is `0`. The
documented default in `config.go:30` is `300` while the code default is `-1`
(resolves to 300 — same value, different sentinel). `DefaultVideoTTFB` should
join this table.

**Fix:** use one sentinel (0 = disabled, negative = default) and represent
“disabled” with an explicit bool or `-1` through the handler, or make
`ImageTTFB` a `*time.Duration`. Add a unit test for the 0/‑1/positive matrix.

### P3-28 — `Decision` and `Images` endpoints have no per-request `RequestTotal`

`proxyCandidatesWithOpts` inherits `ctx` from `r.Context()` and applies
`RequestTotal` only inside `proxyWithMetricsOpts` (`proxy.go:1011-1017`), which
is correct — but the *image* budget (`ImageTTFB` 300 s) can now exceed
`UPSTREAM_HEADER_TIMEOUT_SECONDS` (120 s) while `RequestTotal` stays 0, so the
overall image request has no upper bound other than the TTFB. Setting
`IMAGE_TIMEOUT_SECONDS` above `REQUEST_TOTAL_TIMEOUT_SECONDS` should warn at
boot.

---

## 6. Concurrency / shared state

* No new goroutines, no new package-level mutable state. `ttfbController` is
  per-request and already `sync.Mutex`/`atomic` guarded (`ttfb.go:36-46`).
* `videos.go` row updates: `pollVideoJob` does read-modify-write
  (`videos.go:509-527`) with no optimistic-concurrency guard. Two concurrent
  polls of the same job both read `queued`, both call upstream (2× provider
  quota), and the last writer wins. Not data-corrupting (the columns are the same
  kind of value), but it doubles upstream polling under a client retry storm and
  can regress `completed → in_progress` if responses race.
  **Fix:** conditional update
  `UPDATE video_jobs SET … WHERE id=? AND updated_at=?` (or a `version`
  column), and `SELECT … FOR UPDATE` on Postgres, or simply skip the upstream
  poll when `updated_at` is younger than a few seconds (cheap and effective).
* `imagegen`/`videogen`/`decision` registries are immutable after init — safe.
* `videoServer`-style test helpers mutate a plain `int` from the handler
  goroutine (`videos_test.go:38,103`); tests run without `-race` in CI for this
  package? `ci.yml` does run `-race`, and the counters are only read after the
  request completes, so no race in practice — but `polls` is written by the
  upstream goroutine and read in the test goroutine with no synchronisation;
  `-race` is only quiet because of the client round-trip ordering. Nits, not
  findings.

---

## 7. Minor / nits

* `multipart` header injection: `buildEditMultipart` writes the client-supplied
  `filename` into `Content-Disposition` with `hdr.Set` (`images.go:434`). Go's
  `multipart.Reader` will happily hand back `FileName()` =
  `a\r\nX-Injected: 1` (verified), and the rebuilt part then carries an injected
  header that the upstream will parse (`X-Injected: 1"]`). This is a **relayed
  header injection into the provider request**, not into the gateway's own
  response, so severity is low (P3) — but it can defeat provider-side
  per-request logging/quotas or trip a WAF. **Fix:** sanitize
  `FileName` (`strings.Map` dropping `\r`, `\n`, `"`) or use
  `multipart.Writer.CreateFormFile`, which escapes correctly.
* `parseEditMultipart` caps each part at 64 MiB but not the *total*; 4 references
  can be 256 MiB before the body cap (64 MiB) is what actually binds. Add a
  total-content check so the error names the real limit.
* `images.go:392 atoiStrict` silently ignores negative/overflowing `n` in
  multipart (`v > 0` check), so `n=-5` becomes the default 1 instead of a 400 —
  inconsistent with the JSON path (`imagegen.go:191-197`).
* `ImageEdits` never validates `norm.N` against `MaxImagesPerRequest` for the
  multipart path.
* `videoCost` returns 0 for a model with no curated price, so an unknown video
  model bills $0 with no `incZeroCost` reason (`videos.go:686-693`); use the same
  `price_fallback_*`/metrics path as `costForModel`.
* `VideoCreate` meters the enqueue *before* the upstream confirms the duration
  (`videos.go:172-177`), so a 12 s request billed at 12 s when the provider
  actually returned 4 s over-bills. Acceptable (documented), but note it.
* `insertVideoJob`/`updateVideoJob`/`VideoList`/`VideoDelete` ignore `ctx` — a
  slow SQLite write (`busy_timeout=5000`) can extend a create beyond its budget.
  Use `ExecContext(r.Context(), …)`.
* `decision.BaseName` uses `LastIndex("/")`, so `@cf/cloudflare/clef-flash` →
  `clef-flash` (good) but `cloudflare/clef` → `clef` (good) and
  `some/prefix/clef` → `clef` (fine). No issue found.
* `decision.Validate` accepts `images`/`audio`/`videos` arrays but only bounds
  `images` (≤4) at `decision.go:157-165`; `audio`/`videos` are unbounded (the
  struct fields at `decision.go:56-58` are never validated). Add the
  same caps.
* `systemOneTarget` appends `/v1/systemone` unless the base ends in
  `/v1` or `/systemone`; a base of `https://host/api/v1/` (trailing slash) is
  trimmed first — correct. A base ending `/v1beta` would become
  `/v1beta/v1/systemone`; add a guard.
* `noRouteFor` returns 404 + `invalid_request_error` for a model that *is*
  routable elsewhere (e.g. a video model on chat when legacy fallback is off);
  the message is now actionable, but the status/type pair still says
  `invalid_request_error` — a 400 would match the guard path. Cosmetic.
* `internal/db/migrations/028` allows `key_prefix NULL`; see P2-16.
* `DiscoveryHandler.AddManual` accepts `decision` and `image` but **not**
  `video` (`internal/handler/discovery.go:79-84`), while `Update` decodes the
  whole `models.ProviderModel` and therefore *can* set `video`
  (`discovery.go:120-126`, `models.go:199-210`). An operator cannot convert a
  hand-added row into a video model without an `UPDATE` by hand, and the manual
  `image`/`decision` flags bypass the modalities consensus that the sync path
  carefully enforces — a hand-flagged row will be *overwritten* by the next
  `Enrich` (`discovery.go:477-510`), which re-derives from modalities. Either add
  `Video` to `AddManual` and document that manual flags are advisory, or make
  `enrichFor` honour a `source == "manual"` row.
* `imagegen.MaxPromptLen`/`videogen.MaxPromptLen` are byte-count limits labeled
  “characters” (`imagegen.go:194`, `videogen.go:176`) — a 3-byte UTF-8 prompt
  hits the limit at 1/3 the advertised length.

---

## Verdict

**NEEDS WORK**

The three new protocol surfaces are well-structured, locally validated, and the
dialect translation is largely faithful to the real provider shapes (Gemini /
Imagen / Veo / xAI / Cloudflare were checked field by field and are correct).
The architecture's promise — “new endpoints reuse `proxyCandidates` so they get
retries, failover, accounting and logs for free” — is exactly where the
correctness breaks, because retrying a billable image create and swallowing
video-create errors are properties of that shared machinery, not of the new code.

### Blocking list

1. **P0 — Video models are classified as image models and served by
   `/v1/images/generations`** (`imagegen/registry.go:156-158` +
   `images.go:580-604`). Wrong endpoint accepted, wrong billing rate (7¢ instead
   of $0.60+), wrong text-endpoint message, and `discovery.enrichFor`
   (`discovery.go:488-509`) writes `image=1, modalities=output:[image]` on video
   rows so `/v1/models` advertises them as image models and `Enrich` then clears
   `video`. Fix the family-rule exclusion + reorder the guards + fix the
   discovery precedence, and add “every `videogen.All()` id is not
   `imagegen.IsImageModel`” as a test.
2. **P0 — Billable image creates are retried and failed over**
   (`proxy.go:1148-1170`, `fallback.go:299-305`; reproduced: one client request
   → two upstream generations, billed twice). Add a no-retry/no-failover mode for
   non-idempotent creates (or retry only 429), and emit a metric.
3. **P1 — `/v1/images/edits` has no capability guard**: a decision model
   (`clef`) and a text model (`gpt-4o`) are forwarded to the provider's edit
   endpoint (`images.go:103-149`). Add `rejectNonImageModelOnImages` + test.
4. **P1 — The endpoint guards consult only the curated registry, so
   catalog-declared capabilities are not enforced**: a row with `decision=1`
   (the flag the gateway itself syncs from models.dev, `catalog.go:43-44,134`)
   is listed as a decision model but rejected on `/v1/systemone` and forwarded
   as chat (`systemone.go:62-65`, `images.go:503-513`,
   `decision/registry.go:145-148`); image-only detection has the mirror-image
   asymmetry (`images.go:551-568` vs `images.go:581`). Add a catalog-aware
   decision predicate and test it.
5. **P1 — `lb_rules` `model_override` is ignored by images, edits, videos and
   systemone** (`images.go:83-89,139-146`, `videos.go:124-127`,
   `systemone.go:76-86`), while cost/logging use the override — the upstream runs
   a different model than the one billed. Derive the id from the (rewritten)
   body inside each prepare closure, as chat does.
6. **P1 — `expires_at` is never enforced** and an upstream 404/410 on content
   does not terminate the job (`videos.go:189-263, 604-641`). An expired job
   reports `completed` forever and re-hits the provider on every content
   request.
7. **P1 — Veo `done` with no `generatedSamples` is reported `completed`** and
   then 502s on content with no state change (`videogen/translate.go:322-341`,
   `videos.go:601-618`); `completed_at`/`created_at` are synthesized from
   wall-clock on every poll instead of being set on transition.
8. **P1 — Video create relays the raw provider error body, unsanitized and
   unlogged** (`videos.go:136-144`): no `request_logs` row for a failed create, no
   envelope, no size cap.
9. **P1 — Video endpoints have no per-request timeout**: `DefaultVideoTTFB` is
   declared and never used, `doVideoUpstream`/`pollVideoJob`/
   `streamVideoContent` run on bare `r.Context()` with `RequestTotal=0`, so a
   hung provider blows past the edge window on create and can stall a 512 MiB
   content copy indefinitely.
10. **P1 — Azure video cannot be polled**: `videoCreateRequest` builds the
   deployment URL, `pollVideoJob`/`videoContentURL` build a bare
   `{base}/videos/{id}` (`videos.go:487-494, 604-609`), so an Azure job is
   created and never resolvable. Implement the deployment+`api-version` shape or
   reject Azure on `/v1/videos`.
11. **P2 — Multipart edits blow the TPM limiter** (2 MiB image ≈ 524 411
    “tokens”, `images.go:120` + `proxy.go:191`), and the edit body is parsed
    twice and copied three times (`images.go:111,141,340-350,408-447`).
12. **P2 — `videoJobVisible` fails open on an empty `key_prefix`**
    (`videos.go:672-678`) while the schema allows NULL (`028:15`); make the check
    fail closed and the column `NOT NULL`.
13. **P2 — Non-monotonic video status/progress**: `pollVideoJob` overwrites the
    stored status with whatever the provider last said and copies `progress`
    unclamped (`videos.go:509-527`), so a racing or regressing poll can move a
    job backwards and two concurrent polls double upstream quota. Add a
    conditional update + monotonic transitions + a `[0,100]` clamp.
14. **P2 — `rejectNonImageModelOnImages` refuses catalog text rows that are
    really unlisted image models** (`images.go:596-603`), contradicting its own
    passthrough contract; use the positive-text test the video guard already
    uses.
15. **P2 — `IMAGE_TIMEOUT_SECONDS=0` (documented “disabled”) actually re-enables
    the 300 s default** (`main.go:180-189` vs `images.go:151-156`); the config
    doc says default 300 while the sentinel is `-1`. Unify the sentinel, wire
    `DefaultVideoTTFB` into the same table, and test the 0/‑1/positive matrix.
16. **P3 — Relayed `Content-Disposition` filename injection into the upstream
    multipart request** (`images.go:434`); sanitize or use `CreateFormFile`.
17. **P3 — Unused/lying protocol data**: `videogen.SupportedSeconds/Sizes`,
    `imagegen.MaxN`, `UpstreamSystemOne` are all dead; per-model limits are not
    enforced (Veo accepts `seconds:"12"` locally), so the “fail fast with a 400
    naming the field” promise only holds for the global protocol.
18. **P3 — xAI create body sends an unstripped `model`** (`videogen/translate.go:129`,
    `normalized.Model`), unlike every other dialect.
19. **P3 — No `video_jobs` retention and no tombstone on DELETE**; `DecryptKey`
    failures on the video paths are 502s with no server-side log.
