#!/usr/bin/env bash
# Install the standby stack on toil-daddy. Run on main; it SSHes to standby.
# Usage: ./deploy/failover/setup-standby.sh
set -euo pipefail

STANDBY="root@2402:1060:21ed:0:be24:11ff:fe5f:6dd1"
SSH_KEY="$HOME/.ssh/id_ed25519_20260909"
SSH="ssh -6 -o ConnectTimeout=15 -o StrictHostKeyChecking=no -i $SSH_KEY"

if [[ ! -f "$SSH_KEY" ]]; then
  echo "Missing $SSH_KEY" >&2
  exit 1
fi

echo "Provisioning standby $STANDBY"
$SSH "$STANDBY" "set -eux
apt-get update
apt-get install -y postgresql-17 postgresql-client redis-server curl podman sqlite3
systemctl enable --now postgresql redis-server
pg_lsclusters
redis-cli ping
id gateway || useradd --system --home /var/lib/ai-gateway --shell /usr/sbin/nologin gateway
mkdir -p /var/lib/ai-gateway /opt/ai-gateway /etc/ai-gateway
chown gateway:gateway /var/lib/ai-gateway
"

echo "Standby base packages installed."
echo "Next: run pg_basebackup (see README) then cloudflared-replica.sh"
