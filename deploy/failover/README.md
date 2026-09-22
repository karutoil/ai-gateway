# AI Gateway — Failover (main + standby)

Two-host active-passive setup. Main serves today. Standby (`toil-daddy`)
takes over when main is offline.

## How failover works

- **Domain (`https://ai.karutoil.site`):** Dedicated Cloudflare Tunnel
  (`ai-gateway`, token in `/etc/ai-gateway-cloudflared-gw.env` on main,
  `/etc/ai-gateway/cloudflared-gateway.env` on standby) serving ONLY the
  gateway, separate from the home tunnel that carries all other services.
  - Main runs `cloudflared-gateway.service` (enabled) + the home
    `cloudflared.service` (enabled) — two independent tunnels.
  - Standby runs the same unit DISABLED. Starting it during failover is
    safe: the dedicated tunnel's ingress only covers the gateway hostname,
    so other services are never affected.
  - The old shared-token standby connector (`cloudflared.service` on
    standby) is disabled and must stay off — a token-based tunnel has one
    global ingress table, so its connector could capture requests for
    services it does not run.
- **Database:** Postgres streaming replication. Main runs the primary in
  the production compose stack (container `ai-gateway-postgres`, host port
  **5434** — 5433 is the native cluster, so the container uses 5434).
  Standby runs a native physical replica. Inbound IPv6 to main is
  firewalled, so replication rides a reverse SSH tunnel: main holds
  `pg-replica-tunnel.service` (autossh `-R 15434:127.0.0.1:5434` to
  standby) and the replica streams from `127.0.0.1:15434`.
- **App:** Main runs the compose gateway (SQLite until cutover, then
  Postgres). Standby runs `/opt/ai-gateway/gateway` via systemd against
  the primary through the tunnel; on failover it flips to localhost.
  Secrets (`ADMIN_PASSWORD` / `MASTER_KEY` / `JWT_SECRET`) are identical
  on both hosts.
- **Cache/rate limits:** Main uses the compose Redis (`redis://redis:6379`);
  standby uses native Redis at `127.0.0.1:6379`. Either can fall back to
  memory.

## Layout

```
deploy/failover/
  README.md                  this file
  setup-primary.sh           reference: container primary is already up
  setup-standby.sh           already ran: base packages on standby
  sync-gateway.sh            copy binary + production .env to standby
  migrate-sqlite-to-postgres.sh  reference: data already copied (dry run)
  promote.sh                 run on standby when main is down
  failback.sh                rejoin old main as replica after recovery
  watchdog.sh                health check + optional auto-promote
  systemd/
    ai-gateway-failover-watchdog.service
    ai-gateway-failover-watchdog.timer
  podman-compose.standby.yml standby gateway + redis (Postgres native)
  cloudflared-replica.sh     already ran (uses .deb on Debian 13)
scripts/migrate-sqlite-pg.go row-by-row SQLite -> Postgres copier
```

## Current state

- Container Postgres primary on main (`5434`) holds a full copy of
  production data (7 providers, 5 keys, ~21k request logs). Prod gateway
  still serves from SQLite.
- Standby streams (`pg_stat_replication: streaming/async`), serves its own
  gateway on `:8090` from the primary through the tunnel, and holds a
  second tunnel connector. Watchdog runs every 30s in manual mode.

## Manual cutover (you run this; gateway restart required)

Production still reads SQLite. The container Postgres already has your
data, but a final delta copy + flip needs a brief restart:

```bash
cd /home/karutoil/ai-gateway
sqlite3 deploy/production/data/gateway.db ".backup '/tmp/gw-final.db'"
GW_PW=$(grep '^POSTGRES_PASSWORD=' deploy/production/.env | cut -d= -f2)
PG="postgres://gateway:${GW_PW}@127.0.0.1:5434/gateway?sslmode=disable"
go run ./scripts/migrate-sqlite-pg.go /tmp/gw-final.db "$PG"
# Copy ends with a per-table verify (exit 0 = every source row present).
# "target has extra rows" on models_catalog is expected: booting the gateway
# against Postgres syncs a fresher models.dev catalog into the target.
rm /tmp/gw-final.db
# flip + rebuild + restart (seconds of downtime)
sed -i -E "s#^DATABASE_URL=.*#DATABASE_URL=postgres://gateway:${GW_PW}@postgres:5432/gateway?sslmode=disable#" deploy/production/.env
cd deploy/production && podman compose -f podman-compose.prod.yml up -d --build
sleep 10; curl -fsS http://localhost:8090/health
```

## Failover / failback

- Main down: on standby run `/opt/ai-gateway/promote.sh` (or set
  `AUTO_PROMOTE=true` in `/etc/ai-gateway/watchdog.env`), then
  `systemctl enable --now cloudflared-gateway` on standby to bring the
  dedicated gateway tunnel up (automatic DNS failover within seconds;
  other services stay on the home tunnel and are unaffected).
- Main back: on old main run `failback.sh` to rejoin as replica. On
  standby `systemctl disable --now cloudflared-gateway` to hand the
  gateway hostname back to main.
- Watchdog checks the primary through the tunnel (`pg_isready
  127.0.0.1:15434`); ping is skipped because inbound IPv6 is firewalled.

## Security follow-up

Rotate `POSTGRES_PASSWORD` and the `replicator` password after cutover:
both appeared in local session logs during setup. Rotating means new
`ALTER ROLE` passwords plus updating `deploy/production/.env`,
`/etc/ai-gateway/env` on standby, and the replica `primary_conninfo`.
