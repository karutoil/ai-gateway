# Documentation / Spec Contract Audit — NEW endpoints (images, videos, systemone)

Scope: `openapi.yaml`, `README.md`, `ARCHITECTURE.md`, `.env.example`, `docker-compose.yml`,
`deploy/`, cross-checked against the actual implementation in
`cmd/gateway/main.go`, `internal/proxy/{images,videos,systemone}.go`,
`internal/proxy/endpoint_guards_test.go`, `internal/{imagegen,videogen,decision}/`,
`internal/config/config.go`, `internal/db/migrations/026-029`, and `internal/handler/*.go`
(route wiring reached from `main.go`).

Nothing in the repo was modified. All probes below were run in a temp module under
`.review/` (deleted afterwards) or against `/tmp` copies of `openapi.yaml`.

---

## 1. Route ↔ spec coverage

### 1.1 The new surface is fully covered (verified)

`cmd/gateway/main.go:474-512` registers 16 routes. Every one exists in `openapi.yaml` with the
matching method, and both the `/v1` form and the bare alias are present:

| Registered (`cmd/gateway/main.go`) | openapi.yaml | Verdict |
|---|---|---|
| `POST /v1/images/generations` (:481) | :578 | OK |
| `POST /images/generations` (:482) | :759 | OK |
| `POST /v1/images/edits` (:483) | :603 | OK |
| `POST /images/edits` (:484) | :766 | OK |
| `POST /v1/videos` (:489) | :626 | OK |
| `POST /videos` (:490) | :773 | OK |
| `GET /v1/videos` (:491) | :626 | OK |
| `GET /videos` (:492) | :773 | OK |
| `GET /v1/videos/{id}` (:493) | :664 | OK |
| `GET /videos/{id}` (:494) | :786 | OK |
| `GET /v1/videos/{id}/content` (:495) | :693 | OK |
| `GET /videos/{id}/content` (:496) | :803 | OK |
| `DELETE /v1/videos/{id}` (:497) | :664 | OK |
| `DELETE /videos/{id}` (:498) | :786 | OK |
| `POST /v1/systemone` (:511) | :466 | OK |
| `POST /systemone` (:512) | :745 | OK |

Auth: all 16 sit inside the group at `cmd/gateway/main.go:468-470`
(`GatewayAuthWithJWTRevocation` + `budget.Middleware` + `GatewayRateLimitWithLimits`),
and all 16 spec operations declare `security: [{ gatewayKey: [] }]`. Correct.

### 1.2 One registered gateway route is undocumented

**P2-1 — `GET /v1/models/{id}` is registered but absent from `openapi.yaml`.**

- Evidence: `cmd/gateway/main.go:500` → `r.Get("/v1/models/{id}", proxyHandler.GetModel)`;
  handler at `internal/proxy/proxy.go:3568`.
- `openapi.yaml:546` documents `/v1/models` only. `grep "models/{id}" openapi.yaml`
  returns nothing.
- Pre-existing (not introduced by this diff — it exists at `HEAD` too), but the spec is
  the published contract for the new model-listing shape and this route shares it.
- **Exact fix** — insert immediately after the `/v1/models:` block (before
  `  /v1/images/generations:` at `openapi.yaml:578`):

```yaml
  /v1/models/{id}:
    get:
      summary: Retrieve one model (object/id/owned_by/created)
      tags: [gateway]
      security: [{ gatewayKey: [] }]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string } }
      responses:
        '200':
          description: model object
          content:
            application/json:
              schema:
                type: object
                properties:
                  object: { type: string, example: model }
                  id: { type: string, example: openai/gpt-4o-mini }
                  owned_by: { type: string, example: openai }
                  created: { type: integer }
        '400': { description: model id required }
        '401': { description: authentication_error }
        '404': { description: model not found (also returned when the key's allowed_models excludes it) }
```

### 1.3 Every documented path exists — with one stale pre-existing gap

All `/v1*`, bare-alias, `/health`, `/ready`, `/api/health`, `/metrics`, `/api/*`
paths in the spec resolve to a registered handler *except* the OAuth family, which
I initially could not match and then confirmed is registered through
`oauthHandler.Routes(r)` (`cmd/gateway/main.go:448` → `internal/handler/oauth.go:28-37`)
and `oauthHandler.PublicRoutes(r)` (`cmd/gateway/main.go:410`). The mismatch in my first
mechanical scan was a mount-prefix artifact, not a spec bug.

The only spec-side drift I could confirm:
- `GET /api/models/catalog/*` (`internal/handler/catalog.go:29`) has no OpenAPI
  representation. Wildcard siblings of a documented path are legal to omit (the
  concrete `/api/models/catalog/{id}` is described by the collection), so this is
  **P3-note**, not a blocker.

### 1.4 Bare-alias documentation quality (P3)

`openapi.yaml:709-811` gives each bare alias a `summary` and a `200` but no
request/response schema, **except** `/videos`, `/videos/{id}` and
`/videos/{id}/content`, which do carry the `id` path parameter. That is consistent
with the pre-existing alias style (`/chat/completions` etc.); the `/v1` operations
carry the full schemas and the aliases are documented as identical handlers, so this
is not a correctness gap.

---

## 2. Request/response schema vs reality

I compiled and ran the exact request bodies from README against the real validators
(`internal/{decision,imagegen,videogen}.Validate`). Results:

```
systemone      ok
images1        ok
images2        ok
video-veo      ok
video-grok     ok
```

So every README example is **valid input for the shipped validators**. Findings below
are downstream of that.

### 2.1 `VideoCreateRequest.size` enum is correct and complete

`openapi.yaml:1779-1782` lists `[1280x720, 720x1280, 1024x1792, 1792x1024]`.
`internal/videogen/videogen.go:138-144` `validSizes` matches exactly. Probe:
`size:"1080x1920"` → `size: unsupported size 1080x1920 (expected 720x1280, 1280x720,
1024x1792 or 1792x1024)`. OK.

`seconds` `oneOf int|string` (`openapi.yaml:1774-1778`) matches
`secondsValue()` (`internal/videogen/videogen.go:213-227`): both `"8"` and `8` accepted, `"7"` rejected
with `seconds: must be 4, 8, or 12`. OK.

### 2.2 `ImageGenerationRequest.size` enum is **incomplete** (P2)

`openapi.yaml:1716-1719` documents:

```yaml
enum: [1024x1024, 1536x1024, 1024x1536, 1792x1024, 1024x1792, auto]
```

`internal/imagegen/imagegen.go:143-156` `validSizes` accepts **twelve** values. Probe:
`{"size":"2048x2048"}` → accepted by the gateway but **rejected by any client that
generates its dropdown from the spec**. The reverse also matters: `256x256`, `512x512`,
`2048x1152`, `1152x2048`, `4096x4096` are accepted by the gateway and absent from the
spec.

**Exact fix** (`openapi.yaml:1716-1719`):

```yaml
        size:
          type: string
          enum: [auto, 256x256, 512x512, 1024x1024, 1536x1024, 1024x1536, 1792x1024, 1024x1792, 2048x2048, 2048x1152, 1152x2048, 4096x4096]
          default: auto
```

(`ImageEditRequest.size`, `openapi.yaml:1742-1745`, has the same short list and the same
problem — the multipart parser calls the same `validSizes` via `applyEditField` →
`normalized.size`. Apply the same enum there.)

### 2.3 `quality`, `style`, `resolution` are declared enums but are free-form (P2)

