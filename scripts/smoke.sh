#!/usr/bin/env bash
set -e

BASE=${BASE:-http://localhost:8080}
echo "== AI Gateway Smoke — Phase 1 =="
echo "BASE=$BASE"

# start gateway if not running
if ! curl -sf $BASE/health > /dev/null; then
  echo "Starting gateway on :8080..."
  ADMIN_PASSWORD=admin123 PORT=8080 /tmp/gateway &
  PID=$!
  sleep 2
  trap "kill $PID" EXIT
else
  echo "Gateway already running"
fi

echo "[1] Health"
curl -s $BASE/health | jq . || curl -s $BASE/health; echo

echo "[2] Login"
TOKEN=$(curl -s -X POST $BASE/api/auth/login -H "Content-Type: application/json" -d '{"password":"admin123"}' | jq -r .token)
echo "TOKEN=${TOKEN:0:20}..."

echo "[3] Create mock provider (will not call real upstream, uses httptest in go tests, but for smoke we create dummy)"
# create a dummy provider pointing to mock - for real do openai
curl -s -X POST $BASE/api/providers -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{"name":"openai-smoke","type":"openai","base_url":"https://api.openai.com/v1","api_key":"sk-test-dummy"}' | jq . || true

echo "[4] Create gateway key"
KEY_JSON=$(curl -s -X POST $BASE/api/keys -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{"name":"smoke-test"}')
echo $KEY_JSON | jq . || echo $KEY_JSON
GW=$(echo $KEY_JSON | jq -r .key)
echo "GW=${GW:0:12}..."

echo "[5] Test gateway auth without key (should 401)"
curl -s -X POST $BASE/v1/chat/completions -H "Content-Type: application/json" -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' | head -c 200; echo

echo "[6] List providers"
curl -s $BASE/api/providers -H "Authorization: Bearer $TOKEN" | jq .; echo

echo "[7] List keys"
curl -s $BASE/api/keys -H "Authorization: Bearer $TOKEN" | jq .; echo

echo "Smoke done — if you set real provider key, run:"
echo "curl $BASE/v1/chat/completions -H \"Authorization: Bearer \$GW\" -H \"Content-Type: application/json\" -d '{\"model\":\"gpt-4o-mini\",\"messages\":[{\"role\":\"user\",\"content\":\"Hello\"}]}'"
echo "curl $BASE/v1/messages -H \"Authorization: Bearer \$GW\" -d '{\"model\":\"claude-3-5-sonnet-20241022\",\"max_tokens\":100,\"messages\":[{\"role\":\"user\",\"content\":\"Hello\"}]}'"
