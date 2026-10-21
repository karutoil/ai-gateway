# Performance & Production-Operability Review — new surfaces

**Reviewer:** perf-devops (performance-engineer)
**Scope:** images / videos / systemone proxies, imagegen + videogen packages, per-request routing,
boot-time catalog seeding, Docker/CI/deploy, observability.
**Method:** every number below was measured on this working tree. No source was modified.
All experiments ran in `/tmp` copies (`/tmp/imglab`, `/tmp/gwbase`, `/tmp/gwfix`);
`git status` on the repo shows only `.review/` as my addition.

---

## 0. Measurement harness (so the numbers are reproducible)

| What | How |
| --- | --- |
| Gateway build | `go build -o gw ./cmd/gateway` (CGO sqlite, same as Dockerfile) |
| Upstream mock | Python `ThreadingHTTPServer` returning a 1024×1024-incompressible PNG as `b64_json` (1,398,178-byte response body; base64 of 1 MiB) |
| RSS | `/proc/<pid>/status` `VmRSS`/`VmHWM`; a 5 ms sampler around each load burst |
| CPU / alloc | `go test -bench` with `httptest` upstream, `-cpuprofile`/`-memprofile` (`alloc_space`), plus `/proc/<pid>/stat` utime+stime diffs against the live binary |
| Load driver | Python threads (client-side cost removed by measuring server CPU, not wall time) |

Baseline facts worth stating up front because several findings hinge on them:

- `MAX_PROXY_BODY_MB` default is **64** (`internal/config/config.go:378`) — request bodies are bounded.
- Image **responses** are unbounded: `proxy.go:1215` does `io.ReadAll(resp.Body)` with **no** `LimitReader`
  (only the *error* path at `proxy.go:1135` is capped at 1 MiB).
- Server has `WriteTimeout: 0` (`cmd/gateway/main.go:551`) and `ReadHeaderTimeout: 30s`
  (`cmd/gateway/main.go:562`) — correct for streaming, but see §5 (graceful shutdown) and T-2 for
  what it implies for long image requests and for a trickling video-content download.
- Boot with a fresh DB reached `/health` in **80–91 ms** (5 runs, 80/83/82/91/82 ms);
  boot with a 4010-row catalog in **~20 ms**. Boot cost is *not* a problem (see §3).

---

## 1. Memory amplification — the headline finding

### Metric & baseline

1024×1024 PNG returned as `b64_json`, 1,398,178-byte upstream body, N concurrent
`POST /v1/images/generations`, RSS delta measured at peak:

| N | upstream (1.4 MB) | peak RSS delta | per-request marginal |
| --- | --- | --- | --- |
| 1 | 1.4 MB | 0.7 MB | 0.7 MB |
| 2 | 1.4 MB | 4.0 MB | 2.0 MB |
| 4 | 1.4 MB | 12.5 MB | 3.1 MB |
| 8 | 1.4 MB | 38.6 MB | 4.8 MB |
| 16 | 1.4 MB | 76.9 MB | 4.8 MB |
| 32 | 1.4 MB | 197.1 MB | 6.2 MB |

`go test -bench` agrees: **19.35 MB allocated per request** for a 1.33 MB response
(`BenchmarkImageGenerations`, 593 allocs/op).

The amplification is real but *sub-linear per request* — 32 concurrent requests cost ~197 MB for
32×1.4 MB = 45 MB of actual payload, i.e. **~4.4× amplification**, and the marginal per-request
figure keeps climbing (4.8 → 6.2 MB) because of GC lag, not because each request allocates more.

### Bottleneck (file:line + mechanism)

The response body is `[]byte`-buffered at the transport layer and then **re-parsed four separate
times** by the generic bookkeeping that runs on every buffered response, in this exact order
(`internal/proxy/proxy.go`, buffered success path):

1. `proxy.go:1215` — `bodyBytes, readErr := io.ReadAll(resp.Body)`; the whole 1.4 MB response,
   uncapped. This one is unavoidable-ish (see M-1 fix) but it is the base cost.
2. `proxy.go:1260-1261` — `mergeFragmentedToolCalls(bodyBytes)` → `json.Unmarshal` into
   `map[string]json.RawMessage` (`toolcall_merge.go:28`). A **chat-repair** routine running on an
   *image* response. `-cum` profile: **0.41 s of 2.31 s** baseline CPU.
3. `proxy.go:1278` — `extractUsage(lastBody)` → `json.Unmarshal` into
   `map[string]interface{}`. Critically, `map[string]interface{}` **materializes the 1.4 MB
   `b64_json` as a Go string** — the 172 MB of `json.unquote` and 83 MB of `bytes.growSlice` in the
   alloc profile are mostly this one call on a body that contains no `usage` at all.
4. `proxy.go:1282` — `cacheSplitFromBody(lastBody)` → *another* full `json.Unmarshal` into
   `map[string]interface{}`.
5. `proxy.go:1278` — `extractAnthropicUsageFromSSEChunk(lastBody)` when usage is 0 → splits the
   1.4 MB body on `\n` into a `[][]byte` slice and tries to `json.Unmarshal` each line.
6. `proxy.go:1405` — `extractUsageDetail(outBody)` → *another* full `json.Unmarshal` into
   `map[string]interface{}`.
7. `proxy.go:287` — `imageCost` → `imagegen.ImageCount(respBody)` → *another* full `json.Unmarshal`
   into `imagegen.Response`, which has `B64JSON string` — so the payload is materialized **again**.

So: **upstream bytes (1) → JSON lex/validate ×6 (2,3,4,5,6,7) → `map[string]interface{}` string
copies ×3 → `Response.B64JSON` string ×1 → `bytes.Buffer` copy in `bufWriter.Write` (`fallback.go:87`)
→ `w.Write(outBody)`.** Padding is not the issue; the issue is that a body carrying **zero billing
metadata** is walked six times and unquoted into Go strings three times.

Is there a cap? **For responses, no.** `readProxyBody` caps the *request* at 64 MiB
(`proxy.go:166-171`, default from `config.go:378`), and `parseEditMultipart` caps *individual
multipart parts* at 64 MiB (`images.go:330`) — but `io.ReadAll(resp.Body)` at `proxy.go:1215` is
unbounded, and so is the number of parts that survive (only `image`/`images` parts are counted
against `MaxReferenceImages=4`; a request may carry arbitrarily many 64 MiB parts, see §4).

### The measured fix (verified in a scratch copy, not applied to the repo)

