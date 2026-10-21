# Review Brief — ai-gateway production readiness

## What this repo is
`ai-gateway` — a Go reverse-proxy gateway that fronts many LLM providers behind one
`sk-gw-*` key, serving OpenAI Chat/Completions/Embeddings/Images/Videos/Responses
protocols plus Anthropic `/v1/messages`. SQLite by default, Postgres beta.
Embedded React dashboard in `web/` built into `cmd/gateway/web` (embed.FS).

## What is currently uncommitted (the work under review)
Three brand-new protocol surfaces plus supporting infrastructure:

1. **Decision models / System One** (`POST /v1/systemone`)
   - new: `internal/decision/{decision.go,registry.go,decision_test.go}`
   - new: `internal/proxy/systemone.go` (+ `systemone_test.go`)
   - new: `internal/db/migrations/026_decision_models.sql` (`decision` INTEGER flag on
     `models_catalog` + `provider_models`), seed in `internal/catalog/catalog.go`
     (`SeedDecisionModels`), `internal/catalog/decision_seed_test.go`

2. **Image generation** (`POST /v1/images/generations`, `POST /v1/images/edits`)
   - new: `internal/imagegen/{imagegen.go,registry.go,translate.go,*_test.go}`
   - new: `internal/proxy/images.go` (+ `images_test.go`)
   - new: `internal/db/migrations/027_image_models.sql` (`image` INTEGER flag), seed
     in catalog + `internal/discovery/discovery.go` (models.dev `modalities.output`)

3. **Video generation** (`POST /v1/videos`, `GET /v1/videos`, `GET /v1/videos/{id}`,
   `GET /v1/videos/{id}/content`, `DELETE /v1/videos/{id}`)
   - new: `internal/videogen/{videogen.go,registry.go,translate.go,*_test.go}`
   - new: `internal/proxy/videos.go` (+ `videos_test.go`, 756 lines)
   - new: `internal/db/migrations/028_video_jobs.sql` (`video_jobs` table),
     `029_video_models.sql` (`video` INTEGER flag)

4. **Supporting changes**
   - `internal/db/db.go`: extracted `migrationsList()`, added `LatestMigrationVersion()`,
     added `BoolInt()` helper (integer-column bool coercion for lib/pq)
   - `internal/config/config.go`: new `IMAGE_TIMEOUT_SECONDS` / `ImageTimeoutSecs`
   - `cmd/gateway/main.go`: route registration + image TTFB wiring
   - `internal/proxy/{proxy.go,fallback.go,upstream_routing.go,responses_stream.go}`:
     capability-aware routing / endpoint guards
   - `internal/discovery/discovery.go`: capability flags from models.dev
   - `web/src/pages/Playground.tsx`: +2711 lines (images/videos/decision modes)
   - `openapi.yaml`: +393 lines
   - `README.md`, `ARCHITECTURE.md`, `.env.example`: docs

## Current verified state (as of review start)
- `go build ./...` — clean
- `go vet ./...` — clean
- `go test ./...` — all packages pass

So the review is NOT about "make it compile". It is about: correctness under real
provider responses, security of the new trust boundaries, dialect (SQLite+Postgres)
correctness, contract/spec/UI consistency, missing pieces, and production hardening.

## Cross-cutting invariants the codebase claims to uphold
Read `ARCHITECTURE.md` for the authoritative list. Key ones:
- Capability isolation: an image-only model must never be routed chat traffic;
  a video model must never be served by `/v1/images/*`; decision models must never be
  selected for chat while text providers are never selected for `/v1/systemone`.
- **Dialect trap**: never write a Go `bool` into an INTEGER column on Postgres
  (lib/pq rejects it). Use `db.BoolInt`. Genuine BOOLEAN columns take bools directly.
- Migrations are embedded, ordered, run at boot via `schema_migrations`, and a failed
  migration marks the version **dirty** and aborts boot.
- `httperr` envelope for every error; `translate` owns protocol translation;
  `proxyCandidates` owns retries/failover/usage accounting so new endpoints reuse it.
- Gateway keys scope everything: allowed_models allowlist, TPM/RPM limits, budgets,
  org scoping. New endpoints must not create an unscoped escape hatch.
- Request logs + audit must attribute spend to the right key/provider.

## Known suspicious/unverified leads (do not take these on faith — verify or refute)
- Is there a `videogen` background poller assumption mismatch? Migration 028's comment
  says the gateway is deliberately stateless (client poll → upstream poll). Check that
  `videos.go` actually keeps no goroutines and that `expires_at` is enforced.
- `video_jobs` ownership: is `key_prefix` actually checked on GET/DELETE/content? Could
  key A read key B's video?
- Do image/video/decision endpoints honor the same auth/budget/rate-limit/allowlist
  chain as chat? Check `cmd/gateway/main.go` route group.
- `IMAGE_TIMEOUT_SECONDS` default is `-1` in config (sentinel) but documented as `300`
  in the docs table — check the sentinel plumbing end to end.
- Untracked scratch file at repo root: `ai-gateway-playground (1).html` (53 KB).

## Repo hygiene notes
- `.env`, `token`, `data/`, `backups/` are gitignored and untracked — leave them alone.
- Do NOT commit, do NOT push, do NOT touch `.env*`, `token`, `data/`, `backups/`.
- Working tree is `main` with uncommitted changes. Nothing is staged.

## Output format for reviewers
Write your findings to a markdown file in `.review/` named `<your-role>.md`, and end
with a `## Verdict` section: `PRODUCTION READY` / `NEEDS WORK` and a numbered
blocking-issue list. Every finding needs `file:line` evidence and a concrete fix.
Severity: P0 (security/data-loss/correctness break), P1 (breaks prod under realistic
load/provider behaviour), P2 (quality/consistency), P3 (nit).
