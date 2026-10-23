#!/usr/bin/env bash
# Promote standby to primary when main is down.
# Run on standby. Flips local Postgres to read-write and points the local
# gateway at localhost, then restarts it.
#
# Split-brain guard: unless --force is passed, the primary is re-checked one
# last time (tunnel DB probe, ping, direct gateway URL). Promoting while the
# primary is still up leaves two writable primaries — this script refuses.
# After promoting, fence or shut down the old primary's gateway before it
# recovers, then rejoin it with failback.sh.
set -euo pipefail

FORCE=0
if [[ "${1:-}" == "--force" ]]; then
  FORCE=1
elif [[ "${1:-}" == "--help" ]]; then
  echo "Usage: promote.sh [--force]"
  echo "Promotes local Postgres replica and flips gateway DATABASE_URL to localhost."
  echo "--force skips the 'primary is really down' safety check."
  exit 0
elif [[ -n "${1:-}" ]]; then
  echo "Usage: promote.sh [--force]" >&2
  exit 2
fi

# Load watchdog config for the final primary liveness check (optional).
WATCHDOG_ENV="/etc/ai-gateway/watchdog.env"
if [[ -r "$WATCHDOG_ENV" ]]; then
  # shellcheck disable=SC1090
  source "$WATCHDOG_ENV"
fi
PRIMARY_HOST="${PRIMARY_HOST:-}"
PRIMARY_GATEWAY_URL="${PRIMARY_GATEWAY_URL:-}"
PRIMARY_DB_HOST="${PRIMARY_DB_HOST:-}"
PRIMARY_DB_PORT="${PRIMARY_DB_PORT:-15434}"

if [[ "$FORCE" != 1 ]]; then
  if [[ -z "$PRIMARY_HOST" && -z "$PRIMARY_DB_HOST" ]]; then
    echo "promote: PRIMARY_HOST/PRIMARY_DB_HOST unknown (no $WATCHDOG_ENV)." >&2
    echo "promote: pass --force to skip the primary liveness check." >&2
    exit 1
  fi
  primary_alive=0
  if [[ -n "$PRIMARY_DB_HOST" ]] && pg_isready -h "$PRIMARY_DB_HOST" -p "$PRIMARY_DB_PORT" >/dev/null 2>&1; then
    primary_alive=1
  elif [[ -n "$PRIMARY_GATEWAY_URL" ]] && timeout 8 curl -fsS "$PRIMARY_GATEWAY_URL" >/dev/null 2>&1; then
    primary_alive=1
  elif [[ -n "$PRIMARY_HOST" ]] && timeout 8 ping -6 -c 1 "$PRIMARY_HOST" >/dev/null 2>&1; then
    primary_alive=1
  fi
  if [[ "$primary_alive" == 1 ]]; then
    echo "promote: primary still answers (DB/gateway/ping) — aborting to avoid split-brain." >&2
    echo "promote: if you are certain it is down, re-run with --force." >&2
    exit 1
  fi
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

# The gateway's env file has TWO spellings in this stack: the failover tooling
# (this script, sync-gateway.sh, podman-compose.standby.yml) uses
# /etc/ai-gateway/env, while the shipped systemd unit reads
# /etc/ai-gateway.env. Rewriting only one of them left the promoted service
# pointed at the dead primary through the old tunnel while this script printed
# success — the outage simply continued. Resolve the file the running service
# actually reads, and update every existing copy so both deployment shapes work.
ENV_FILES=()
for candidate in /etc/ai-gateway.env /etc/ai-gateway/env; do
  if sudo test -f "$candidate"; then
    ENV_FILES+=("$candidate")
  fi
