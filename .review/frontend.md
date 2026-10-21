# Frontend review — Playground (images / video / decision) + dashboard shell

Scope reviewed (working tree, uncommitted):
`web/src/pages/Playground.tsx` (+2711), `web/src/App.tsx`, `web/src/pages/Models.tsx`,
`web/src/pages/Providers.tsx`, `web/src/components/ui.tsx`, `web/src/components/ModelCombobox.tsx`,
`web/tailwind.config.js`, and the committed embed build in `cmd/gateway/web/`.

Nothing in the source tree was modified. Verification was done with
`npx tsc --noEmit`, a byte comparison of a fresh `vite build` against the committed
embed assets, and a headless Chromium harness (a Node stub server that replays the
**exact frames/documents the Go handlers emit**, as read from `internal/proxy/*.go`
and `internal/openapi/openapi.yaml`, plus the production CSP from
`cmd/gateway/main.go:746`). Reproduce commands are in §8.

---

## 1. Correctness of the new request bodies (vs handlers + openapi.yaml)

| Mode | Body built at | Handler contract | Verdict |
|---|---|---|---|
| Images | `Playground.tsx:715` `{model, prompt, n, size, response_format:'b64_json'}` | `imagegen.ParseRequest` (`internal/imagegen/imagegen.go:161`) requires `model`+`prompt`, `n` 1..10, `size` ∈ `validSizes`, `response_format` ∈ {url,b64_json} — all satisfied. Sizes offered (`1024x1024,1536x1024,1024x1536,1792x1024,1024x1792,auto`) are all in `validSizes` (`imagegen.go:118-131`). | **Correct** |
| Video | `Playground.tsx:716` `{model, prompt, seconds, size}` | `videogen.ParseRequest` (`internal/videogen/videogen.go:157`): `seconds` ∈ {4,8,12} strings, `size` ∈ the 4 accepted values — exactly the two lists the UI hardcodes at `Playground.tsx:1869-1876`. | **Correct** |
| Decision | `Playground.tsx:717` → `buildDecisionBody` (`:2295`) `{model, state, questions:{id:{type,instructions,criteria}}}` | `decision.Validate` (`internal/decision/decision.go:118`): `state` non-null (the UI sends `''` for empty input, which is a legal JSON string — `parseState` at `:2291`), `choice.criteria` = object ≥2 options, `score.criteria` = array 2..10. The editors (`parseChoiceCriteria:2270`, `parseScoreCriteria:2283`) produce exactly those shapes and pre-validate client-side. | **Correct** |

Field names, enums and required fields all match. No wrong-enum or missing-required
defects found in the three new bodies.

**Gap 1 (P1) — the UI drops reference images for images/video even though the
endpoints accept them.** `buildBody` (`:715-716`) ignores `atts` entirely, so a
user who attaches an image gets it silently discarded. Two user-visible lies result:

* `Playground.tsx:1881` — “Attach a reference image in the composer to condition the
  generation where the model supports it.” Confirmed empirically: with `ref.png`
  attached and the chip visible in the composer, the captured body was
  `{"model":"xai/grok-imagine-video","prompt":"a slow orbit","seconds":"8","size":"1280x720"}`
  — no `input_reference`.
* The drop overlay (`:1396`) and composer hint say “Images become vision input”.

The backend is ready for this: `imagegen.Request.Image`/`Images` (`imagegen.go:72-79`)
and `videogen.Request.InputReference` (`videogen.go:75`), and
`videoCreateRequest` already forwards `input_reference` (`internal/proxy/videos.go:401`).
Fix: in `:715` add `...(atts.filter(a=>a.dataUrl).length ? { image: atts[0].dataUrl } : {})`
and in `:716` add `...(first image dataUrl ? { input_reference: first.dataUrl } : {})`;
otherwise change the copy at `:1881` and hide/disable Attach in these modes.

