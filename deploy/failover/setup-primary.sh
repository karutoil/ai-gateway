#!/usr/bin/env bash
# Configure Postgres primary on main for streaming replication.
# Run once on main as root or with sudo.
set -euo pipefail

PG_CONF="/etc/postgresql/17/main/postgresql.conf"
PG_HBA="/etc/postgresql/17/main/pg_hba.conf"
REPL_USER="replicator"
GW_DB="gateway"
GW_USER="gateway"

if [[ $EUID -ne 0 ]]; then
  echo "Run as root (sudo $0)" >&2
  exit 1
fi

# Standby IPv6 is passed as $1 or prompted. Example:
#   sudo ./setup-primary.sh 2402:1060:21ed:0:be24:11ff:fe5f:6dd1
STANDBY_IP="${1:-}"
if [[ -z "$STANDBY_IP" ]]; then
  read -rp "Standby IPv6 address: " STANDBY_IP
fi

echo "Configuring Postgres primary for standby $STANDBY_IP"

# WAL + replication slots.
grep -q "^wal_level" "$PG_CONF" || echo "wal_level = replica" >>"$PG_CONF"
sed -i -E "s/^#?wal_level\s*=.*/wal_level = replica/" "$PG_CONF"
grep -q "^max_wal_senders" "$PG_CONF" || echo "max_wal_senders = 5" >>"$PG_CONF"
sed -i -E "s/^#?max_wal_senders\s*=.*/max_wal_senders = 5/" "$PG_CONF"
grep -q "^max_replication_slots" "$PG_CONF" || echo "max_replication_slots = 5" >>"$PG_CONF"
sed -i -E "s/^#?max_replication_slots\s*=.*/max_replication_slots = 5/" "$PG_CONF"
grep -q "^listen_addresses" "$PG_CONF" || echo "listen_addresses = 'localhost'" >>"$PG_CONF"
sed -i -E "s/^#?listen_addresses\s*=.*/listen_addresses = 'localhost,::'/" "$PG_CONF"

# Replication + gateway credentials.
REPL_PW="$(openssl rand -hex 24)"
if sudo -u postgres psql -p 5433 -tAc "SELECT 1 FROM pg_roles WHERE rolname='$REPL_USER'" | grep -q 1; then
  echo "Role $REPL_USER exists, rotating password"
fi
sudo -u postgres psql -p 5433 -c "DO \$\$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='$REPL_USER') THEN CREATE ROLE $REPL_USER WITH REPLICATION LOGIN; END IF; END \$\$;"
sudo -u postgres psql -p 5433 -c "ALTER ROLE $REPL_USER WITH PASSWORD '$REPL_PW';"
sudo -u postgres psql -p 5433 -c "SELECT 1 FROM pg_database WHERE datname='$GW_DB'" | grep -q 1 || sudo -u postgres psql -p 5433 -c "CREATE DATABASE $GW_DB OWNER $GW_USER;"
sudo -u postgres psql -p 5433 -c "GRANT ALL ON DATABASE $GW_DB TO $GW_USER;"

# pg_hba: standby may only replicate; gateway user connects locally + from standby.
if ! grep -q "$STANDBY_IP" "$PG_HBA"; then
  cat >>"$PG_HBA" <<EOF
host    replication     $REPL_USER      $STANDBY_IP/128          scram-sha-256
host    $GW_DB          $GW_USER         $STANDBY_IP/128          scram-sha-256
EOF
fi

pg_ctlcluster 17 main restart
sleep 2
pg_lsclusters

echo
echo "Primary ready. Save these for setup-standby.sh (store securely, do not commit):"
echo "  REPL_USER=$REPL_USER"
echo "  REPL_PASSWORD=<shown once below>"
echo "  PRIMARY_HOST=<this host IPv6>"
echo "  PRIMARY_PORT=5433"
echo "REPL_PASSWORD=$REPL_PW"
echo
echo "Next: ./deploy/failover/sync-gateway.sh"
