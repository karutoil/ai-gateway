#!/usr/bin/env bash
# Promote standby to primary when main is down.
# Run on standby. Flips local Postgres to read-write and points the local
# gateway at localhost, then restarts it.
set -euo pipefail

if [[ "${1:-}" == "--help" ]]; then
  echo "Usage: promote.sh [--force]"
  echo "Promotes local Postgres replica and flips gateway DATABASE_URL to localhost."
  exit 0
fi

echo "Checking local replica state"
if sudo -u postgres pg_isready -p 5432 >/dev/null 2>&1; then
  PGPORT=5432
elif sudo -u postgres pg_isready -p 5433 >/dev/null 2>&1; then
  PGPORT=5433
else
  echo "Local Postgres is not running" >&2
  exit 1
fi
echo "Postgres on $PGPORT"

if [[ -f /var/lib/postgresql/17/main/standby.signal ]]; then
  echo "Promoting replica"
  sudo -u postgres pg_ctlcluster 17 main promote
  sleep 3
else
  echo "No standby.signal: already primary (or not a replica)"
fi

ENV_FILE="/etc/ai-gateway/env"
if grep -q "^DATABASE_URL=postgres" "$ENV_FILE" 2>/dev/null; then
  # Reuse the gateway DB password already in the env; only retarget the host.
  GW_PW="$(grep '^DATABASE_URL=' "$ENV_FILE" | sed -E 's#^DATABASE_URL=postgres://[^:]+:([^@]+)@.*#\1#')"
  if [[ -n "$GW_PW" ]]; then
    sudo sed -i -E "s#^DATABASE_URL=.*#DATABASE_URL=postgres://gateway:${GW_PW}@localhost:$PGPORT/gateway?sslmode=disable#" "$ENV_FILE"
    echo "DATABASE_URL now points at local promoted primary (port $PGPORT)"
  else
    echo "Could not parse gateway password from $ENV_FILE; fix DATABASE_URL manually:"
    echo "  DATABASE_URL=postgres://gateway:<password>@localhost:$PGPORT/gateway?sslmode=disable"
  fi
else
  echo "Add to $ENV_FILE:"
  echo "  DATABASE_URL=postgres://gateway:<password>@localhost:$PGPORT/gateway?sslmode=disable"
fi

echo "Restarting gateway"
systemctl restart ai-gateway 2>/dev/null || podman restart ai-gateway-prod 2>/dev/null || true
sleep 3
curl -fsS http://localhost:8090/health || curl -fsS http://localhost:8080/health
echo
# Dedicated ai-gateway tunnel: safe to start here (ingress covers only the
# gateway hostname; other services stay on the home tunnel).
systemctl enable --now cloudflared-gateway 2>/dev/null || true
echo "Promoted. Verify https://ai.karutoil.site/health"