**Gap 2 (P2) — decision mode sends a chat-shaped prompt nobody uses.**
`buildBody` returns `buildDecisionBody(model, stateInput, questions)` and never looks
at `text`, yet the composer placeholder is
“Ask a follow-up about this decision…” (`Playground.tsx:1450`) and the welcome copy
promises conversational follow-ups. Anything typed goes nowhere. Fix: replace the
placeholder with “State to evaluate (string or JSON)…”, bind the composer to
`stateInput` in decision mode, or drop the composer there.

---

## 2. Video polling, timers, blob URLs

**P0 — a permanently failing job polls forever, with no error and no stop.**
`videoJobs` (`:1005-1015`) only removes a job from the poll set when `status` is
`completed`/`failed` or a `url` exists. It never records a poll failure, and
`tick` swallows it: `if (!res.ok) continue` (`:1026`). So a 404 (job purged /
`expires_at` passed / owned by another key — `videos.go:194` returns 404 to hide
ownership), a 401, or a 500 leaves the card frozen on “Queued” and re-polls every
4 s forever.

Measured in headless Chromium against a stub that returns 404 for
`GET /v1/videos/{id}`: **5 polls in 22 s, indefinitely**, card stayed at
“Queued / queued / video_gone” and never showed an error. `setInterval` (`:1032`)
is also never cleared because `videoJobs` never shrinks, and there is no
backoff, no attempt cap, no `document.visibilityState` guard (no
`visibilitychange` listener anywhere in `web/src`), and the timer keeps firing in
a hidden tab. Every tick also loops over *all* live jobs sequentially, so N
pending jobs = N requests per 4 s.

Fix: persist `pollErrors`/`pollCount` on `m.video`; on `!res.ok` with
4xx other than 429 (and a 404/410 in particular) set
`error: 'HTTP ${res.status} — <message>'` and mark the job unsatisfiable so it
leaves `videoJobs`; add exponential backoff (4s→8s→16s→30s cap) keyed on
attempt count; stop ticking when `document.hidden` and poll once on
`visibilitychange`; return a `{cancel}` and abort the in-flight fetch on cleanup.

Correct in this area: cleanup and unmount handling are sound —
`clearInterval` + `stopped` flag (`:1033`), the content-fetch effect has a
`cancelled` flag (`:1073`), the send stream aborts on unmount (`:483`), and
`<video>` object URLs are collected in `videoUrls` (`:452`) and revoked on unmount
(`:485`). The only leak is *within* a session: a URL is never revoked when its
message is deleted/cleared, and `videoUrls` grows monotonically. Minor (blob is
small, page-lifetime bounded).

**P1 — the video preview can never play under the gateway's own CSP.**
`VideoCard` renders `<video controls src={video.url}>` (`:1743`) where `url` is a
`URL.createObjectURL(blob)` (`:1064`). The gateway sends
`default-src 'self'` with **no `media-src`** (`cmd/gateway/main.go:746-747`), so
`media-src` falls back to `'self'` and `blob:` is refused. Verified with the real
CSP header in place: `video.error = 4 "MEDIA_ELEMENT_ERROR: Media load rejected by
URL safety check"` and a `securitypolicyviolation` with `directive: "media-src",
blocked: "blob"`. Every completed video renders as a black, unplayable box.
Fix: add `media-src 'self' blob:` to the CSP in `main.go:747`. (The generated-image
path is fine: `img-src 'self' data:` covers the `data:` URIs at `:1598`.)

**P2 — the completed-video download is re-entered on every session mutation.**
`completedKey` is `completedVideos.map(j => j.mid).join('|')` (`:1046`) — it
changes whenever *any* message id enters the completed set, which is correct — but
the effect body re-downloads every entry in `completedVideos` on each run, so a
second video completing re-downloads the first. Cheap (bytes are already local?) —
no, it re-fetches `/content`. Guard with a `useRef<Set<string>>` of already-fetched
message ids.

---

## 3. Error surfacing

