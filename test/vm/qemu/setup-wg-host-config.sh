#!/bin/bash

# Setup WireGuard on the host machine to join the ClusterOS network
# This allows direct SSH access to VMs through WireGuard tunnel

set -e

WG_INTERFACE="wg-clusteros"
WG_SUBNET="10.42.0.0/16"
WG_HOST_IP="10.42.0.1/24"
WG_PORT="51820"

echo "Setting up WireGuard on host for ClusterOS cluster access"
echo "=========================================================="

# Check if wg-quick is installed
if ! command -v wg-quick &> /dev/null; then
    echo "Installing wireguard-tools..."
    sudo apt-get update
    sudo apt-get install -y wireguard-tools
fi

# Check if interface already exists
if ip link show $WG_INTERFACE 2>/dev/null; then
    echo "WireGuard interface $WG_INTERFACE already exists"
    echo "Run: sudo wg-quick up $WG_INTERFACE"
    exit 0
fi

echo ""
echo "Generating WireGuard keys for host..."

# Generate host keys
mkdir -p ~/.wireguard
if [ ! -f ~/.wireguard/clusteros_private.key ]; then
    wg genkey > ~/.wireguard/clusteros_private.key
    chmod 600 ~/.wireguard/clusteros_private.key
fi

if [ ! -f ~/.wireguard/clusteros_public.key ]; then
    wg pubkey < ~/.wireguard/clusteros_private.key > ~/.wireguard/clusteros_public.key
fi

HOST_PRIVATE_KEY=$(cat ~/.wireguard/clusteros_private.key)
HOST_PUBLIC_KEY=$(cat ~/.wireguard/clusteros_public.key)

echo "Host WireGuard public key: $HOST_PUBLIC_KEY"
echo ""
echo "Creating WireGuard configuration file..."

# Create WireGuard config for host
# VMs will have IPs: 10.42.0.2, 10.42.0.3, 10.42.0.4, etc.
# Host will be: 10.42.0.1
# NOTE: AllowedIPs is restricted to 10.42.0.0/24 to only route cluster traffic through tunnel
# This prevents WireGuard from becoming the default gateway for all traffic
cat > /tmp/$WG_INTERFACE.conf <<'WGCONF'
[Interface]
Address = 10.42.0.1/24
PrivateKey = HOST_PRIVATE_KEY_PLACEHOLDER
ListenPort = 51820
# Only route cluster subnet traffic through WireGuard, not all traffic
# AllowedIPs for peers will be restricted to 10.42.0.0/24

# These peers will be auto-configured when VMs start
# The VMs will generate their own keys and share them
WGCONF

# Replace placeholder
sed -i "s|HOST_PRIVATE_KEY_PLACEHOLDER|$HOST_PRIVATE_KEY|g" /tmp/$WG_INTERFACE.conf

# Create initial config
sudo mkdir -p /etc/wireguard
sudo cp /tmp/$WG_INTERFACE.conf /etc/wireguard/$WG_INTERFACE.conf
sudo chmod 600 /etc/wireguard/$WG_INTERFACE.conf

echo "WireGuard config created at /etc/wireguard/$WG_INTERFACE.conf"
echo ""
echo "IMPORTANT: This WireGuard setup is CLUSTER-ONLY"
echo "  - Only traffic to/from 10.42.0.0/24 will route through the tunnel"
echo "  - All other traffic remains on the normal network path"
echo "  - Your host's default gateway is NOT affected"
echo ""
echo "To start WireGuard on host:"
echo "  sudo wg-quick up $WG_INTERFACE"
echo ""
echo "To bring it down:"
echo "  sudo wg-quick down $WG_INTERFACE"
echo ""
echo "To check status:"
echo "  sudo wg show"
echo ""
echo "Once WireGuard is up and VMs are running, you can SSH to them at:"
echo "  ssh -i ~/.ssh/id_rsa clusteros@10.42.0.2  # for node1"
echo "  ssh -i ~/.ssh/id_rsa clusteros@10.42.0.3  # for node2"
echo "  ssh -i ~/.ssh/id_rsa clusteros@10.42.0.4  # for node3"
echo ""
echo "Your host's public key (for VMs):"
echo "  cat ~/.wireguard/clusteros_public.key"
