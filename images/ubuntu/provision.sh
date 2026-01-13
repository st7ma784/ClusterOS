#!/bin/bash
set -e

# Cluster OS Image Provisioning Script
# This script is run by Packer to configure the base image

echo "========================================="
echo "Cluster OS Provisioning Started"
echo "========================================="

# Configuration
NODE_AGENT_BIN="/usr/local/bin/node-agent"
CONFIG_DIR="/etc/cluster-os"
DATA_DIR="/var/lib/cluster-os"
LOG_DIR="/var/log/cluster-os"

# Create directories
echo "Creating cluster-os directories..."
sudo mkdir -p "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR"
sudo mkdir -p /etc/wireguard /etc/slurm /etc/munge

# Set permissions
sudo chmod 755 "$CONFIG_DIR"
sudo chmod 700 "$DATA_DIR"
sudo chmod 755 "$LOG_DIR"
sudo chmod 700 /etc/wireguard
sudo chmod 755 /etc/slurm
sudo chmod 700 /etc/munge

# Verify node-agent binary
if [ -f "$NODE_AGENT_BIN" ]; then
    echo "Node agent installed: $($NODE_AGENT_BIN --version || echo 'version check failed')"
else
    echo "ERROR: Node agent binary not found at $NODE_AGENT_BIN"
    exit 1
fi

# Configure systemd services
echo "Configuring systemd services..."

# Disable k3s by default (will be enabled by node-agent if role assigned)
sudo systemctl disable k3s 2>/dev/null || true
sudo systemctl disable k3s-agent 2>/dev/null || true

# Disable SLURM by default (will be enabled by node-agent if role assigned)
sudo systemctl disable slurmctld 2>/dev/null || true
sudo systemctl disable slurmd 2>/dev/null || true
sudo systemctl disable munge 2>/dev/null || true

# Enable node-agent to start on boot
sudo systemctl enable node-agent.service

# Network configuration
echo "Configuring network..."

# Ensure netplan applies on boot
sudo systemctl enable systemd-networkd
sudo systemctl enable systemd-resolved

# Enable IP forwarding for WireGuard and Kubernetes
cat <<EOF | sudo tee /etc/sysctl.d/99-cluster-os.conf
# Cluster OS Network Configuration
net.ipv4.ip_forward = 1
net.ipv6.conf.all.forwarding = 1

# Optimize for cluster networking
net.core.rmem_max = 134217728
net.core.wmem_max = 134217728
net.ipv4.tcp_rmem = 4096 87380 67108864
net.ipv4.tcp_wmem = 4096 65536 67108864

# Kubernetes requirements
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
EOF

# Load br_netfilter module for Kubernetes
echo "br_netfilter" | sudo tee /etc/modules-load.d/cluster-os.conf

# SSH hardening
echo "Configuring SSH..."
sudo sed -i 's/#PermitRootLogin yes/PermitRootLogin no/' /etc/ssh/sshd_config
sudo sed -i 's/#PasswordAuthentication yes/PasswordAuthentication yes/' /etc/ssh/sshd_config

# Create default cluster configuration
cat <<EOF | sudo tee "$CONFIG_DIR/node.yaml"
# Cluster OS Node Configuration
# This file is managed by the node-agent

node:
  # Node identity will be generated on first boot
  data_dir: "$DATA_DIR"
  log_dir: "$LOG_DIR"

discovery:
  # Discovery bootstrap nodes (empty means this node will bootstrap)
  bootstrap_nodes: []
  # Serf bind port
  bind_port: 7946

wireguard:
  # WireGuard listen port
  listen_port: 51820
  # MTU for WireGuard interface
  mtu: 1420

roles:
  # Roles are assigned dynamically by the cluster
  # Possible roles: slurm-controller, slurm-worker, k8s-control-plane, k8s-worker
  enabled: []
EOF

sudo chmod 644 "$CONFIG_DIR/node.yaml"

echo "========================================="
echo "Cluster OS Provisioning Complete"
echo "========================================="
echo ""
echo "Installed components:"
echo "  - Node Agent: $NODE_AGENT_BIN"
echo "  - WireGuard: $(wg --version 2>&1 | head -n1)"
echo "  - K3s: $(k3s --version 2>&1 | head -n1 || echo 'not installed')"
echo "  - SLURM: $(sinfo --version 2>&1 || echo 'not installed')"
echo ""
echo "Services configured:"
echo "  - node-agent.service: enabled"
echo "  - systemd-networkd: enabled"
echo "  - systemd-resolved: enabled"
echo ""
