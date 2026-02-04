#!/bin/bash
# ClusterOS Remote Node Installer
# Installs all ClusterOS services on a remote Ubuntu node
# Run with: bash remote-node-installer.sh [options]
set -e

echo "========================================="
echo "ClusterOS Remote Node Installer"
echo "========================================="
echo "Started at: $(date)"
echo ""

# Default values (can be overridden with environment variables or command line)
TAILSCALE_OAUTH_CLIENT_ID="${TAILSCALE_OAUTH_CLIENT_ID:-}"
TAILSCALE_OAUTH_CLIENT_SECRET="${TAILSCALE_OAUTH_CLIENT_SECRET:-}"
TAILSCALE_AUTHKEY="${TAILSCALE_AUTHKEY:-}"
CLUSTER_KEY="${CLUSTER_KEY:-}"
WIFI_SSID="${WIFI_SSID:-}"
WIFI_KEY="${WIFI_KEY:-}"

# Auto-detect cluster key from live image
if [ -z "$CLUSTER_KEY" ] && [ -f "/cluster.key" ]; then
    echo ">>> Auto-detected cluster key from live image"
    CLUSTER_KEY=$(cat /cluster.key)
fi

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --tailscale-oauth-id)
            TAILSCALE_OAUTH_CLIENT_ID="$2"
            shift 2
            ;;
        --tailscale-oauth-secret)
            TAILSCALE_OAUTH_CLIENT_SECRET="$2"
            shift 2
            ;;
        --tailscale-authkey)
            TAILSCALE_AUTHKEY="$2"
            shift 2
            ;;
        --cluster-key)
            CLUSTER_KEY="$2"
            shift 2
            ;;
        --wifi-ssid)
            WIFI_SSID="$2"
            shift 2
            ;;
        --wifi-key)
            WIFI_KEY="$2"
            shift 2
            ;;
        --help)
            echo "Usage: $0 [options]"
            echo ""
            echo "Options:"
            echo "  --tailscale-oauth-id ID        Tailscale OAuth Client ID"
            echo "  --tailscale-oauth-secret SECRET Tailscale OAuth Client Secret"
            echo "  --tailscale-authkey KEY        Tailscale Auth Key (fallback)"
            echo "  --cluster-key KEY              Cluster encryption key"
            echo "  --wifi-ssid SSID               WiFi SSID"
            echo "  --wifi-key KEY                 WiFi password"
            echo ""
            echo "Environment variables can also be set instead of command line options."
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            echo "Use --help for usage information."
            exit 1
            ;;
    esac
done

# ==============================================================================
# Phase 1: System Update and Base Packages
# ==============================================================================
echo ">>> Phase 1: Installing base packages..."

sudo apt-get update
sudo DEBIAN_FRONTEND=noninteractive apt-get upgrade -y

sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \
    curl \
    wget \
    unzip \
    ca-certificates \
    gnupg \
    jq \
    net-tools \
    iproute2 \
    iputils-ping \
    dnsutils \
    build-essential \
    isc-dhcp-client \
    wpasupplicant \
    wireless-tools \
    iw \
    rfkill \
    netplan.io

echo ">>> Base packages installed"

# ==============================================================================
# Phase 2: Tailscale (Primary Mesh Network)
# ==============================================================================
echo ">>> Phase 2: Installing Tailscale..."

# Install Tailscale using official install script
curl -fsSL https://tailscale.com/install.sh | sh

# Enable tailscaled service
sudo systemctl enable tailscaled

# Create ClusterOS config directory
sudo mkdir -p /etc/cluster-os

# Configure Tailscale OAuth credentials
if [ -n "$TAILSCALE_OAUTH_CLIENT_ID" ] && [ -n "$TAILSCALE_OAUTH_CLIENT_SECRET" ]; then
    sudo tee /etc/cluster-os/tailscale-oauth.conf > /dev/null <<TSOAUTH
# Tailscale OAuth Client Credentials
TAILSCALE_OAUTH_CLIENT_ID="${TAILSCALE_OAUTH_CLIENT_ID}"
TAILSCALE_OAUTH_CLIENT_SECRET="${TAILSCALE_OAUTH_CLIENT_SECRET}"
TAILSCALE_TAILNET="-"
TSOAUTH
    sudo chmod 600 /etc/cluster-os/tailscale-oauth.conf
    echo ">>> Tailscale OAuth credentials configured"
else
    echo ">>> No Tailscale OAuth credentials provided"
fi

