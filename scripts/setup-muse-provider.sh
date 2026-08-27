#!/usr/bin/env bash
set -e
# Setup Muse Spark 1.2 provider via ckff.dev
# Usage: ./scripts/setup-muse-provider.sh [--gateway http://localhost:8989] [--key sk-...]

GATEWAY_URL=${GATEWAY_URL:-http://localhost:8989}
ADMIN_PASSWORD=${ADMIN_PASSWORD:-admin123}
PROVIDER_NAME=${PROVIDER_NAME:-ckff-muse}
BASE_URL=${BASE_URL:-https://ckff.dev}
MODEL=${MODEL:-muse-spark-1.2-contributor}
API_KEY=${CKFF_API_KEY:-${MUSE_API_KEY:?set CKFF_API_KEY or MUSE_API_KEY to your upstream provider key}}

echo "Setting up provider $PROVIDER_NAME @ $BASE_URL for model $MODEL"
echo "Gateway: $GATEWAY_URL"

# Login
TOKEN=$(curl -s -X POST $GATEWAY_URL/api/auth/login -H "Content-Type: application/json" -d "{\"password\":\"$ADMIN_PASSWORD\"}" | jq -r .token)
if [ -z "\$TOKEN" ] || [ "\$TOKEN" = "null" ]; then
  TOKEN=$(curl -s -X POST $GATEWAY_URL/api/auth/login -H "Content-Type: application/json" -d '{"username":"admin","password":"'"$ADMIN_PASSWORD"'"}' | jq -r .token)
fi
if [ -z "\$TOKEN" ] || [ "\$TOKEN" = "null" ]; then
  echo "✗ Admin login failed"
  exit 1
fi
echo "✓ Admin login"

# Check existing
EXISTING=$(curl -s $GATEWAY_URL/api/providers -H "Authorization: Bearer $TOKEN" | jq -r ".[] | select(.name==\"$PROVIDER_NAME\") | .id" | head -n1)
if [ -n "\$EXISTING" ] && [ "\$EXISTING" != "null" ]; then
  echo "✓ Provider $PROVIDER_NAME exists: $EXISTING"
  echo "  Updating health..."
  curl -s -X POST $GATEWAY_URL/api/providers/$EXISTING/discover -H "Authorization: Bearer $TOKEN" | jq . 2>/dev/null | head -n 20 || true
else
  echo "→ Creating provider $PROVIDER_NAME"
  RESP=$(curl -s -X POST $GATEWAY_URL/api/providers -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d "{\"name\":\"$PROVIDER_NAME\",\"type\":\"anthropic\",\"base_url\":\"$BASE_URL\",\"api_key\":\"$API_KEY\"}")
  echo "\$RESP" | jq . 2>/dev/null || echo "\$RESP"
  ID=$(echo "\$RESP" | jq -r .id 2>/dev/null)
  if [ -n "\$ID" ] && [ "\$ID" != "null" ]; then
    echo "✓ Created $ID"
    echo "  Discovering models..."
    curl -s -X POST $GATEWAY_URL/api/providers/$ID/discover -H "Authorization: Bearer $TOKEN" | jq . 2>/dev/null | head -n 20 || true
    EXISTING=\$ID
  else
    echo "✗ Failed to create"
    exit 1
  fi
fi

# Verify model
echo ""
echo "Verifying model $MODEL..."
FOUND=$(curl -s "$GATEWAY_URL/api/provider-models?limit=100" -H "Authorization: Bearer $TOKEN" | jq -r ".data[] | select(.model_id==\"$MODEL\") | .model_id" | head -n1)
if [ "\$FOUND" = "\$MODEL" ]; then
  echo "✓ Model $MODEL found in provider_models"
  curl -s "$GATEWAY_URL/api/provider-models?limit=100" -H "Authorization: Bearer $TOKEN" | jq ".data[] | select(.model_id==\"$MODEL\") | {model_id, display_name, reasoning, tool_call, reasoning_type, reasoning_levels}" | head -n 20
else
  echo "⚠ Model $MODEL not yet in provider_models, trying discover..."
  curl -s -X POST $GATEWAY_URL/api/providers/$EXISTING/discover -H "Authorization: Bearer $TOKEN" | jq . | head -n 20
  sleep 2
  FOUND2=$(curl -s "$GATEWAY_URL/api/provider-models?limit=100" -H "Authorization: Bearer $TOKEN" | jq -r ".data[] | select(.model_id==\"$MODEL\") | .model_id" | head -n1)
  if [ "\$FOUND2" = "\$MODEL" ]; then
    echo "✓ Model found after discover"
  else
    echo "⚠ Still not found - check provider health:"
    curl -s $GATEWAY_URL/api/providers -H "Authorization: Bearer $TOKEN" | jq ".[] | select(.id==\"$EXISTING\") | {name, health_status, last_health}"
  fi
fi

# Create gateway key for testing
echo ""
echo "Creating gateway key..."
KEY=$(curl -s -X POST $GATEWAY_URL/api/keys -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{"name":"muse-setup-'$(date +%s)'"}' | jq -r .key)
if [ -n "\$KEY" ] && [ "\$KEY" != "null" ]; then
  echo "✓ Gateway key: ${KEY:0:20}..."
  echo ""
  echo "Test it:"
  echo "  curl $GATEWAY_URL/v1/chat/completions -H \"Authorization: Bearer \$KEY\" -H \"Content-Type: application/json\" -d '{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":20}'"
else
  echo "✗ Failed to create gateway key"
fi
