#!/bin/bash

echo "=========================================="
echo "Cluster-OS Node Container Starting"
echo "=========================================="

# Parse environment variables
NODE_NAME="${NODE_NAME:-$(hostname)}"
NODE_BOOTSTRAP="${NODE_BOOTSTRAP:-false}"
NODE_JOIN="${NODE_JOIN:-}"
NODE_K3S_ENABLED="${NODE_K3S_ENABLED:-false}"
SERF_BIND_PORT="${SERF_BIND_PORT:-7946}"
RAFT_BIND_PORT="${RAFT_BIND_PORT:-7373}"
WIREGUARD_PORT="${WIREGUARD_PORT:-51820}"
CLUSTER_AUTH_KEY="${CLUSTER_AUTH_KEY:-}"

echo "Node Name: $NODE_NAME"
echo "Bootstrap Mode: $NODE_BOOTSTRAP"
echo "Join Address: ${NODE_JOIN:-none}"
echo "K3S Enabled: $NODE_K3S_ENABLED"
echo "Cluster Auth: ${CLUSTER_AUTH_KEY:0:16}..." # Show only first 16 chars for security

# Verify wg and wg-quick commands are available
if ! command -v wg &> /dev/null; then
    echo "ERROR: wg command not found - wireguard-tools not installed"
    exit 1
fi

if ! command -v wg-quick &> /dev/null; then
    echo "ERROR: wg-quick command not found - wireguard-tools not installed"
    exit 1
fi

echo "WireGuard tools verified"

# Create configuration directory if it doesn't exist
mkdir -p /etc/cluster-os /var/lib/cluster-os /etc/wireguard

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

# Add roles from environment variable or auto-detect based on node type
if [ -n "$NODE_ROLES" ]; then
    IFS=',' read -ra ROLES <<< "$NODE_ROLES"
    for role in "${ROLES[@]}"; do
        echo "    - $role" >> /etc/cluster-os/node.yaml
    done
elif [ "$NODE_BOOTSTRAP" = "true" ]; then
    # Bootstrap node gets controller roles
    echo "    - slurm-controller" >> /etc/cluster-os/node.yaml
    echo "    - k3s-server" >> /etc/cluster-os/node.yaml
else
    # Non-bootstrap nodes get worker roles
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
  election_mode: serf  # Use stateless Serf-based election for Docker (no persistent storage)
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

# This should show in logs
echo "=========================================="
echo "DEBUG: NODE_BOOTSTRAP=$NODE_BOOTSTRAP"
echo "DEBUG: NODE_K3S_ENABLED=$NODE_K3S_ENABLED"  
echo "=========================================="

# Start k3s server in background if enabled
if [ "$NODE_BOOTSTRAP" = "true" ] && [ "$NODE_K3S_ENABLED" = "true" ]; then
    echo "K3S: Condition met - Starting k3s..."
    
    if command -v k3s &> /dev/null; then
        echo "K3S: Binary found, starting..."
        
        # Get the container's IP address (from cluster network)
        CONTAINER_IP=$(hostname -i | awk '{print $1}')
        echo "K3S: Using container IP: $CONTAINER_IP"
        
        # Create a k3s config with TLS SANs to fix certificate issues
        mkdir -p /etc/rancher/k3s
        cat > /etc/rancher/k3s/config.yaml <<EOFK3S
tls-san:
  - "$CONTAINER_IP"
  - "127.0.0.1"
  - "0.0.0.0"
  - "$NODE_NAME"
  - "localhost"
EOFK3S
        
        k3s server \
            --snapshotter=fuse-overlayfs \
            --disable=traefik \
            --disable=servicelb \
            --disable=local-storage \
            --node-name="$NODE_NAME" \
            --bind-address=0.0.0.0 \
            --advertise-address="$CONTAINER_IP" \
            --flannel-backend=vxlan \
            --kubelet-arg=cgroups-per-qos=false \
            --kubelet-arg=enforce-node-allocatable="" \
            --kubelet-arg=make-iptables-util-chains=false \
            --kubelet-arg=feature-gates=KubeletInUserNamespace=true \
            --kube-apiserver-arg=enable-aggregator-routing=true \
            > /var/log/cluster-os/k3s-server.log 2>&1 &
        
        echo "K3S: Started with PID $!"
    else
        echo "K3S: Binary NOT found"
    fi
else
    echo "K3S: Condition NOT met (BOOTSTRAP=$NODE_BOOTSTRAP, ENABLED=$NODE_K3S_ENABLED)"
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

# Execute the CMD
exec "$@"