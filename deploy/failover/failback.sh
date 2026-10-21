#!/usr/bin/env bash
# Rejoin an old primary as a replica after recovery.
# Run on the recovered host. Wipes local Postgres data and re-bases from
# the current primary via pg_basebackup.
#
# Usage: failback.sh <current-primary-ipv6> <repl-password> [--force]
# The wipe is destructive: the script refuses unless the cluster is stopped,
# and asks for confirmation unless --force / FAILBACK_FORCE=1 is set.
set -euo pipefail

PRIMARY="${1:-}"
REPL_PW="${2:-}"
FORCE="${FAILBACK_FORCE:-0}"
if [[ "${3:-}" == "--force" || "${1:-}" == "--force" ]]; then
  FORCE=1
fi
if [[ -z "$PRIMARY" || -z "$REPL_PW" || "$PRIMARY" == "--force" ]]; then
  echo "Usage: $0 <current-primary-ipv6> <repl-password> [--force]" >&2
  exit 1
fi

PGDATA="/var/lib/postgresql/17/main"
if [[ ! -d "$PGDATA" ]]; then
  echo "failback: $PGDATA does not exist — wrong host or cluster version?" >&2
  exit 1
fi

echo "Stopping gateway + postgres"
systemctl stop ai-gateway 2>/dev/null || podman stop ai-gateway-prod 2>/dev/null || true
pg_ctlcluster 17 main stop || true

# Safety: never wipe a cluster that is still running.
if pg_ctlcluster 17 main status >/dev/null 2>&1; then
  echo "failback: cluster 17/main still running — refusing to wipe $PGDATA" >&2
  exit 1
fi

if [[ "$FORCE" != 1 ]]; then
  echo "WARNING: this permanently deletes all data in $PGDATA and re-bases"
  echo "         it as a replica of $PRIMARY."
  read -rp "Type WIPE to continue: " confirm
  [[ "$confirm" == "WIPE" ]] || { echo "Aborted."; exit 1; }
fi

# Back up postgresql.conf / pg_hba.conf (wiped with the data dir) so the
# rejoin does not lose local auth/tuning settings.
mkdir -p /var/backups/ai-gateway
if [[ -f "$PGDATA/postgresql.conf" ]]; then
  cp "$PGDATA/postgresql.conf" "/var/backups/ai-gateway/pg-conf-$(date +%Y%m%d-%H%M%S).conf"
fi
if [[ -f "$PGDATA/pg_hba.conf" ]]; then
  cp "$PGDATA/pg_hba.conf" "/var/backups/ai-gateway/pg-hba-$(date +%Y%m%d-%H%M%S).conf"
fi

echo "Wiping $PGDATA (configs backed up to /var/backups/ai-gateway)"
rm -rf "${PGDATA:?}"/*
chown postgres:postgres "$PGDATA"
chmod 700 "$PGDATA"

echo "Re-basing from $PRIMARY"
sudo -u postgres PGPASSWORD="$REPL_PW" pg_basebackup -h "$PRIMARY" -p 5433 -U replicator -D "$PGDATA" -Fp -Xs -P -R
pg_ctlcluster 17 main start
sleep 3
recovery="$(sudo -u postgres psql -p 5433 -tAc "SELECT pg_is_in_recovery();" | tr -d '[:space:]')"
if [[ "$recovery" != "t" ]]; then
  echo "failback: cluster reports pg_is_in_recovery()=false — expected a replica!" >&2
  echo "failback: investigate before pointing anything at this host." >&2
  exit 1
fi
echo "Rejoined as replica (pg_is_in_recovery=t). Point the gateway back at the primary or keep local read-only."