**Good.** Non-2xx responses surface the provider's own message, not a JSON wall:
`errorMessage()` (`:2187`) probes `{error:string}`, `{error:{message}}`,
`{message}`, `{detail}` and truncates the raw body. Verified against a real
`httperr` envelope: `HTTP 400 — model 'nope' is a System One / decision model; use
POST /v1/systemone instead of /v1/chat/completions` and
`HTTP 429 — monthly spend budget exceeded for this key` both render as an error
bubble. Unroutable-model 503s (`imageNoRoute`, `videoNoRoute`,
`notDecisionModelMessage`) reach the user the same way. Empty decision/image/video
payloads degrade to readable text rather than a blank pane. The “No models
available for this endpoint” banner exists in both the Welcome (`:1516`) and the
settings panel (`:1824`).

**P0 (same root cause as §1 Gap 1b) — an in-band error frame after a partial answer
is silently dropped.** `deltaFrom` (`:770`) only extracts text deltas;
`writeSSEUpstreamError`/`writeStreamTerminator` emit
`data: {"error":{"message":"…","type":"server_error"}}` followed by `[DONE]`
(`internal/proxy/upstream.go:269-277`) when the upstream dies mid-stream (quota
429, provider 5xx after commit, TTFB abort). Because `acc` is non-empty the
`if (!acc.trim())` fallback at `:960` never runs, so the error is never shown.
Verified end-to-end: thread showed `"Partial answer "` and the rate-limit message
never appeared. The user believes a truncated answer is complete. Fix: in the SSE
loop, track `streamErr` — if a frame parses to `{error:{message}}` (or Anthropic
`{type:"error", error:{message}}`), record it and append/replace with
`\n\n⚠ <message>` (and mark the message `role: 'error'`); `extractUsage` already
walks these lines.

**P1 — a restored image/decision/video message renders as a blank “still
generating” bubble.** `persist` (`:365-366`) strips `images` and `decision` and
keeps only `video.job`, but `MessageRow` picks its branch on those stripped fields
(`:1593-1618`). After a reload the message has no text, no `decision`, no
`images`, so it falls through to `{msg.text ? <RichText/> : <TypingDots/>}`
(`:1618`) and shows the animated typing dots forever. Verified: after reload the
thread showed only the user turn plus usage metadata, the assistant bubble was
empty, and 3 pulsing dots were rendered.

Worse, image messages lose the actual images on reload: `images: undefined` in
`persist` means the generated picture is gone after any refresh — the user paid
for it and cannot get it back (`calls` payloads are dropped too,
`:368`). Two fixes, pick one: (a) keep non-base64 metadata (e.g.
`images.map(d => ({ revised_prompt: d.revised_prompt }))` and a
“generated image — not retained after reload” placeholder instead of
`TypingDots`), or (b) persist `video.url` is impossible (blob) but *do* persist a
`render: 'images'` discriminator plus `imagesRetained:false` so `MessageRow`
renders “Image not retained — see Request inspector” rather than a spinner. The
minimum bar is that a terminal message must never render as loading.

---

## 4. State-management quality (3 000-line component)

The page is 2 455 lines of source after the change (+2 711 in the diff) and holds
34 `useState`s, 9 `useRef`s and 12 effects in one component, plus 20 helper
components in the same file. `tsc` is clean, but three identifiers are already
dead — `acceptsImages` (`:517`), `addMessage` (`:545`), `busyLabel` (`:1080`) —
and were introduced by this change (verified they do not exist at `HEAD`), which
is the usual early symptom of a file growing faster than it is pruned.
`--noUnusedLocals` would fail the build on all three.

Concrete, low-risk cut lines (no behaviour change), in priority order:

1. **`web/src/lib/playground/protocol.ts`** — `splitDataUrl`, `openAIContent`,
   `anthropicContent`, `responsesContent`, `buildDecisionBody`,
   `parseChoiceCriteria`, `parseScoreCriteria`, `parseState`, `deltaFrom`,
   `tryParseRich`, `extractText`, `extractUsage`, `extractUsageFromObject`,
   `errorMessage`, `safeParse` (`:170-260`, `:770-796`, `:2178-2260`,
   `:2270-2320`). These are pure functions with zero React dependencies and
   `deltaFrom` in particular needs unit tests (see the P0 in §3).
