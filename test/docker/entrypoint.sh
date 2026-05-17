#!/bin/bash

echo "=========================================="
echo "Cluster-OS Node Container Starting"
echo "=========================================="

# Parse environment variables
NODE_NAME="${NODE_NAME:-$(hostname)}"
NODE_BOOTSTRAP="${NODE_BOOTSTRAP:-false}"
NODE_JOIN="${NODE_JOIN:-}"
SERF_BIND_PORT="${SERF_BIND_PORT:-7946}"
RAFT_BIND_PORT="${RAFT_BIND_PORT:-7373}"
CLUSTER_AUTH_KEY="${CLUSTER_AUTH_KEY:-$(cat /etc/clusteros/cluster.key 2>/dev/null | tr -d '[:space:]')}"

echo "Node Name: $NODE_NAME"
echo "Bootstrap Mode: $NODE_BOOTSTRAP"
echo "Join Address: ${NODE_JOIN:-none}"
echo "Cluster Auth: $([ -n "$CLUSTER_AUTH_KEY" ] && echo "[set]" || echo "[missing]")"
echo "Tailscale: $([ -f /etc/clusteros/tailscale.env ] && echo "credentials baked in" || echo "no credentials — LAN-only")"

# Create configuration directory if it doesn't exist
mkdir -p /etc/cluster-os /var/lib/cluster-os /var/log/cluster-os

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
  interface: ""
  listen_port: 51820
  subnet: "10.42.0.0/16"
  ipv6: false
  wifi:
    enabled: false

roles:
  enabled:
EOF

# Add roles from environment variable or auto-detect based on node type
if [ -n "$NODE_ROLES" ]; then
    IFS=',' read -ra ROLES <<< "$NODE_ROLES"
    for role in "${ROLES[@]}"; do
        echo "    - $role" >> /etc/cluster-os/node.yaml
    done
elif [ "$NODE_BOOTSTRAP" = "true" ]; then
    echo "    - slurm-controller" >> /etc/cluster-os/node.yaml
    echo "    - k3s-server" >> /etc/cluster-os/node.yaml
else
    echo "    - slurm-worker" >> /etc/cluster-os/node.yaml
    echo "    - k3s-agent" >> /etc/cluster-os/node.yaml
fi

cat >> /etc/cluster-os/node.yaml <<EOF
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
  auth_key: $CLUSTER_AUTH_KEY
  election_mode: serf
EOF

# If NODE_JOIN is set, add bootstrap peers
if [ -n "$NODE_JOIN" ]; then
    echo "Configuring to join via: $NODE_JOIN"
    sed -i "s/bootstrap_peers: \[\]/bootstrap_peers: [\"$NODE_JOIN\"]/" /etc/cluster-os/node.yaml
fi

# Set Raft bind address as environment variable
export CLUSTEROS_RAFT_BIND_ADDR="0.0.0.0"
export CLUSTEROS_RAFT_BIND_PORT="${RAFT_BIND_PORT}"
export CLUSTEROS_BOOTSTRAP="${NODE_BOOTSTRAP}"

echo "Configuration written to /etc/cluster-os/node.yaml"
echo "=========================================="

# Start Tailscale using baked-in OAuth credentials (same flow as OS images)
# clusteros-tailscale-init handles OAuth token → ephemeral auth key → tailscale up
if [ -f /etc/clusteros/tailscale.env ]; then
    echo "Tailscale: baked credentials found — starting tailscaled..."
    mkdir -p /var/lib/tailscale /run/tailscale

    tailscaled \
        --state=/var/lib/tailscale/tailscaled.state \
        --socket=/run/tailscale/tailscaled.sock \
        > /var/log/cluster-os/tailscaled.log 2>&1 &

    # Set per-container hostname before the auth script reads the env file
    sed -i "s/^TAILSCALE_HOSTNAME=.*/TAILSCALE_HOSTNAME=cluster-os-$NODE_NAME/" \
        /etc/clusteros/tailscale.env

    # Wait for socket (clusteros-tailscale-init also waits, but give it a head start)
    for i in $(seq 1 15); do
        [ -S /run/tailscale/tailscaled.sock ] && break
        sleep 1
    done

    clusteros-tailscale-init \
        && echo "Tailscale: connected as cluster-os-$NODE_NAME" \
        || echo "Tailscale: WARNING — auth failed (non-fatal, LAN/macvlan discovery still works)"
else
    echo "Tailscale: no credentials baked in — LAN discovery only"
fi

echo "=========================================="

# Start D-Bus system daemon for SLURM integration
echo "Starting D-Bus system daemon..."
mkdir -p /var/run/dbus
if command -v dbus-daemon &> /dev/null; then
    dbus-daemon --system --fork 2>&1 || echo "Warning: D-Bus daemon failed to start (non-fatal)"
    echo "D-Bus daemon started"
else
    echo "Warning: dbus-daemon not found (non-fatal)"
fi

# node-agent owns k3s lifecycle — do not pre-start k3s here
exec "$@"
