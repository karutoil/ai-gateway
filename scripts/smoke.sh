#!/usr/bin/env bash
# Live smoke test: health, login, providers, keys, 401 handling.
# Usage: make smoke            # builds bin/gateway, starts one on a smoke port
#        BASE=http://localhost:8989 ./scripts/smoke.sh   # against a running gateway
#
# The script starts its OWN gateway on a dedicated ephemeral port (override
# with SMOKE_PORT) instead of assuming whatever listens on :8080 is this
# gateway — the old "adopt whatever is on 8080" behavior made the smoke suite
# run against unrelated local services and fail with confusing parse errors.
set -euo pipefail

pick_free_port() {
  if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
  else
    echo 8091
  fi
}

SMOKE_PORT=${SMOKE_PORT:-$(pick_free_port)}
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
BASE=${BASE:-http://localhost:$SMOKE_PORT}

command -v curl >/dev/null 2>&1 || { echo "ERROR: curl is required" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "ERROR: jq is required" >&2; exit 1; }

echo "== AI Gateway Smoke =="
echo "BASE=$BASE"

# ours reports true when /health answers AND parses as OUR gateway (has a
# version + status field) — never trust an arbitrary listener.
ours() {
  curl -sf -m 2 "$BASE/health" 2>/dev/null | jq -e '.status == "ok" and .version' >/dev/null 2>&1
}

GATEWAY_PID=""
cleanup() {
  if [ -n "$GATEWAY_PID" ]; then
    kill "$GATEWAY_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

if ours; then
  echo "Gateway already running at $BASE (verified /health shape)"
else
  if [ -n "${BASE_OVERRIDE:-}" ]; then
    echo "ERROR: BASE was overridden but no healthy AI Gateway answered at $BASE/health" >&2
    exit 1
  fi
  echo "Starting gateway from bin/gateway on :$SMOKE_PORT..."
  if [ ! -x "$ROOT/bin/gateway" ]; then
    echo "bin/gateway not found — building (needs CGO + gcc for go-sqlite3)..."
    (cd "$ROOT" && go build -o bin/gateway ./cmd/gateway)
  fi
  ADMIN_PASSWORD=admin123 PORT=$SMOKE_PORT "$ROOT/bin/gateway" &
  GATEWAY_PID=$!
  ok=""
  for _ in $(seq 1 30); do
    if ours; then ok=1; break; fi
    if ! kill -0 "$GATEWAY_PID" 2>/dev/null; then
      echo "ERROR: gateway process exited during startup" >&2
      exit 1
    fi
    sleep 1
  done
  if [ -z "$ok" ]; then
    echo "ERROR: gateway did not become healthy within 30s" >&2
    exit 1
  fi
fi

echo "[1] Health"
curl -s "$BASE/health" | jq .

echo "[2] Login"
TOKEN=$(curl -s -X POST "$BASE/api/auth/login" -H "Content-Type: application/json" -d '{"username":"admin","password":"admin123"}' | jq -r .token)
if [ -z "$TOKEN" ] || [ "$TOKEN" = "null" ]; then
  echo "ERROR: login failed (no token) — is ADMIN_PASSWORD still admin123?" >&2
  exit 1
fi
echo "TOKEN=${TOKEN:0:20}..."

echo "[3] Create mock provider (will not call real upstream, uses httptest in go tests, but for smoke we create dummy)"
# create a dummy provider pointing to mock - for real do openai
curl -s -X POST "$BASE/api/providers" -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{"name":"openai-smoke","type":"openai","base_url":"https://api.openai.com/v1","api_key":"sk-test-dummy"}' | jq . || true

echo "[4] Create gateway key"
KEY_JSON=$(curl -s -X POST "$BASE/api/keys" -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{"name":"smoke-test"}')
echo "$KEY_JSON" | jq .
GW=$(echo "$KEY_JSON" | jq -r .key)
if [ -z "$GW" ] || [ "$GW" = "null" ]; then
  echo "ERROR: key creation failed" >&2
  exit 1
fi
echo "GW=${GW:0:12}..."

echo "[5] Test gateway auth without key (should 401)"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/chat/completions" -H "Content-Type: application/json" -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}')
if [ "$CODE" != "401" ]; then
  echo "ERROR: expected 401 without a gateway key, got $CODE" >&2
  exit 1
fi
echo "401 ok"

echo "[6] List providers"
curl -s "$BASE/api/providers" -H "Authorization: Bearer $TOKEN" | jq .; echo

echo "[7] List keys"
curl -s "$BASE/api/keys" -H "Authorization: Bearer $TOKEN" | jq .; echo

echo "[8] Audit trail (admin)"
curl -s "$BASE/api/audit?limit=5" -H "Authorization: Bearer $TOKEN" | jq 'length' >/dev/null && echo "audit ok"

echo "[9] Logs pagination headers"
curl -s -D - -o /dev/null "$BASE/api/logs?limit=5" -H "Authorization: Bearer $TOKEN" | grep -i '^x-total-count:' && echo "logs headers ok"

echo "Smoke done — if you set real provider key, run:"
echo "curl $BASE/v1/chat/completions -H \"Authorization: Bearer \$GW\" -H \"Content-Type: application/json\" -d '{\"model\":\"gpt-4o-mini\",\"messages\":[{\"role\":\"user\",\"content\":\"Hello\"}]}'"
echo "curl $BASE/v1/messages -H \"Authorization: Bearer \$GW\" -d '{\"model\":\"claude-3-5-sonnet-20241022\",\"max_tokens\":100,\"messages\":[{\"role\":\"user\",\"content\":\"Hello\"}]}'"