2. **`web/src/lib/playground/attachments.ts`** — `prepareImage`, `prepareFile`,
   `inlinedText`, `attachmentManifest`, `promptText`, `MAX_*` (`:180-300`).
   ~120 lines, pure.
3. **`web/src/hooks/useVideoJobs.ts`** — the two polling effects plus their
   `useMemo` selectors and the `videoUrls` set (`:1005-1077`). This is the part
   that needs the backoff/abort/visibility logic; it should not live inside a
   render function.
4. **`web/src/pages/playground/SettingsPanel.tsx`** (`:1781-1968`) and
   **`Inspector.tsx`** (`:1990-2115`) — both are already prop-driven with a single
   `SettingsProps`/`inspectorProps` bag; the refactor is a pure move.
5. **`web/src/pages/playground/messages/`** — `MessageRow`, `RichText`,
   `CodeBlock`, `VideoCard`, `AttachmentChip`, `DecisionAnswers*`
   (`:1556-1780`, `:2372-2455`).

Duplication to remove while cutting: `headers()` is a `useCallback` over
`gatewayKey` (`:758`), but the two polling effects intentionally omit it from
their deps (`eslint-disable` at `:1034`, `:1077`) — that is how a stale key gets
sent by a poll. Pass `headers` (or a ref) properly instead of suppressing the
dep. `extractText` is implemented three slightly different ways (`:2225`,
`:2218` chain, and inline in `tryParseRich`), which is exactly how the Responses
doubling below survived.

---

## 5. Accessibility

Verified in the browser (headless Chromium, 1440×900 and 1024×800):

* **No unlabelled visible controls** — 0 unnamed interactive elements; the 5
  icon-only buttons all carry `aria-label` or `title` (`Copy last answer`,
  `Conversation options`, `New conversation` via `title`; `Toggle theme`,
  `Close …` via `aria-label`). Good.
* **The model combobox has no combobox semantics.** `ModelCombobox` (`:229-231`)
  renders a bare `<input>` with no `role="combobox"`, `aria-expanded`,
  `aria-controls`, `aria-activedescendant`, `aria-autocomplete` or `aria-label`,
  and the popup has no `role="listbox"`/`option`. Measured: all attributes
  `null`, `role=listbox` absent. Keyboard *works* (Arrow/Enter/Escape/Backspace
  at `:109-129`) but a screen reader announces a plain text field with no
  popup and no current option. Fix in `ModelCombobox.tsx`: label the input
  (`aria-label` from a new prop), `role="combobox"`, `aria-expanded={open}`,
  `aria-controls={listId}`, `aria-activedescendant={rows[highlight]?.id}`,
  `role="listbox"` on the dropdown, `role="option"` + `aria-selected` on rows.
* **Video job status is not announced.** There is no `aria-live`, `aria-busy`,
  `role="status"` or `role="log"` anywhere in `Playground.tsx` (grep returns
  nothing) — measured 0 live regions while a video job was `queued`/`in_progress`.
  `Progress` (`ui.tsx:189`) is a bare div pair with no
  `role="progressbar"`/`aria-valuenow`. Add
  `<div aria-live="polite" aria-busy={!done}>` around the `VideoCard` status line
  and `role="progressbar" aria-valuenow={value} aria-valuemin={0} aria-valuemax={100}`
  to `Progress`; add `aria-live="polite"` to the streaming bubble
  (`:1590` “streaming…” indicator).
* **Drawers are not dialogs.** The narrow-viewport Settings/Inspector overlays
  (`:1373-1388`) render a `fixed inset-x-0 bottom-0` scrim + panel with **no**
  `role="dialog"`, no `aria-modal`, no label, no focus move and no focus trap;
  `role="dialog"` count while open = 0, `aria-modal` = 0, and
  `document.activeElement` stayed on `BODY`. The inline branch of the same panel
  (`:1184`) is a legitimately non-modal `aside`, so the fix is: only wrap the
  overlay variants in a dialog role, move focus to the panel's close button on
  open, restore the trigger on close, and trap Tab (or use the existing `Modal`
  primitive from `ui.tsx:374`, which already does `role="dialog" aria-modal`,
  Escape and body-scroll lock, but does not restore focus — see next item).
