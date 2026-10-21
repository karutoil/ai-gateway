#!/usr/bin/env bash
# Second Cloudflare Tunnel connector on standby — OPT-IN.
#
# WARNING: only run this if the tunnel is configured to serve ONLY the AI
# gateway. A token-based (remotely-managed) tunnel has ONE ingress table
# shared by all connectors; with multiple services behind it, a standby
# connector can capture requests for services it does not run, breaking
# them. The token is read from /etc/systemd/system/cloudflared.service.
# Debian 13 (trixie) has no cloudflared apt repo yet; install via .deb.
set -euo pipefail

STANDBY="root@2402:1060:21ed:0:be24:11ff:fe5f:6dd1"
SSH_KEY="$HOME/.ssh/id_ed25519_20260909"
# accept-new: first connection records the host key; later MITM/key changes
# are rejected. (-o StrictHostKeyChecking=no would accept ANY key, forever.)
SSH="ssh -6 -o ConnectTimeout=15 -o StrictHostKeyChecking=accept-new -i $SSH_KEY"

# Pin the cloudflared release (e.g. CLOUDFLARED_VERSION=2025.10.0). The
# .deb is checksum-verified before dpkg -i; no unverified installs.
CLOUDFLARED_VERSION="${CLOUDFLARED_VERSION:-latest}"
if [[ "$CLOUDFLARED_VERSION" == "latest" ]]; then
  REL_URL="https://github.com/cloudflare/cloudflared/releases/latest/download"
else
  REL_URL="https://github.com/cloudflare/cloudflared/releases/download/${CLOUDFLARED_VERSION}"
fi

TOKEN="$(sudo grep -oP '(?<=--token )\S+' /etc/systemd/system/cloudflared.service | head -n 1 || true)"
if [[ -z "$TOKEN" ]]; then
  echo "Could not read tunnel token from cloudflared.service" >&2
  exit 1
fi

# Write token to a protected file via stdin so it never appears in ps output.
printf '%s' "$TOKEN" | $SSH "$STANDBY" "cat > /etc/ai-gateway/cloudflared-token && chmod 600 /etc/ai-gateway/cloudflared-token &&
curl -fsSL -o /tmp/cloudflared.deb $REL_URL/cloudflared-linux-amd64.deb &&
curl -fsSL -o /tmp/cloudflared-checksums.txt $REL_URL/cloudflared-checksums.txt &&
want=\"\$(awk '\$2 ~ /cloudflared-linux-amd64.deb\$/ {print \$1}' /tmp/cloudflared-checksums.txt | head -n 1)\" &&
[ -n \"\$want\" ] &&
echo \"\$want  /tmp/cloudflared.deb\" | sha256sum -c - &&
dpkg -i /tmp/cloudflared.deb && rm -f /tmp/cloudflared.deb /tmp/cloudflared-checksums.txt &&
cat >/etc/systemd/system/cloudflared.service <<'UNIT'
[Unit]
Description=cloudflared (standby replica)
After=network-online.target
Wants=network-online.target
[Service]
TimeoutStartSec=15
Type=notify
EnvironmentFile=/etc/ai-gateway/cloudflared-token-env
ExecStart=/usr/bin/cloudflared --no-autoupdate tunnel run --token \${CLOUDFLARED_TOKEN}
Restart=on-failure
RestartSec=5s
[Install]
WantedBy=multi-user.target
UNIT
TOKEN=\$(cat /etc/ai-gateway/cloudflared-token)
printf 'CLOUDFLARED_TOKEN=%s\n' \"\$TOKEN\" > /etc/ai-gateway/cloudflared-token-env
chmod 600 /etc/ai-gateway/cloudflared-token-env /etc/ai-gateway/cloudflared-token
rm -f /etc/ai-gateway/cloudflared-token
systemctl daemon-reload
systemctl enable --now cloudflared
sleep 3
systemctl is-active cloudflared
"

echo "Standby tunnel connector running. Cloudflare edge now has two replicas."
echo "Verify: https://ai.karutoil.site/health (stop main gateway briefly to test)"
