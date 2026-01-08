#!/bin/bash
set -e

echo "=========================================="
echo "Cluster-OS Node Container Starting"
echo "=========================================="

# Parse environment variables
NODE_NAME="${NODE_NAME:-$(hostname)}"
NODE_BOOTSTRAP="${NODE_BOOTSTRAP:-false}"
NODE_JOIN="${NODE_JOIN:-}"
SERF_BIND_PORT="${SERF_BIND_PORT:-7946}"
RAFT_BIND_PORT="${RAFT_BIND_PORT:-7373}"
WIREGUARD_PORT="${WIREGUARD_PORT:-51820}"

echo "Node Name: $NODE_NAME"
echo "Bootstrap Mode: $NODE_BOOTSTRAP"
echo "Join Address: ${NODE_JOIN:-none}"

# Create configuration directory if it doesn't exist
mkdir -p /etc/cluster-os /var/lib/cluster-os

# Generate node configuration
cat > /etc/cluster-os/node.yaml <<EOF
# Cluster-OS Node Configuration (Auto-generated)

identity:
  path: /var/lib/cluster-os/identity.json

discovery:
  bind_addr: 0.0.0.0
  bind_port: ${SERF_BIND_PORT}
  bootstrap_peers: []
  node_name: "${NODE_NAME}"
  encrypt_key: ""

networking:
  interface: wg0
  listen_port: ${WIREGUARD_PORT}
  subnet: "10.42.0.0/16"
  ipv6: false
  wifi:
    enabled: false

roles:
  enabled:
EOF

# Add roles from environment variable
if [ -n "$NODE_ROLES" ]; then
    IFS=',' read -ra ROLES <<< "$NODE_ROLES"
    for role in "${ROLES[@]}"; do
        echo "    - $role" >> /etc/cluster-os/node.yaml
    done
else
    echo "    - wireguard" >> /etc/cluster-os/node.yaml
fi

cat >> /etc/cluster-os/node.yaml <<'EOF'
  capabilities:
    cpu: 0
    ram: ""
    gpu: false
    arch: ""

logging:
  level: info
  format: text
  output: stdout

cluster:
  name: cluster-os-test
  region: docker
  datacenter: test
EOF

# If NODE_JOIN is set, add bootstrap peers
if [ -n "$NODE_JOIN" ]; then
    echo "Configuring to join via: $NODE_JOIN"
    # Update config with bootstrap peers
    sed -i "s/bootstrap_peers: \[\]/bootstrap_peers: [\"$NODE_JOIN\"]/" /etc/cluster-os/node.yaml
fi

# Set Raft bind address as environment variable
export CLUSTEROS_RAFT_BIND_ADDR="0.0.0.0"
export CLUSTEROS_RAFT_BIND_PORT="${RAFT_BIND_PORT}"
export CLUSTEROS_BOOTSTRAP="${NODE_BOOTSTRAP}"

echo "Configuration written to /etc/cluster-os/node.yaml"
echo "=========================================="

# Execute the CMD (systemd)
exec "$@"