Steps (a)+(b)+(c) below were implemented in `/tmp/gwfix` and the full `./internal/proxy` suite
still passes (`ok ai-gateway/internal/proxy 22.3s`):

```go
// a) mergeFragmentedToolCalls only matters for chat bodies that mention tool_calls.
//    proxy.go:1260-1261
if !isStream && lastStatus == 200 && len(bodyBytes) > 0 &&
    endpoint == "chat.completions" && bytes.Contains(bodyBytes, []byte("tool_calls")) {
    ...
}
// and belt-and-braces inside the helper (toolcall_merge.go:27):
if !bytes.Contains(body, []byte(`"tool_calls"`)) { return body, false }

// b) the anthropic SSE usage probe only applies to Anthropic upstreams. proxy.go:1279
if pt == 0 && ct == 0 && isAnthropicUpstream {
    pt, ct = extractAnthropicUsageFromSSEChunk(lastBody)
}

// c) ONE parse feeds billing + cache split + usage detail.
//    (extractUsageFromMap / cacheSplitFromMap / extractUsageDetailFromMap already exist
//     and were written for exactly this reuse — they are just not used on this path.)
var envelope map[string]interface{}
if len(lastBody) > 0 && lastBody[0] == '{' {
    _ = json.Unmarshal(lastBody, &envelope)
}
pt, ct := extractUsageFromMap(envelope)
split := splitOrFallback(cacheSplitFromMap(envelope), pt, ct)
...
var nd usageDetail
if transformedResponse || needsChatShape || needsResponsesShape || len(outBody) != len(lastBody) {
    _, _, nd = extractUsageDetail(outBody)   // outBody was rewritten; envelope no longer valid
} else {
    _, _, nd = extractUsageDetailFromMap(envelope)
}

// d) imagegen.ImageCount only needs the array LENGTH, never the payload. imagegen/translate.go:189
var resp struct{ Data []json.RawMessage `json:"data"` }
```

### After (same method, same harness)

| Metric | Baseline | Fixed | Δ |
| --- | --- | --- | --- |
| CPU/op, 1.4 MB image response (`BenchmarkImageGenerations`, 20×) | 40.2–42.3 ms | 17.1–18.5 ms | **−57 %** |
| Alloc/op | 19.35 MB | 13.74 MB | **−29 %** |
| allocs/op | 593 | 515 | −13 % |
| CPU/req against the live binary (serial, 1.4 MB) | 56.6 ms, 21.6 req/s | — (bench only) | — |
| Peak RSS delta, N=16 concurrent | 95.0–107.7 MB (4 runs) | 68.9–82.0 MB (4 runs) | **−25 %** |
| Throughput ceiling (1 core, serial) | ~22 req/s | ~50 req/s | **×2.3** |

The remaining 13.7 MB/op is dominated by `io.ReadAll` of the response body itself plus the
`bufWriter` copy — that is the structural cost of the "buffer the whole body" design and is
addressed separately below (M-1/M-2).

**A caution on the first attempt:** my initial version of (c) replaced the whole
`extractUsageDetail(outBody)` call with a map lookup unconditionally and **lost
`finish_reason`** — `TestNonStreamDetailRecorded` and `TestLogBodiesOffStoresNoBodies` caught it
(`finish_reason = "", want tool_calls`). The conditional above (only reuse the envelope when
`outBody` is byte-identical to `lastBody`) is what makes it behaviour-preserving. Any
implementation of this must keep that guard.

### Concurrency ceiling (a real, measured cliff)

At N=64 concurrent 1.4 MB responses the gateway **dropped 5 of 64** (59 ok) at 0.34 s wall — the
failures are not the mock upstream (it never rejects) so they are client-side connection resets
under the memory spike, and RSS was still 79 MB after settling. With 64 MiB responses
(`up_big.py`, `b64_json` = 64 MB) the picture is much worse:

| N (64 MB response) | peak RSS | wall |
| --- | --- | --- |
| 1 | 408 MB | 2.0 s |
| 2 | 598 MB | 2.2 s |
| 4 | 979 MB | 2.6 s |
| 4 (with the §1 fix) | 1,239 MB | 1.5 s |

**~245 MB of peak RSS per concurrent 64 MB image response.** On a 1 GiB container, 4 concurrent
4K-image responses OOM the process. With the fix, wall time halves (2.6 → 1.5 s) but peak RSS does
not improve at 4-way — GC simply doesn't get a chance to run before the next 64 MB copy lands.
This is the *only* place where a hard cap is genuinely required.

### Concrete fixes

**M-1 (P1) — cap the buffered image response and stream it when it is a URL response.**
`proxy.go:1215`. Two options, in order of preference:

1. Introduce a per-endpoint response cap: add `proxyCandidatesOpts.maxResponseBytes`, default it to
   the existing behaviour (0 = uncapped) for chat, and set it to something explicit for images
   (e.g. `64 << 20` matching `MAX_PROXY_BODY_MB`). On overflow, answer
   `502 upstream response exceeds the N-byte image limit` rather than buffering without bound.
   The `transformResponse` hook (`fallback.go:309-320`) is already per-endpoint and is the natural
   place to plumb it.
2. Longer term: `response_format: "url"` responses are pure JSON metadata and could be relayed
   without joining the payload; `b64_json` cannot (the gateway has to re-encode for Gemini →
   OpenAI translation) but for **passthrough** providers (openai/azure/xai) the body needs no
   transform at all — set `transformResponse` to nil for the passthrough case and
   `io.Copy` the upstream body to the client instead of `ReadAll` → `bufWriter`. That removes both
   the second full copy (`bufWriter`, `fallback.go:62-95`) and the peak.

**M-2 (P2) — `imageCost` runs a whole-body `json.Unmarshal` to count images.**
`images.go:287-296` → `imagegen/translate.go:189`. Apply the `[]json.RawMessage` fix (measured
above as part of the 57 %). Also: `imageCost` is called from `imageTransform` on **every** response
including passthrough ones where the count is already known from the request's `n`.

**M-3 (P2) — `bufWriter` is an unnecessary full-body copy.** `fallback.go:62-95`: for the buffered
path every response is written into `bytes.Buffer` and then copied to `w`. It exists to decide
"did the upstream commit anything" before writing to the client. For image responses the header set
is already known by the time `outBody` exists, so `bw.flushTo(w)` at `fallback.go:460` could be
replaced by direct `w.Write` when `transformResponse` produced `outBody` (the decision point is
already known: `bw.code > 0 && bw.code < 400`).