* **Escape/focus housekeeping.** Escape closes the drawers (`:614-621`) but
  leaves focus on `<body>` (verified), so keyboard users lose their place. The
  shared `Modal` (`ui.tsx:374-397`) has the same defect: verified
  `focus after modal close: BODY`. Add a `previouslyFocused` ref restore to
  `Modal`.
* **Session tabs are not tabs and are mouse-only.** Each session chip is a
  `<div onClick>` (`:1238`) with no `role="tab"`, no `tabIndex`, no keyboard
  handler and no `role="tablist"` on the strip (`:1236`). The close button inside
  is only reachable by Tab-after-nothing and is `opacity-0` until
  `group-hover` (it is still focusable, but invisible until focus lands). Make
  the chip a `<button>` (or `role="tab" tabIndex={0} onKeyDown`) and add
  `focus-visible:opacity-100` to the close button.
* **`AttachmentChip` uses `<span onClick>`** (`:1694`) for the image preview
  toggle — not focusable, not operable by keyboard. Same pattern in
  `ModelCombobox`'s wrapper `<div onClick={() => inputRef.current?.focus()}>`
  (`:229`) which is a harmless redundancy, and `Welcome`'s template cards are
  real `<button>`s (fine).
* **Contrast.** Measured (WCAG 2.x, light theme, `:root` tokens from
  `web/src/index.css`): `text-amber` on `bg-amber/10` = **4.11** (banner at
  `:1517`, 12 px) and **4.39** (`:1826`/`:1923`, 11 px) — below 4.5 AA for normal
  text; `text-danger` on `bg-danger/5` = **4.47** (`:1611` error bubble) and on
  `bg-danger/10` = **4.13**; `text-muted/80` = **3.72** (`:1546`, the model chip in
  the welcome footer, 10 px). Dark theme passes everywhere (≥5.1). Fixes:
  use `text-amber` on plain `bg-surface` (5.02) or bump the tint to
  `bg-amber/5`; use `text-danger` on `bg-surface` (4.83) for the error bubble;
  drop the `/80` at `:1546`. `ErrorNote` in `ui.tsx:353` shares the `danger/10`
  4.13 problem, so preferring the solid-surface variant there too fixes it
  globally.
* **Reduced motion** is honoured globally (`index.css:86-92` forces
  `animation-duration:.01ms`), so `animate-pulse-soft`/`animate-sidebar` are
  covered.
* **Narrow viewports.** 360/768/1024/1440/1920 at 900 px tall: no horizontal
  overflow (`documentElement.scrollWidth ≤ innerWidth`), composer stays on screen
  in all five, no element with `overflow-x: visible` overflows. `tailwind.config.js:7`
  replaces `theme.screens` wholesale; all breakpoints used in `src/`
  (`sm,md,lg,xl,2xl,3xl`) are declared, and no `max-*`/`xs`/`4xl` variants are
  used anywhere (`grep` = 0), so the override is safe.

---

## 6. Typecheck / lint

* `cd web && npx tsc --noEmit` → **exit 0, no output.** No errors, pre-existing
  or otherwise. `npx tsc -b` also exit 0.
* There is no ESLint config in the repo (`web/package.json` has no `lint`
  script, no `eslint.config.*`/`.eslintrc*`), so nothing lints these files. The
  four `// eslint-disable-line react-hooks/exhaustive-deps` comments at `:512`,
  `:1034`, `:1077` and `:1412` are inert — they silence a linter that does not
  run, and at `:1034`/`:1077` they hide a real stale-closure risk (`headers`).
* With `--noUnusedLocals` (not enabled in `tsconfig.json`, but
  `noUnusedLocals:false` is explicit) three new dead identifiers appear from this
  change: `acceptsImages` (`:517`), `addMessage` (`:545`), `busyLabel` (`:1080`).
- Pre-existing unused-locals noise elsewhere (`report.tsx`, `Logs.tsx`,
  `Settings.tsx`, …) is unrelated and untouched by this diff.

---

## 7. Is the committed embed build stale?