done
if [[ ${#ENV_FILES[@]} -eq 0 ]]; then
  ENV_FILES=("/etc/ai-gateway.env")
fi
ENV_FILE="${ENV_FILES[0]}"
if sudo grep -q "^DATABASE_URL=postgres" "$ENV_FILE" 2>/dev/null; then
  # Reuse the gateway DB password already in the env; only retarget the host.
  # The password is never passed on any command line (no ps/argv exposure):
  # it is read from the env file under sudo and written to a 0600 temp file
  # that awk reads directly.
  GW_PW="$(sudo grep '^DATABASE_URL=' "$ENV_FILE" | sed -E 's#^DATABASE_URL=postgres://[^:]+:([^@]+)@.*#\1#')" || GW_PW=""
  if [[ -n "$GW_PW" ]]; then
    umask 077
    REPL_FILE="$(mktemp)"
    printf 'DATABASE_URL=postgres://gateway:%s@localhost:%s/gateway?sslmode=disable\n' "$GW_PW" "$PGPORT" >"$REPL_FILE"
    NEW_ENV="$(mktemp)"
    rewrote=0
    for target in "${ENV_FILES[@]}"; do
      if ! sudo grep -q "^DATABASE_URL=postgres" "$target" 2>/dev/null; then
        continue
      fi
      if sudo cat "$target" | awk -v replfile="$REPL_FILE" '
        BEGIN { line = ""; have = 0; while ((getline l < replfile) > 0) { line = l; have = 1 } }
        have != 1 { exit 3 }
        /^DATABASE_URL=/ { print line; replaced = 1; next }
        { print }
        END { if (replaced != 1) exit 4 }
      ' >"$NEW_ENV"; then
        sudo cp "$NEW_ENV" "$target"
        sudo chmod 600 "$target"
        rewrote=$((rewrote + 1))
      else
        echo "Could not rewrite $target; fix DATABASE_URL manually:" >&2
        echo "  DATABASE_URL=postgres://gateway:<password>@localhost:$PGPORT/gateway?sslmode=disable" >&2
      fi
    done
    if [[ "$rewrote" -gt 0 ]]; then
      echo "DATABASE_URL now points at local promoted primary (port $PGPORT) in: ${ENV_FILES[*]}"
    else
      echo "Could not rewrite $ENV_FILE; fix DATABASE_URL manually:" >&2
      echo "  DATABASE_URL=postgres://gateway:<password>@localhost:$PGPORT/gateway?sslmode=disable" >&2
    fi
    rm -f "$REPL_FILE" "$NEW_ENV"
  else
    echo "Could not parse gateway password from $ENV_FILE; fix DATABASE_URL manually:" >&2
    echo "  DATABASE_URL=postgres://gateway:<password>@localhost:$PGPORT/gateway?sslmode=disable" >&2
  fi
else
  echo "Add to $ENV_FILE:"
  echo "  DATABASE_URL=postgres://gateway:<password>@localhost:$PGPORT/gateway?sslmode=disable"
fi

echo "Restarting gateway"
if ! systemctl restart ai-gateway 2>/dev/null; then
  podman restart ai-gateway-prod 2>/dev/null || {
    echo "promote: could not restart the gateway service — promotion FAILED" >&2
    exit 1
  }
fi

# Wait for the promoted gateway to actually serve. A promotion that leaves
# nothing answering is a failed promotion, and it MUST exit non-zero: the
# watchdog writes its "promoted" marker (which suppresses all further attempts)
# only when this script succeeds, so exiting 0 here marked a dead standby as
# handled and the outage silently continued.
ready=0
for _ in $(seq 1 30); do
  if curl -fsS http://localhost:8090/health >/dev/null 2>&1 || curl -fsS http://localhost:8080/health >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 2
done
if [[ "$ready" != 1 ]]; then
  echo "promote: gateway did not become healthy within 60s — promotion FAILED (check: journalctl -u ai-gateway)" >&2
  exit 1
fi
echo
# Dedicated ai-gateway tunnel: safe to start here (ingress covers only the
# gateway hostname; other services stay on the home tunnel).
systemctl enable --now cloudflared-gateway 2>/dev/null || true
echo "Promoted. Verify https://ai.karutoil.site/health"
echo "SPLIT-BRAIN REMINDER: stop/fence the old primary's gateway, then rejoin it with failback.sh."
