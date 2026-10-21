# Security review — new uncommitted surfaces

Scope: `internal/proxy/{videos,images,systemone,upstream_routing,fallback}.go`,
`internal/{decision,imagegen,videogen}/*`, `internal/handler/discovery.go`,
`cmd/gateway/main.go` route groups, migrations 026–029.
Method: read-only trace from untrusted entry point → sink. No source was modified.

Baseline that matters for everything below: every new proxy route is registered
inside the single gateway-auth group (`cmd/gateway/main.go:467-513`), so
`GatewayAuthWithJWTRevocation` → `budget.Middleware` → `GatewayRateLimitWithLimits`
precede every handler. `X-Gateway-Key-Prefix` is *overwritten* from the verified
key (`internal/middleware/middleware.go:277`, `attachGatewayKey`), so a client
cannot forge its own prefix. That closes the generic "new endpoint is unscoped"
class of bug — see the per-item verification below.

---

## 1. Findings (confirmed, reachable)

### F1 — Video content download fetches an arbitrary provider-supplied URL with the provider credential attached
**Severity: HIGH** · `internal/proxy/videos.go:601-618` (`videoContentURL`) →
`internal/proxy/videos.go:620-658` (`streamVideoContent`)

`videoContentURL` returns `job.VideoURL` verbatim whenever the poll response
carried one (`videos.go:602-604`). `job.VideoURL` comes straight from the
provider body (`videogen/translate.go:302-310` `XAIVideoURL`,
`translate.go:377-388` `VeoVideoURI`) and is persisted unvalidated
(`videos.go:519`, `videos.go:715-718`). `streamVideoContent` then does a plain
server-side `h.Client.Do(req)` on it (`videos.go:629`) and relays the bytes to
the client. Three distinct problems, all verified:

**(a) The credential is attached by host string comparison, not by provenance.**
```go
// videos.go:634-643
if sameOrigin(contentURL, p.BaseURL) {
    if isGoogleNativeAPI(contentURL) { req.Header.Set("x-goog-api-key", apiKey) }
    else                             { req.Header.Set("Authorization", "Bearer "+apiKey) }
}
```
I verified `sameOrigin` (`videos.go:660-668`, plain `url.Parse` + `Host`
`EqualFold`) with a standalone program:

* `http://api.provider.com/x` vs base `https://api.provider.com/v1` ⇒ **true**.
  The scheme is never compared, so a provider-supplied `http://` URL gets the
  decrypted key sent **in cleartext**.
* `//api.provider.com/x` ⇒ **true**, with `req.URL.Scheme == ""` — an
  empty-scheme URL matches the origin check, and `http.NewRequest` accepts it.
* Any `Host`-suffix, port, or userinfo trick is correctly rejected, so the
  host comparison itself is sound; it is the *scheme* and *provenance* that are
  not checked.