**No — it is current and reproducible, but it was built with a different env.**
`cmd/gateway/web/index.html` points at `assets/index-U1ABf980.js` +
`assets/index-Bc4Qat39.css`, both present and untracked (old pair deleted). I
rebuilt from `web/` into a temp dir: **byte-identical** to the committed assets
(`md5 376256eb…` / `59386049…`, `index.html` identical). The bundle also contains
all the new UI strings (`v1/systemone`, `v1/videos`, `images/generations`,
“No decision models found.”, …), so it is not the pre-change build.

Two caveats:

* The committed JS was built with `VITE_GATEWAY_URL:""` and `VITE_PORT:"8777"`
  baked into `import.meta.env`; a rebuild from the *tracked* `vite.config.ts` in
  a clean checkout bakes only `BASE_URL/DEV/MODE/PROD/SSR` (55-byte difference).
  Harmless for behaviour (`getApiBase()` prefers `VITE_API_URL/PUBLIC_URL` and
  falls back to relative paths either way, and `scripts/check-bundle.mjs` guards
  against absolute dev URLs), but it means **the committed artifact is not
  reproducible from the repo** — a rebuild will change the content hash and
  produce a dirty `cmd/gateway/web`. That is how stale embeds start. Build the
  embed in CI (`.github/workflows/release.yml:71-80` already does) rather than
  committing it, or pin the env used to produce it.
* `web/package.json build` is `tsc -b && vite build && node scripts/check-bundle.mjs`;
  note `vite.config.js` (a compiled artifact) exists in the working tree and is
  **gitignored** (`.gitignore:6`), yet Vite prefers it over `vite.config.ts`
  (`vite:config bundled config file loaded … vite.config.js`). It is a stale
  2026-09-30 copy. Any local build today silently uses the ignored file. Delete
  it, or add `--config vite.config.ts`; do not let a gitignored file decide the
  production bundle.

---

## 8. Verification performed

```
cd web && npx tsc --noEmit                # exit 0, no output
cd web && npx tsc --noEmit --noUnusedLocals  # 3 new dead ids in Playground.tsx
cd web && npx vite build --outDir /tmp/webbuild-check
  md5sum /tmp/webbuild-check/assets/* cmd/gateway/web/assets/*   # identical
```

Headless Chromium harness (stub server replaying the handlers' real frames and the
gateway CSP), results:

| Probe | Result |
|---|---|
| 400/429 `httperr` envelope → bubble | `HTTP 400 — model 'nope' is a System One / decision model…`, `HTTP 429 — monthly spend budget exceeded for this key` ✅ |
| `/v1/responses` stream (real `sseEventWriter` frames) | answer rendered **twice** ❌ |
| chat stream + in-band `{"error":…}` frame | partial answer shown, error **dropped** ❌ |
| video job 404 forever | **5 polls / 22 s**, card stuck “Queued”, no error ❌ |
| completed video with real CSP | `video.error=4`, `securitypolicyviolation media-src blob` ❌ |
| image generated → page reload | image gone, bubble renders as pulsing “typing” dots ❌ |
| zero eligible models after mode switch | send still enabled, request sent with the old chat model, server 400 shown as “No answers returned.” (misleading) ❌ |
| attach image in video mode | chip shown, `input_reference` absent from body ❌ |
| icon-only buttons | 0 unlabelled ✅ |
| combobox ARIA / listbox | all absent ❌ |
| aria-live for job status | 0 regions ❌ |
| drawer dialog semantics | `role=dialog` 0, `aria-modal` 0, focus stays on BODY ❌ |
| 360/768/1024/1440/1920 px layout | no horizontal overflow, composer visible ✅ |
| contrast (light theme) | amber/10 4.11, danger/10 4.13, muted/80 3.72 ❌ |

---

## 9. Blocking issues

**P0**

