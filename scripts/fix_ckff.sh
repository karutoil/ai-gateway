#!/usr/bin/env bash
set -e
BASE=http://127.0.0.1:8787
CKFF_API_KEY=${CKFF_API_KEY:?set CKFF_API_KEY to your upstream provider key}
TOKEN=$(curl -s --max-time 5 $BASE/api/auth/login -H "Content-Type: application/json" -d '{"password":"admin123"}' | jq -r .token)
echo "token ok"
curl -s --max-time 5 $BASE/api/providers -H "Authorization: Bearer $TOKEN" | jq .
OLD=$(curl -s --max-time 5 $BASE/api/providers -H "Authorization: Bearer $TOKEN" | jq -r '.[] | select(.name=="ckff") | .id')
if [ ! -z "$OLD" ] && [ "$OLD" != "null" ]; then
  echo "deleting $OLD"
  curl -s --max-time 5 -X DELETE $BASE/api/providers/$OLD -H "Authorization: Bearer $TOKEN" > /dev/null
fi
curl -s --max-time 5 -X POST $BASE/api/providers -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d "{\"name\":\"ckff\",\"type\":\"anthropic\",\"base_url\":\"https://ckff.dev\",\"api_key\":\"$CKFF_API_KEY\"}" | jq .
PROV=$(curl -s --max-time 5 $BASE/api/providers -H "Authorization: Bearer $TOKEN" | jq -r '.[] | select(.name=="ckff") | .id')
echo "new prov $PROV"
curl -s --max-time 10 -X POST $BASE/api/providers/$PROV/discover -H "Authorization: Bearer $TOKEN" | jq .
GW=$(curl -s --max-time 5 -X POST $BASE/api/keys -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{"name":"playground-fix"}' | jq -r .key)
echo "gw $GW" | cut -c1-30
echo "test anthropic"
curl -s --max-time 10 -X POST $BASE/v1/messages -H "Authorization: Bearer $GW" -H "Content-Type: application/json" -d '{"model":"muse-spark-1.2-contributor","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}' | head -c 500
echo ""
echo "test openai"
curl -s --max-time 10 -X POST $BASE/v1/chat/completions -H "Authorization: Bearer $GW" -H "Content-Type: application/json" -d '{"model":"muse-spark-1.2-contributor","messages":[{"role":"user","content":"hi"}],"max_tokens":20}' | head -c 500
echo ""
