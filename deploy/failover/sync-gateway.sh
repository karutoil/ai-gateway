#!/usr/bin/env bash
# Copy gateway binary + production env to standby (secrets stay mode 0600).
# Run on main. Never prints secret values.
set -euo pipefail

STANDBY="root@2402:1060:21ed:0:be24:11ff:fe5f:6dd1"
SSH_KEY="$HOME/.ssh/id_ed25519_20260909"
SSH="ssh -6 -o ConnectTimeout=15 -o StrictHostKeyChecking=no -i $SSH_KEY"
SCP="scp -6 -o StrictHostKeyChecking=no -i $SSH_KEY"

make build
echo "Pushing binary + compose to standby"
$SSH "$STANDBY" "mkdir -p /opt/ai-gateway/deploy/failover /etc/ai-gateway"
$SCP bin/gateway "$STANDBY:/opt/ai-gateway/gateway"
$SCP deploy/failover/podman-compose.standby.yml "$STANDBY:/opt/ai-gateway/podman-compose.yml"
$SSH "$STANDBY" "chmod +x /opt/ai-gateway/gateway"

echo "Pushing production env ( ADMIN_PASSWORD / MASTER_KEY / JWT_SECRET )"
$SCP deploy/production/.env "$STANDBY:/etc/ai-gateway/env"
$SSH "$STANDBY" "chmod 600 /etc/ai-gateway/env && chown root:gateway /etc/ai-gateway/env || chmod 600 /etc/ai-gateway/env"

echo "Sync complete. Binary version:"
$SSH "$STANDBY" "/opt/ai-gateway/gateway --help 2>&1 | head -n 3 || true"