- `openapi.yaml:1720` — `quality: enum [low, medium, high, auto, hd, standard]`.
  Probe `{"quality":"ultra"}` → **accepted** (`internal/imagegen/imagegen.go:228-234` stores it verbatim;
  no validation, no rejection anywhere).
- `openapi.yaml:1721` — `style: { type: string, example: vivid }` — correct (free-form).
- `openapi.yaml:1724` — `resolution: enum [1k, 2k]`. Probe `{"resolution":"4k"}` →
  **accepted** (`internal/imagegen/imagegen.go:250`); `Normalized.Resolution` is then only forwarded to
  OpenAI-compatible upstreams as a form/JSON field (`internal/proxy/images.go:387,425`).

A spec enum that the server does not enforce lies to generated clients in the
*permissive* direction (they will refuse values that work). Either enforce it or drop
the enum.

**Exact fix — prefer documenting reality** (`openapi.yaml:1720,1724`):

```yaml
        quality: { type: string, description: 'Provider-specific quality tier; passed through for OpenAI-compatible upstreams (typically low/medium/high/auto/hd/standard).' }
        ...
        resolution: { type: string, description: 'Provider-specific resolution tier; passed through unchanged (typically 1k or 2k).' }
```

(If enforcement is preferred instead, add the check in
`imagegen.ParseRequest` next to the `size` check at `internal/imagegen/imagegen.go:209-219` and keep the
enums — but then `resolution` needs a real meaning, since nothing in `internal/imagegen`
consumes it for Gemini/Imagen; see `internal/imagegen/translate.go:36-43` where only `aspectRatio` is
emitted.)

### 2.4 `ImageEditRequest.image` is documented `required` and single-typed (P2)

`openapi.yaml:1730-1739`:

```yaml
      required: [model, prompt, image]
      ...
        image:
          type: array
          items: { type: string, format: binary }
```

Reality (`internal/proxy/images.go:310-370`, `parseEditMultipart`): the parts accepted
are **`image` or `images`** (any number, up to `imagegen.MaxReferenceImages = 4` —
`internal/imagegen/imagegen.go:55`), plus `mask`. `openapi.yaml` has no `images` field on
`ImageEditRequest` at all, so a client that follows the spec can never express more
than one source file even though the server supports four.

Also note the OpenAI Images API names the field `image[]`; this gateway keys on
`image`/`images`. That difference is worth stating explicitly.

**Exact fix** (`openapi.yaml:1730-1740`):

```yaml
      required: [model, prompt]
      properties:
        model: { type: string, example: gpt-image-1 }
        prompt: { type: string }
        image:
          type: array
          maxItems: 4
          items: { type: string, format: binary }
          description: 'One or more source image files. Repeat the `image` part (or use `images`); at most 4. At least one image part is required — validated after parse, so it is not marked required here.'
        images:
          type: array
          maxItems: 4
          items: { type: string, format: binary }
          description: Alias for `image`, for clients that upload an `images` array.
        mask: { type: string, format: binary, description: Optional mask image (transparent areas are edited). }
```

(The 400 for zero images is `internal/proxy/images.go:124`: `"image: at least one image file is
required for edits"`.)

### 2.5 `ImagesResponse` / `ImageDatum`: `b64_json` vs `url` honouring is documented correctly

`openapi.yaml:1746-1761` (`b64_json`, `url`, `mime_type`, `revised_prompt`) matches
`imagegen.Response/ImageDatum` (`internal/imagegen/imagegen.go:97-103`) exactly, including the
`omitempty` fields. The `url` semantics are the non-obvious part and they *are*
documented at `openapi.yaml:1759` as "Image URL **or data URI**", which is what
`imageDatum()` (`internal/imagegen/translate.go:177-190`) actually produces for Gemini/Imagen
(`data:<mime>;base64,...`). Correct — the `url` vs `b64_json` selection is driven by the request's `response_format` (`internal/proxy/images.go:92` and `:146` pass `norm.ResponseFormat` into `imageTransform`), which the spec documents as an enum with default `b64_json`. OK.

`ImageUsage` (`openapi.yaml:1762-1767`) matches `imagegen.Usage` field-for-field. OK.

### 2.6 Video job object: fields and status values match

`openapi.yaml:1784-1799` (`Video`) vs `internal/videogen/videogen.go:102-120`:

- `status` enum `[queued, in_progress, completed, failed]` — exactly
  `StatusQueued/StatusInProgress/StatusCompleted/StatusFailed` (`internal/videogen/videogen.go:62-67`),
  and `NormalizeStatus` only ever emits those four (`internal/videogen/videogen.go:265-279`). OK.
- `completed_at` / `expires_at` / `error` are `nullable: true` — matches the pointer
  fields that serialize as `null` on a fresh job (`internal/videogen/videogen.go:104-118` and comment).
  OK.
- `progress` 0-100 int — matches `Progress int` and Veo's `Progress = 100` on done
  (`internal/videogen/translate.go:364`). Note the spec calls it "Percent complete (best effort)"; for
  OpenAI-style dialect the upstream's own `progress` is relayed
  (`internal/proxy/videos.go:570`), so "best effort" is accurate.
- `seconds` is documented as a **string** (`openapi.yaml:1816`). `videoJob.toVideo`
  always sets `Seconds: j.Seconds` where `Seconds` is a string
  (`internal/proxy/videos.go:65`). Correct — and this is a real trap for SDK authors,
  because the create request accepts int *or* string while the response is
  string-only. It is documented, so OK.
- `VideoDeleted` (`openapi.yaml:1807-1811`) matches the literal written by
  `VideoDelete` (`internal/proxy/videos.go:303-304`): `{id, object:"video.deleted", deleted:true}`. OK.
- `VideoList` (`object:"list", data:[Video]`) matches `VideoList` (`internal/proxy/videos.go:283`). OK.

### 2.7 Missing documented status codes on the video surface (P2)

The handlers emit statuses the spec does not list:

| Reality | Evidence | Spec today |
|---|---|---|
| `409` on `/v1/videos/{id}/content` when the job is not terminal, and when it failed | `internal/proxy/videos.go:234, 241` | only `200/401/404` `openapi.yaml:700-707` |
| `502` on get/content when the owning provider is gone, the credential is unavailable, or the content URL cannot be resolved | `internal/proxy/videos.go:206-211, 247-258` | not documented |
| `502` on create when the upstream call fails or returns no job handle | `internal/proxy/videos.go:136, 151` | not documented |
| `500` on create/list/delete when the DB write fails | `internal/proxy/videos.go:170-174, 271-276, 299-302` | not documented |
| `503` when no video-capable provider exists | `internal/proxy/videos.go:323-331` | not documented |
| `400` for a non-video model ("is an image model, not a video model") | `internal/proxy/videos.go:336-350` | documented only on POST `openapi.yaml:661` |

**Exact fix** — add to `/v1/videos/{id}/content` (`openapi.yaml:700-707`):

```yaml
        '404': { description: not_found_error (unknown id, or a job owned by another gateway key) }
        '409': { description: 'video not ready yet, or the job failed (invalid_request_error; the message names the current status or the provider error)' }
        '502': { description: proxy_error (owning provider removed, or the upstream could not serve the bytes) }
```

and to `/v1/videos/{id}` `get` (`openapi.yaml:671-678`):

```yaml
        '502': { description: proxy_error (owning provider removed / credential unavailable) }
```

and to `POST /v1/videos` (`openapi.yaml:655-663`):