# Configure static auth key as fallback
if [ -n "$TAILSCALE_AUTHKEY" ]; then
    echo "$TAILSCALE_AUTHKEY" | sudo tee /etc/cluster-os/tailscale-authkey > /dev/null
    sudo chmod 600 /etc/cluster-os/tailscale-authkey
    echo ">>> Tailscale auth key configured (fallback)"
else
    echo ">>> No static Tailscale auth key provided"
fi

# Create script to generate auth key via OAuth
sudo tee /usr/local/bin/tailscale-get-authkey > /dev/null <<'TSGETKEY'
#!/bin/bash
set -e

if [ -f /etc/cluster-os/tailscale-oauth.conf ]; then
    source /etc/cluster-os/tailscale-oauth.conf
fi

if [ -z "$TAILSCALE_OAUTH_CLIENT_ID" ] || [ -z "$TAILSCALE_OAUTH_CLIENT_SECRET" ]; then
    echo "OAuth not configured" >&2
    exit 1
fi

ACCESS_TOKEN=$(curl -s -X POST "https://api.tailscale.com/api/v2/oauth/token" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    -d "client_id=${TAILSCALE_OAUTH_CLIENT_ID}&client_secret=${TAILSCALE_OAUTH_CLIENT_SECRET}" | jq -r '.access_token')

if [ "$ACCESS_TOKEN" = "null" ] || [ -z "$ACCESS_TOKEN" ]; then
    echo "Failed to get access token" >&2
    exit 1
fi

