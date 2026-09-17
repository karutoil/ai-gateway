#!/usr/bin/env bash
# Standby watchdog: checks main, promotes only after repeated failures.
# Manual by default; set AUTO_PROMOTE=true in /etc/ai-gateway/watchdog.env
# to promote automatically. Logs to journald when run under systemd.
set -euo pipefail

PRIMARY_HOST="${PRIMARY_HOST:-}"
PRIMARY_GATEWAY_URL="${PRIMARY_GATEWAY_URL:-}"
PUBLIC_HEALTH_URL="${PUBLIC_HEALTH_URL:-https://ai.karutoil.site/health}"
# Reverse-tunnel Postgres check (main pushes tunnel to standby, so standby
# reaches the primary at localhost:15434 even when inbound IPv6 to main is
# firewalled). When set, a reachable primary DB means healthy regardless of
# ping/direct-gateway checks.
PRIMARY_DB_HOST="${PRIMARY_DB_HOST:-}"
PRIMARY_DB_PORT="${PRIMARY_DB_PORT:-15434}"
# Inbound IPv6 to residential main is firewalled; skip ping there.
SKIP_PING="${SKIP_PING:-false}"
STATE_DIR="${STATE_DIR:-/var/lib/ai-gateway}"
FAIL_FILE="$STATE_DIR/failover-fails"
FAIL_THRESHOLD="${FAIL_THRESHOLD:-3}"
AUTO_PROMOTE="${AUTO_PROMOTE:-false}"

if [[ -z "$PRIMARY_HOST" ]]; then
  echo "watchdog: set PRIMARY_HOST in /etc/ai-gateway/watchdog.env (main IPv6)" >&2
  exit 2
fi
if [[ -z "$PRIMARY_GATEWAY_URL" ]]; then
  PRIMARY_GATEWAY_URL="http://[$PRIMARY_HOST]:8090/health"
fi

fails=0
if [[ -f "$FAIL_FILE" ]]; then
  fails="$(cat "$FAIL_FILE" 2>/dev/null || echo 0)"
fi

check_ok=true
if [[ -n "$PRIMARY_DB_HOST" ]]; then
  if pg_isready -h "$PRIMARY_DB_HOST" -p "$PRIMARY_DB_PORT" >/dev/null 2>&1; then
    echo 0 >"$FAIL_FILE"
    echo "watchdog: primary DB reachable via tunnel"
    exit 0
  fi
  echo "watchdog: primary DB $PRIMARY_DB_HOST:$PRIMARY_DB_PORT unreachable"
  check_ok=false
fi
if [[ "$SKIP_PING" != true ]]; then
  if ! timeout 8 ping -6 -c 1 "$PRIMARY_HOST" >/dev/null 2>&1; then
    echo "watchdog: ping $PRIMARY_HOST failed"
    check_ok=false
  fi
fi
if ! timeout 8 curl -fsS "$PRIMARY_GATEWAY_URL" >/dev/null 2>&1; then
  echo "watchdog: primary gateway $PRIMARY_GATEWAY_URL unhealthy"
  check_ok=false
fi
if ! timeout 8 curl -fsS "$PUBLIC_HEALTH_URL" >/dev/null 2>&1; then
  echo "watchdog: public $PUBLIC_HEALTH_URL unhealthy"
  check_ok=false
fi

if [[ "$check_ok" == true ]]; then
  echo 0 >"$FAIL_FILE"
  echo "watchdog: primary healthy"
  exit 0
fi

fails=$((fails + 1))
echo "$fails" >"$FAIL_FILE"
echo "watchdog: failure $fails/$FAIL_THRESHOLD"

if [[ "$fails" -ge "$FAIL_THRESHOLD" ]]; then
  if [[ "$AUTO_PROMOTE" == true ]]; then
    echo "watchdog: threshold reached, promoting"
    PROMOTE="$(dirname "$0")/promote.sh"
    [[ -x "$PROMOTE" ]] || PROMOTE="/opt/ai-gateway/deploy/failover/promote.sh"
    "$PROMOTE" --force
    echo 0 >"$FAIL_FILE"
  else
    echo "watchdog: threshold reached (manual mode). Run promote.sh on standby."
  fi
  exit 1
fi
exit 1