```yaml
        '500': { description: proxy_error (job could not be persisted) }
        '502': { description: proxy_error (upstream create failed, or returned no job handle) }
        '503': { description: no video-capable provider configured for this model, or no member below its OAuth quota threshold }
```

### 2.8 `/v1/systemone` request: three real drifts (P1/P2)

I validated the schema against `internal/decision/decision.go` line by line.

**(a) `criteria` is required for `choice` and `score`, but the spec ties it to nothing.**
`openapi.yaml:501-503` says `criteria` is just "choice: object of 2-255 options…".
Reality (`internal/decision/decision.go:260-262, 282-284`): `choice` without `criteria` → 400
`questions.<id>.criteria: required map of option to description`; `score` likewise.
Only `noul` has optional criteria (`internal/decision/decision.go:244-246`). The probe confirmed it:

```
systemone probe {"model":"clef","state":"x","questions":{"a":{"type":"choice",...}}}  -> <nil>   (with criteria)
```

(dropping `criteria` returns the 400 above — asserted by
`internal/decision/decision_test.go:46-47`).

**(b) `noul` criteria keys are restricted to the literal strings `"true"`/`"false"`.**
`internal/decision/decision.go:244-258` rejects any other key: `questions.<id>.criteria: keys must be
"true" and/or "false"`. The spec's `{true,false} descriptions` phrasing is *almost*
right but reads like a boolean-keyed map; a JSON object cannot have boolean keys, so
this should be spelled out.

**(c) `instructions` accepts string **or** object **or** array, but empty containers are
rejected, and the spec calls it optional-ish.** `openapi.yaml:501` gives
`instructions: { description: "String, object, or array." }` with no `minProperties`/
`minItems`, while `internal/decision/decision.go:213-241` rejects `""`, `{}` and `[]` with
`must be a non-empty string, object, or array`. Similarly `questions` ids are charset-
restricted (`internal/decision/decision.go:169-183`: letters, digits, `_`, `.`, `-`, ≤100 chars) and the
spec only says `minProperties/maxProperties` — a client can construct an id the server
rejects. Probe confirms: `{"a b": …}` → `questions.a b: question id may only use
letters, digits, '_', '.' and '-'`.

**(d) `state` is declared without a type** — `openapi.yaml:490-491` is
`state: { description: … }`. `internal/decision/decision.go:129-130` requires it to be present and non-null
but accepts any JSON value. Because it has no `type`, tools that generate clients from
the spec typically emit `interface{}`/`any`, which is fine, but a validator will not
flag a missing `state` against a schema that does not require it… except
`required: [model, state, questions]` does cover presence. OK, but note
`state: null` is also rejected (`internal/decision/decision.go:129-130`) and a schema without `nullable`
implies non-null. That part is right.

**Exact fix** — replace `openapi.yaml:478-516` request body with:

```yaml
            schema:
              type: object
              required: [model, state, questions]
              properties:
                model:
                  type: string
                  example: clef
                  description: "Decision model selector (clef, clef-flash, clef-omni, kev-0.8b, kev-4b, kev-9b, kev-27b, jev-latest, laya, …). Prefer provider/model to pin a route; an unknown id is rejected with 400."
                state:
                  description: "Content to evaluate: string, object or array. Must be present and non-null."
                questions:
                  type: object
                  minProperties: 1
                  maxProperties: 64
                  description: "Map of question id → typed question. Ids must be 1-100 chars of [A-Za-z0-9_.-]; any other id is a 400 naming the id."
                  additionalProperties:
                    type: object
                    required: [type, instructions]
                    properties:
                      type:
                        type: string
                        enum: [noul, choice, score]
                        description: "noul = yes/no (P(yes)); choice = one of N options; score = ordered rubric. Any other value is a 400 (unknown question type)."
                      instructions:
                        description: "String, object, or array — but not empty: '', {} and [] are all rejected with 400."
                      criteria:
                        description: |
                          choice: REQUIRED object of 2-255 non-empty option keys → description.
                          score: REQUIRED ordered array of 2-10 levels.
                          noul: optional object whose only permitted keys are the strings "true" and "false".
```

**(e) `images` maxItems is enforced, `audio`/`videos` are not.** `openapi.yaml:504-515`
describes `images` (maxItems 4), `audio`, `videos`. `internal/decision/decision.go:157-165` validates
`images` (≤4) and **never touches `audio`/`videos`** — they pass through unchecked to
the upstream. The spec does not claim a limit for them, so this is consistent; flag as
P3 doc-note only (unbounded arrays are forwarded verbatim).

### 2.9 `/v1/systemone` response: `usage` is documented as optional, reality is always present

`openapi.yaml:517-545` uses `type: object` + `properties` for `model`, `answers`,
`usage` — no `required`, so `usage` is optional. `decision.Response`
(`internal/decision/decision.go:79-83`) has `Usage Usage` **without** `omitempty`, so the field is always
emitted (possibly as zeros). Also the spec's `usage.output_tokens` is documented
implicitly as a real number, while the package comment says output is free
(`internal/decision/decision.go:66-74`) and `README.md:114` says "output tokens are free".

Minor, but a schema-driven client can treat `usage` as absent. **Exact fix**
(`openapi.yaml:523-542`): add `required: [answers]` at the response object level and
change the usage description:

```yaml
                usage:
                  type: object
                  description: 'Always present. Decision models bill input tokens only; output_tokens is reported for shape compatibility and is not billed.'
```

**P3.** The built-in `legend` / `probabilities` / `confidence` answer fields
(`internal/decision/decision.go:50-60`) are documented and correctly optional.

### 2.10 Error envelope: documented shape is right, but the new endpoints do not always use it (P1 doc risk)

`openapi.yaml:11` says `Errors: {error:{message,type}}`, and `httperr.Write`
(`internal/httperr/httperr.go:19-31`) does emit exactly that. All three new handlers
use `httperr` for their own failures — **except two paths that relay raw upstream
bytes**:

- `internal/proxy/videos.go:143-148` — a non-2xx upstream create response is written
  verbatim (`w.Write(respBody)`) with the upstream's status. That is a deliberate
  design choice (the comment says so) but it means `POST /v1/videos` can return an
  error body that is **not** the documented envelope.
- `internal/proxy/videos.go:216-219` — transient poll failure serves the last known
  job (200), which is fine.

Docs should state the exception rather than leave SDK authors to discover it.

**Exact fix** — add to the `POST /v1/videos` description (`openapi.yaml:640-647`):

```yaml
        An upstream 4xx/5xx from the **create** call is relayed verbatim (status and
        body) so the provider's own message reaches the caller; those responses are
        not the gateway `{error:{message,type}}` envelope. Every other failure on
        this surface uses the standard envelope.
```

and the same sentence to the `info.description` error line (`openapi.yaml:11`) as a
global caveat:

```yaml
    Errors: {error:{message,type}}. Exception: POST /v1/videos relays the upstream
    create error verbatim so the provider's own message reaches the caller.
```

### 2.11 `type` enum in `CreateProvider` was extended — it matches the code (verified)

`openapi.yaml:1685` now allows `systemone, cloudflare, google, xai` on top of the
original six. `internal/provider/store.go:319` accepts exactly:
`openai, anthropic, azure, openai_compatible, antigravity, devin, systemone,
cloudflare, google, xai`. **In sync.** Note `internal/provider/store.go:325` exempts only
`antigravity`/`devin`/`systemone` from the API-key requirement — `cloudflare` and
`google` **require** a key, which the `api_key` description at `openapi.yaml:1687`
("Not required for OAuth providers (antigravity, devin)") does not mention.
**P3 exact fix** (`openapi.yaml:1687`):

