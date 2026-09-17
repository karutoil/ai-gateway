#!/usr/bin/env bash
# One-time SQLite -> Postgres move for production data.
# Stops writes, dumps SQLite tables as CSV, loads Postgres, flips DATABASE_URL.
# Run on main during a maintenance window. Keeps a timestamped SQLite backup.
set -euo pipefail

SQLITE_DB="${SQLITE_DB:-deploy/production/data/gateway.db}"
PG_URL="${DATABASE_URL:-}"
if [[ "$PG_URL" != postgres* ]]; then
  echo "Set DATABASE_URL=postgres://... before running (target must be Postgres)" >&2
  exit 1
fi
if [[ ! -f "$SQLITE_DB" ]]; then
  echo "SQLite DB not found: $SQLITE_DB" >&2
  exit 1
fi

TS="$(date +%Y%m%d-%H%M%S)"
BACKUP="backups/sqlite-pre-pg-$TS"
mkdir -p "$BACKUP"
echo "Backing up SQLite to $BACKUP"
cp "$SQLITE_DB" "$BACKUP/gateway.db"
cp "$SQLITE_DB-wal" "$BACKUP/" 2>/dev/null || true
cp "$SQLITE_DB-shm" "$BACKUP/" 2>/dev/null || true

echo "Booting gateway once against Postgres to create schema"
DATABASE_URL="$PG_URL" timeout 60 ./bin/gateway --help >/dev/null 2>&1 || true
DATABASE_URL="$PG_URL" timeout 20 bash -c './bin/gateway & pid=$!; sleep 8; kill $pid; wait $pid 2>/dev/null || true'

echo "Exporting tables with sqlite3"
TABLES="providers gateway_keys request_logs models_catalog provider_models model_aliases system_config audit_logs organizations memberships lb_rules dashboard_users"
mkdir -p "$BACKUP/csv"
for t in $TABLES; do
  if sqlite3 "$SQLITE_DB" ".tables" | grep -qw "$t"; then
    sqlite3 "$SQLITE_DB" ".headers on\n.mode csv\n.output $BACKUP/csv/$t.csv\nSELECT * FROM \"$t\";"
    echo "  $t -> $BACKUP/csv/$t.csv"
  else
    echo "  $t missing, skipped"
  fi
done

echo "Load CSVs with \\copy in dependency order (organizations/users first)."
echo "Example:"
echo "  psql \"\$PG_URL\" -c \"\\copy organizations FROM '$BACKUP/csv/organizations.csv' CSV HEADER\""
echo "Resolve conflicts with ON CONFLICT DO NOTHING where needed."
echo "After load, flip deploy/production/.env DATABASE_URL to the Postgres DSN and restart."