1. **Responses-mode answers are rendered twice.**
   `Playground.tsx:785` (`if (typeof j.text === 'string') return j.text`) matches the
   terminal `response.output_text.done` frame, whose payload carries the **full**
   `text` (`internal/proxy/responses_stream.go:928`), after the same text was
   already accumulated from the deltas. Verified in-browser: bubble text =
   `"Hello worldHello world"`. Fix: remove the `j.text` / `j.output_text` /
   `j.content[0].text` branches from `deltaFrom` and only accept them in the
   `tryParseRich` fallback; better, key on `j.type`:
   accept only `response.output_text.delta` and `content_block_delta`
   (`{type:"text_delta", text}`) plus `choices[0].delta.content`, and ignore
   everything else.
2. **An upstream error arriving mid-stream is silently swallowed.**
   `Playground.tsx:770-796` + `:960`; `internal/proxy/upstream.go:269-277` sends
   the error in band. Verified: “Partial answer ” with the 429 message dropped —
   the user sees a truncated answer as if it were finished. Fix: parse
   `{error:{message}}` / Anthropic `{type:"error",error:{message}}` frames, keep
   the message, and surface it (append to the bubble + `role:'error'`, or show a
   distinct error strip under the bubble).
3. **A video job that never succeeds polls forever with no error.**
   `Playground.tsx:1026` (`if (!res.ok) continue`) + `:1005-1015` (no failure
   state) + `:1032` (fixed 4 s `setInterval`, no visibility guard, no abort).
   Measured 5 polls in 22 s against a permanent 404, card frozen at “Queued”.
   Fix: record the status/message on the message (`m.video.error`), make
   `videoJobs` exclude messages with a terminal fetch error, add
   backoff + attempt cap, pause on `document.hidden`, and abort in-flight fetches
   in cleanup.
4. **`video.error` is never rendered, so a failed job looks stuck.**
   `VideoCard` shows `job.error.message` only when `job.status === 'failed'`
   (`Playground.tsx:1744`); the client-side `video.error` (content-fetch failure at
   `:1059`/`:1069`) is only shown as “file could not be fetched” when
   `done` is also true (`:1748`). Combined with P0-3 that is the blank-stuck pane.
   Render `video.error` unconditionally as an error strip with a Retry action.
5. **The video preview can never play under the gateway's own CSP.**
   `<video src={blob:…}>` at `Playground.tsx:1743` vs
   `default-src 'self'` with no `media-src` at `cmd/gateway/main.go:747`. Verified
   `MEDIA_ELEMENT_ERROR: Media load rejected by URL safety check` +
   `media-src blob` violation. Fix: add `media-src 'self' blob:` to the CSP.

**P1**

6. **Reference images are silently dropped for images/video modes** while the UI
   says otherwise (`Playground.tsx:715-716` vs `:1881` and `:1396`); the handlers
   already accept `image`/`images`/`input_reference`
   (`imagegen.go:72-79`, `videogen.go:75`, `videos.go:401`).
7. **Switching to a mode with no eligible model keeps the old chat model and lets
   the user send.** `Playground.tsx:506-512` returns early when `pool.length===0`
   and never clears `model`; verified: with 0 decision models the Send button was
   enabled, the request carried `openai/gpt-4o-mini` to `/v1/systemone`, and the
   400 was rendered as “No answers returned.” Fix: `setModel('')` when the pool is
   empty, disable Send (`:1342` already checks `!model`), and render the
   server's error text in `DecisionAnswers` when `data.error` is present
   (`:2373`).
8. **Restored transcripts render terminal messages as “still generating”.**
   `persist` strips `images`/`decision` (`:365-366`) but `MessageRow` branches on
   them (`:1593-1618`), so after a reload the bubble shows `TypingDots` forever
   and the paid-for image is unrecoverable. Fix: persist a `render` discriminator
   and an explicit “not retained” state; never fall through to `TypingDots` unless
   `msg.streaming`.
9. **Drawers expose no dialog semantics, no focus move, no focus restore.**
   `Playground.tsx:1373-1388` (verified `role=dialog` 0, `aria-modal` 0, focus
   left on BODY, focus not restored on Escape). Reuse/extend `Modal`
   (`ui.tsx:374`) which already handles role/Escape/scroll-lock but also needs
   focus restore.