```yaml
        api_key: { type: string, example: sk-..., description: 'Required for every provider type except antigravity, devin and systemone (which may use an empty key for a local llama.cpp/Kev server).' }
```

### 2.12 `/v1/models` new fields: documented, and mostly real (P2 gap on `decision`)

`openapi.yaml:570-577` documents `image, image_only, video, video_only, video_cost,
modalities, kind, image_cost`. All of these are emitted by
`internal/proxy/proxy.go:3706-3744` and `3790-3824`.

**Gap:** the `decision` boolean and `kind: "decision"` value that
`internal/proxy/proxy.go:3706-3707` emit are **not** in the `/v1/models` schema, even though
`ARCHITECTURE.md:167` tells operators "`GET /v1/models` includes `"decision": true`,
`"kind": "decision"`" and `README.md:152` advertises "`decision` flag in model
listings".

**Exact fix** — add after `owned_by` (`openapi.yaml:569`):

```yaml
                        decision: { type: boolean, description: 'true for System One / decision models (POST /v1/systemone). Emitted on the provider_models path; on the catalog-fallback path the field is present when the catalog row carries it.' }
```

and widen the `kind` example (`openapi.yaml:576`):

```yaml
                        kind: { type: string, enum: [image, video, decision], description: 'Capability class; absent for a plain text model. A model with several capabilities takes the last flag applied — decision < video < image.' }
```

(`internal/proxy/proxy.go:3694-3744` sets `kind` decision → video → image in that order, so
`kind` reflects the *last* capability, not a list. Documenting the precedence avoids
a support question. The precedence itself is a code smell worth flagging to the backend
reviewer, not a docs fix.)

Also: `video_cost` is documented as "USD per generated second" (`openapi.yaml:573`) —
matches `videogen.CostSecond` via `internal/proxy/proxy.go:3713` and `videoCost`
(`internal/proxy/videos.go:687-693`). OK.

### 2.13 Auth semantics on `/v1/*`: correct and already documented

`openapi.yaml:1638-1642` (`gatewayKey`) says both `Authorization: Bearer sk-gw-…` and
`x-api-key` are accepted, and that the dashboard `gw_token` cookie works.
`internal/middleware/middleware.go:189-200` accepts all three. `cmd/gateway/main.go:283-285` CORS
allowlists exactly `Authorization, x-api-key, X-Provider, anthropic-version,
X-Gateway-Key-Prefix, X-Gateway-Org, X-Request-ID` — no missing header for the new
endpoints. OK.

---

## 3. Is `openapi.yaml` valid? — **No** (P0 for any spec-consuming consumer)

Tooling: `openapi-spec-validator` (installed in a throwaway venv at `/tmp/oasvenv`),
`PyYAML 6` for the parse/`$ref` check.

### 3.1 It parses as YAML. `$ref`s resolve. No duplicate keys.

```
OK 3.0.3 85                      # yaml.safe_load, 85 paths
total refs 14 unresolved []      # every internal $ref resolves
dup scan done                    # duplicate-key scanner: none
```

### 3.2 It is **not a valid OpenAPI document** — 1 syntax-class error + 3 pre-existing

```
INVALID: OpenAPIValidationError
... is not valid under any of the given schemas
On instance['paths']['/api/lb/rules/{model}']['put']['responses']['400']:
    {'description': 'Unknown provider',
     'duplicate (provider': None,
     'model) option': None,
     'or invalid body': None}
```

**P1-1 — unquoted comma in a flow-mapping `description` splits it into four keys.**
`openapi.yaml:1613`:

```yaml
        '400': { description: Unknown provider, duplicate (provider, model) option, or invalid body }
```

`description` becomes the string `Unknown provider`, and three junk keys are created.
Any spec-driven client generator (openapi-generator, orval, `openapi-typescript`) will
either fail or silently drop the 400 response for `PUT /api/lb/rules/{model}`.
The change is **pre-existing in content** (it is the same line at `HEAD:1334`) but was
introduced with the appended description text — this diff restores the missing quotes on
the *sibling* line at `openapi.yaml:1428` (`/api/logs/export`) and leaves this one
broken, so it is fair to fix in the same pass.

**Exact fix** (`openapi.yaml:1613`):

```yaml
        '400': { description: 'Unknown provider, duplicate (provider, model) option, or invalid body' }
```

With that single change the validator still fails, on path parameters:

**P1-2 — three OAuth operations use `{id}` in the path but declare no `id` parameter.**

```
STILL INVALID: Path parameter 'id' for 'get' operation in
'/api/providers/{id}/oauth/status' was not resolved
```

Pre-existing (identical at `HEAD`, where these blocks are at `:974-990`), but the
spec is the artifact this review is auditing and OpenAPI 3.0 requires every
templated variable to be declared.

**Exact fixes** — add a `parameters:` block to each of the three operations
(`openapi.yaml:1253`, `:1260`, `:1283`), immediately after their `security:` line:

```yaml
      parameters:
        - { name: id, in: path, required: true, schema: { type: string } }
```

Verification: applying only the comma-quote fix and these three parameter blocks
yields a fully valid document:

```
VALID after 3 param fixes + comma quote
```

So the new endpoint blocks themselves (`/v1/videos*`, `/v1/images/*`, `/v1/systemone`,
the new `components.schemas`) are structurally valid — the document's invalidity is
entirely in pre-existing admin paths. That is good news for the new work but does not
change the verdict: **`openapi.yaml` as committed is not a valid OpenAPI 3.0.3
document**, and it is served to clients at `/openapi.yaml` (`cmd/gateway/main.go:539`).

**Recommendation:** add the validator to CI so this cannot regress (see P2-5).

---

## 4. Docs↔reality drift in README / ARCHITECTURE / .env.example

### P1-3 — `IMAGE_TIMEOUT_SECONDS` is documented as a 300 s default; `0` is documented as "disabled" but re-enables 300 s

Three statements, all in the same diff, that do not compose:

| Source | Claim |
|---|---|
| `internal/config/config.go:30` | "`IMAGE_TIMEOUT_SECONDS` \| no \| **300** \| … 0 disables" |
| `internal/config/config.go:404` | `getInt("IMAGE_TIMEOUT_SECONDS", **-1**)` |
| `internal/config/config.go:84-89` | "0 **disables** the budget; negative falls back to the built-in default (300s)" |
| `.env.example:51-54` | "**0 disables the budget**; unset uses the built-in default (300s)" |
| `cmd/gateway/main.go:182-186` | `cfg.ImageTimeoutSecs == 0` → `proxyHandler.ImageTTFB = 0` and logs `"IMAGE_TIMEOUT_SECONDS=0: image first-byte budget disabled"` |
| `internal/proxy/images.go:150-156` | `if h.ImageTTFB > 0 { return h.ImageTTFB }; return DefaultImageTTFB` |

The sentinel chain is coherent **except for the `0` case**: `main.go` sets
`ImageTTFB = 0` intending "disabled", but `imageTTFB()`
(`internal/proxy/images.go:151-156`) treats `<= 0` as "use the default", and
`proxyCandidatesWithOpts` (`internal/proxy/fallback.go:340-344`) only overrides the
chat TTFB when `extra.ttfb > 0`. Net effect: `IMAGE_TIMEOUT_SECONDS=0` silently gets
the **300 s** image budget and the **85 s** chat budget depending on which layer you
ask — it never becomes "disabled". This is a **behaviour** bug (backend review P3-26
has the same finding) with a **documentation** consequence: `.env.example:53` and
`internal/config/config.go:88` both tell operators `0` disables the budget, which is false.

