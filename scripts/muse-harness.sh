#!/usr/bin/env bash
set -e

# Muse Spark 1.2 Live Harness Runner
# Ensures provider exists, builds harness-muse, and runs it against live gateway
# Usage: ./scripts/muse-harness.sh [--gateway http://localhost:8989] [--rebuild-gateway]

GATEWAY_URL=${GATEWAY_URL:-http://localhost:8989}
MODEL=${MODEL:-muse-spark-1.2-contributor}
PROVIDER=${PROVIDER:-ckff-muse}
CKFF_API_KEY=${CKFF_API_KEY:?set CKFF_API_KEY to your upstream provider key}
ADMIN_PASSWORD=${ADMIN_PASSWORD:-admin123}

echo "╔════════════════════════════════════════════════════════════╗"
echo "║  Muse Harness — Setup & Run                             ║"
echo "╚════════════════════════════════════════════════════════════╝"
echo "Gateway: $GATEWAY_URL"
echo "Model:   $MODEL"
echo ""

# 1. Ensure gateway is running, start if needed
if ! curl -sf $GATEWAY_URL/health > /dev/null 2>&1; then
  echo "⚠ Gateway not running on $GATEWAY_URL, starting..."
  if [ -f bin/gateway ]; then
    PORT=$(echo $GATEWAY_URL | sed -E 's|.*:([0-9]+).*|\1|')
    if [ "$PORT" = "$GATEWAY_URL" ]; then PORT=8989; fi
    echo "  Starting bin/gateway on :$PORT"
    nohup env PORT=$PORT ADMIN_PASSWORD=$ADMIN_PASSWORD ./bin/gateway > /tmp/gateway-muse.log 2>&1 &
    sleep 3
    if ! curl -sf $GATEWAY_URL/health > /dev/null; then
      echo "  Failed to start gateway. Logs:"
      cat /tmp/gateway-muse.log | tail -n 30
      exit 1
    fi
    echo "  Gateway started (pid \$(ss -tlnp | grep $PORT | head -n1))"
  else
    echo "  bin/gateway not found, run: make build"
    exit 1
  fi
else
  echo "✓ Gateway is running"
fi

# 2. Ensure provider exists
echo ""
echo "── Provider Setup ─────────────────────────────────"
ADMIN_TOKEN=$(curl -s -X POST $GATEWAY_URL/api/auth/login -H "Content-Type: application/json" -d "{\"password\":\"$ADMIN_PASSWORD\"}" | jq -r .token)
if [ -z "\$ADMIN_TOKEN" ] || [ "\$ADMIN_TOKEN" = "null" ]; then
  ADMIN_TOKEN=$(curl -s -X POST $GATEWAY_URL/api/auth/login -H "Content-Type: application/json" -d '{"username":"admin","password":"'"$ADMIN_PASSWORD"'"}' | jq -r .token)
fi
if [ -z "\$ADMIN_TOKEN" ] || [ "\$ADMIN_TOKEN" = "null" ]; then
  echo "  ✗ Admin login failed"
  exit 1
fi
echo "  ✓ Admin login"

EXISTING=$(curl -s $GATEWAY_URL/api/providers -H "Authorization: Bearer $ADMIN_TOKEN" | jq -r ".[] | select(.name==\"$PROVIDER\" or .name==\"ckff\") | .name" | head -n1)
if [ -n "\$EXISTING" ]; then
  echo "  ✓ Provider exists: $EXISTING"
  PROV_ID=$(curl -s $GATEWAY_URL/api/providers -H "Authorization: Bearer $ADMIN_TOKEN" | jq -r ".[] | select(.name==\"$EXISTING\") | .id" | head -n1)
  echo "    id: $PROV_ID"
else
  echo "  → Creating provider $PROVIDER (anthropic @ https://ckff.dev)"
  RESP=$(curl -s -X POST $GATEWAY_URL/api/providers -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" -d "{\"name\":\"$PROVIDER\",\"type\":\"anthropic\",\"base_url\":\"https://ckff.dev\",\"api_key\":\"$CKFF_API_KEY\"}")
  echo "    $RESP" | jq . 2>/dev/null || echo "    $RESP"
  PROV_ID=$(echo "$RESP" | jq -r .id 2>/dev/null)
  if [ -n "\$PROV_ID" ] && [ "\$PROV_ID" != "null" ]; then
    echo "  Discovering models for $PROV_ID..."
    curl -s -X POST $GATEWAY_URL/api/providers/$PROV_ID/discover -H "Authorization: Bearer $ADMIN_TOKEN" | jq . 2>/dev/null | head -n 20
  fi
fi

# Verify model in provider_models
echo ""
echo "  Checking model $MODEL in provider_models..."
FOUND=$(curl -s "$GATEWAY_URL/api/provider-models?limit=100" -H "Authorization: Bearer $ADMIN_TOKEN" | grep -c "$MODEL" || true)
if [ "\$FOUND" -gt 0 ]; then
  echo "  ✓ Model found in provider_models"
else
  echo "  ⚠ Model not found in provider_models (may need manual discover)"
  curl -s -X POST $GATEWAY_URL/api/providers/$PROV_ID/discover -H "Authorization: Bearer $ADMIN_TOKEN" | jq . 2>/dev/null | head -n 5 || true
fi

# 3. Build harness-muse
echo ""
echo "── Build Harness ──────────────────────────────────"
if [ ! -f /tmp/harness-muse ] || [ cmd/harness-muse/main.go -nt /tmp/harness-muse ]; then
  echo "  Building cmd/harness-muse..."
  go build -o /tmp/harness-muse ./cmd/harness-muse
  echo "  ✓ Built /tmp/harness-muse"
else
  echo "  ✓ /tmp/harness-muse up to date"
fi

# 4. Run harness-muse
echo ""
echo "── Run Harness (Real Provider: $MODEL) ────────────"
GATEWAY_URL=$GATEWAY_URL MODEL=$MODEL PROVIDER=$PROVIDER ADMIN_PASSWORD=$ADMIN_PASSWORD /tmp/harness-muse
HARNESS_EXIT=\$?
if [ \$HARNESS_EXIT -ne 0 ]; then
  echo ""
  echo "✗ Harness failed with exit $HARNESS_EXIT"
  exit \$HARNESS_EXIT
fi

# 5. Also run Python official SDK harness if available
echo ""
echo "── Python Official SDK Harness (optional) ─────────"
if command -v python3 >/dev/null 2>&1 && [ -f scripts/harness_official_sdks.py ]; then
  if python3 -c "import openai, anthropic, requests" 2>/dev/null; then
    echo "  Running scripts/harness_official_sdks.py..."
    GATEWAY_URL=$GATEWAY_URL ADMIN_PASSWORD=$ADMIN_PASSWORD python3 scripts/harness_official_sdks.py || echo "  ⚠ Python harness had failures (see above)"
  else
    echo "  Skipping Python harness (openai/anthropic not installed)"
    echo "  Install: pip install openai anthropic requests"
  fi
else
  echo "  Skipping Python harness"
fi

# 6. Run go live e2e if requested
if [ "\$1" = "--with-go-e2e" ]; then
  echo ""
  echo "── Go Live E2E (internal/e2e) ─────────────────────"
  GATEWAY_URL=$GATEWAY_URL go test -tags=live -run TestMuseLive -v ./internal/e2e 2>&1 | tail -n 100
fi

echo ""
echo "╔════════════════════════════════════════════════════════════╗"
echo "║  Done — Check gateway logs: /tmp/gateway-muse.log      ║"
echo "║  Metrics: $GATEWAY_URL/metrics                          ║"
echo "╚════════════════════════════════════════════════════════════╝"