10. **Model combobox has zero combobox/listbox ARIA** (`ModelCombobox.tsx:229-231`,
   `:132-190`) — keyboard works, screen-reader semantics do not.
11. **No `aria-live`/`aria-busy`/`role="progressbar"` for async job status**
    (`Playground.tsx` has none; `ui.tsx:189` `Progress` is decorative).

**P2**

12. Light-theme contrast: `text-amber` on `bg-amber/10` = 4.11/4.39
    (`:1517`, `:1826`, `:1923`), `text-danger` on `bg-danger/5|/10` =
    4.47/4.13 (`:1611`, `ui.tsx:353`), `text-muted/80` = 3.72 (`:1546`).
13. Session tabs are mouse-only `<div onClick>` with no `role="tab"`/`tabIndex`
    (`:1238`); the close button is invisible until hover.
14. `AttachmentChip` preview toggle is `<span onClick>` (`:1694`) — not keyboard
    operable.
15. Decision mode's composer is decorative: the placeholder promises a follow-up
    (`:1450`) but `buildBody` (`:717`) never reads the draft.
16. Dead identifiers introduced by this change: `acceptsImages` (`:517`),
    `addMessage` (`:545`), `busyLabel` (`:1080`); enable `noUnusedLocals` in CI.
17. No linter exists (`web/package.json` has no lint script/config); the four
    `eslint-disable` comments (`:512`, `:1034`, `:1077`, `:1412`) are inert and
    mask a real stale-`headers` dep in the polling effects.
18. Completed-video download re-fetches `/content` for every previously completed
    message on each new completion (`:1046-1077`).
19. In-session blob URLs are never revoked on message delete/clear (only on
    unmount, `:485`).

**P3**

20. `Playground.tsx` is 2 455 lines with 34 `useState`s in one component; §4
    gives the exact module boundaries to extract (protocol helpers, attachment
    pipeline, video polling hook, SettingsPanel, Inspector, message components).
21. Committed embed build is reproducible byte-for-byte *only* from the current
    (gitignored) `vite.config.js`; a rebuild from the tracked `vite.config.ts`
    changes the hash because of baked `VITE_GATEWAY_URL/VITE_PORT`. Build the
    embed in CI instead of committing it, and delete the stale ignored
    `web/vite.config.js` so the tracked TS config governs.
22. `Playground.tsx:1080` `busyLabel` (“Deciding/Generating/Starting”) is computed
    but never rendered — the running button says “Stop” with no mode-specific
    verb.

## Verdict

**NEEDS WORK**

Blocking list (P0→P3): P0-1 duplicated Responses answers (`:785`); P0-2 dropped
in-band stream errors (`:770-796`, `:960`); P0-3 infinite no-error video polling
(`:1005-1035`); P0-4 `video.error` never rendered (`:1744`, `:1748`); P0-5 video
preview blocked by gateway CSP (`main.go:747`, `:1743`); P1-6 reference images
dropped (`:715-716` vs `:1881`); P1-7 stale model when the new mode has no
candidates (`:506-512`); P1-8 restored messages render as `TypingDots` and lose
generated images (`:365-366`, `:1618`); P1-9 drawer dialog/focus semantics
(`:1373-1388`); P1-10 combobox ARIA (`ModelCombobox.tsx:229`); P1-11 no
`aria-live`/`progressbar` for job status; P2-12 light-theme contrast; P2-13
session-tab keyboard; P2-14 `<span onClick>` chips; P2-15 decorative decision
composer; P2-16 dead identifiers; P2-17 no linter + inert disables; P2-18
repeated `/content` fetch; P2-19 blob revoke on delete; P3-20 2 455-line
component (cut lines in §4); P3-21 non-reproducible committed embed / stale
ignored `vite.config.js`; P3-22 unused `busyLabel`.

The three request bodies themselves are **contract-correct** against
`images.go`/`videos.go`/`systemone.go` and `openapi.yaml`, `tsc --noEmit` is
clean, and the committed embed is current — the blockers are all in the client's
handling of *responses* and *job lifecycle*, plus accessibility of the new
surfaces.
