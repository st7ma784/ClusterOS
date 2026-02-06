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
echo "[1/6] Installing node-agent and CLI tools..."

sudo install -m 755 /tmp/node-agent /usr/local/bin/node-agent

# Install cluster-os-install (the remote installer script)
if [ -f /tmp/cluster-os-install ]; then
    sudo install -m 755 /tmp/cluster-os-install /usr/local/bin/cluster-os-install
    echo "  cluster-os-install installed"
fi

# Create directories
sudo mkdir -p /etc/clusteros /var/lib/clusteros /var/log/clusteros
sudo chmod 755 /etc/clusteros /var/log/clusteros
sudo chmod 700 /var/lib/clusteros

# Copy config
sudo cp /tmp/clusteros-files/config/node.yaml /etc/clusteros/
sudo chmod 644 /etc/clusteros/node.yaml

# Copy and enable systemd service
sudo cp /tmp/clusteros-files/systemd/node-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable node-agent.service

# ------------------------------------------------------------------------------
# Install k3s (disabled by default)
# ------------------------------------------------------------------------------
echo "[2/6] Installing k3s..."

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
echo "[3/6] Installing SLURM..."

sudo apt-get install -y munge slurm-wlm slurm-client

# Install tools needed for cluster-os-install
# systemd-boot provides systemd-bootx64.efi for UEFI booting
sudo apt-get install -y rsync gdisk dosfstools efibootmgr parted systemd-boot

# Install WiFi support packages and firmware
echo "  Installing WiFi support and firmware..."
sudo apt-get install -y wpasupplicant wireless-tools iw rfkill

# Install WiFi firmware for common chipsets
echo "  Installing WiFi firmware..."
# Main firmware package - contains most WiFi firmware (Intel, Realtek, Atheros, etc.)
sudo apt-get install -y linux-firmware || true

# CRITICAL: Cloud images use minimal kernels without WiFi drivers
# We need to install linux-modules-extra for the INSTALLED kernel, not the running one
# The running kernel during Packer build is the VM's kernel, not the target
INSTALLED_KERNEL=$(ls /boot/vmlinuz-* 2>/dev/null | sed 's|/boot/vmlinuz-||' | sort -V | tail -1)
echo "  Installed kernel: $INSTALLED_KERNEL"

if [ -n "$INSTALLED_KERNEL" ]; then
    echo "  Installing extra kernel modules for WiFi support..."
    sudo apt-get install -y "linux-modules-extra-${INSTALLED_KERNEL}" || {
        echo "  Trying generic modules-extra package..."
        sudo apt-get install -y linux-modules-extra-generic || true
    }
fi

# Also install the generic kernel modules package as fallback
sudo apt-get install -y linux-modules-extra-generic 2>/dev/null || true

# Broadcom WiFi - proprietary driver for some chipsets
sudo apt-get install -y broadcom-sta-dkms 2>/dev/null || true
# Broadcom open-source driver (brcmfmac/brcmsmac)
sudo apt-get install -y bcmwl-kernel-source 2>/dev/null || true

# Realtek USB WiFi adapters (rtl8xxxu, rtl88xxau)
sudo apt-get install -y realtek-rtl88xxau-dkms 2>/dev/null || true

# NetworkManager as backup for complex WiFi scenarios  
sudo apt-get install -y network-manager 2>/dev/null || true

# Verify WiFi modules are available
echo "  Verifying WiFi modules..."
for mod in iwlwifi ath9k ath10k_pci brcmfmac rtw88_pci mt7921e cfg80211 mac80211; do
    if modinfo "$mod" &>/dev/null; then
        echo "    ✓ $mod available"
    else
        echo "    ✗ $mod NOT found"
    fi
done

# Rebuild initramfs to include firmware and modules
echo "  Rebuilding initramfs..."
sudo update-initramfs -u -k all 2>/dev/null || true

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
echo "[4/6] Installing Tailscale..."

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
    
    # Install and enable auto-auth service
    if [ -f /tmp/clusteros-files/systemd/tailscale-auth.service ]; then
        sudo cp /tmp/clusteros-files/systemd/tailscale-auth.service /etc/systemd/system/
        sudo systemctl daemon-reload
        sudo systemctl enable tailscale-auth.service
        echo "  Tailscale auto-auth enabled"
    fi
fi

# ------------------------------------------------------------------------------
# Network configuration
# ------------------------------------------------------------------------------
echo "[5/6] Configuring network..."

# Copy netplan config (includes WiFi configuration)
sudo cp /tmp/clusteros-files/netplan/99-clusteros.yaml /etc/netplan/
sudo chmod 600 /etc/netplan/99-clusteros.yaml

# Ensure networkd is used
sudo systemctl enable systemd-networkd
sudo systemctl enable systemd-resolved

# Install systemd-networkd-wait-online override to reduce boot timeout
# This prevents long delays when network interfaces aren't available
if [ -d /tmp/clusteros-files/systemd/systemd-networkd-wait-online.service.d ]; then
    sudo mkdir -p /etc/systemd/system/systemd-networkd-wait-online.service.d
    sudo cp /tmp/clusteros-files/systemd/systemd-networkd-wait-online.service.d/*.conf \
        /etc/systemd/system/systemd-networkd-wait-online.service.d/
    echo "  Network wait timeout reduced to 10 seconds"
fi

# DISABLE system wpa_supplicant - our cluster-wifi.service manages it
# The system service conflicts with manual wpa_supplicant management
sudo systemctl disable wpa_supplicant.service 2>/dev/null || true
sudo systemctl mask wpa_supplicant.service 2>/dev/null || true

# Disable NetworkManager WiFi management - we use systemd-networkd + cluster-wifi
sudo systemctl disable NetworkManager.service 2>/dev/null || true

# Install and enable WiFi setup service
if [ -f /tmp/clusteros-files/systemd/cluster-wifi.service ]; then
    sudo cp /tmp/clusteros-files/systemd/cluster-wifi.service /etc/systemd/system/
    sudo systemctl daemon-reload
    sudo systemctl enable cluster-wifi.service
    echo "  WiFi auto-connect service enabled (manages wpa_supplicant)"
fi

# WireGuard directory
sudo mkdir -p /etc/wireguard
sudo chmod 700 /etc/wireguard

# ------------------------------------------------------------------------------
# Final setup
# ------------------------------------------------------------------------------
echo "[6/6] Final setup..."

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
    sudo chmod +x /usr/local/bin/cluster-autostart 2>/dev/null || true
    sudo chmod +x /usr/local/bin/cluster-wifi-setup 2>/dev/null || true
fi

# Install and enable cluster-autostart service
echo "  Installing cluster auto-assembly service..."
if [ -f /tmp/clusteros-files/systemd/cluster-autostart.service ]; then
    sudo cp /tmp/clusteros-files/systemd/cluster-autostart.service /etc/systemd/system/
    sudo systemctl daemon-reload
    sudo systemctl enable cluster-autostart.service
    echo "  Cluster auto-assembly enabled"
fi

# SSH config - disable root login
sudo sed -i 's/^#*PermitRootLogin.*/PermitRootLogin no/' /etc/ssh/sshd_config

# Apply sysctl settings
sudo sysctl --system

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
systemctl list-unit-files | grep -E "node-agent|k3s|slurm|munge|tailscale|cluster-autostart|cluster-wifi" | head -20
echo ""
echo "WiFi configured: MANDER84"
echo ""
echo "============================================"
echo "Provisioning Complete"
echo "============================================"
