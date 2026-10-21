#!/usr/bin/env bash
# Copy gateway binary + production env to standby (secrets stay mode 0600).
# Run on main. Never prints secret values.
set -euo pipefail

STANDBY="root@2402:1060:21ed:0:be24:11ff:fe5f:6dd1"
SSH_KEY="$HOME/.ssh/id_ed25519_20260909"
# accept-new: first connection records the host key; later MITM/key changes
# are rejected. (-o StrictHostKeyChecking=no would accept ANY key, forever.)
SSH="ssh -6 -o ConnectTimeout=15 -o StrictHostKeyChecking=accept-new -i $SSH_KEY"
SCP="scp -6 -o StrictHostKeyChecking=accept-new -i $SSH_KEY"

make build
echo "Pushing binary + compose to standby"
$SSH "$STANDBY" "mkdir -p /opt/ai-gateway/deploy/failover /etc/ai-gateway"
$SCP bin/gateway "$STANDBY:/opt/ai-gateway/gateway"
$SCP deploy/failover/podman-compose.standby.yml "$STANDBY:/opt/ai-gateway/podman-compose.yml"
$SSH "$STANDBY" "chmod +x /opt/ai-gateway/gateway"

echo "Pushing production env ( ADMIN_PASSWORD / MASTER_KEY / JWT_SECRET )"
# Stream the env into place under umask 077 so it is never world-readable on
# the remote (scp would leave a default-mode file until the later chmod).
$SSH "$STANDBY" "umask 077 && cat > /etc/ai-gateway/env" < deploy/production/.env
$SSH "$STANDBY" "chown root:gateway /etc/ai-gateway/env 2>/dev/null || true; chmod 600 /etc/ai-gateway/env"

echo "Sync complete. Binary version:"
$SSH "$STANDBY" "/opt/ai-gateway/gateway --help 2>&1 | head -n 3 || true"
