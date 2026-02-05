#!/bin/bash
# ClusterOS Provisioning Script
# Runs after cloud-init during packer build
set -euo pipefail

echo "============================================"
echo "ClusterOS Provisioning"
echo "============================================"

# ------------------------------------------------------------------------------
# Install node-agent and cluster-os-install
# ------------------------------------------------------------------------------
echo "[1/7] Installing node-agent and CLI tools..."

sudo install -m 755 /tmp/node-agent /usr/local/bin/node-agent

# Install cluster-os-install (the remote installer script)
if [ -f /tmp/cluster-os-install ]; then
    sudo install -m 755 /tmp/cluster-os-install /usr/local/bin/cluster-os-install
    echo "  cluster-os-install installed"
fi

# Create directories
sudo mkdir -p /etc/clusteros /var/lib/clusteros /var/lib/cluster-os /var/log/clusteros
sudo chmod 755 /etc/clusteros /var/log/clusteros
sudo chmod 700 /var/lib/clusteros /var/lib/cluster-os

# ------------------------------------------------------------------------------
# Install cluster authentication keys
# ------------------------------------------------------------------------------
echo "[2/7] Installing cluster authentication keys..."

# Install cluster auth key
if [ -f /tmp/clusteros-files/secrets/cluster.key ]; then
    CLUSTER_KEY=$(cat /tmp/clusteros-files/secrets/cluster.key)
    echo "  Cluster auth key found"
    
    # Update node.yaml with the cluster key
    if [ -f /tmp/clusteros-files/config/node.yaml ]; then
        # Add auth_key to the cluster section
        if grep -q "auth_key:" /tmp/clusteros-files/config/node.yaml; then
            sed -i "s|auth_key:.*|auth_key: \"$CLUSTER_KEY\"|" /tmp/clusteros-files/config/node.yaml
        else
            echo "" >> /tmp/clusteros-files/config/node.yaml
            echo "cluster:" >> /tmp/clusteros-files/config/node.yaml
            echo "  auth_key: \"$CLUSTER_KEY\"" >> /tmp/clusteros-files/config/node.yaml
        fi
    fi
    
    # Also save standalone key file for cluster-os-install
    echo "$CLUSTER_KEY" | sudo tee /etc/clusteros/cluster.key > /dev/null
    sudo chmod 600 /etc/clusteros/cluster.key
    echo "  Cluster key installed to /etc/clusteros/cluster.key"
else
    echo "  WARNING: No cluster key found - nodes won't be able to authenticate"
fi

# Install Serf encryption key
if [ -f /tmp/clusteros-files/secrets/serf.key ]; then
    SERF_KEY=$(cat /tmp/clusteros-files/secrets/serf.key)
    echo "  Serf encryption key found"
    
    # Update node.yaml with Serf key
    if [ -f /tmp/clusteros-files/config/node.yaml ]; then
        if grep -q "encrypt_key:" /tmp/clusteros-files/config/node.yaml; then
            sed -i "s|encrypt_key:.*|encrypt_key: \"$SERF_KEY\"|" /tmp/clusteros-files/config/node.yaml
        else
            # Add to discovery section
            sed -i "/discovery:/a\\  encrypt_key: \"$SERF_KEY\"" /tmp/clusteros-files/config/node.yaml
        fi
    fi
    
    echo "$SERF_KEY" | sudo tee /etc/clusteros/serf.key > /dev/null
    sudo chmod 600 /etc/clusteros/serf.key
    echo "  Serf key installed to /etc/clusteros/serf.key"
fi

# Copy config (now with embedded keys)
sudo cp /tmp/clusteros-files/config/node.yaml /etc/clusteros/
sudo chmod 644 /etc/clusteros/node.yaml

# Copy and enable systemd service
sudo cp /tmp/clusteros-files/systemd/node-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable node-agent.service

# ------------------------------------------------------------------------------
# Install k3s (disabled by default)
# ------------------------------------------------------------------------------
echo "[3/7] Installing k3s..."

# Pin to a known stable version to avoid checksum issues with latest
K3S_VERSION="v1.31.4+k3s1"

# Retry k3s install up to 3 times (handles transient download issues)
for i in 1 2 3; do
  echo "  k3s install attempt $i (version: $K3S_VERSION)..."
  # Clear any partial downloads
  sudo rm -f /usr/local/bin/k3s 2>/dev/null || true
  
  if curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION="$K3S_VERSION" INSTALL_K3S_SKIP_START=true INSTALL_K3S_SKIP_ENABLE=true sh -; then
    echo "  k3s installed successfully"
    break
  fi
  
  if [ $i -eq 3 ]; then
    echo "  k3s install failed after 3 attempts, continuing anyway..."
    # Don't fail the build - k3s can be installed later
    break
  fi
  echo "  Retrying in 10 seconds..."
  sleep 10
done

# Disable k3s services (node-agent will enable when needed)
sudo systemctl disable k3s.service 2>/dev/null || true
sudo systemctl disable k3s-agent.service 2>/dev/null || true

# ------------------------------------------------------------------------------
# Install SLURM (disabled by default)
# ------------------------------------------------------------------------------
echo "[4/7] Installing SLURM..."

sudo apt-get install -y munge slurm-wlm slurm-client

