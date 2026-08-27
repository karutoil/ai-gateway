# PI harness — Muse Spark 1.2 via AI Gateway (dedicated test dir)
#
# The gateway is STRICT: anthropic models only work on /v1/messages.
# This dir registers a PI Anthropic provider pointing at the local gateway
# (http://localhost:8989) so PI can be used as a real harness.

Gateway: http://localhost:8989
Provider: ckff-muse (anthropic @ https://ckff.dev)
Models: muse-spark-1.2-contributor, muse-spark-1.1 (via gateway-anthropic provider)
Api: anthropic-messages

## Quick start

# 1. Ensure gateway is running (from repo root):
PORT=8989 ./bin/gateway &
curl -sf http://localhost:8989/health && echo ok

# 2. Ensure provider + gateway key (or reuse /tmp/gw-key-pi):
ADMIN_TOKEN=$(curl -s -X POST http://localhost:8989/api/auth/login -H "Content-Type: application/json" -d '{"password":"admin123"}' | python3 -c "import sys,json;print(json.load(sys.stdin)['token'])")
curl -s -X POST http://localhost:8989/api/keys -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" -d '{"name":"pi-harness-'$(date +%s)'"}' | python3 -m json.tool

# 3. Run PI through the gateway (Anthropic native) — real write+exec prompt:
GATEWAY_API_KEY=sk-gw-xxx GATEWAY_URL=http://localhost:8989 \
  pi -e /home/karutoil/ai-gateway/pi-harness-test/gateway-anthropic-provider.js \
     --provider gateway-anthropic --model muse-spark-1.2-contributor \
     -p "In /home/karutoil/ai-gateway/pi-harness-test, create hello-pi.txt containing 'hello from pi via gateway' and then run: cat hello-pi.txt"

# 4. Verify gateway logs (request was Anthropic, not translated):
curl -s "http://localhost:8989/api/logs?limit=5" -H "Authorization: Bearer $ADMIN_TOKEN" | python3 -m json.tool | head -n 80

## What this tests

- `api: "anthropic-messages"` + `baseUrl: http://localhost:8989` means PI sends Anthropic /v1/messages natively.
- The gateway's strict policy: openai endpoints (chat/completions, completions, responses) 400 with "anthropic model" for this model.
- Muse Spark 1.2's real read/write/exec tools round-trip through the gateway to ckff.dev.

## Files

- gateway-anthropic-provider.js — PI extension registering the gateway as an Anthropic provider
- hello-pi.txt — created by the PI harness run above (proof of write+exec)