**(b) No fetch restriction at all.** Any provider-supplied absolute URL that
*does not* match the origin is still fetched (just without the key), and the
response body is relayed to the client. `NewGatewayTransport`
(`internal/proxy/upstream.go:124-160`) only denies cloud-metadata IPs; RFC1918,
loopback, and link-local ranges are deliberately allowed (for homelab/vLLM
providers). A reseller (this gateway explicitly targets them — "resellers expose
models the gateway cannot enumerate", `images.go:576`) that returns
`video.url = "http://10.0.0.5:8080/admin"` turns `GET /v1/videos/{id}/content`
into an authenticated-network SSRF read relayed verbatim to any gateway client.

**(c) Provider-relative job ids are not a safe path segment.** My first draft
claimed `PathEscape` fails to contain traversal; I tested it and that was
**wrong** — `url.PathEscape` correctly encodes `/` (`../../x` →
`..%2F..%2Fx`), and `..` alone is the only value that reaches the server as a
real dot segment (it resolves to `/` in a `ServeMux`). So `pollVideoTarget`'s
`/videos/`+`PathEscape(id)` (`videos.go:483-490`) is **not** exploitable by
itself. It is still wrong to persist an unvalidated provider string as an id,
but I am downgrading that sub-claim from "exfiltration" to "harden it".

Net impact: (a) provider credential disclosure over plaintext / to an
empty-scheme URL; (b) provider-controlled SSRF into the private network with the
response exfiltrated to the caller. (a) and (b) both need only a provider that
returns a crafted `video.url`, i.e. any reseller the operator does not fully
trust.

**Remediation**
1. In `streamVideoContent`, require **same scheme *and* host** as
   `p.BaseURL` before attaching the credential, and reject
   `contentURL.Scheme != "https"` when the base is https.
2. For the CDN case (xAI), stop server-side fetching entirely: 302-redirect the
   client to the provider URL, or fetch only when it matches an
   operator-configured host allowlist.
3. Add a private-range denylist (RFC1918 + loopback + link-local) to the
   content-download path specifically.
4. Validate the persisted `upstream_id` against `^[A-Za-z0-9._:\-]{1,256}$` in
   `videoUpstreamID` before insert (defense in depth).

---

### F2 — Video-jobs ownership is key-prefix-only; org scope is never checked, and the dashboard escape hatch is wide
**Severity: MEDIUM** · `internal/proxy/videos.go:670-678` (`videoJobVisible`),
`videos.go:265-273` (`VideoList`), `videos.go:292-304` (`VideoDelete`)

```go
func (h *Handler) videoJobVisible(r *http.Request, job *videoJob) bool {
	prefix := r.Header.Get("X-Gateway-Key-Prefix")
	if prefix == "" || job.KeyPrefix == "" { return true }   // <-- escape hatch
	return prefix == job.KeyPrefix
}
```

Verified — reading the assignment's question directly:

* **Can key A read/poll/delete/download key B's video?** Not through the normal
  path: `VideoGet` (`videos.go:196`), `VideoContent` (`videos.go:229`) and
  `VideoDelete` (`videos.go:295`) all call `videoJobVisible`, and the test at
  `videos_test.go:288-320` covers it. `VideoList` is scoped in SQL
  (`videos.go:270`: `WHERE key_prefix=?`). So the answer to (1) is
  **refuted for same-org keys**.
* **But the scoping key is only ever the 8-hex-character key prefix**
  (`apikey.Prefix`, `internal/apikey/apikey.go:45-56`), *not* the key id and
  *not* the org. `X-Gateway-Org` is set by the middleware (`middleware.go:279`)
  and is completely ignored by `VideoGet/VideoContent/VideoDelete/VideoList`.
  Two consequences:
  * The prefix is 8 hex chars = 32 bits (`apikey.Generate` is 32 random bytes,
    `apikey.go:21-27`), so a *collision* is birthday-bounded at ~77k keys.
    Low but non-zero, and it is the same scoping primitive `request_logs` uses,
    so this is consistent rather than a new regression. The real problem is the
    org omission below, which is unambiguous and needs no collision.
  * An org-scoped key can read a job whose `provider_id` now belongs to a
    *different* org: `VideoGet` re-resolves the provider
    (`videos.go:202`) and decrypts its key without an `orgAllows(keyOrg, p)` check,
    unlike every other proxy path (`fallback.go:132-136`). A provider reassigned
    to org B keeps serving org A's stale jobs.
* **The empty-prefix escape hatch is real and load-bearing.** Any request with
  an empty `X-Gateway-Key-Prefix` sees *every* job. Today `attachGatewayKey`
  always sets a non-empty prefix, so this is only reachable through paths that
  do not run `GatewayAuth` — i.e. it is a latent fail-open, exactly the shape of
  bug that becomes critical the moment a route is registered outside the auth
  group or a test/internal caller forgets the middleware. Deny-by-default is the
  correct posture for an ownership check.

**Remediation:** scope on `key_id` (already resolved by `h.gatewayKeyID`) *and*
`org_id`; on a mismatch return 404. Delete the `prefix == "" || job.KeyPrefix == ""`
clause and instead treat an empty prefix as "no access" except for an explicit
`IsAdmin(r)` branch. Add `orgAllows(h.requestKeyOrg(r), p)` before
`DecryptKey` in `VideoGet`/`VideoContent`.

---

### F3 — `video_jobs` has no retention or expiry enforcement; prompts are stored in plaintext forever
**Severity: MEDIUM** · `internal/db/migrations/028_video_jobs.sql`,
`internal/proxy/videos.go:699-706` (`insertVideoJob`)

* `prompt` is persisted verbatim (`videos.go:120`, `videos.go:699-706`) with no
  scrubbing. Unlike `request_logs`, whose bodies are gated behind the opt-in
  `LOG_BODIES` flag and passed through `ScrubSecrets`
  (`internal/proxy/proxy.go:2517-2534`), the video prompt is **always** stored
  and is returned to the client on every poll and list (`videos.go:70`).
  A prompt is frequently the most sensitive field in the request (PII, customer
  data pasted into "summarize this ticket"). It also lands in `VideoList`
  responses for the whole key.
* `expires_at` is parsed and echoed (`videos.go:71-75`) but **never enforced**.
  `VideoContent` will keep proxying the provider's bytes after the provider-side
  expiry, and `video_jobs` rows are never purged: `runLogRetention`
  (`cmd/gateway/main.go:704-731`) only deletes from `request_logs`. A job row
  (with its prompt) is therefore retained indefinitely regardless of
  `LOG_RETENTION_DAYS`, and `DELETE /v1/videos/{id}` is the only way to remove
  one. `VideoList` even returns the prompt in the list body.
* The same row also stores `video_url`, which for xAI/Veo is a
  provider-signed URL — a bearer token for the asset, in a table with no
  retention.

**Remediation:** drop `prompt` from `toVideo()`/`VideoList` (or store a hash),
add `video_jobs` to the retention purge with its own (short) window, enforce
`expires_at` in `VideoContent`/`VideoGet`, and never return `video_url` to the
client.

---

### F4 — `/v1/images/edits` never applies the JSON path's field validation, and its buffering is unbounded in aggregate
**Severity: MEDIUM** · `internal/proxy/images.go:318-360` (`parseEditMultipart`),
`images.go:380-406` (`applyEditField`)

The multipart parser shares the *shape* of `imagegen.Normalized` with the JSON
path but **none of its validation**. `applyEditField` only validates `n`
(`atoiStrict` + `>0`, no upper bound — the JSON path enforces `1..MaxImagesPerRequest`,
`imagegen.go:229-238`) and does not check at all:

* `prompt` — `MaxPromptLen = 32000` is enforced on the JSON path
  (`imagegen.go:222-227`) but **never** on the multipart path
  (`images.go:389-390` just assigns `n.Prompt = value`). A 64 MiB `prompt`
  part passes validation and is forwarded upstream.
* `size` — the JSON path rejects unknown sizes against `validSizes`
  (`imagegen.go:240-249`); the multipart path only lower-cases it
  (`images.go:392-393`). So `size=99999x99999` reaches the provider.
* `quality`, `style`, `response_format`, `aspect_ratio`, `resolution` — all
  assigned verbatim (`images.go:394-404`). `response_format` in particular
  drives `imageTransform`'s output shaping (`images.go:255-280`) but is not
  constrained to `url`/`b64_json` here.

Note this is *validation asymmetry*, not a TPM bypass: `checkTPM` runs on the
whole raw body (`images.go:124`) so the size is still metered.

**Buffering.** Each part is read with a *per-part* 64 MiB cap
(`images.go:330`, `io.ReadAll(io.LimitReader(part, 64<<20))`) with no aggregate
cap and no count cap on non-image parts; the outer `readProxyBody` bound is
`MAX_PROXY_BODY_MB` (default 64 MiB, `proxy.go:510` / `config.go:378`). Image
bytes are then held **twice** — once in `multipartFile.Data` and again in
`imagegen.Normalized.Images` (`images.go:348-357`) — and a Google target
base64-re-encodes them into the upstream body (`imagegen/translate.go:26-31`),
i.e. ~2-3× the request body resident per in-flight edit. At the documented
maximum (`MAX_PROXY_BODY_MB` up to 256, `main.go:557`) that is several hundred
MiB per concurrent request.

Also note the empty-filename branch itself is *legitimate* (plain form fields
such as `prompt`/`n` carry no filename); I initially read it as a smuggling
primitive and that was wrong — the defect is the missing validation, not the
dispatch.

**Remediation:** factor the scalar validation out of `imagegen.ParseRequest`
into a `ValidateFields(*Normalized) error` and call it from *both* paths after
`parseEditMultipart`; cap the aggregate part bytes and the part count; cap the
edits route's request body to a few MiB rather than inheriting the 64 MiB LLM
cap; drop `Normalized.Images[i].Data` once `files[i].Data` is retained (or vice
versa) to halve peak memory.

---

### F5 — Multipart filename and Content-Type are copied into the rebuilt upstream part header unescaped
**Severity: LOW** (defense in depth; not currently exploitable) ·
`internal/proxy/images.go:431-437` (`buildEditMultipart`)

```go
hdr.Set("Content-Disposition", `form-data; name="`+f.Field+`"; filename="`+f.FileName+`"`)
hdr.Set("Content-Type", f.ContentType)
```

`f.FileName` comes from `part.FileName()` (attacker-controlled) and
`f.ContentType` from `part.Header.Get("Content-Type")` (attacker-controlled).
I verified empirically against `mime/multipart` on Go 1.25:

* `Content-Type` **can** carry injected bytes: `image/png\r\nX-Evil: 1` is
  emitted verbatim into the upstream part header block, producing a real
  `X-Evil: 1` header in the request the provider receives.
* `filename` cannot break out of the header today: Go's MIME scanner rejects raw
  CR/LF inside a header line (`malformed MIME header line`), and a literal
  backslash-escaped `\r\n` round-trips as the two-character text. So filename
  cannot forge a part header **on the current Go stdlib**.
* `name=` (`f.Field`) is `part.FormName()` and is only ever assigned from the
  literal switch in `parseEditMultipart`, so it is safe.

The Content-Type injection is a genuine header-injection primitive into the
upstream request (poison the provider's edge/WAF, smuggle a `Content-Length`, or
inject auth-adjacent headers if the provider is lenient). It is bounded by
whatever CRLF handling the MIME writer permits — which today is "anything".

**Remediation:** strip `\r`, `\n`, and non-token characters from
`f.ContentType` (allow only a `type/subtype` + parameters), and validate
`f.FileName` against `^[A-Za-z0-9._-]{1,128}$` before use.

---

### F6 — Client-supplied model id is concatenated into the URL unescaped, letting the caller set the upstream query string
**Severity: LOW** · `internal/proxy/images.go:245-254` (`googleImageTarget`),
`internal/proxy/videos.go:416-433` (`googleVeoTarget`,
`googleVeoOperationURL`), `internal/proxy/systemone.go:139-160`

```go
// images.go:253
return b + "/v1beta/models/" + model + action
```
```go
// videos.go:425
return b + "/v1beta/models/" + model + ":predictLongRunning"
```

`model` here is the client-supplied model id after `stripProviderPrefix`
(`fallback.go:45-58`), which only strips a known prefix — it does not sanitize.
The JSON validators check `prompt`, `seconds`, `size`, `n`,
`response_format`, and `criteria`, but **never the model id's character set**.

Verified with `net/http`: model `x:generateContent?key=ATTACKER` produces the
request target
`/v1beta/models/x:generateContent?key=ATTACKER:generateContent` — i.e. the
attacker controls the upstream **query string**. `?key=` is precisely how
Google's native `generativelanguage` API accepts a credential, and the gateway
sets the real key in `x-goog-api-key` instead (`proxy.go:752-755`), so a caller
can substitute their own Google key into the path (attribution/billing escape)
and, generally, drive arbitrary query parameters on an authenticated upstream
request. A `#` in the model id truncates the path instead.

Not an SSRF (the host is still the provider's), and the default
`url.Parse`-based construction rejects a full-scheme override. It is a
request-shaping primitive, hence LOW.

**Remediation:** `url.PathEscape(model)` in `googleImageTarget`,
`googleVeoTarget`, `googleVeoOperationURL`, `systemOneTarget`'s caller and
`cloudflareDecisionTarget`; reject model ids containing `?`, `#`, or more than
one `/`.

---

### F7 — Image generation runs through the exact-match response cache: stale images and silently unmetered spend
**Severity: MEDIUM** · `internal/proxy/images.go:80-94` / `images.go:135-148`,
`internal/proxy/proxy.go:972-986`, `internal/proxy/fallback.go:309-321`

`ImageGenerations` and `ImageEdits` both call
`proxyCandidatesWithOpts(..., isStream=false, ...)`, which takes the
exact-match response cache branch:

```go
// proxy.go:973-986
if !isStream && len(body) <= cacheMaxBodyBytes {
    cacheKey = cacheKeyFor(endpoint, model, body, cacheScopeFor(r, providerID))
    if cached, status, headers, ok := c.Get(cacheKey); ok && status >= 200 && status < 400 {
        h.serveCacheHit(w, cached, status, headers)
        h.logRequestExtendedBodies(..., 0, 0, 0, false, nil, nil, &logMeta{CacheHit: true, ...})
        return
    }
```

The cache is wired unconditionally at boot (`cmd/gateway/main.go:194-205`) with
`CACHE_TTL_SECONDS` default 10 (`internal/config/config.go:380`). Consequences:

* **Stale generation.** Image generation is contractually non-deterministic. A
  client re-submitting the same prompt to get a different image gets the first
  one back, with `X-Cache: HIT`. Correctness bug for a documented use case.
* **Unmetered spend.** The cache-hit path logs `cost_usd = 0` and calls no
  `recordUsage`, so the operator's cost caps never see the re-roll. Since
  `apikey.MonthSpendUSD` sums `request_logs.cost_usd`
  (`apikey.go:712-722`), repeated identical prompts are effectively free at the
  provider's expense only if the cache actually saves the call — but the
  *client* is still served a billable-looking response for $0, and any operator
  cost accounting built on `request_logs` under-reports.
* **Cross-key disclosure — narrower than it first looks.** `cacheScopeFor`
  (`proxy.go:520-532`) returns `prefix + "|" + org + "|" + providerID`, so the
  key **prefix is in the scope**. Key A therefore cannot read key B's cached
  image unless their 8-hex prefixes collide (the same 32-bit birthday bound as
  F2). I initially claimed org-only scoping here; that was wrong. The residual
  risk is the prefix-collision case plus the fact that the scope is not the
  immutable key id.
* `ImageEdits` includes the uploaded image bytes in the hashed body, so it is
  effectively per-request; the concern is `ImageGenerations`, where the body is
  small and prompt-guessable.

`VideoCreate` correctly never enters this path (it calls `doVideoUpstream`
directly, `videos.go:135`), so the video surface is unaffected.

**Remediation:** pass an explicit `noCache` flag (or zero TTL) through
`proxyCandidatesOpts` for both image endpoints — generated media must never be
cached. If a cache is wanted later, scope it on the key *id*, not the prefix.

---

### F8 — `MonthlyBudgetUSD` is never enforced for dashboard-session keys, and there is no pre-flight cost check on the new dollar-scale endpoints
**Severity: MEDIUM** · `internal/middleware/middleware.go:249-256`,
`internal/proxy/proxy.go:2592-2598` (`gatewayKeyID`), `internal/apikey/apikey.go:712-722`,
`internal/proxy/videos.go:169-176`

`VideoCreate` meters the whole clip at enqueue time — `h.recordUsage(keyPrefix, r, 0, cost)`
plus `logRequestExtendedBodies(..., cost, ...)` (`videos.go:169-176`) — which is
the right call. Two gaps around it:

**(a) Session keys never accumulate `key_id`, so their spend is invisible to the
budget check.** The pre-admission cap reads
`SELECT SUM(cost_usd) FROM request_logs WHERE key_id = ?` (`apikey.go:713-717`),
but `key_id` is stamped by a best-effort follow-up UPDATE gated on
`gatewayKeyID(prefix)` resolving a row in `gateway_keys`:

```go
// proxy.go:2592-2598
if keyPrefix != "" && h.DB != nil {
    if keyID := h.gatewayKeyID(keyPrefix); keyID != "" {
        h.DB.Exec(db.Q(`UPDATE request_logs SET key_id=? WHERE id=?`), keyID, id)
    }
}
```

Dashboard JWT sessions get a *virtual* key whose prefix is `sess<subject>`
(`middleware.go:288-309`), which has no `gateway_keys` row. `gatewayKeyID`
returns `""`, no UPDATE runs, `key_id` stays NULL, `MonthSpendUSD` sums nothing,
and the `MonthlyBudgetUSD` branch (`middleware.go:250-256`) is skipped on every
request. So a logged-in dashboard user has **no monthly cost ceiling at all** —
and the new endpoints make that expensive: one 12 s Veo clip is $4.80
(`videogen/registry.go:246-249`, $0.40/s), one 10-image `n=10` gpt-image-2 call
is $0.60 (`imagegen/registry.go:88-91`). (This was already true for chat, but a
chat request costs fractions of a cent, so the exposure is new.)

Note this does *not* affect `sk-gw-*` keys: their prefix always resolves, so
`key_id` is stamped and the cap works.

**(b) The only pre-flight gate is token-shaped.** `budget.Middleware`
(`budget.go:478-503`) estimates `r.ContentLength/4` tokens. A video create body
is ~150 bytes → ~37 "tokens", and an image body similar. The daily/monthly
**token** budget is therefore unenforceable on these routes; only the cost cap
applies, and only when (a) does not apply. There is no equivalent pre-flight
check against `videogen.CostForModelSeconds(model, seconds)` or
`imagegen.CostPerImage(model) * n`.

**Remediation:** (1) make `gatewayKeyID` resolve session keys (stamp
`session:<subject>` into `key_id`, or key the monthly cap off `key_prefix` for
sessions); (2) add an explicit pre-flight cost check in `VideoCreate` and
`ImageGenerations` that rejects with 429 when the modelled cost would cross
`MonthlyBudgetUSD`; (3) never skip the `key_id` UPDATE silently — log it.

---

---

## 2. Verified-sound (explicitly refuted, with evidence)

| Claim | Verdict | Evidence |
|---|---|---|
| New routes sit outside auth/budget/rate-limit | **Refuted** | `cmd/gateway/main.go:467-513` — one `r.Group` with `GatewayAuthWithJWTRevocation` + `budget.Middleware` + `GatewayRateLimitWithLimits`, containing chat, images, videos (all 5 verbs, both `/v1` and bare), models, messages, responses, systemone. The only route registered outside is the bare `GET /models` alias, which carries a manually-duplicated identical chain (`main.go:523-528`). |
| `allowed_models` allowlist not honored | **Refuted** | `VideoCreate` `videos.go:99`, `ImageGenerations` `images.go:53`, `ImageEdits` `images.go:120`, `SystemOne` `systemone.go:39` all call `h.enforceModelAllowlist` before routing (`proxy.go:149-160`). Note `ImageEdits` extracts the model from the *multipart* body (`norm.Model`, `images.go:116`) — correct, since the JSON extractor would return "". |
| TPM not honored on new endpoints | **Refuted** | `checkTPM` is called on all four (`videos.go:103`, `images.go:57`, `images.go:124`, `systemone.go:43`). Approximation quality varies (`estimateTokens` strips base64, `proxy.go:180-215`), but the gate exists. |
| Decision endpoint lets a huge `questions` map through | **Refuted** | `MaxQuestions = 64`, `MaxChoiceOptions = 255`, `MaxScoreLevels = 10`, `MaxQuestionIDLen = 100`, `images > 4` rejected — `internal/decision/decision.go:36-43,180-215`. Deep nesting is bounded by `encoding/json`'s `maxNestingDepth = 10000` scanner plus `MAX_PROXY_BODY_MB`. `state` size is bounded only by the body cap (no explicit field limit) — acceptable, noted below. |
| `SystemOne` output is not validated | **Refuted** — and correctly so | The handler deliberately relays the upstream body (`systemone.go:76-96`); output-shape validation is the provider's contract and re-validating floats would add no security property. No sink consumes it server-side. |
| Video `upstream_id` traversal via `url.PathEscape` | **Refuted** | I tested it: `url.PathEscape("../../x")` = `..%2F..%2Fx`, which Go's server sees as `RawPath` with `URL.Path` = `/videos/../../x` — no directory escape, because `net/http` does not re-clean a `RawPath`-carrying request. Only the bare id `..` produces a real dot segment (resolves to `/`). Persisting an unvalidated id is still worth hardening, but it is not an exfiltration vector. |
| `response_format=url` causes a server-side fetch | **Refuted** | Gemini/Imagen inline base64 are converted to a `data:` URL (`imagegen/translate.go:196-206`); OpenAI-dialect `url` values are relayed verbatim. No code path fetches an image URL. |
| JSON `image`/`images` reference URLs are fetched server-side | **Refuted** | Explicitly rejected: `imagegen/imagegen.go:378-381` and `videogen/translate.go:119-122` ("remote image URLs are not supported"). |
| `b64_json`/image responses are unbounded-buffered | **Partially refuted** | Buffered by design in `proxyWithMetricsOpts` (`proxy.go:1215` `io.ReadAll(resp.Body)` — **no limit**, an inherited issue) but capped again on relay. For native image dialects the body is re-marshalled from base64 (`translate.go:96-153`), and `n ≤ 10` (`imagegen.go:41`) × `4096x4096` PNG is bounded in practice. No *new* unbounded read was introduced; the pre-existing lack of a read cap on buffered upstream bodies is worth a follow-up but is not new surface. |
| Video content relayed without a size cap | **Refuted** | `io.Copy(w, io.LimitReader(resp.Body, videogen.MaxContentBytes))` — `videos.go:656`, cap 512 MiB (`videogen.go:80`). |
| Cross-tenant read of images/videos via the *routing* layer | **Refuted** | `orgAllows(keyOrg, p)` is enforced on every candidate (`fallback.go:132-136,175-241`). The gap is only the *job-resume* path (F2). |
| Video create is a stateless request (no goroutines) | **Confirmed as designed** | `videos.go` spawns no goroutines; polling is one upstream call per client poll (`videos.go:474-533`). Correct per migration 028's comment. |
| Model-capability isolation (image/video/decision never cross endpoints) | **Sound** | Guards installed on all five text endpoints (`proxy.go:3190-3196, 3324-3330, 3522-3528, 4042-4048, 4188-4194`) and on the three new endpoints (`images.go:62,580-601`; `videos.go:109,336-364`; `systemone.go:52-55`). |
| Secrets in logs | **Sound** | `log.Error` in the new paths logs `p.Name`, `job.ID`, `model`, and `errSnippet(respBody)` — a control-char-stripped 200-byte slice (`proxy.go:668-679`), not credentials. `request_logs` bodies stay behind `LOG_BODIES` + `ScrubSecrets`. The video `prompt` is the one plaintext-PII exception (F3). |
| `X-Gateway-Key-Prefix` spoofable | **Refuted** | Overwritten from the verified key: `middleware.go:277` `r.Header.Set("X-Gateway-Key-Prefix", key.Prefix)`. |
| `image`/`decision` flags on admin `AddManual` allow privilege escalation | **Refuted** | Route is behind `RequirePerm(rbac.PermCatalogWrite)` (`main.go:440`); flags only affect capability tagging, not authorization. |
| SQL injection in `video_jobs` queries | **Refuted** | All five statements use `db.Q` placeholders (`videos.go:270,299,699,715,725`). |
| SSRF via provider `base_url` | **Refuted (existing control)** | `validateBaseURL` + dial-time/redirect-time metadata blocking (`provider/store.go:249-286`, `proxy/upstream.go:124-184`). Private ranges remain allowed by design. |

## 3. Assumptions & limits

* I could not run the gateway end-to-end (no `.env`, no live providers) and did
  not execute the test suite; every finding above is from reading the code, plus
  four standalone Go programs I wrote under `/tmp/sectest` to confirm
  `mime/multipart` behaviour (CRLF injection into part headers, header-value
  control-character handling, and `net/http` URL construction from an
  unescaped model id).
* F1's "malicious reseller" premise requires an operator to have configured a
  provider they do not fully trust. The SSRF half also requires the provider to
  return a crafted `video.url`; both are realistic for aggregator/reseller
  providers, which this gateway explicitly targets ("resellers expose models the
  gateway cannot enumerate", `images.go:576`).
* F7's cross-key image disclosure assumes the same effective cache and
  overlapping `allowed_models`; the cache backend's cross-process behaviour
  (in-memory vs Redis) was not verified.
* The Go stdlib MIME scanner's CRLF rejection (which makes F5 filename injection
  non-exploitable) could change across Go versions; the Content-Type vector does
  not depend on it.
* I did not audit `web/` (Playground.tsx, +2711 lines) or `openapi.yaml`; another
  reviewer owns those.

## Verdict

**NEEDS WORK**

Blocking issues, worst first:

1. **P1 — F1**: `streamVideoContent` server-side-fetches a **provider-supplied
   URL** and attaches the decrypted provider credential whenever the *host*
   matches — scheme is not compared (verified: `http://` and empty-scheme URLs
   both pass `sameOrigin`), and any non-matching host is still fetched and
   relayed. Fix: require same scheme *and* host before attaching the key,
   refuse to server-side-fetch off-origin URLs (302 the client instead, or use a
   host allowlist), and add a private-range denylist.
2. **P1 — F8**: `MonthlyBudgetUSD` is **never enforced for dashboard-session
   keys** — `gatewayKeyID` returns `""` for the `sess…` virtual prefix, so
   `request_logs.key_id` stays NULL and `MonthSpendUSD` sums to 0. Combined with
   dollar-scale clip prices ($4.80 for 12 s of Veo) this is an unbounded spend
   path. Also add a pre-flight modelled-cost check; the existing gate only
   estimates tokens.
3. **P2 — F3**: no retention/expiry for `video_jobs`; the user's prompt is stored
   and returned in plaintext indefinitely, and `expires_at` is decorative.
4. **P2 — F2**: video ownership keys off the 8-hex key prefix and ignores
   `org_id`, with an empty-prefix fail-open (`prefix == ""` ⇒ allow all). Add an
   `orgAllows` check before `DecryptKey` and make empty-prefix deny.
5. **P2 — F7**: image generation/edits run through the exact-match response
   cache — stale images on re-roll and `cost_usd = 0` accounting, with residual
   cross-key disclosure bounded by the 32-bit prefix. Disable caching for the
   image endpoints.
6. **P2 — F4**: `/v1/images/edits` applies none of the JSON path's field
   validation (`MaxPromptLen`, `size` vocabulary, `n` upper bound,
   `response_format`), and has no aggregate-size/part-count bound while holding
   image bytes 2-3× in memory. Validate the shared scalar fields in both paths.
7. **P3 — F5**: multipart `Content-Type` is copied verbatim into the rebuilt
   upstream part header — a header-injection primitive; strip CR/LF and treat it
   as a token.
8. **P3 — F6**: unescaped model id in `googleImageTarget` / `googleVeoTarget` /
   `googleVeoOperationURL` / decision targets lets a caller control the upstream
   query string. `url.PathEscape` it.
