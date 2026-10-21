# Deploy notes — invariants that are not obvious from the compose files
#
# Applies to: deploy/production/podman-compose.prod.yml,
#             deploy/failover/podman-compose.standby.yml,
#             docker-compose.yml (local), deploy/systemd/*.service,
#             and the Dockerfile image.
#
# Each section states the invariant, why it exists, how it fails when violated,
# and where it is enforced. If you change one side of a pair, change the other.

## 1. `SHUTDOWN_GRACE_SECONDS` >= `IMAGE_TIMEOUT_SECONDS`  (P1-16)

**Invariant.** The graceful drain window must be at least as long as the budget
a slow request is allowed to consume. Concretely, with the shipped defaults:

| Knob | Default | Meaning |
| --- | --- | --- |
| `IMAGE_TIMEOUT_SECONDS` | unset / `-1` = **300s** | client first-byte budget for `/v1/images/generations` and `/v1/images/edits`. `0` **disables** the watchdog (documented, and genuinely disabled as of P1-12). |
| `SHUTDOWN_GRACE_SECONDS` | **90s** (config default) | how long `srv.Shutdown` waits for in-flight requests after SIGTERM. |
| `stop_grace_period` / `TimeoutStopSec` | 360s / 360s | how long the *supervisor* waits before SIGKILL. Must exceed the drain window. |

**Why.** Image generation legitimately runs 10–60s and is allowed up to 300s.
On a rolling deploy (or `compose up`, or any container recreate) the supervisor
sends SIGTERM; `cmd/gateway/main.go` drains for `SHUTDOWN_GRACE_SECONDS`, then
force-closes whatever is left, then closes the database. With 90 < 300 an
in-flight generation is cancelled mid-flight and the client receives a 502
(`all provider attempts failed` / `context canceled`) — and its `request_logs`
row is never written, because `database.Close()` has already run. The provider
has still been billed; the gateway forgets it happened.

**Fails like this** (reproduced in the perf review): three concurrent image
requests, SIGTERM, grace 3s → all three clients see a connection error after
~4s, the log shows `Post ".../images/generations": context canceled`,
`all provider attempts failed`, and `request_logs insert failed: sql: database is
closed`. With grace ≥ request duration all three return 200 with full bodies.

**Where it is enforced.**
- `docker-compose.yml` sets `SHUTDOWN_GRACE_SECONDS=330` and
  `stop_grace_period: 360s`.
- `deploy/production/podman-compose.prod.yml` sets the same pair.
- `deploy/systemd/ai-gateway.service` sets `TimeoutStopSec=360` — raise it
  together with `SHUTDOWN_GRACE_SECONDS` from `/etc/ai-gateway.env`.
- `cmd/gateway/main.go` logs a boot-time WARNING when
  `ShutdownGraceSecs < ImageTimeoutSecs` (see §1.1). This is the backstop: a
  correct compose file cannot help an operator who overrides the env by hand.

### 1.1 Boot-time warning — exact code for `cmd/gateway/main.go` (not owned here)

Place this **after** the image-budget wiring block (the `switch` on
`cfg.ImageTimeoutSecs` that assigns `proxyHandler.ImageTTFB`, ~line 180-188)
so the effective value it reports is the one actually in use:

```go
	// P1-16: SHUTDOWN_GRACE_SECONDS must cover the slowest request budget, or a
	// rolling deploy cancels in-flight image generations (502 to the client and
	// no billing row, because the DB is closed immediately after the drain).
	// Warn loudly at boot instead of discovering it during a deploy.
	imageBudget := proxyHandler.ImageTTFB
	if proxyHandler.ImageTTFBDisabled {
		imageBudget = 0
	} else if imageBudget == 0 {
		imageBudget = proxy.DefaultImageTTFB
	}
	if imageBudget > 0 && time.Duration(cfg.ShutdownGraceSecs)*time.Second < imageBudget {
		log.Warn().
			Int("shutdown_grace_seconds", cfg.ShutdownGraceSecs).
			Dur("image_budget", imageBudget).
			Msg("SHUTDOWN_GRACE_SECONDS is shorter than the image budget: a rolling deploy will cancel in-flight image generations (clients get 502 and the request_logs billing row is lost). Set SHUTDOWN_GRACE_SECONDS >= the image budget and raise the supervisor's stop timeout above it.")
	}
```

