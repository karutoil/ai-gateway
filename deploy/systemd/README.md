# Production deploy — restart resilience

This directory contains deployment assets for running the gateway so that
**restarts don't drop user connections**.

## Why restarts used to drop connections

The gateway binds its own TCP socket. The socket lives and dies with the
process, so during every restart there is a window (process exit → new
process bind) where new connections are refused. API clients (OpenAI /
Anthropic SDKs) usually retry, but non-retrying callers see connection
errors, and retry storms amplify load right when the service comes back.

## What makes reconnects work now

1. **Socket activation (systemd)** — `ai-gateway.socket` owns the listening
   socket; the gateway inherits it via the `LISTEN_FDS` protocol
   (`internal` change in `cmd/gateway/main.go`, `inheritListener`). The
   socket keeps accepting while the gateway restarts; queued connections are
   served by the new process. Restart window: ~0 dropped connections.
2. **Persistent secrets** — `JWT_SECRET` / `MASTER_KEY` (env file or
   auto-generated key files next to the DB) survive restarts, so dashboard
   sessions stay valid and provider credentials keep decrypting. Users do
   not need to re-login after a deploy.
3. **Graceful drain** — on SIGTERM the gateway stops accepting and lets
   in-flight streams finish within `SHUTDOWN_GRACE_SECONDS` (default 90),
   then force-closes stragglers and flushes the SQLite WAL.
   `TimeoutStopSec=95` in the service unit gives systemd the same window.
4. **Readiness probes** — `GET /ready` (503 while the DB is down) and
   `GET /health` (liveness) let orchestrators gate traffic correctly.

## Install (systemd host)

```bash
sudo useradd --system --home /var/lib/ai-gateway --shell /usr/sbin/nologin gateway
sudo mkdir -p /var/lib/ai-gateway && sudo chown gateway:gateway /var/lib/ai-gateway
sudo cp bin/gateway /usr/local/bin/gateway
sudo cp deploy/systemd/ai-gateway.socket deploy/systemd/ai-gateway.service /etc/systemd/system/

# Secrets (adjust to your deployment)
sudo install -m 600 /dev/null /etc/ai-gateway.env
sudo tee -a /etc/ai-gateway.env >/dev/null <<EOF
ADMIN_PASSWORD=<strong password>
MASTER_KEY=$(openssl rand -hex 32)
JWT_SECRET=$(openssl rand -hex 32)
DATABASE_URL=/var/lib/ai-gateway/gateway.db
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now ai-gateway.socket
```

The service starts on first connection. Verify activation:

```bash
systemctl status ai-gateway.socket
sudo journalctl -u ai-gateway -f          # should log "socket activation: serving on inherited listener"
```

Now `sudo systemctl restart ai-gateway` drops no connections: while the old
process drains and the new one boots, the socket keeps queueing requests.

### Podman/Docker (no systemd socket handoff)

The production compose stack (`podman-compose.prod.yml`) uses
`restart: unless-stopped`. Container restarts still have a brief
refuse-window; put a tunnel/reverse proxy with retry in front (Cloudflare
Tunnel does this by default) for the same effect. The gateway's graceful
drain minimizes how long the window is open.

## Verifying zero-drop restarts

```bash
# While the gateway runs under socket activation:
while true; do curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/health; sleep 0.2; done
# In another terminal:
sudo systemctl restart ai-gateway
# Expect a continuous stream of 200s — no 000/connection-refused lines.
```
