#!/usr/bin/env bash
# Rejoin an old primary as a replica after recovery.
# Run on the recovered host. Wipes local Postgres data and re-bases from
# the current primary via pg_basebackup.
# Usage: failback.sh <current-primary-ipv6> <repl-password>
set -euo pipefail

PRIMARY="${1:-}"
REPL_PW="${2:-}"
if [[ -z "$PRIMARY" || -z "$REPL_PW" ]]; then
  echo "Usage: $0 <current-primary-ipv6> <repl-password>" >&2
  exit 1
fi

echo "Stopping gateway + postgres"
systemctl stop ai-gateway 2>/dev/null || podman stop ai-gateway-prod 2>/dev/null || true
pg_ctlcluster 17 main stop || true
rm -rf /var/lib/postgresql/17/main/*
chown postgres:postgres /var/lib/postgresql/17/main
chmod 700 /var/lib/postgresql/17/main

echo "Re-basing from $PRIMARY"
sudo -u postgres PGPASSWORD="$REPL_PW" pg_basebackup -h "$PRIMARY" -p 5433 -U replicator -D /var/lib/postgresql/17/main -Fp -Xs -P -R
pg_ctlcluster 17 main start
sleep 3
sudo -u postgres psql -p 5433 -c "SELECT pg_is_in_recovery();"

echo "Rejoined as replica. Point the gateway back at the primary or keep local read-only."
