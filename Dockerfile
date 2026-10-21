# AI Gateway — production image.
#
# Build:   docker build -t ai-gateway .
# Run:     docker run -p 8080:8080 -v gateway-data:/data --env-file .env ai-gateway
#
# Stages
#   1. web      node:20 — builds the vite UI into /src/cmd/gateway/web, which
#               cmd/gateway/main.go embeds with `//go:embed all:web`.
#   2. builder  golang:1.25 — compiles the gateway with that UI embedded.
#   3. runtime  debian:12-slim — the binary + CA bundle, non-root, no shell tools.
#
# Notes:
# - CGO is kept ENABLED because the SQLite driver (mattn/go-sqlite3) compiles its
#   C amalgamation at build time. The builder stage ships gcc on Debian bookworm.
# - The UI is built IN the image on purpose. `//go:embed all:web` embeds whatever
#   exists at compile time, so relying on a committed cmd/gateway/web means an
#   image built from a clean checkout silently ships an index.html that points at
#   asset files which were never committed. Building it here removes that class of
#   failure and is the same source of truth as .github/workflows/release.yml.
# - SQLite migrations are embedded in internal/db (go:embed migrations/*.sql), so
#   a new migration file needs no Dockerfile change — it only needs to exist and
#   to be added to the go:embed/migrationsList in internal/db/db.go (enforced by
#   internal/db/retention_test.go). Nothing here excludes them.
# - Tag pins: minor/patch precision so a rebuild cannot silently change the
#   toolchain or libc. Bump deliberately.
# - No HEALTHCHECK directive in earlier revisions: the runtime image is
#   deliberately slim and curl/wget are not installed, so probing required an
#   extra binary. It now uses /bin/bash's /dev/tcp pseudo-device instead (no new
#   package) against GET /health, with /ready (DB ping) reserved for the
#   orchestrator's readiness probe. Long-running image requests do NOT make
#   /health or /ready useless: both are separate mux routes, and readiness is
#   about the database and the process, not in-flight work. What long requests
#   need is a graceful-drain window, and that is SHUTDOWN_GRACE_SECONDS — see
#   deploy/README.md for the invariant.

FROM node:20.19-bookworm-slim AS web

WORKDIR /src/web

# Cache node deps separately from source: a one-line TSX edit must not
# reinstall 93MB of node_modules. The host-independence concern below is
# handled by .dockerignore (web/node_modules never enters the context), so the
# lockfile-only layer can come first; the rm -rf stays as belt-and-suspenders
# against a context that somehow carries them anyway.
COPY web/package.json web/package-lock.json ./
RUN rm -rf node_modules *.tsbuildinfo \
 && npm ci --no-audit --no-fund

# Copy the rest of the UI project. .dockerignore already excludes host
# node_modules and *.tsbuildinfo from the context, so nothing here can clobber
# the node_modules installed above (do NOT rm them — this layer runs after npm ci).
COPY web/ ./

# vite.config.ts writes to ../cmd/gateway/web (outDir), so the go:embed input is
# produced by this step. package.json's build also runs scripts/check-bundle.mjs.
RUN npm run build && test -f /src/cmd/gateway/web/index.html


FROM golang:1.25.0-bookworm AS builder

WORKDIR /src

# Cache module downloads separately from source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Replace the checked-in UI with the one just built, so the go:embed input is
# always the freshly compiled bundle rather than whatever happens to be
# committed (and never a stale or gitignored directory).
RUN rm -rf ./cmd/gateway/web && mkdir -p ./cmd/gateway
COPY --from=web /src/cmd/gateway/web ./cmd/gateway/web

# Default CGO_ENABLED=1 (required by sqlite3); static flags must NOT be added.
# -buildvcs=false keeps the build reproducible without a .git directory.
#
# The cache mount persists GOCACHE across builds on the host. Without it every
# source change recompiled the whole dependency tree from scratch (~47s here,
# dominated by the CGO SQLite amalgamation); with it, only the gateway's own
# packages rebuild (~5s). Buildah/podman 5.4 supports RUN --mount=type=cache.
RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -buildvcs=false -o /out/gateway ./cmd/gateway


FROM debian:12.11-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# Non-root runtime user.
RUN useradd --uid 10001 --user-group --system --shell /usr/sbin/nologin gateway

# Data volume for the SQLite database (+ auto-generated key files).
RUN mkdir -p /data && chown -R gateway:gateway /data
VOLUME ["/data"]

COPY --from=builder /out/gateway /usr/local/bin/gateway

# HEALTHCHECK without adding curl/wget to the image: /bin/bash's /dev/tcp
# pseudo-device issues one HTTP/1.0 request and we match the status line.
#   /health -> liveness: 200 whenever the process serves (no DB check)
#   /ready  -> readiness: 503 when the database is unreachable
# --start-period covers migrations on a cold volume. The generous interval is
# safe because /health is its own mux route: a busy proxy (long image requests)
# must not look like a dead one.
# Long image requests are NOT a healthcheck concern — in-flight work is drained
# by SHUTDOWN_GRACE_SECONDS, not by probe timing. Keep
# SHUTDOWN_GRACE_SECONDS >= IMAGE_TIMEOUT_SECONDS (see deploy/README.md).
#
# NOTE the HEALTHCHECK below is metadata only for podman users: podman defaults
# to the OCI image format, which has no HEALTHCHECK field, and silently drops it
# ("HEALTHCHECK is not supported for OCI image format and will be ignored").
# `podman build --format docker` or docker/buildx honours it. The compose stacks
# therefore keep an explicit `healthcheck:` of their own so the behaviour does
# not depend on the builder's output format.
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=3 \
  CMD /bin/bash -c 'exec 3<>/dev/tcp/127.0.0.1/${PORT:-8080}; printf "GET /health HTTP/1.0\r\n\r\n" >&3; head -1 <&3 | grep -q " 200 "'

# IMAGE_TIMEOUT_SECONDS is intentionally NOT set here: unset/-1 selects the
# built-in 300s image first-byte budget, which is the correct production value.
# Do not set it to 0 (that disables the watchdog) unless you also accept that an
# image request can hang until the client gives up.
# SHUTDOWN_GRACE_SECONDS likewise stays at its 90s default in the image; the
# deploy manifests raise it to stay >= IMAGE_TIMEOUT_SECONDS (see
# deploy/README.md). Any override belongs in the environment, not in the image.
ENV PORT=8080 \
    DATABASE_URL=/data/gateway.db

EXPOSE 8080

USER gateway

ENTRYPOINT ["/usr/local/bin/gateway"]
