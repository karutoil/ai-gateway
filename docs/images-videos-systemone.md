# Images, Videos and System One — operator & client notes

This page covers the three non-chat surfaces: `POST /v1/images/generations`,
`POST /v1/images/edits`, the async `/v1/videos` job API, and `POST /v1/systemone`.
Every claim here was read from the implementation
(`internal/proxy/{images,videos,systemone}.go`,
`internal/{imagegen,videogen,decision}/`, `internal/config/config.go`) — where the
code and the older prose disagreed, the code wins and the disagreement is noted.

Everything uses the same gateway key (`Authorization: Bearer sk-gw-…` or
`x-api-key`). Bare aliases without `/v1` exist for every route.

---

## Images — `POST /v1/images/generations`, `POST /v1/images/edits`

| Property | Behaviour | Source |
|---|---|---|
| Billing | Per generated image (curated `image_cost` × number of images returned) | `internal/proxy/images.go` `imageCost` |
| Cache | **Bypassed** — `X-Cache: BYPASS`, no lookup, no store | `proxyCandidatesOpts.noCache`, set in `images.go` |
| Retry / failover | **Disabled** for both endpoints — a billable create is never retried or failed over, so one request cannot bill two assets | `noRetry`, `noFailover` in `images.go` |
| First-byte budget | `IMAGE_TIMEOUT_SECONDS`, default **300s** (not the 85s chat budget) | `DefaultImageTTFB`, `config.ImageTimeoutSecs` |
| Response shape | OpenAI Images: `created`, `model`, `data[]`, optional `usage` | `internal/imagegen/imagegen.go` |

### Accepted `size` values (both endpoints)

```
auto  256x256  512x512  1024x1024  1536x1024  1024x1536
1792x1024  1024x1792  2048x2048  2048x1152  1152x2048  4096x4096
```

Anything else is a `400` naming the field. The set is `imagegen.validSizes`.

### Fields that are NOT validated

`quality`, `style`, `aspect_ratio` and `resolution` are stored verbatim and
passed through to the provider (OpenAI-compatible upstreams receive them as form
or JSON fields). The gateway has no vocabulary for them, so
`quality: "ultra"` and `resolution: "4k"` are accepted. Treat them as
provider-specific passthrough.

### Reference images (edits)

- The file part may be named `image` (repeat it) **or** `images`.
- At most **4** source files (`imagegen.MaxReferenceImages`); the 5th returns
  `400 image: too many reference images`.
- `mask` is optional.
- Zero source files is `400 image: at least one image file is required for edits`.
- The OpenAI Images edit API keys on `image` / `image[]` — this gateway accepts
  `image` and the `images` alias, which is the one naming difference an SDK user
  will hit.

For the JSON generation endpoint, `image` / `images` are reference *inline*
images: a data URL, a bare base64 string, or an object with
`url`/`b64_json`/`data`. Remote `http(s)` URLs are rejected because the gateway
does not fetch user URLs server-side.

---

## Videos — the async job API

`POST /v1/videos` → `GET /v1/videos/{id}` (poll) → `GET /v1/videos/{id}/content`
(download). `GET /v1/videos` lists, `DELETE /v1/videos/{id}` forgets.

### Read this before building a client

1. **Create is not the work.** `POST /v1/videos` enqueues upstream and returns a
   job in `queued` immediately. Nothing waits for the render.
2. **Polling is what advances a job.** The gateway runs no background poller:
   each `GET /v1/videos/{id}` on a live job is exactly one upstream poll (and
   updates the stored row); a `completed`/`failed` job is answered from the
   `video_jobs` table without touching the provider. A transient upstream poll
   failure serves the last known state as `200`, never an error.
3. **`409` means "not yet".** `/content` answers `409` while the job is
   `queued`/`in_progress` (message names the status) and `409` carrying the
   provider's error message for a `failed` job. Poll to a terminal status first.
4. **Billing happens at enqueue**, priced per generated second from the requested
   duration. A job you never poll — or one that later fails — is still billed,
   because the provider was already asked to render it.
5. **`DELETE` is not a cancel.** It removes the local row only; the provider's
   copy and its expiry are untouched, so deleting does not refund.

### Expiry, `410`, and why media is not cached

The gateway stores the provider's `expires_at` and **enforces it**:

- `GET /v1/videos/{id}` and `GET /v1/videos/{id}/content` both check expiry
  *first* (`videoJob.expired()`): a `completed` job whose `expires_at` has passed
  answers **`410 Gone`**.
- A provider `404`/`410` on a poll or on the content download is treated the same
  way (`failVideoJob(job, "expired", …)`).
- In both cases the row is rewritten to `failed` with `error.code: "expired"` and
  `error.message` naming the expiry time, so **every later request answers `410`**
  from the row and the provider is never contacted for it again. This is what
  stops an expired job from polling `200` forever.

**Generated media is never cached.** There is no media store and no
response-cache entry on any video route: `/content` re-proxies from the provider
on every call. Two consequences:

- The provider's retention policy is the real availability limit. Expiry
  enforcement above is driven by what the provider told us (or by its 404/410),
  not by a gateway-owned TTL.
- A content URL that is **not on the provider's own origin** is not fetched
  server-side at all — the provider credential must not travel to a third-party
  host. The gateway answers **`302`** with `Location` set plus
  `Cache-Control: no-store`, and the client fetches the bytes itself.

A content download is bounded by a first-byte budget and a per-chunk idle
watchdog, so a trickling provider cannot pin the connection (`504` when either
fires). A `409` means the job is simply not terminal yet (or failed); poll first.

### Ownership

Jobs created by a gateway key are scoped to that key: another key's id answers
`404` (never `403`, so the endpoint does not confirm that an id exists). A
**dashboard session** (`gw_token` / JWT) sees every job, and a job row whose
`key_prefix` is empty is not visible to gateway keys (only to a dashboard
session).

### Table growth — operator action required

Expiry is enforced for *clients*, but `video_jobs` rows are **never deleted**.
There is no sweep and no retention job, so every create adds a permanent row —
and those rows contain the `prompt` in plaintext and, for some dialects, a
signed `video_url`. A gateway serving video at volume grows its database without
bound.

Schedule your own prune, e.g. nightly:

```sql
-- SQLite
DELETE FROM video_jobs WHERE created_at < datetime('now','-30 days');
-- Postgres
DELETE FROM video_jobs WHERE created_at < now() - interval '30 days';
```

---

## System One — `POST /v1/systemone`

A gateway-defined protocol (TypeSafe Jev / Cloudflare Workers AI Clef, Kev,
llama.cpp), **not** an OpenAI API — there is no OpenAI SDK mode for it.

Validated locally before routing; every rule below is a `400` naming the field
(`internal/decision/decision.go`):

| Field | Rule |
|---|---|
| `model` | Required non-empty; must be a decision model |
| `state` | Required and not `null`; otherwise any JSON value |
| `questions` | Object, 1–64 entries |
| question id | 1–100 chars of `[A-Za-z0-9_.-]` — no spaces |
| `instructions` | Required; non-empty string, object or array (`""`, `{}`, `[]` rejected) |
| `criteria` — `choice` | **Required** object of 2–255 non-empty option keys → description |
| `criteria` — `score` | **Required** ordered array of 2–10 levels (strings or objects) |
| `criteria` — `noul` | Optional; only the keys `"true"` and `"false"` are permitted |
| `images` | At most 4; inline only (remote URLs rejected) |
| `audio`, `videos` | Forwarded unchecked — no gateway-side validation or limit |

Billing is **input tokens only**; `usage.output_tokens` is reported for shape
compatibility and is not billed. There is no streaming, and unlike the image
endpoints this route **does** use the exact-match response cache (default 10s
TTL): identical bodies are replayed as `X-Cache: HIT` with no upstream call and
`cost_usd=0`. Filtering and routing are bidirectional: decision providers never
receive chat traffic and text providers never receive `/v1/systemone` traffic.

---

## `IMAGE_TIMEOUT_SECONDS` — final semantics

```
unset, or -1   -> built-in default (300s)
0              -> budget DISABLED (no first-byte deadline)
N > 0          -> N seconds
```

This applies to `/v1/images/generations` and `/v1/images/edits` only; every other
endpoint keeps `TTFB_TIMEOUT_SECONDS` (default 85s). Parsing is signed
(`config.getIntSigned`) so `-1` is reachable from the environment.

> **Status (checked against the tree, not assumed):** the docs above — this page,
> `.env.example`, the README table and ARCHITECTURE — state the tri-state as the
> contract. `cmd/gateway/main.go` already maps `0` to "disabled", **but**
> `internal/proxy/images.go imageTTFB()` still returns `DefaultImageTTFB` for any
> value `<= 0`, so today `IMAGE_TIMEOUT_SECONDS=0` yields the 300s budget and the
> boot log claims it is disabled. The handler fix is owned by the backend work;
> until it lands, treat `0` as "300s" in practice. `-1`/unset and `N>0` behave as
> documented.

## Upgrade note (migrations 026–029)

| Migration | Adds |
|---|---|
| 026 | `decision` capability flag |
| 027 | `image` capability flag |
| 028 | `video_jobs` table |
| 029 | `video` capability flag |

`db.LatestMigrationVersion()` is **29**. All four are additive and safe on a live
database. Rolling *back* to a pre-026 binary against a 029 schema is not
supported: `schema_migrations` stays at 29 and the older binary will not undo it.

---

## Error shapes on this surface

All three endpoints answer the standard gateway envelope
`{"error":{"message":…,"type":…}}` — including a failed video **create**: the
provider's non-2xx body is parsed, credential-scrubbed, truncated to 4 KiB and
re-emitted as `{"error":{"message":…,"type":"upstream_error","code":…}}`. You get
the provider's own text, but never its raw HTML dump or anything
credential-shaped.

The one non-`{error:…}` body on the surface is the video **content** route's
`302` redirect (see above), which carries the media URL in `Location` and no
body.