**Exact doc fix** (`.env.example:51-54`):

```
# Client-facing first-byte budget for the image endpoints (/v1/images/*).
# Image generation is legitimately slow, so it does NOT use the chat TTFB
# budget. Unset (or any negative value) uses the built-in default of 300s;
# a positive value overrides it. NOTE: 0 does not currently disable the
# budget — the handler falls back to the 300s default (see
# internal/proxy/images.go imageTTFB).
# IMAGE_TIMEOUT_SECONDS=300
```

**Exact doc fix** (`internal/config/config.go:30`, the env table):

```go
// | IMAGE_TIMEOUT_SECONDS| no       | 300 (via -1 sentinel)| positive = client first-byte budget for /v1/images/*; 0 or negative currently resolves to the 300s default |
```

**Exact doc fix** (`internal/config/config.go:84-89`):

```go
	// ImageTimeoutSecs is the client-facing first-byte budget for
	// /v1/images/generations and /v1/images/edits. Image generation is
	// legitimately slow (10–60s+), so it must NOT inherit the 85s chat TTFB
	// default. Positive values override the budget; the sentinel -1 (the env
	// default) selects the built-in 300s default. NOTE: 0 currently ALSO
	// resolves to the 300s default because Handler.ImageTTFB uses 0 as
	// "unset" — see imageTTFB() in internal/proxy/images.go.
```

(If the backend team instead fixes the handler so 0 really disables it, then
`.env.example`/`internal/config/config.go:30` should keep the "0 disables" wording and the *code*
comment at `cmd/gateway/main.go:184` is already right. Either way the current combination is
wrong — do not ship the docs as they stand.)

### P2-2 — `IMAGE_TIMEOUT_SECONDS` is missing from the README configuration table

`README.md:169-184` lists the env vars operators actually set; `IMAGE_TIMEOUT_SECONDS`
is absent, so an operator reading only the README cannot discover the knob at all
(`.env.example:54` ships it commented out, and `README.md:186` points at
`.env.example` for "full reference", which is where it lives — acceptable, but the new
*image/video* sections at `README.md:71-147` never mention the 300 s budget a slow
Gemini image call will hit).

**Exact fix** — add a row to the table (`README.md:184`, after
`ROUTING_LEGACY_FALLBACK`):

