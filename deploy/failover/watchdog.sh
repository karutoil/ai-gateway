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

# The timer fires every 30s but a full check round can take up to ~24s
# (three 8s timeouts). A lock prevents overlapping runs from double-counting
# failures or promoting twice (split-brain risk).
mkdir -p "$STATE_DIR" 2>/dev/null || true
LOCK_FILE="$STATE_DIR/watchdog.lock"
exec 9>"$LOCK_FILE" || { echo "watchdog: cannot open $LOCK_FILE" >&2; exit 2; }
flock -n 9 || { echo "watchdog: another instance is running" >&2; exit 0; }

# One-shot marker: auto-promote must fire at most once per outage. Cleared
# when the primary becomes reachable again (rejoin via failback.sh).
PROMOTED_FILE="$STATE_DIR/promoted"

fails=0
if [[ -f "$FAIL_FILE" ]]; then
  fails="$(cat "$FAIL_FILE" 2>/dev/null || echo 0)"
fi
# Counter file may be corrupt/truncated — never let it break arithmetic.
[[ "$fails" =~ ^[0-9]+$ ]] || fails=0

# A MISSING PROBE TOOL IS NOT AN OUTAGE.
#
# pg_isready/ping/curl used to run as bare commands, so "binary not on PATH"
# (exit 127) was indistinguishable from "primary unreachable" and incremented the
# failure counter — three rounds of a missing `pg_isready` promoted the standby
# while the primary was still accepting writes (two writable primaries; the
# later failback then wipes the old primary's rows). Refuse to run at all
# instead, without touching FAIL_FILE.
for tool in curl pg_isready; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "watchdog: required tool '$tool' not found on PATH — refusing to judge primary health" >&2
    exit 2
  fi
done
if [[ "$SKIP_PING" != true ]] && ! command -v ping >/dev/null 2>&1; then
  echo "watchdog: required tool 'ping' not found on PATH (set SKIP_PING=true to skip ICMP)" >&2
  exit 2
fi

check_ok=true
if [[ -n "$PRIMARY_DB_HOST" ]]; then
  if pg_isready -h "$PRIMARY_DB_HOST" -p "$PRIMARY_DB_PORT" >/dev/null 2>&1; then
    echo 0 >"$FAIL_FILE"
    if [[ -f "$PROMOTED_FILE" ]]; then
      rm -f "$PROMOTED_FILE"
      echo "watchdog: primary DB reachable via tunnel again (run failback.sh on old primary)"
    else
      echo "watchdog: primary DB reachable via tunnel"
    fi
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
  if [[ -f "$PROMOTED_FILE" ]]; then
    echo "watchdog: already promoted; waiting for primary recovery (run failback.sh on old primary)"
    exit 1
  fi
  if [[ "$AUTO_PROMOTE" == true ]]; then
    echo "watchdog: threshold reached, promoting"
    PROMOTE="$(dirname "$0")/promote.sh"
    [[ -x "$PROMOTE" ]] || PROMOTE="/opt/ai-gateway/deploy/failover/promote.sh"
    # NO --force. --force is precisely the flag that skips promote.sh's own
    # primary-liveness re-check, and this watchdog's inputs can be stale (a
    # tunnel blip, a false ICMP negative, a public-health false alarm). Promoting
    # while the primary is alive produces two writable primaries; the later
    # failback re-bases the old primary and destroys everything written to it in
    # the meantime. The re-check is the only split-brain guard in the stack, so
    # auto-promotion must never disable it.
    if "$PROMOTE"; then
      touch "$PROMOTED_FILE"
      echo 0 >"$FAIL_FILE"
    else
      echo "watchdog: promote.sh failed — manual intervention required" >&2
    fi
  else
    echo "watchdog: threshold reached (manual mode). Run promote.sh on standby."
  fi
  exit 1
fi
exit 1