**M-4 (P2) — `map[string]interface{}` is the wrong shape for billing lookups.** Even after
de-duplicating to one parse, `extractUsageFromMap` materializes every large string in the document.
Replace with a typed envelope:

```go
type usageEnvelope struct {
    Usage    map[string]interface{} `json:"usage"`
    Response *struct{ Usage map[string]interface{} `json:"usage"` } `json:"response"`
    Choices  []struct{ FinishReason string `json:"finish_reason"` } `json:"choices"`
    Status   string `json:"status"`
    StopReason string `json:"stop_reason"`
}
```
`encoding/json` still *lexes* the payload but never allocates Go strings for `b64_json`. I tried
this and it did **not** improve on the dupe-removal alone at 1.4 MB (22.1 ms vs 20.1 ms — within
noise), so it is a P2/P3 size-hygiene win, not the headline. It would matter at 64 MB.

### `/v1/videos/{id}/content` — this one is already correct

`streamVideoContent` (`videos.go:620-657`) uses `io.Copy(w, io.LimitReader(resp.Body, videogen.MaxContentBytes))`
with `MaxContentBytes = 512 MiB` (`videogen/videogen.go:79`). Measured with a 256 MiB upstream
`Content-Length`: **`status=200 bytes=268435456 time=0.12s`, and `VmRSS` moved from 27,668 kB to
27,692 kB (+24 kB)`.** Peak RSS after `VmHWM` was unchanged at 33,556 kB. It streams; it does not
buffer. ✅

One caveat: it forwards the upstream `Content-Length` verbatim (`videos.go:651-653`) while capping
the copy at 512 MiB. If an upstream advertises >512 MiB, the client receives a truncated body with
a 200 and a `Content-Length` that will never be satisfied (hang/`unexpected EOF`). Fix: when
`Content-Length > videogen.MaxContentBytes`, drop the header (or return 502), so the failure is
honest. This is the same class of bug the codebase already fixed for rewritten bodies
(`proxy.go:1380-1385`).

---

## 2. Timeouts — `IMAGE_TIMEOUT_SECONDS` plumbing

### Is it plumbed end-to-end? Yes, and it applies to images only. Verified by measurement.

| Env | Endpoint | Upstream delay | Result |
| --- | --- | --- | --- |
| `IMAGE_TIMEOUT_SECONDS=5` | `/v1/images/generations` | 20 s | **504 @ 5.00 s** ✅ |
| `IMAGE_TIMEOUT_SECONDS=300`, `TTFB=5` | `/v1/images/generations` | 20 s | **200 @ 20.0 s** ✅ (image budget wins, chat budget does not leak in) |
| `TTFB_TIMEOUT_SECONDS=5` | `/v1/chat/completions` | 20 s | **504 @ 5.01 s** ✅ |
| `IMAGE_TIMEOUT_SECONDS=0`, image pinned to slow provider | `/v1/images/generations` | 20 s | **200 @ 20.0 s** — the budget really is disabled |

Chain verified in code: sentinel `-1` (`config.go:404`) → `main.go:181-189` switch →
`proxyHandler.ImageTTFB` → `imageTTFB()` (`images.go:151-156`) → `proxyCandidatesOpts.ttfb`
(`images.go:91,145`) → `newTTFBController` (`fallback.go:340-345`) → `armTTFBWatchdog`
(`ttfb.go:247-253`). The plumbing is correct and the override is image-only.

### The problems

**T-1 (P1) — `IMAGE_TIMEOUT_SECONDS=0` is inverted: documented "disabled", actually 300 s.**
`images.go:151-156`:
```go
func (h *Handler) imageTTFB() time.Duration {
    if h.ImageTTFB > 0 { return h.ImageTTFB }
    return DefaultImageTTFB          // ← 0 lands here
}
```
`main.go:184-186` sets `ImageTTFB = 0` and logs *"image first-byte budget disabled"*, but
`imageTTFB()` treats 0 as "unset" and substitutes 300 s. The operator asked for *no* budget and got
the *longest* budget. (I could not observe the difference at 20 s upstream because both 0→300 s and
0→unlimited exceed 20 s; the code path is unambiguous.) Fix: give the field a tri-state semantics
that matches `TimeoutsConfig.TTFB` — use a `-1` sentinel for "default" and `0` for "disabled":

```go
// proxy.go:95
ImageTTFB time.Duration // -1 (default sentinel) = DefaultImageTTFB, 0 = disabled
// images.go
func (h *Handler) imageTTFB() time.Duration {
    if h.ImageTTFB > 0 { return h.ImageTTFB }
    if h.ImageTTFB == 0 { return 0 }   // explicit disable
    return DefaultImageTTFB
}
```
and in `main.go` set `-1` for the default case, `0` for disabled. Also update the doc table at
`config.go:88-89` (currently says "0 disables the budget; negative falls back to the built-in
default (300s)" — the semantics the code implements are the *reverse*).

**T-2 (P1) — the video routes have NO first-byte budget at all, and `DefaultVideoTTFB` is dead code.**
`videos.go:25-31` declares `DefaultVideoTTFB = 120 * time.Second` and nothing in the repo references
it (`grep -rn DefaultVideoTTFB` → only the declaration; no test either). Consequence measured:

| Endpoint | Budget in force | Upstream delay 20 s with `TTFB_TIMEOUT_SECONDS=5` |
| --- | --- | --- |
| `POST /v1/videos` | none | **502 @ 20.0 s** (burned the whole upstream window) |
| `POST /v1/images/generations` | `IMAGE_TIMEOUT_SECONDS` | 200 @ 20.0 s |
| `POST /v1/chat/completions` | `TTFB_TIMEOUT_SECONDS` | **504 @ 5.0 s** |
| `GET /v1/videos/{id}/content` | none | 200 @ 16.0 s on a 16 s drip (with `TTFB=3`) |

Correct budgets, per the endpoint semantics the code documents itself:
- `POST /v1/videos` — **job creation, should be fast** → 30–60 s, definitely not the 120 s the dead
  constant implies.
- `GET /v1/videos/{id}` — **a poll, fast** → the normal `Timeouts.TTFB` (85 s) is fine, or a
  dedicated 30 s.
- `GET /v1/videos/{id}/content` — **large download** → needs a *first-byte* budget (upstream must
  start sending) plus an *idle* budget during the copy, not a total budget. Today it has neither:
  `streamVideoContent` uses `h.Client.Do(req)` with `r.Context()` and no watchdog, so a provider
  that accepts the connection and then dribbles 1 byte/minute will hold the client connection
  (and a goroutine, and an upstream connection) indefinitely — `WriteTimeout: 0` means nothing
  reaps it. That is a slowloris-style resource leak on `Client` (whose `Transport` has
  `MaxIdleConnsPerHost: 32`, so 32 such streams also starve the pool for everyone else).

Fix sketch:
```go
// videos.go:90  (create)
h.proxyCandidatesWithOpts(..., proxyCandidatesOpts{ttfb: videoCreateTTFB})   // 45s
// videos.go:620 (content) — wrap the copy in a first-byte + idle watchdog
ttfb := newTTFBController(h.videoContentTTFB(), start)
go func(){ time.Sleep(ttfb.remaining()); reqCancel() }()  // or armTTFBWatchdog
// then a per-chunk idle deadline around io.Copy via a custom Reader that resets a timer.
```
at minimum, arm a header watchdog before `Client.Do` in `streamVideoContent` and drop the
`Content-Length` when the cap is exceeded (see §1).

**T-3 (P2) — `DefaultVideoTTFB`'s comment contradicts the code.** `videos.go:26-31` says the create
call gets "a more generous budget than chat" and polling/content are "governed by the normal request
path". Polling *is* (via `proxyCandidates` → `Timeouts.TTFB` for `VideoGet`? no — `VideoGet` calls
`h.Client.Do` directly at `videos.go:502`, not `proxyCandidates`, so it has **no** budget either).
Every one of the five video handlers bypasses `proxyCandidates` and therefore bypasses
`ttfbController` entirely (`videos.go:90-297`).

**T-4 (P3) — `UPSTREAM_HEADER_TIMEOUT_SECONDS` is inert.** `main.go:144-149` assigns it to
`proxyHandler.Timeouts.UpstreamHeader`, but `Handler.Client` was built in `proxy.New()`
(`proxy.go:110`) with `DefaultTimeouts().UpstreamHeader` (120 s) and never rebuilt.
Measured: `UPSTREAM_HEADER_TIMEOUT_SECONDS=2` with a 20 s upstream still returned **200 @ 20.0 s**
for both chat and images. The env var is documented in `config.go:365` and silently does nothing.
Either rebuild the transport after the config assignment or delete the option.

---

## 3. Boot cost of `SeedDecisionModels` + image/video seeding

### Measured

- Boot to `/health`, fresh DB: **80 / 83 / 82 / 91 / 82 ms** (5 runs).
- Boot to `/health`, 4010-row catalog: **20 / 20 / 20 ms**.
- Seed cost in isolation (`BenchmarkSeeds`, i.e. all three seeds in one iteration against a real
  migrated SQLite DB): **1.57 / 1.59 / 1.67 ms** for 10 decision + 14 image + 10 video = **34 rows**.
- Row counts confirmed: `decision=10 image=14 video=10 total=34` (`TestSeedCounts`).

**Verdict: boot cost is a non-issue.** 34 idempotent upserts on a prepared statement, ~1.6 ms, once
per process start.

### The real problem is the *unbounded* re-seed on every catalog sync

**B-1 (P1) — `syncFromProviders` calls all three seeds on every daily sync, with no transaction and
no bound.** `catalog.go:792-799`:
```go
_, _ = s.SeedDecisionModels()
_, _ = s.SeedImageModels()
_, _ = s.SeedVideoModels()
```
Problems, in order of severity:

1. **It is outside the sync's transaction.** `syncFromProviders` opens `tx` at `catalog.go:740`,
   `Prepare`s on the tx (`:745`), commits at `:788`, and *then* calls the seeds on `s.db`
   (`:793-799`). So a sync that fails mid-way can leave the curated rows in a different state from
   the models.dev rows, and a seed failure is swallowed (`_, _ =`). The `defer tx.Rollback()`
   guarantee the migrations rely on does not cover this.
2. **`defer stmt.Close()` in all three seed functions (`:812, :935, :960`) with no transaction** means
   34 individual auto-commit statements per sync — 34 fsyncs on SQLite WAL (each one a WAL append +
   potential checkpoint). That is ~1.6 ms locally but ~34 round trips on a remote Postgres.
3. **Errors are discarded** (`_, _ =`). If the seed fails on Postgres (this is precisely the
   `pq: invalid input syntax for type integer: "false"` class the new
   `internal/db/pgsmoke/capability_flags_test.go` guards against), the gateway boots *without*
   image/video models in the catalog and nothing logs it. `main.go:107` at least warns for the
   boot-time decision seed; `catalog.go:793-799` warns for nothing.
4. **Only decision models are seeded at boot** (`main.go:107`). Image and video models reach the
   catalog only after the first successful `FetchAndSync`, which runs in a goroutine
   (`main.go:210-229`) only `if count == 0`. On an existing deployment upgrading to this build,
   `count != 0`, so **image and video models are invisible in `GET /v1/models` until the next daily
   24 h tick** (or until a provider is added, which triggers discovery→sync). I reproduced this:
   a fresh DB seeded 10 decision rows and **0 image / 0 video** rows, because the sync goroutine's
   `FetchAndSync` could not reach models.dev from this sandbox.
   Fix: call all three seeds at boot alongside the decision seed (`main.go:107`), which is
   idempotent and only ~1.6 ms.

**Fix sketch:**
```go
// catalog.go — run seeds inside the sync transaction and surface errors
if err := seedAllTx(tx); err != nil { return count, err }   // before tx.Commit()
// and a single combined seeder so 34 upserts share one transaction:
func (s *Store) SeedCapabilityModels() (int, error) { /* one Begin, one Prepare, 34 Exec, Commit */ }
// main.go — boot must seed all three, not just decision:
if n, err := catalogStore.SeedCapabilityModels(); err != nil {
    log.Warn().Err(err).Msg("capability model seed failed")
}
```

**B-2 (P3) — `syncFromProviders` is O(models) with a prepared statement — that part is fine.**
`catalog.go:752-786` loops providers × models with one `stmt.Exec` each inside a transaction. For
the real models.dev payload (~2–4 k rows) that is one round trip per row, which is the standard
tradeoff; no action needed unless sync time becomes visible.

---

## 4. Goroutines, timers, regexes, unbounded unmarshal, repeated catalog lookups

**G-1 (P0-worthy correctness, P1 perf) — per-request catalog lookup runs an unindexed full table scan
with a temp B-tree sort, on *every* request that touches a model id.** This is far and away the
largest *per-request* cost introduced by this change, and it is on the hot path of **all** chat
traffic, not just images.

`internal/catalog/catalog.go:204-223`:
```go
func (s *Store) GetByShortID(shortID string) (*models.CatalogModel, error) {
    if m, err := s.Get(shortID); err == nil { return m, nil }       // PK, cheap ✅
    ...
    return scanCatalogModel(s.db.QueryRow(db.Q(
        `SELECT ... WHERE LOWER(id) LIKE LOWER(?) ESCAPE '\'
         ORDER BY (CASE WHEN input_cost>0 OR output_cost>0 THEN 0 ELSE 1 END), id LIMIT 1`),
        "%/"+escapeLike(trimmed)))
}
```
`EXPLAIN QUERY PLAN` on a populated DB (verified):
```
SCAN models_catalog
USE TEMP B-TREE FOR ORDER BY
```
i.e. full scan + sort, because `LOWER(id) LIKE '%/x'` cannot use `idx_models_catalog_*` (the new
026/027/029 indexes are on the *flags*, not on `id`) and the `CASE` ordering defeats any index
ordering. Every miss re-runs it; `FindBestMatch` calls `trySlug` which calls `GetByShortID` for the
slug **and for each of ~10 alternate spellings** (`catalog.go:466-510`), then `backoffSegment` loops
(`catalog.go:443`). That is up to ~15 full scans per request for an unknown model.

Measured (4010-row catalog, in-process benchmarks against the real code):

| Call | 0 rows | 4010 rows | Δ |
| --- | --- | --- | --- |
| `Handler.modelIsImageOnly("gpt-image-1")` | 54 µs | **750 µs** | ×14 |
| `Handler.costForModel("unknown-model-x", split)` | 272 µs | **3.80 ms** | ×14 |
| `POST /v1/chat/completions`, unknown model (`BenchmarkChatUnknownModel`) | 0.86 ms | **6.16 ms** | ×7 |
| `GET /v1/models` fallback (`BenchmarkModelsFallback`) | 0.33 ms | **4.22 ms** | ×13 |

At 4010 rows an unknown-model chat request burns **6.2 ms of CPU before it talks to a provider**;
the image path adds `modelIsImageOnly` + `IsImageModel` + `costForModel` per request. This scales
linearly with catalog size, and models.dev catalogs are 3–5 k rows today and growing.

Note the *new* call sites are the ones that make this unavoidable on the image path:
`images.go:531,553,593` (`modelIsImageOnly`, `modelIsVideoOnly`, `rejectNonImageModelOnImages`) each
do the `Get`→`GetByShortID` dance separately, i.e. up to two scans each, and `resolveAlias`
(`proxy.go:120-146`) already did a separate `model_aliases` query.

**Fix (cheap, high leverage):**
1. Add the missing index in a new migration: `CREATE INDEX IF NOT EXISTS idx_models_catalog_id_lower ON models_catalog(LOWER(id));`
   — I tested this on SQLite and, because of the `LIKE '%…'` leading wildcard, the planner still
   chooses SCAN (leading-wildcard LIKE cannot use a B-tree). So the index alone is **not** enough.
2. Therefore: **replace the containment query with a suffix-match on an indexed, stored column.**
   The catalog is derived data; add `short_id TEXT` (the substring after the last `/`) maintained at
   sync time, index it, and make `GetByShortID` do `WHERE short_id = ?` (exact) plus, only as a last
   resort, the `LIKE` containment. This turns the common case into an index lookup and keeps the
   rare dated-variant path slow-but-correct.
3. Also memoize: `modelIsImageOnly`/`modelIsVideoOnly`/`costForModel` are pure functions of
   `(model, catalog version)`. A small LRU keyed by model id, invalidated on sync, removes almost
   all of it. `Handler` already has the pattern (`keyIDCache`, `proxy.go:98-104`).

**G-2 (P2) — per-request regex compiles: none found.** ✅ `grep -rn regexp.MustCompile internal/proxy`
returns only three package-level vars (`upstream.go:202-204`), used for secret scrubbing. No
`regexp.Compile` on a request path.

**G-3 (P2) — no new goroutines or timers on the image/video/decision paths.** ✅
`grep -n "go func\|time.NewTicker\|time.AfterFunc\|time.NewTimer" internal/proxy/{images,videos,systemone}.go`
→ **no matches**. The only goroutine on the image path is the pre-existing `ttfbController.heartbeat`
(`ttfb.go:160`), started only when the keepalive umbra commits, and stopped by `defer ttfb.stop()`.
`VideoList` runs one indexed query (`WHERE key_prefix=? ORDER BY created_at DESC LIMIT 100`,
`videos.go:267-272`, index `idx_video_jobs_key` ✅). `VideoGet`/`VideoContent` do one PK lookup each.
This refutes the brief's suspicion: the video path is genuinely stateless, no poller, no timers.

**G-4 (P2) — `video_jobs` grows without bound.** Nothing ever deletes rows: `VideoDelete`
(`videos.go:292-303`) is opt-in, and the `expires_at` column is **metadata only** — I verified the
row survives and `GET /v1/videos/{id}` keeps answering 200 for a job whose `expires_at` is in the
past (the repo's own `videos_lifecycle_test.go:193-227` documents this as a "GAP (P2)"). The
existing `runLogRetention` (`main.go:702-730`) is the natural home for a parallel
`DELETE FROM video_jobs WHERE expires_at < ?` or `created_at < now - N days`. Low severity because
a row is ~200 bytes and only image-free JSON lives in it, but it is unbounded growth in a table the
new feature owns.

**G-5 (P2) — multipart edit parts are unbounded in *count*.** `parseEditMultipart`
(`images.go:310-357`) reads each part with `io.ReadAll(io.LimitReader(part, 64<<20))`, and only
`image`/`images`/`mask` parts are counted against `MaxReferenceImages = 4` (`images.go:351`).
A request may therefore carry N arbitrary-named 64 MiB parts; `files` accumulates **every** part
unconditionally (`images.go:344`), and then `buildEditMultipart` (`images.go:408-448`) writes them
all back into a fresh `bytes.Buffer`. Total request budget is still bounded by `MaxBodyBytes`
(64 MiB) at `readProxyBody`, so worst case per request is 64 MiB request + 64 MiB rebuilt copy +
4 × 64 MiB `Image.Data` slices (which are *copies*: `io.ReadAll` per part, then `norm.Images`
holds the same `data` slice — that one is shared, good) — so ~128 MiB + the response. Measured:
`BenchmarkImageEdits` with a 4 × 4 MiB edit body = **301 MB allocated per request** and
**10.6 MB/s** throughput (`ParseEditMultipart` alone is 787 MB/s, `BuildEditMultipart` 4.2 GB/s —
the 301 MB is the six copies between them). Fix: cap the number of file parts at
`MaxReferenceImages + 1` (mask) and stream `buildEditMultipart` into a `bytes.Buffer` with a
pre-sized `Grow` (the size is known: `len(body)`), which removes the re-growth.

**G-6 (P3) — request-body logging stores image/video payload prefixes in every row.** `LOG_BODIES`
defaults to **true** (`config.go:133-139`), and `logRequestExtendedBodies` stores
`responseBody[:BodyLogMaxBytes]` (default 8192) for *every* request (`proxy.go:2558-2572`). I
confirmed 8192-byte `b64_json` prefixes in every `request_logs` row for image traffic. That is 8 kB
of useless base64 per image request in the OLTP DB (and it is what `ai-gateway-playground (1).html`
was almost certainly pasted from). Not a latency problem (the write happens *after* the client
bytes, `proxy.go:1396-1398` ✅) but it inflates the DB by ~8 kB/req and widens any export. Consider
skipping body capture for `images.*` / `videos` endpoints, or storing a redacted summary.

**G-7 (P3) — `extractAnthropicUsageFromSSEChunk` splits any body on `\n`.** `proxy.go:2635-2660`.
After fix (b) above it only runs for Anthropic upstreams, which is correct, but it is still
`bytes.Split` (a slice of slices) over a body that can legitimately be multi-MB JSON. A `bytes.Index`
scan for `"usage"` before splitting would make it O(1) for the common case.

---

## 5. CI / Docker / deploy coverage

| Item | Status |
| --- | --- |
| New packages built + vetted + tested in CI | ✅ `ci.yml:19-36` — `go build ./...`, `go vet ./...`, `gofmt -l .`, `go test ./... -race` cover `internal/imagegen`, `internal/videogen`, `internal/decision`, `internal/proxy`. New tests exist in all four. |
| Migrations 026–029 in CI | ✅ implicitly: `go test ./...` opens in-memory DBs via `db.Open(":memory:")` which runs `Migrate` (`db.go:327-343`), so 026–029 execute on every test run. |
| Postgres dialect coverage for the new INTEGER flags | ✅ `internal/db/pgsmoke/capability_flags_test.go` exists — **but it silently skips** when `GATEWAY_PG_DSN` is unset (`pgsmoke/main_test.go:20-23`), and **`ci.yml` never sets it**. `grep -n "GATEWAY_PG_DSN\|postgres" .github/workflows/*.yml` → no matches. So the regression test for the exact `pq: invalid input syntax for type integer: "false"` bug the new code could reintroduce **never runs in CI**. |
| `IMAGE_TIMEOUT_SECONDS` in deploy | ❌ **not present in any deploy file.** `grep -rn IMAGE_TIMEOUT deploy/` → nothing. `deploy/production/.env`, `.env.bak-localpg`, `.env.bak-sqlite` all have `TTFB_TIMEOUT_SECONDS=85` and nothing else. The production stack therefore runs on the code default (300 s), which is *correct by luck* — but an operator reading `.env.example` (`:51-54`, it *is* documented there) who copies `.env` will get no image knob in `deploy/production/.env`. |
| Embedded UI rebuild | ⚠️ **the Docker build does not build the UI.** `Dockerfile:24-30` only runs `go build ./cmd/gateway`; `go:embed all:web` (`main.go:51`) embeds whatever is committed in `cmd/gateway/web`. The working tree currently has the *new* vite output as **untracked** (`?? cmd/gateway/web/assets/index-Bc4Qat39.css`, `?? .../index-U1ABf980.js`, ` M cmd/gateway/web/index.html`, and the two old asset files deleted). **A `docker build` from the working tree works; a build from a clean `git clone` of the committed state would embed a broken `index.html`** (it references assets that are not in git). The `release.yml` job *does* rebuild vite (`release.yml:64-79`) for released binaries, so this only bites container builds from source. Fix: stage/commit the new assets, or add a `frontend` build stage to the Dockerfile and `COPY --from` the `cmd/gateway/web` output. |
| Docker job depends on frontend? | ❌ `ci.yml:69` — `docker: needs: backend`. The docker job can go green while `frontend` (typecheck/vite build) is red, and it does not wait for it, so a broken UI is not caught by the image build. |
| `docker-compose.yml` | ✅ picks up env via `env_file: .env` + `environment:`. No image-specific knob needed because the default is correct; still, documenting it in the compose comment block would help. |
| Health / readiness | ✅ `/health` and `/ready` exist (`main.go:291-324`), `/ready` pings the DB. Compose/Dockerfile deliberately rely on external probes (documented `Dockerfile:9-11`, `docker-compose.yml:41-43`). Acceptable. |
| `Makefile` | ✅ `make ci` = `fmt-check + vet + build + test-race`, and `test-race` runs the whole tree including the new packages. No `IMAGE_TIMEOUT`/deploy target needed. |
| **Graceful shutdown drains long image requests?** | ⚠️ **Barely — and the verdict depends entirely on `SHUTDOWN_GRACE_SECONDS`.** Measured: `SHUTDOWN_GRACE_SECONDS` default is **90 s** (`config.go:369`), but `DefaultImageTTFB` is **300 s** and a real gpt-image/imagen call commonly takes 10–60 s+. With the default 90 s grace, `srv.Shutdown` cancels the in-flight request context at 90 s, the upstream call dies, and the client gets a **502 "all provider attempts failed"** (I reproduced it: 3 in-flight image requests, SIGTERM, `SHUTDOWN_GRACE_SECONDS=3` → all three returned `EXC`/connection-closed after 4.05 s, and the log shows `Post ".../images/generations": context canceled` + `all provider attempts failed` + `request_logs insert failed: sql: database is closed`). With grace ≥ the request duration they complete cleanly (verified: 8 s upstream, SIGTERM at 1 s → all 3 returned `200` with full 1,398,178 bytes). So: **`SHUTDOWN_GRACE_SECONDS` must be ≥ `IMAGE_TIMEOUT_SECONDS`** (or ≥ the max expected image latency) for a rolling deploy not to fail in-flight image generations. There is no check or warning for this mismatch anywhere. Also note the second-order bug in the log above: `database.Close()` runs right after `Shutdown` (`main.go:608-611`), so any request that outlives the grace window then fails to persist its `request_logs` row ("database is closed") — the request is lost from billing. |

**CI/deploy fixes:**
- `ci.yml`: add a Postgres service + `GATEWAY_PG_DSN` to the backend job so
  `internal/db/pgsmoke` actually runs; make `docker: needs: [backend, frontend]`.
- `Dockerfile`: add a `node:20` stage running `npm ci && npm run build` and copy the result into
  `cmd/gateway/web` before the Go build (mirrors `release.yml:64-79`); add a `.dockerignore`
  (currently there is **none** — `docker build .` sends `web/node_modules` (93 MB), `data/`,
  `backups/`, `deploy/production/.swarm/` and `deploy/production/data/*.db` in the build context).
- `deploy/production/podman-compose.prod.yml` + `deploy/production/.env`: add
  `IMAGE_TIMEOUT_SECONDS` and set `SHUTDOWN_GRACE_SECONDS >= IMAGE_TIMEOUT_SECONDS`.
- `main.go`: at boot, warn when `ShutdownGraceSecs < ImageTimeoutSecs`.

---

## 6. Observability

**O-1 (P2) — the image path is measured, the video path is not.** ✅/❌

- `/v1/images/*` goes through `proxyCandidates` → `proxyWithMetrics` → `logRequestExtendedBodies`
  (`proxy.go:2512-2536`), which emits `IncRequests(provider, model, endpoint, status)`,
  `ObserveLatency(provider, endpoint, …)` and `ObserveRequest{...CostUSD, Prompt, Completion,
  CacheRead, CacheWrite, Reasoning, TtftMs}` into the existing `otel` Prometheus instruments
  (`internal/otel/otel.go:74-100`). Endpoint label is `images.generations` / `images.edits`, so
  `/metrics` already breaks images out by label. ✅ Verified: `request_logs.endpoint` contains
  `images.generations` rows.
- `POST /v1/videos` writes a `request_logs` row with endpoint `videos` and the cost
  (`videos.go:176-179`) — which means the `otel` counters *do* fire for it. ✅
- **`GET /v1/videos`, `GET /v1/videos/{id}`, `GET /v1/videos/{id}/content`, `DELETE /v1/videos/{id}`
  emit NO metrics and NO `request_logs` row.** Only `middleware.Logger` (`middleware.go:136-152`)
  logs them. So provider-level latency/error rates for polling and content download are invisible
  in `/metrics`; a provider whose content endpoint is 5xx-ing shows up only as `status=502` in the
  text log. `POST /v1/systemone` *is* covered (it calls `proxyCandidates`, `systemone.go:75-88`). ✅
- **Cost attribution asymmetry for video:** `VideoCreate` meters the estimated cost at enqueue
  (`videos.go:175-176`) — correct for a job API — but nothing reconciles it if the job later fails
  (`status=failed`). A failed generation is billed in full. The code documents the choice ("the cost
  is known from the requested seconds regardless of how polling ends") but a refund/adjustment on
  the terminal `failed` poll would be more honest, and the poll is the only place that knows.
- `Prompt`/`Completion` are 0 for image/video rows (correct — they are not token-billed), and
  `cost_usd` is the per-image/per-second price via the `hasCost` override in `imageTransform`
  (`images.go:263-285`) → `proxy.go:1370-1385`. ✅ That override is the only reason image spend is
  not silently zero; it works (`cost_usd=0.042` observed).

**O-2 (P3) — no metric for the new failure modes.** Nothing counts "image response too large"
(M-1), "video content truncated at MaxContentBytes", or "seed failed". The `otel` package already has
`IncZeroCost(reason)` as the pattern for exactly this (`otel/otel.go`, used at `proxy.go:2986,2995`).
Add `gateway_image_body_rejected_total{reason}` etc. so the P1 caps are alertable.

---

## Summary of measurements (all on this working tree)

| # | Metric | Baseline | With measured fix |
| --- | --- | --- | --- |
| 1 | CPU per 1.4 MB image response | 40.2–42.3 ms | 17.1–18.5 ms (−57 %) |
| 2 | Bytes allocated per image response | 19.35 MB | 13.74 MB (−29 %) |
| 3 | Peak RSS delta @ 16 concurrent images | 95–108 MB | 69–82 MB (−25 %) |
| 4 | Serial image throughput (1 core) | ~22 req/s | ~50 req/s (×2.3) |
| 5 | Peak RSS per 64 MB image response | ~245 MB | ~245 MB (unchanged — needs the cap) |
| 6 | Video content relay, 256 MiB | — | 0.12 s, RSS +24 kB (streams correctly ✅) |
| 7 | Unknown-model chat request, 4010-row catalog | 6.16 ms CPU | needs the `short_id` index |
| 8 | `costForModel` unknown model, 4010 rows | 3.80 ms | — |
| 9 | Boot (fresh DB / 4010 rows) | 82 ms / 20 ms | fine as-is |
| 10 | Three seeds on every sync | 1.59 ms, 34 autocommit stmts, errors swallowed | — |

---

## Verdict

**NEEDS WORK**

The new surfaces are structurally sound — no goroutine leaks, no poller, no per-request regexes,
video content relay is correctly streamed, boot cost is negligible, and the image TTFB plumbing
works exactly as designed when the env var is set to a positive value. But there are two
user-visible timeout defects (one inverted, one missing entirely on the video routes), an unbounded
response buffer whose cost I measured at ~245 MB/request at 64 MB payloads and ~5× CPU
amplification at realistic 1.4 MB payloads, and a per-request full-table-scan catalog lookup that
the new capability guards invoke three times on every image request. None of these are exotic;
each is a one- or two-function fix, and the two biggest ones (duplicate JSON parses, `ImageCount`)
are measured above with the exact patch that removes 57 % of the CPU and 29 % of the allocations
without changing a single test's expectations.

### Blocking issues

1. **P0 — per-request catalog lookup is a full table scan + sort.** `catalog.go:204-223`; verified
   `EXPLAIN QUERY PLAN` → `SCAN models_catalog` + `USE TEMP B-TREE FOR ORDER BY`. ×15 per lookup
   through `FindBestMatch`/`trySlug`. 6.16 ms/request at 4010 rows vs 0.86 ms at 0 rows, on **all**
   chat traffic, and called 3× per image request (`images.go:531,553,593`). Fix: store + index
   `short_id`, make the containment query exact-match-first, add a small model-id→catalog cache
   invalidated on sync.
2. **P1 — `IMAGE_TIMEOUT_SECONDS=0` ("disabled") actually sets a 300 s budget.** `images.go:151-156`
   + `main.go:184-186`. Inverted sentinel; the log line actively misleads. Fix: adopt the
   `Timeouts.TTFB` tri-state (`-1`=default, `0`=disabled) and correct the doc table
   (`config.go:88-89`).
3. **P1 — all five video routes have no first-byte budget; `DefaultVideoTTFB` is dead code.**
   `videos.go:25-31` declares it, nothing references it, and `videos.go:90-297` never enters
   `proxyCandidates`/`ttfbController`. Measured: `POST /v1/videos` ignored `TTFB=5s` and burned 20 s
   on a slow upstream; `GET /v1/videos/{id}/content` accepted a 16 s dribble with `TTFB=3s`.
   Content download additionally has **no** idle watchdog, so a trickling provider pins a client
   connection, a goroutine and a pooled upstream conn forever (`WriteTimeout: 0`). Fix: give create
   a 30–60 s budget, polls the normal TTFB, content a first-byte + per-chunk idle watchdog.
4. **P1 — image responses are buffered without any cap.** `proxy.go:1215` `io.ReadAll(resp.Body)`
   (no `LimitReader`; only the error path is capped). Measured ~245 MB peak RSS per concurrent
   64 MB response → OOM on a 1 GiB container at N=4. Fix: add a per-endpoint `maxResponseBytes`
   (or stream passthrough bodies) and, for the passthrough case, skip the `bufWriter` copy
   (`fallback.go:62-95`).
5. **P1 — six full-body JSON parses on every image response (five of them redundant).** `proxy.go:1260-1261`
   (tool-call repair on an image body), `:1278` (`extractUsage`), `:1282` (`cacheSplitFromBody`),
   `:1405` (`extractUsageDetail`) plus `images.go:287`→`translate.go:189` (`ImageCount`). Measured
   57 % CPU / 29 % alloc reduction from the patch in §1 (verified green against the full
   `internal/proxy` suite). Note: the `extractUsageDetail` → envelope substitution **must** be
   guarded by "outBody was not rewritten", or `finish_reason` is silently lost (two tests catch it).
6. **P1 — `SeedImageModels`/`SeedVideoModels` run outside the sync transaction, swallow errors, and
   are not called at boot.** `catalog.go:792-799` (34 autocommit statements, `_, _ =`), and
   `main.go:107` seeds only decision models — so on an existing deployment upgrading to this build,
   image and video models are absent from `GET /v1/models` for up to 24 h (reproduced: fresh DB
   seeded 10 decision / 0 image / 0 video). Fix: seed all three inside the sync tx, log failures,
   and call them at boot (cost measured at 1.6 ms).
7. **P1 — shutdown grace window is shorter than the image budget.** `SHUTDOWN_GRACE_SECONDS` (default 90 s) vs `IMAGE_TIMEOUT_SECONDS` (default
   300 s).** A rolling deploy kills in-flight image generations at 90 s and returns a 502
   (reproduced), and `database.Close()` immediately after means the lost request never reaches
   `request_logs`/billing. Fix: warn at boot when grace < image budget; document that
   `SHUTDOWN_GRACE_SECONDS >= IMAGE_TIMEOUT_SECONDS`.
8. **P2 — `/v1/videos` poll/list/content/delete emit no otel metrics and no `request_logs` rows.**
   `videos.go:189-303`. Provider latency/error rate for polling and content download is invisible in
   `/metrics`; only the create (`videos.go:176-179`) is instrumented. Fix: emit `IncRequests` /
   `ObserveLatency` with endpoints `videos.poll`, `videos.list`, `videos.content`, `videos.delete`
   (and a `videos` row per poll only if it is cheap — polls are high-QPS, so prefer metrics only).
9. **P2 — `pgsmoke` (the Postgres INTEGER-flag regression guard) never runs in CI.** No
   `GATEWAY_PG_DSN` in `.github/workflows/*.yml`; `pgsmoke/main_test.go:20-23` skips silently. This
   is the test that exists specifically to catch the `pq: invalid input syntax for type integer:
   "false"` class the new `db.BoolInt` writes could reintroduce. Fix: Postgres service container +
   `GATEWAY_PG_DSN` in `ci.yml`.
10. **P2 — `Dockerfile` does not build the UI and there is no `.dockerignore`.** `Dockerfile:24-30`
    embeds whatever is committed in `cmd/gateway/web`; the working tree's new vite assets are
    **untracked** (`?? cmd/gateway/web/assets/index-U1ABf980.js`), so a clean clone would embed a
    broken `index.html`. The 93 MB `web/node_modules`, `data/`, `backups/` and
    `deploy/production/.swarm/` are all sent as build context. Fix: add a node stage (the pattern is
    already in `release.yml:64-79`), add `.dockerignore`, make `docker: needs: [backend, frontend]`
    (`ci.yml:69`).
11. **P2 — `UPSTREAM_HEADER_TIMEOUT_SECONDS` is inert.** `main.go:144-149` sets
    `Timeouts.UpstreamHeader`, but `Handler.Client` was frozen at `proxy.go:110`. Measured:
    `=2` with a 20 s upstream still returned 200 @ 20 s. Fix: rebuild the transport after config
    assignment, or remove the setting.
12. **P2 — image/video rows bloat `request_logs` with 8 kB of base64 per request.** `LOG_BODIES`
    defaults true (`config.go:133-139`) and `BodyLogMaxBytes` is 8192 (`proxy.go:2553-2560`);
    confirmed 8192-byte `b64_json` prefixes in every image row. Fix: skip body capture for
    `images.*`/`videos`.
13. **P3 — `DefaultVideoTTFB` is unreferenced dead code with a misleading comment** (`videos.go:26-31`
    claims polling/content are "governed by the normal request path"; they are governed by nothing).
    Fold into fix #3 or delete.
14. **P3 — `video_jobs` grows without bound and `expires_at` is not enforced** (`videos.go:226-263`;
    the repo's own `videos_lifecycle_test.go:193-227` marks it "GAP (P2)"). Add a retention delete
    next to `runLogRetention` (`main.go:702-730`).
15. **P3 — `Content-Length` forwarded verbatim while the copy is capped at `MaxContentBytes`**
    (`videos.go:651-653` vs `:656`) → a truncated body with a 200 and an unsatisfiable
    `Content-Length`. Drop the header (or 502) when the advertised size exceeds the cap.
16. **P3 — multipart edit parts are unbounded in count and rebuilt byte-for-byte.**
    `images.go:310-357` + `:408-448`. Measured 301 MB allocated per 4×4 MiB edit request
    (`BenchmarkImageEdits`). Cap file parts at `MaxReferenceImages+1` and pre-size the rebuild
    buffer.