Notes for whoever applies it:
- `proxy.DefaultImageTTFB` (300s) and `proxyHandler.ImageTTFBDisabled` already
  exist; no new fields are needed. `time` and `proxy` are already imported.
- When `IMAGE_TIMEOUT_SECONDS=0` the watchdog is disabled, so there is no
  meaningful budget to compare against — the condition deliberately skips the
  warning in that case rather than warning forever.
- Cheap and side-effect free; it runs once at boot.

## 2. New environment knobs

Surfaced in the compose stacks so they are visible in `compose config` instead
of only in the binary's defaults:

| Variable | Where it is set | Notes |
| --- | --- | --- |
| `IMAGE_TIMEOUT_SECONDS` | `docker-compose.yml`, `deploy/production/podman-compose.prod.yml` | tri-state (`-1`/unset = 300s, `0` = disabled, `N` = seconds). Production keeps 300. |
| `SHUTDOWN_GRACE_SECONDS` | both compose files | 330s, i.e. image budget + 30s slack. |
| `stop_grace_period` | both compose files | 360s, above the drain window. |
| `TTFB_TIMEOUT_SECONDS` | already in production `.env` | chat first-byte budget (85s), deliberately **not** applied to images. |
| `MAX_PROXY_BODY_MB` | already in production `.env` | 256 in production for large agentic contexts. |
| `GATEWAY_PG_DSN` | CI only (`ci.yml`, job `postgres`) | test-only DSN for `internal/db/pgsmoke`. Never set it in a deploy: the suite drops schema `public`. |

The canonical, commented list lives in `.env.example` (owned by the docs track);
this file only records the deployment-side invariants.

Two practical notes:

- `deploy/production/.env` is gitignored (it holds real secrets) and is loaded
  via `env_file:`. The `environment:` block in the compose file **wins over**
  values coming from `env_file`, which is why both knobs are declared there as
  `${VAR:-default}`: compose interpolates those from the shell or from the
  project-directory `.env` (the same file), so an override still works and the
  effective default stays visible in `compose config`. No edit to the
  gitignored `.env` was needed for this change.
- If you override `SHUTDOWN_GRACE_SECONDS` in `.env`, keep it >= the image
  budget anyway — the compose default (330) encodes that rule, and lowering it
  reintroduces P1-16. `compose config` is the quickest way to see what will
  actually be applied.
- `.env.example` documents `IMAGE_TIMEOUT_SECONDS` but not yet
  `SHUTDOWN_GRACE_SECONDS`; it is owned by the docs track — worth adding there
  with the `>= image budget` rule.

## 3. Health and readiness

- `GET /health` — liveness. Always 200 while the process serves; includes
  `config_ok` and a DB status field. Safe to use as a container healthcheck.
- `GET /ready` — readiness. 503 when the database is unreachable. Use this as
  the load-balancer/orchestrator readiness probe.
- Both are separate mux routes, so a slow in-flight image request does **not**
  make either probe fail. Long requests are handled by the drain window (§1),
  not by probe tuning.
- The Dockerfile gained a `HEALTHCHECK` that issues one HTTP/1.0 request over
  `/bin/bash`'s `/dev/tcp` (no curl/wget added to the image). Podman's default
  **OCI** output format silently drops `HEALTHCHECK`, so the compose files carry
  their own equivalent `healthcheck:` block and behaviour does not depend on the
  builder format.

## 4. Rollback

- Image: redeploy the previous tag (`podman tag ai-gateway:prod-<sha> ai-gateway:prod`
  then `compose up -d`). The binary is self-contained; no schema downgrade is
  needed because migrations 026-030 are additive (new tables/columns/indexes).
- Env-only change (§1): revert `SHUTDOWN_GRACE_SECONDS` / `stop_grace_period`
  in the compose file and `compose up -d` again. Safe in either direction.
- The Dockerfile's `web` stage is additive; reverting to the previous Dockerfile
  restores "embed whatever is committed in `cmd/gateway/web`".
