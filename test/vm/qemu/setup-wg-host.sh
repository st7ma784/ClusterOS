#!/bin/bash

# Add host to ClusterOS WireGuard network for VM access
# This allows the build host to communicate with cluster VMs through WireGuard

set -e

WG_INTERFACE="wg-cluster"
WG_SUBNET="10.42.0.0/16"
WG_HOST_IP="10.42.0.1"
WG_PORT="51820"

echo "Setting up WireGuard on host for ClusterOS cluster access"
echo "==========================================================="

# Check if wg-quick is installed
if ! command -v wg-quick &> /dev/null; then
    echo "Installing wireguard-tools..."
    sudo apt-get update
    sudo apt-get install -y wireguard-tools
fi

# Check if interface already exists
if ip link show $WG_INTERFACE 2>/dev/null; then
    echo "WireGuard interface $WG_INTERFACE already exists"
    exit 0
fi

echo ""
echo "Creating WireGuard configuration..."

# Generate host keys if they don't exist
if [ ! -f ~/.wireguard/host_private.key ]; then
    mkdir -p ~/.wireguard
    wg genkey | tee ~/.wireguard/host_private.key | wg pubkey > ~/.wireguard/host_public.key
    chmod 600 ~/.wireguard/host_private.key
    echo "Generated new WireGuard keys"
fi

HOST_PRIVATE_KEY=$(cat ~/.wireguard/host_private.key)
HOST_PUBLIC_KEY=$(cat ~/.wireguard/host_public.key)

echo "Host public key: $HOST_PUBLIC_KEY"
echo ""
echo "Creating WireGuard configuration file..."

# Create WireGuard config
cat > /tmp/wg-cluster.conf <<EOF
[Interface]
PrivateKey = $HOST_PRIVATE_KEY
Address = $WG_HOST_IP/24
ListenPort = $WG_PORT

# Add VMs as peers
[Peer]
# node1
PublicKey = TO_BE_CONFIGURED_FROM_VM1
AllowedIPs = 10.42.0.2/32

[Peer]
# node2
PublicKey = TO_BE_CONFIGURED_FROM_VM2
AllowedIPs = 10.42.0.3/32

[Peer]
# node3
PublicKey = TO_BE_CONFIGURED_FROM_VM3
AllowedIPs = 10.42.0.4/32
EOF

echo "WireGuard config created at /tmp/wg-cluster.conf"
echo ""
echo "To complete setup, you need to:"
echo "1. Get each VM's WireGuard public key from /etc/wireguard/wg0.pub"
echo "2. Update the peer sections in /tmp/wg-cluster.conf with the actual public keys"
echo "3. Run: sudo cp /tmp/wg-cluster.conf /etc/wireguard/$WG_INTERFACE.conf"
echo "4. Run: sudo wg-quick up $WG_INTERFACE"
echo ""
echo "Then you can SSH through WireGuard to the VMs:"
echo "  ssh clusteros@10.42.0.2  # for node1"
echo "  ssh clusteros@10.42.0.3  # for node2"
echo "  ssh clusteros@10.42.0.4  # for node3"
