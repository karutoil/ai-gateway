#!/usr/bin/env bash
set -e
BASE=http://127.0.0.1:8791
DB=/tmp/gateway15_test.db
rm -f $DB
PORT=8791 ADMIN_PASSWORD=admin123 DATABASE_URL=$DB bin/gateway &
PID=$!
sleep 4
echo "== health =="
curl -s $BASE/health | jq .
echo "== login =="
TOKEN=$(curl -s -X POST $BASE/api/auth/login -H "Content-Type: application/json" -d '{"password":"admin123"}' | jq -r .token)
echo "token ok len=${#TOKEN}"
echo "== catalog status =="
curl -s $BASE/api/models/status -H "Authorization: Bearer $TOKEN" | jq .
echo "== catalog list gpt =="
curl -s "$BASE/api/models/catalog?q=gpt&limit=2" -H "Authorization: Bearer $TOKEN" | jq .data[0]
echo "== catalog get specific =="
curl -s $BASE/api/models/catalog/openai%2Fgpt-4o -H "Authorization: Bearer $TOKEN" | jq '{id, context_window, max_output, input_cost, output_cost, reasoning}'
echo "== alias create =="
curl -s -X POST $BASE/api/models/aliases -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{"alias":"fast","target":"openai/gpt-4o-mini"}' | jq .
curl -s $BASE/api/models/aliases -H "Authorization: Bearer $TOKEN" | jq .
echo "== stats =="
curl -s $BASE/api/stats -H "Authorization: Bearer $TOKEN" | jq .
echo "== provider health =="
curl -s $BASE/api/providers -H "Authorization: Bearer $TOKEN" | jq .
echo "== key with rpm =="
K=$(curl -s -X POST $BASE/api/keys -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{"name":"test"}' | jq -r .key)
KID=$(curl -s $BASE/api/keys -H "Authorization: Bearer $TOKEN" | jq -r .[0].id)
echo "key $KID"
curl -s -X PUT $BASE/api/keys/$KID/rate-limit -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{"rpm":120}' | jq .
# mock upstream for cost test
python3 scripts/mock_upstream2.py &
MOCK_PID=$!
sleep 1
curl -s -X POST $BASE/api/providers -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{"name":"mock","type":"openai","base_url":"http://127.0.0.1:8788/v1","api_key":"sk-mock"}' | jq .
echo "== chat with cost (non-stream) =="
curl -s $BASE/v1/chat/completions -H "Authorization: Bearer $K" -H "Content-Type: application/json" -d '{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' | jq .
sleep 0.5
curl -s $BASE/api/logs -H "Authorization: Bearer $TOKEN" | jq '.[0] | {model, prompt_tokens, completion_tokens, cost_usd}'
echo "== alias test fast -> openai/gpt-4o-mini =="
curl -s $BASE/v1/chat/completions -H "Authorization: Bearer $K" -H "Content-Type: application/json" -d '{"model":"fast","messages":[{"role":"user","content":"hi"}]}' | jq .
curl -s $BASE/api/logs -H "Authorization: Bearer $TOKEN" | jq '.[0] | {model}'
echo "== rate limit test (rpm 2) =="
curl -s -X PUT $BASE/api/keys/$KID/rate-limit -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{"rpm":2}' | jq .
for i in 1 2 3; do echo -n "req $i: "; curl -s -w "%{http_code}" -o /dev/null $BASE/v1/models -H "Authorization: Bearer $K"; echo ""; done
echo "== v1/models enriched =="
curl -s $BASE/v1/models -H "Authorization: Bearer $K" | jq '.data[0]'
kill $PID $MOCK_PID || true
echo "done"