# Install tools needed for cluster-os-install
sudo apt-get install -y rsync gdisk dosfstools efibootmgr parted jq

# Create directories
sudo mkdir -p /etc/slurm /etc/munge /var/spool/slurm /var/log/slurm
sudo chmod 700 /etc/munge
sudo chmod 755 /etc/slurm /var/spool/slurm /var/log/slurm

# Disable SLURM services (node-agent will enable when needed)
sudo systemctl disable munge.service 2>/dev/null || true
sudo systemctl disable slurmctld.service 2>/dev/null || true
sudo systemctl disable slurmd.service 2>/dev/null || true

# ------------------------------------------------------------------------------
# Install Tailscale
# ------------------------------------------------------------------------------
echo "[5/7] Installing Tailscale..."

# Install Tailscale
curl -fsSL https://tailscale.com/install.sh | sh

# Enable tailscaled but don't start (will auth on first boot)
sudo systemctl enable tailscaled

# Install Tailscale auth config if provided
if [ -f /tmp/clusteros-files/tailscale/tailscale.env ]; then
    echo "  Installing Tailscale credentials..."
    sudo mkdir -p /etc/clusteros
    sudo cp /tmp/clusteros-files/tailscale/tailscale.env /etc/clusteros/
    sudo chmod 600 /etc/clusteros/tailscale.env
    
    # Check if credentials are actually set
    if grep -q "TAILSCALE_OAUTH_CLIENT_ID=." /etc/clusteros/tailscale.env 2>/dev/null; then
        echo "  Tailscale OAuth credentials configured"
    else
        echo "  WARNING: Tailscale OAuth credentials not set - nodes won't auto-join Tailnet"
    fi
    
    # Install and enable auto-auth service
    if [ -f /tmp/clusteros-files/systemd/tailscale-auth.service ]; then
        sudo cp /tmp/clusteros-files/systemd/tailscale-auth.service /etc/systemd/system/
        sudo systemctl daemon-reload
        sudo systemctl enable tailscale-auth.service
        echo "  Tailscale auto-auth enabled"
    fi
fi

# Install tailscale-auth script
if [ -f /tmp/clusteros-files/bin/tailscale-auth ]; then
    sudo install -m 755 /tmp/clusteros-files/bin/tailscale-auth /usr/local/bin/tailscale-auth
    echo "  tailscale-auth script installed"
fi

# ------------------------------------------------------------------------------
# Network configuration
# ------------------------------------------------------------------------------
echo "[6/7] Configuring network..."

# Copy netplan config
sudo cp /tmp/clusteros-files/netplan/99-clusteros.yaml /etc/netplan/
sudo chmod 600 /etc/netplan/99-clusteros.yaml

# Ensure networkd is used
sudo systemctl enable systemd-networkd
sudo systemctl enable systemd-resolved

# WireGuard directory
sudo mkdir -p /etc/wireguard
sudo chmod 700 /etc/wireguard

# ------------------------------------------------------------------------------
# Final setup
# ------------------------------------------------------------------------------
echo "[7/7] Final setup..."

# Install MOTD scripts
echo "  Installing MOTD..."
sudo mkdir -p /etc/update-motd.d
if [ -d /tmp/clusteros-files/motd ]; then
    sudo cp /tmp/clusteros-files/motd/* /etc/update-motd.d/
    sudo chmod +x /etc/update-motd.d/*
fi
# Disable default Ubuntu MOTD components we don't want
sudo chmod -x /etc/update-motd.d/10-help-text 2>/dev/null || true
sudo chmod -x /etc/update-motd.d/50-motd-news 2>/dev/null || true
sudo chmod -x /etc/update-motd.d/88-esm-announce 2>/dev/null || true
sudo chmod -x /etc/update-motd.d/91-contract-ua-esm-status 2>/dev/null || true

# Install CLI commands
echo "  Installing ClusterOS commands..."
if [ -d /tmp/clusteros-files/bin ]; then
    sudo cp /tmp/clusteros-files/bin/* /usr/local/bin/
    sudo chmod +x /usr/local/bin/cluster-*
    sudo chmod +x /usr/local/bin/tailscale-auth 2>/dev/null || true
fi

# SSH config - disable root login
sudo sed -i 's/^#*PermitRootLogin.*/PermitRootLogin no/' /etc/ssh/sshd_config

# Apply sysctl settings
sudo sysctl --system

# Clean up secrets from tmp (they're now in /etc/clusteros)
sudo rm -rf /tmp/clusteros-files/secrets

# Verify installation
echo ""
echo "============================================"
echo "Installation Summary"
echo "============================================"
echo "node-agent:  $(node-agent --version 2>&1 || echo 'installed')"
echo "k3s:         $(k3s --version 2>&1 | head -1 || echo 'installed')"
echo "slurm:       $(sinfo --version 2>&1 || echo 'installed')"
echo "wireguard:   $(wg --version 2>&1 | head -1 || echo 'installed')"
echo ""
echo "ClusterOS commands installed:"
ls -1 /usr/local/bin/cluster-* 2>/dev/null || echo "  (none)"
echo ""
echo "Enabled services:"
systemctl list-unit-files | grep -E "node-agent|k3s|slurm|munge" | head -10
echo ""
echo "============================================"
echo "Provisioning Complete"
echo "============================================"
