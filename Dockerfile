# AI Gateway — production image.
#
# Build:   docker build -t ai-gateway .
# Run:     docker run -p 8080:8080 -v gateway-data:/data --env-file .env ai-gateway
#
# Notes:
# - CGO is kept ENABLED because the SQLite driver (mattn/go-sqlite3) compiles its
#   C amalgamation at build time. The builder stage ships gcc on Debian bookworm.
# - Runtime is debian:bookworm-slim with CA certificates so outbound provider
#   calls verify TLS against the system bundle copied from the builder.
# - No HEALTHCHECK directive: the image is deliberately slim and curl/wget are
#   not installed. Use your orchestrator's HTTP probe against GET /health
#   (liveness) or GET /ready (checks DB ping) instead.

FROM golang:1.25 AS builder

WORKDIR /src

# Cache module downloads separately from source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Default CGO_ENABLED=1 (required by sqlite3); static flags must NOT be added.
RUN go build -trimpath -o /out/gateway ./cmd/gateway

FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# Non-root runtime user.
RUN useradd --uid 10001 --user-group --system --shell /usr/sbin/nologin gateway

# Data volume for the SQLite database (+ auto-generated key files).
RUN mkdir -p /data && chown -R gateway:gateway /data
VOLUME ["/data"]

COPY --from=builder /out/gateway /usr/local/bin/gateway

ENV PORT=8080 \
    DATABASE_URL=/data/gateway.db

EXPOSE 8080

USER gateway

ENTRYPOINT ["/usr/local/bin/gateway"]
