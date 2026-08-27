#!/usr/bin/env bash
set -e
BASE=http://127.0.0.1:8797
CKFF_API_KEY=${CKFF_API_KEY:?set CKFF_API_KEY to your upstream provider key}
TOKEN=$(curl -s -X POST $BASE/api/auth/login -H "Content-Type: application/json" -d '{"password":"admin123"}' | jq -r .token)
echo "token ok"
curl -s $BASE/api/providers -H "Authorization: Bearer $TOKEN" | jq .
PROV=$(curl -s $BASE/api/providers -H "Authorization: Bearer $TOKEN" | jq -r '.[] | select(.name=="ckff") | .id')
if [ -z "$PROV" ] || [ "$PROV" = "null" ]; then
  echo "creating ckff provider"
  curl -s -X POST $BASE/api/providers -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d "{\"name\":\"ckff\",\"type\":\"anthropic\",\"base_url\":\"https://ckff.dev\",\"api_key\":\"$CKFF_API_KEY\"}" | jq .
  PROV=$(curl -s $BASE/api/providers -H "Authorization: Bearer $TOKEN" | jq -r '.[] | select(.name=="ckff") | .id')
fi
echo "prov $PROV"
echo "discover"
curl -s -X POST $BASE/api/providers/$PROV/discover -H "Authorization: Bearer $TOKEN" | jq .
echo "list provider models"
curl -s "$BASE/api/provider-models?provider_id=$PROV" -H "Authorization: Bearer $TOKEN" | jq '.total, .data[0]' | head -c 3000
echo ""