```markdown
| `IMAGE_TIMEOUT_SECONDS` | `300` (sentinel −1) | Client first-byte budget for `/v1/images/*`. Image generation is much slower than chat, so it does not use `TTFB_TIMEOUT_SECONDS`. Positive values override; unset/negative use 300s. |
```

### P2-3 — ARCHITECTURE.md claims the migration range ends at 026; it is 029

Two places, both wrong by three migrations:

- `ARCHITECTURE.md:49` — `db/migrations/  # 001_initial … 026_decision_models (embedded, run at boot)`
- `ARCHITECTURE.md:108` — "Authoritative DDL lives in
  `internal/db/migrations/001_initial.sql … 026_decision_models.sql`"

Reality: `internal/db/db.go:94-104` embeds through `029_video_models.sql`, and
`db.LatestMigrationVersion()` (`internal/db/db.go:150-155`) returns **29**. Migration 028
(`video_jobs`) is *the whole persistence layer for the videos feature the same document
describes at `ARCHITECTURE.md:209-224`* — so the doc contradicts itself.

**Exact fixes:**

`ARCHITECTURE.md:49`
```markdown
    db/migrations/  # 001_initial … 029_video_models (embedded, run at boot; LatestMigrationVersion() = 29)
```

`ARCHITECTURE.md:108`
```markdown
Authoritative DDL lives in `internal/db/migrations/001_initial.sql … 029_video_models.sql` (embedded, applied at boot through `schema_migrations`; a failed migration marks the version **dirty** and boot aborts until an operator intervenes). The highest embedded version is reported by `db.LatestMigrationVersion()`. Highlights:
```

### P2-4 — ARCHITECTURE.md's Data Model bullet omits `video_jobs` and two of the three capability flags

`ARCHITECTURE.md:110-116` bullet list:

- `:114` mentions only the `decision` flag: "(`decision` flag marks System One /
  decision models on both model tables)".
- Nothing mentions `image` (027) or `video` (029).
- Nothing mentions the `video_jobs` table from 028, even though the video section at
  `:217-219` says "The `video_jobs` table (migration 028) is the durable mapping …".

**Exact fix** — replace `ARCHITECTURE.md:113`:

```markdown
- `models_catalog`, `provider_models`, `model_aliases`, `system_config` — capability flags on both model tables: `decision` INT (026), `image` INT (027), `video` INT (029). Default 0 = text model; a model can carry several.
- `video_jobs` (028) id / provider_id / key_prefix / dialect / model / upstream_id / prompt / seconds / size / status / progress / error_code / error_message / video_url / created_at / updated_at / completed_at / expires_at — the durable mapping from a gateway-issued `video_…` id to the provider's own handle, and the last observed state (indexed on `key_prefix`, `provider_id`, `created_at`)
```

### P2-5 — The "Dialect trap" paragraph over-claims what pgsmoke catches

`ARCHITECTURE.md:120-129` says `internal/db/pgsmoke` "exercises every flag write path
against a real Postgres, because SQLite cannot catch this class of bug."

Reality: `internal/db/pgsmoke/main_test.go:18-23` **skips the whole suite unless
`GATEWAY_PG_DSN` is set**, and `.github/workflows/ci.yml:18-41` never sets it. So in
CI — the only place a regression would be caught — the guard does nothing. The
database reviewer independently found this (`.review/database.md:296-326`).

**Exact fix** (`ARCHITECTURE.md:128-129`):

```markdown
`internal/db/pgsmoke` is the guard: it exercises every flag write path against a real
Postgres, because SQLite cannot catch this class of bug. It **skips itself when
`GATEWAY_PG_DSN` is unset**, and CI currently does not set it — run it locally with a
disposable Postgres (`GATEWAY_PG_DSN=postgres://… go test ./internal/db/pgsmoke`) and
wire a `postgres:16` service into `ci.yml` before relying on it.
```

### P2-6 — README claims OpenAI SDK compatibility; the actual field names differ

`README.md:73-92` and `README.md:116-147` repeatedly say "official SDKs work by changing
`baseURL`" and "mirroring the OpenAI Videos API exactly". Three concrete divergences a
client will hit:

1. **Image edits field name.** OpenAI's Images *edit* endpoint takes `image` (a file, or
   `image[]` for multiples). This gateway accepts `image` **or `images`**
   (`internal/proxy/images.go:344-356`) and caps at 4 (`internal/imagegen/imagegen.go:55`). Not
   documented in `README.md:87` (it says "one or more `image` files" — directionally
   right, but the `images` alias is missing).
2. **`/v1/systemone` is not an OpenAI API at all** — that is fine, but `README.md:96`
   opens with "Decision models — …" without saying the endpoint is a *gateway-defined*
   protocol (the wire format is TypeSafe's System One API). A reader could conclude it
   is OpenAI-compatible.
3. **Video create `seconds`.** OpenAI Sora accepts `seconds` as a string `"4"|"8"|"12"`;
   this gateway accepts int or string on input (`internal/videogen/videogen.go:213-227`) but **always
   returns a string** (`internal/proxy/videos.go:65`, `internal/videogen/videogen.go:110`). Documented in the spec,
   not in the README.

**Exact fix** — append to `README.md:87`:

```markdown
The multipart edit endpoint accepts the file part as either `image` (repeat for
multiple sources) or `images`, up to **4** source files, plus an optional `mask`.
```

Append to `README.md:114` (before the video section):

```markdown
> `/v1/systemone` is a *gateway-defined* endpoint speaking the System One wire protocol
> (TypeSafe Jev / Cloudflare Workers AI). It is not an OpenAI API; there is no OpenAI SDK
> mode for it.
```

Append to `README.md:116`:

```markdown
`seconds` may be sent as `8` or `"8"` on create; the job object always reports it as a
string (`"seconds": "8"`), matching the OpenAI Videos schema.
```

### P2-7 — `[OI]` is used 25 times (on 19 lines) as if it were a defined term; it defines nothing

`README.md:73,81,87,92,150,151`, `ARCHITECTURE.md:70,71,140,141,142,171,172,176`,
`openapi.yaml:580,583,585,605,609` (25 occurrences across 19 lines, **all new in this diff** — zero at
`HEAD`). Reading the context, `[OI]` is a stand-in for "OpenAI" (probably a
scrub/replace artifact: `"name":"[OI]"` also appears in
`internal/catalog/image_seed_test.go:48`). It appears in published API descriptions
served at `/openapi.yaml`, in the README's image section, and in the architecture
tables.

This is a **documentation correctness bug**: the spec's `summary` for
`/v1/images/generations` currently reads *"Image generation ([OI]-compatible Images
API, gateway auth)"*.

**Exact fix** — replace every `[OI]` with `OpenAI` in `README.md`, `ARCHITECTURE.md`
and `openapi.yaml` (and `internal/catalog/image_seed_test.go:48` if the intent was the
provider display name). Verify with `grep -rn "\[OI\]" README.md ARCHITECTURE.md
openapi.yaml` → no results.

### P3-1 — README's architecture ASCII diagram does not show the new endpoints

`README.md:222-234`:

```
  ├─ /health /ready /metrics /openapi.yaml
  ├─ /api/*   admin control plane …
  └─ /v1/*    gateway API (sk-gw-*): rate limits + budgets →
              translate? → resolve route → decrypt key → proxy
              (SSE-aware, retry, cache) → log usage/cost
```

That is still true (the new endpoints are under `/v1/*`), but the README spends 76
lines describing images/videos/systemone and the diagram lists none of them. A reader
scanning the diagram concludes the gateway is chat-only. Low severity.

**Exact fix** (`README.md:231-233`):

```
  └─ /v1/*    gateway API (sk-gw-*): rate limits + budgets →
              translate? → resolve route → decrypt key → proxy
              (SSE-aware, retry, cache) → log usage/cost
              chat/completions · embeddings · messages · responses
              images/{generations,edits} · videos (async jobs) · systemone
```

### P3-2 — `ARCHITECTURE.md:82-96` middleware diagram predates the gateway group

`ARCHITECTURE.md:88-96` shows `/v1/*` → GatewayAuth → budget → RateLimit, which is
accurate. But the bare alias `/models` (`cmd/gateway/main.go:517-533`) goes through a *separate*
`chi.NewRouter()` with a duplicated middleware chain (`cmd/gateway/main.go:524-526`) plus an
`Accept: text/html` SPA escape hatch. The doc says nothing about it, and this same
pattern is what the new bare aliases (`/videos`, `/images/edits`, `/systemone`) do
**not** use — worth one line so a future reviewer does not think the aliases are
inconsistent.

**Exact fix** — append after `ARCHITECTURE.md:96`:

```markdown
  /v1/*   -> ... (as above)
  bare aliases (/chat/completions, /images/generations, /videos, /systemone, …)
          -> identical middleware chain, registered on the same gateway group.
          Exception: GET /models is served by a private sub-router with the same
          chain, because a browser refresh on the dashboard's /models screen must
          get the SPA instead (Accept: text/html falls through to serveWeb).
```

---

## 5. Deploy / ops documentation

### 5.1 `IMAGE_TIMEOUT_SECONDS` reaches the container — no manifest change required

`docker-compose.yml:21-27` uses `env_file: [.env.example, .env]`, and
`deploy/production/podman-compose.prod.yml:30-31` uses `env_file: [.env]`. The binary
reads the variable through `getInt`/`os.Getenv` (`internal/config/config.go:93-106` and `:404`) after loading
`.env` itself, so an operator *can* set it. **No missing manifest entry.**

`docker-compose.yml:26-28` and `deploy/production/podman-compose.prod.yml:37-44` only
hard-code `PORT`, `DATABASE_URL`, `REDIS_URL`; every other knob rides through
`env_file`. `IMAGE_TIMEOUT_SECONDS` is therefore *reachable* but not *discoverable* from
the compose files — which matches how `TTFB_TIMEOUT_SECONDS`,
`UPSTREAM_HEADER_TIMEOUT_SECONDS`, `STREAM_IDLE_TIMEOUT_SECONDS`, `SHUTDOWN_GRACE_SECONDS`
are already handled. Consistent; **P3-note only**.

### 5.2 No new env var was introduced that the manifests miss

The diff adds exactly one env var (`IMAGE_TIMEOUT_SECONDS`, `internal/config/config.go:404`). No
image/video/decision endpoint needs one — model routing comes from providers +
`model_aliases` + `lb_rules`, not config. Verified by diffing the `os.Getenv` /
`getInt` / `getBool` names in `internal/config/config.go` against
`docker-compose.yml`, `deploy/production/podman-compose.prod.yml`,
`deploy/systemd/ai-gateway.service` (`EnvironmentFile=/etc/ai-gateway.env`, so any var
is reachable) and `.env.example`.

**One inconsistency found:** `deploy/production/.env` (the live production file, left
untouched) contains no `IMAGE_TIMEOUT_SECONDS`, but it *does* contain
`TTFB_TIMEOUT_SECONDS=`, `MAX_PROXY_BODY_MB=`, `CACHE_TTL_SECONDS=`,
`CACHE_STREAMS=`. That is an **operations action item, not a doc bug**: production
will run with the 300 s image default. Flagging so the ops owner knows the behaviour
changed under them.

### 5.3 P2-8 — `deploy/` contains no doc that names the new endpoints

`deploy/systemd/README.md` (79 lines) covers install + socket activation and lists the
env file contents (`:41-47`: `ADMIN_PASSWORD`, `MASTER_KEY`, `JWT_SECRET`,
`DATABASE_URL`). It never mentions images/videos/systemone, nor that the image budget
is 300 s. `deploy/failover/README.md` covers failover and backend probing. There is no
`deploy/README.md`.

Is that a problem? The BRIEF asks "are the new endpoints described in the deploy/ops
docs". Answer: **no, and they do not need to be, with one exception** — the ops
*upgrade* path. Migration 026-029 add columns and a table; a rollback to a pre-026
binary against a post-029 database is not covered anywhere, and
`README.md:194` ("Migrations run automatically at boot; a failed migration is flagged
dirty") is the only sentence about upgrade safety. `video_jobs` rows also have no
retention story (nothing in `internal/` purges them — `grep -rn "video_jobs"
internal/ | grep -v videos.go` → only the `go:embed` line, so **no cleanup job
exists**), and `expires_at` is persisted but never *enforced* on read
(`internal/proxy/videos.go:670-679` `videoJobVisible` checks only `key_prefix`; nothing
consults `Expires`). That means `video_jobs` grows without bound and the spec's
`expires_at` description ("Unix time after which the job or asset **may be purged**")
is honest about being advisory — but the ops docs should say so.

**Exact fix** — add a paragraph to `deploy/systemd/README.md` (after the env block, at
`:48`) and to the README Operations section (`README.md:194-197`):

```markdown
**Upgrading past migration 029.** Boots 026-029 add the `decision`/`image`/`video`
columns and the `video_jobs` table; they are additive and safe to run on a live
database. Rolling *back* to a pre-026 binary is not supported against a 029 schema
(`schema_migrations` is left at 29 and the older binary will not undo it). Video jobs
are never purged by the gateway: `video_jobs` grows with every create, and the
`expires_at` field is informational only — the gateway does not enforce it. Add your own
retention (`DELETE FROM video_jobs WHERE created_at < …`) if you run the videos API at
volume.
```

---

## 6. Things I checked that are correct (so they are not re-litigated)

- New route registration, middleware chain and auth on all 16 new routes: correct
  (`cmd/gateway/main.go:468-512`).
- Model-capability guards are wired on **every** text endpoint, not just chat:
  `proxy.go:3190-3196` (chat), `3324-3330` (completions), `3522-3528` (embeddings),
  `4042-4048` (messages), `4188-4194` (responses). The guards run *before* candidate
  resolution, so the actionable 400 is reachable — which is exactly what
  `internal/proxy/endpoint_guards_test.go` asserts and what the tests pass
  (`go test ./internal/proxy -run 'TestDecisionModel|TestImageOnly|TestImageCapable|TestWrongModelOnSystemOne|TestRealDecisionModel|TestNoRouteForIsCapabilityAware|TestImage'`
  → `ok`).
- `POST /v1/images/edits` really is multipart (`internal/proxy/images.go:96-137`,
  `parseEditMultipart` at `:331`), and the spec's `multipart/form-data` content type is
  right.
- Migration 026-029 contents match ARCHITECTURE's dialect-trap paragraph: the three
  new flags are `INTEGER` (026:10-11, 027:12-13, 029:9-10), while the sibling
  capability columns are `BOOLEAN` (`001_initial.sql:50-55, 84-87`). The claim is
  accurate.
- `db.BoolInt` is used at every write site I could find: `internal/catalog/catalog.go:781,820,942,967`,
  `internal/discovery/discovery.go:411,416,622,628,689,776`. `db.BoolInt(true)` is inlined in the seeds
  (`internal/catalog/catalog.go:820,942,967`) — correct, though `db.BoolInt(true)` for a constant is
  noise (P3).
- Every `INSERT INTO models_catalog` / `provider_models` I parsed has matching column
  and placeholder counts (25/25 ×4 in `catalog.go`; 25/25, 25/25, 26/26, 7/7 in
  `discovery.go`), and `catalogColumns` (25) matches the 25 `Scan` destinations
  (`internal/catalog/catalog.go:77,107-110`). The new columns did not break any row-mapping.
- `internal/proxy/videos.go` contains **no goroutines** (`grep "go func\|go h\." →
  nothing`), which matches the stateless-by-design claim in `ARCHITECTURE.md:215-216`
  and migration 028's header comment.
- Video job ownership **is** checked on GET/content/DELETE (`internal/proxy/videos.go:196, 229, 295`
  → `videoJobVisible` at `:670-679`), and a mismatch returns `404`, not `403`. Good
  (avoids an existence oracle). One caveat for the docs: `videoJobVisible` returns
  `true` when *either* prefix is empty (`:674-676`), and a dashboard JWT always
  produces a session prefix (`middleware.go:264,284-296`), so the "dashboard sessions
  see everything" comment is not actually reachable — every request has a non-empty
  prefix. Worth a doc correction if anyone documents this scoping.
  **Exact fix** (`internal/proxy/videos.go:670-671`):
  ```go
  // videoJobVisible scopes a job to the gateway key that created it. Every
  // authenticated request carries a prefix (gateway keys use the key prefix,
  // dashboard JWTs use a username-derived session prefix), so a non-empty
  // prefix always matches only its own jobs. The empty-prefix branch is a
  // defensive fallback for jobs written before the column existed.
  ```
- `/v1/models` `image_only` / `video_only` semantics in `openapi.yaml:571,573` match
  the implementation and the tests: `ImageOnlyFromModalities` treats
  `output:["image"]` as pure and `output:["text","image"]` as chat-capable
  (`internal/imagegen/capability_test.go:14-27`), and `rejectImageModelOnText`
  (`internal/proxy/images.go:486-499`) only refuses the former.
- The `X-Cache` header on `/v1/models` (`openapi.yaml:553-554`) is real
  (`internal/proxy/proxy.go:616` MISS, `:599` HIT).
- `openapi.yaml:4` `version: 1.7.0` matches `internal/handler/admin.go:67`
  `GatewayVersion = "1.7.0"`. OK.

---

## 7. Sources consulted

| Claim area | Files |
|---|---|
| Route registration, auth groups, SPA/bare-alias handling | `cmd/gateway/main.go:263-545`, `internal/handler/{admin,catalog,users,profile,org,webhooks,discovery,routing,oauth}.go`, `internal/middleware/middleware.go:182-300` |
| Image request/response contract | `internal/proxy/images.go` (full), `internal/imagegen/{imagegen,translate,registry}.go`, `internal/proxy/images_test.go` |
| Video request/response contract | `internal/proxy/videos.go` (full), `internal/videogen/{videogen,translate,registry}.go` |
| System One contract | `internal/proxy/systemone.go`, `internal/decision/{decision,registry}.go`, `internal/decision/decision_test.go` |
| Model listing fields | `internal/proxy/proxy.go:3568-3608, 3620-3830, 3855-3990` |
| Env config + sentinel | `internal/config/config.go:16-105, 84-89, 365-410`, `cmd/gateway/main.go:143-190`, internal/proxy/ttfb.go, `internal/proxy/fallback.go:327-346` |
| Migrations + dialect trap | `internal/db/migrations/{001_initial,026,027,028,029}.sql`, `internal/db/db.go:94-155`, `internal/db/pgsmoke/{main_test,capability_flags_test}.go`, `.github/workflows/ci.yml` |
| Deploy | `Dockerfile`, `docker-compose.yml`, `deploy/production/podman-compose.prod.yml`, `deploy/production/.env` (names only), `deploy/systemd/*` |
| Guards / tests | `internal/proxy/endpoint_guards_test.go`, `internal/imagegen/capability_test.go` |

## 8. Examples verified (actually executed)

- `go test ./internal/decision/ ./internal/imagegen/ ./internal/videogen/` → **all ok**
- `go test ./internal/proxy -run 'TestDecisionModel|TestImageOnly|TestImageCapable|TestWrongModelOnSystemOne|TestRealDecisionModel|TestNoRouteForIsCapabilityAware|TestImage'` → **ok**
- README request bodies fed to the real validators (temp program, compiled against the
  repo, since deleted): `systemone`, both image examples, both video examples → all
  **ok**. Negative probes: `seconds:"7"`, `size:"1080x1920"`, `n:11`, bad question id →
  the exact 400s quoted above.
- `yaml.safe_load(openapi.yaml)` → **parses** (85 paths, OpenAPI 3.0.3).
- `$ref` resolution (14 refs) → **0 unresolved**; duplicate-key scan → **none**.
- `openapi-spec-validator` on the file as committed → **INVALID** (2 classes of error,
  quoted above). Applying only the P1-1 + P1-2 fixes → **VALID**.
- `grep`/`python` cross-check of registered routes (main.go + handler `Routes()`)
  against `openapi.yaml` paths → new surface 16/16 documented; `GET /v1/models/{id}`
  the one gateway gap.

## 9. Unverified / not executed

- I did **not** run the stack end-to-end against a real provider. The smoke attempt
  used a copy of `data/gateway.db` and bound `:8799`; the binary I launched exited with
  `bind: address already in use` (an older instance was still listening on 8799 and
  answered `curl /v1/videos` with the SPA shell without auth), so **no runtime
  status-code claims in this report come from a live server** — they are read from the
  handlers. Claims about status codes are all backed by handler source lines.
- `openapi-spec-validator` was installed into `/tmp/oasvenv` (system Python is
  externally managed). No repo files changed.
- I did not verify provider-side wire compatibility (e.g. whether xAI really accepts
  `seconds` in its create body); that is the backend reviewer's remit.
- `README.md` "input tokens only" pricing for decision models was verified against
  `internal/decision/registry.go` comments and `internal/catalog/catalog.go:817-820`
  (`OutputCostPerM` stored, defaulting to 0) — but not against a live invoice.

---

## Verdict

**NEEDS WORK**

None of the blocking items are in the new endpoint *routing* — the 16 new routes are
correctly registered, documented, authenticated and capability-guarded — but the
published spec is not a valid OpenAPI document, the image-request schema misstates the
accepted vocabulary in three places, the System One request schema omits the required
`criteria` fields, the `IMAGE_TIMEOUT_SECONDS` documentation states a behaviour the
code does not implement, and ARCHITECTURE.md's migration range contradicts the document's
own video section.

### Blocking list

1. **P0 — `openapi.yaml` is not a valid OpenAPI 3.0.3 document** (served to clients at
   `/openapi.yaml`, `cmd/gateway/main.go:539`). `openapi-spec-validator` fails on
   `paths./api/lb/rules/{model}.put.responses.400` because of an unquoted comma
   (`openapi.yaml:1613`), then on undeclared `{id}` path parameters in
   `/api/providers/{id}/oauth/{status,refresh}` and the `DELETE` of
   `/api/providers/{id}/oauth` (`:1253, :1260, :1283`). Fix both classes (exact text in
   §3.2); the document then validates clean. Add `openapi-spec-validator` (or Redocly
   lint) to CI.

2. **P1 — `POST /v1/systemone` request schema omits required `criteria` for `choice`
   and `score`, misstates `noul` criteria keys, and does not document the question-id
   charset or the non-empty-`instructions` rule** (`openapi.yaml:478-516` vs
   `internal/decision/decision.go:169-183, 213-241, 260-262, 282-284`). A client generated from the spec
   produces requests the gateway rejects with 400. Replacement schema in §2.8.

3. **P1 — `IMAGE_TIMEOUT_SECONDS=0` is documented as "disables the budget" but
   resolves to the 300 s default.** `.env.example:51-54`, `internal/config/config.go:30`
   and `:84-89` all state the false behaviour; `cmd/gateway/main.go:184` logs the false
   claim at boot. Either fix the handler (`internal/proxy/images.go:151-156` / `internal/proxy/fallback.go:341`) so 0
   disables, or fix all four doc sites (§4 P1-3). Do not ship the docs as they stand.

4. **P1 — `ImageGenerationRequest.size` enum lists 6 values; the gateway accepts 12**
   (`openapi.yaml:1716-1719` vs `internal/imagegen/imagegen.go:143-156`). `2048x2048` works but is
   rejected by spec-driven clients. Same short list in `ImageEditRequest`
   (`openapi.yaml:1742-1745`). Exact enum in §2.2.

5. **P2 — `quality` and `resolution` are declared enums the server does not enforce**
   (`openapi.yaml:1720,1724`; probes: `quality:"ultra"`, `resolution:"4k"` both
   accepted). Document them as free-form provider passthrough, or enforce them (§2.3).

6. **P2 — `ImageEditRequest` cannot express what the server accepts.** `image` is
   documented as a single required binary; the server takes `image` *or* `images`, 1-4
   files, plus `mask` (`internal/proxy/images.go:310-370`, `internal/imagegen/imagegen.go:55`). Add `images` and the
   4-file cap to the schema (§2.4).

7. **P2 — ARCHITECTURE.md migration range is wrong by three migrations.**
   `ARCHITECTURE.md:49` and `:108` say `… 026_decision_models`; the embedded set ends at
   `029_video_models` (`internal/db/db.go:94-104`, `LatestMigrationVersion() = 29`), and
   `ARCHITECTURE.md:218` itself cites "migration 028" for `video_jobs`. Fix both
   (§4 P2-3), and add `video_jobs` + the `image`/`video` flags to the Data Model
   bullets (`:110-116`, §4 P2-4).

8. **P2 — `/v1/models` schema omits `decision`, which the docs promise.** The handler
   emits `decision: true` and `kind: "decision"` (`internal/proxy/proxy.go:3706-3707`) and both
   `ARCHITECTURE.md:167` and `README.md:152` advertise it, but `openapi.yaml:566-577`
   has no `decision` property (§2.12).

9. **P2 — `/v1/videos*` response codes are under-documented.** `409` (not ready /
   failed), `502` (provider gone, upstream failure), `500` (persist failure), `503`
   (no video provider) are all returned (`internal/proxy/videos.go:136, 151, 170-174, 206-211,
   234, 241, 247-258, 271-276, 299-302, 323-331`) and none is in the spec. Also
   document the verbatim upstream-error relay on create (`internal/proxy/videos.go:139-147`), which is
   the one place the `{error:{message,type}}` envelope is not used (§2.7, §2.10).

10. **P2 — `[OI]` appears 25 times in newly-added prose** (`README.md`,
    `ARCHITECTURE.md`, `openapi.yaml`) as an undefined stand-in for "OpenAI", including
    the published `summary` of `/v1/images/generations`. Replace with `OpenAI` (§4
    P2-7).

11. **P2 — ARCHITECTURE.md over-claims the pgsmoke guard.** "exercises every flag write
    path against a real Postgres" (`ARCHITECTURE.md:128-129`) is only true when `GATEWAY_PG_DSN` is
    set; CI never sets it (`.github/workflows/ci.yml:18-41`, `internal/db/pgsmoke/main_test.go:18-23`). Qualify the
    claim and add the CI service (§4 P2-5).

12. **P3 — Discoverability/nits:** `GET /v1/models/{id}` undocumented (§1.2);
    `IMAGE_TIMEOUT_SECONDS` missing from the README config table (§4 P2-2); README
    architecture diagram omits the new endpoints (§4 P3-1); API-key requirement for
    `cloudflare`/`google` providers not stated (`openapi.yaml:1687` vs
    `internal/provider/store.go:325`); `decision` `kind` precedence undocumented (§2.12);
    `deploy/systemd/README.md` has no upgrade/retention note for migrations 026-029 and
    the never-purged, never-enforced `video_jobs`/`expires_at` (§5.3);
    `videoJobVisible`'s "dashboard sessions see everything" comment is unreachable
    (`internal/proxy/videos.go:670-676`).