AUTH_KEY=$(curl -s -X POST "https://api.tailscale.com/api/v2/tailnet/-/keys" \
    -H "Authorization: Bearer ${ACCESS_TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"capabilities":{"devices":{"create":{"reusable":false,"preauthorized":true,"tags":["tag:cluster-node"]}}}}' | jq -r '.key')

if [ "$AUTH_KEY" = "null" ] || [ -z "$AUTH_KEY" ]; then
    echo "Failed to generate auth key" >&2
    exit 1
fi

echo "$AUTH_KEY"
TSGETKEY

sudo chmod +x /usr/local/bin/tailscale-get-authkey

echo ">>> Tailscale installed"

# ==============================================================================
# Phase 3: K3s (Kubernetes)
# ==============================================================================
echo ">>> Phase 3: Installing K3s..."

# Install K3s binary without starting the service
curl -sfL https://get.k3s.io | INSTALL_K3S_SKIP_START=true INSTALL_K3S_SKIP_ENABLE=true sh -

# Disable K3s services by default
sudo systemctl disable k3s 2>/dev/null || true
sudo systemctl disable k3s-agent 2>/dev/null || true

echo ">>> K3s installed (disabled by default)"

# ==============================================================================
# Phase 4: SLURM
# ==============================================================================
echo ">>> Phase 4: Installing SLURM..."

sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \
    munge \
    slurm-wlm \
    slurm-client

# Create SLURM directories
sudo mkdir -p /etc/slurm /etc/munge /var/spool/slurm /var/log/slurm
sudo chmod 755 /etc/slurm
sudo chmod 700 /etc/munge
sudo chmod 755 /var/spool/slurm
sudo chmod 755 /var/log/slurm

# Generate Munge key
echo ">>> Generating Munge key..."
sudo mkdir -p /etc/munge
sudo /usr/sbin/create-munge-key -f 2>/dev/null || {
    # Fallback: generate key manually
    sudo openssl rand -out /etc/munge/munge.key 1024
    sudo chmod 400 /etc/munge/munge.key
    sudo chown munge:munge /etc/munge/munge.key 2>/dev/null || true
}
echo ">>> Munge key created"

# Disable SLURM services by default
sudo systemctl disable slurmctld 2>/dev/null || true
sudo systemctl disable slurmd 2>/dev/null || true
sudo systemctl disable munge 2>/dev/null || true

echo ">>> SLURM installed (disabled by default)"

# ==============================================================================
# Phase 5: Node Agent
# ==============================================================================
echo ">>> Phase 5: Installing Node Agent..."

# Download node-agent binary (assuming it's available at a URL - adjust as needed)
# For now, we'll assume it's built and available in the repo
# In production, this should be a release URL
NODE_AGENT_URL="https://github.com/your-org/cluster-os/releases/download/latest/node-agent"
if curl -f -s "$NODE_AGENT_URL" -o /tmp/node-agent; then
    sudo mv /tmp/node-agent /usr/local/bin/node-agent
    sudo chmod +x /usr/local/bin/node-agent
    echo ">>> Node agent binary downloaded and installed"
else
    echo ">>> WARNING: Could not download node-agent binary from $NODE_AGENT_URL"
    echo ">>> Please manually install the node-agent binary to /usr/local/bin/node-agent"
fi

# Create systemd service file
sudo tee /etc/systemd/system/node-agent.service > /dev/null <<'SERVICE'
[Unit]
Description=ClusterOS Node Agent
After=network.target tailscaled.service
Wants=tailscaled.service

[Service]
Type=simple
ExecStart=/usr/local/bin/node-agent
Restart=always
RestartSec=5
Environment=HOME=/root

[Install]
WantedBy=multi-user.target
SERVICE

sudo systemctl daemon-reload
sudo systemctl enable node-agent.service

# Create ClusterOS directories
sudo mkdir -p /etc/cluster-os /var/lib/cluster-os /var/log/cluster-os
sudo chmod 755 /etc/cluster-os
sudo chmod 700 /var/lib/cluster-os
sudo chmod 755 /var/log/cluster-os

echo ">>> Node agent setup complete"

# ==============================================================================
# Phase 6: Network Configuration
# ==============================================================================
echo ">>> Phase 6: Configuring network..."

# Generate netplan config
sudo tee /etc/netplan/01-clusteros-network.yaml > /dev/null <<'NETPLAN_BASE'
network:
  version: 2
  renderer: networkd

  ethernets:
    all-en:
      match:
        name: "en*"
      dhcp4: true
      dhcp6: false
      optional: true
NETPLAN_BASE

# Add WiFi if configured
if [ -n "$WIFI_SSID" ] && [ -n "$WIFI_KEY" ]; then
    echo ">>> Adding WiFi network: $WIFI_SSID"
    sudo tee -a /etc/netplan/01-clusteros-network.yaml > /dev/null <<NETPLAN_WIFI

  wifis:
    all-wifi:
      match:
        name: "w*"
      dhcp4: true
      dhcp6: false
      optional: true
      access-points:
        "$WIFI_SSID":
          password: "$WIFI_KEY"
NETPLAN_WIFI
fi

sudo netplan apply

echo ">>> Network configuration complete"

# ==============================================================================
# Phase 7: Cluster Configuration
# ==============================================================================
echo ">>> Phase 7: Creating cluster configuration..."

# Get cluster key
if [ -n "$CLUSTER_KEY" ]; then
    CLUSTER_AUTH_KEY="$CLUSTER_KEY"
else
    echo "WARNING: No cluster key provided! Generating random key."
    echo "WARNING: This node will NOT be able to join existing clusters!"
    echo "WARNING: Use --cluster-key to specify the cluster key from cluster.key"
    CLUSTER_AUTH_KEY=$(openssl rand -base64 32)
fi

# Generate Serf encryption key
SERF_ENCRYPT_KEY=$(openssl rand -base64 16)

# Set WiFi enabled flag
if [ -n "$WIFI_SSID" ] && [ -n "$WIFI_KEY" ]; then
    WIFI_ENABLED="true"
else
    WIFI_ENABLED="false"
fi

# Create node configuration
sudo tee /etc/cluster-os/node.yaml > /dev/null <<NODECONFIG
# ClusterOS Node Configuration
identity:
  path: /var/lib/cluster-os/identity.json

discovery:
  bind_addr: 0.0.0.0
  bind_port: 7946
  bootstrap_peers: []
  node_name: ""
  encrypt_key: "${SERF_ENCRYPT_KEY}"

networking:
  use_tailscale: true
  wifi:
    enabled: ${WIFI_ENABLED:-false}
    ssid: "${WIFI_SSID:-}"
    key: "${WIFI_KEY:-}"

roles:
  enabled:
    - slurm-controller
    - slurm-worker
    - k3s-server
    - k3s-agent
  capabilities:
    cpu: 0
    ram: ""
    gpu: false
    arch: ""

logging:
  level: info
  format: json
  output: stdout

cluster:
  name: cluster-os
  region: default
  datacenter: default
  auth_key: "${CLUSTER_AUTH_KEY}"
NODECONFIG

sudo chmod 644 /etc/cluster-os/node.yaml

echo ">>> Cluster configuration created"

# ==============================================================================
# Phase 8: Final Setup
# ==============================================================================
echo ">>> Phase 8: Final setup..."

# Start services
echo ">>> Starting services..."
sudo systemctl start tailscaled
sudo systemctl start node-agent

echo ""
echo "========================================="
echo "ClusterOS Remote Installation Complete!"
echo "========================================="
echo ""
echo "Next steps:"
echo "1. Connect to Tailscale: tailscale up"
echo "2. Check node status: node-agent status"
echo "3. Monitor logs: journalctl -u node-agent -f"
echo ""
echo "Installation completed at: $(date)"