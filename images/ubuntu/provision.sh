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

# Inject cluster auth key (generated at build time from git repo identity)
if [ -f /tmp/cluster.key ]; then
    CLUSTER_KEY=$(cat /tmp/cluster.key | tr -d '[:space:]')
    sudo sed -i "s|auth_key:.*|auth_key: \"$CLUSTER_KEY\"|" /etc/clusteros/node.yaml
    echo "  Cluster auth key injected from cluster.key"
else
    echo "  WARNING: No cluster.key found — nodes will fail to authenticate!"
    echo "  Run 'make cluster-key' before building the image."
fi

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

sudo apt-get install -y munge slurm-wlm slurm-client libpmix-dev \
    openmpi-bin libopenmpi-dev python3-mpi4py build-essential

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
sudo mkdir -p /etc/slurm /etc/munge /var/spool/slurm /var/log/slurm /var/lib/munge /var/run/munge
sudo chmod 700 /etc/munge /var/lib/munge
sudo chmod 755 /etc/slurm /var/spool/slurm /var/log/slurm /var/run/munge

# Set munge directory ownership (munge user created by apt install)
sudo chown -R munge:munge /etc/munge /var/lib/munge /var/run/munge /var/log/munge 2>/dev/null || true

# Mask SLURM services — node-agent manages these daemons directly via exec.Command.
# "disable" only removes WantedBy symlinks; "mask" symlinks to /dev/null and prevents
# ALL activation (boot, dependencies, dbus, manual start). Without this, slurmd.service
# starts before slurm.conf exists and fails with DNS SRV lookup errors.
sudo systemctl disable munge.service 2>/dev/null || true
sudo systemctl mask munge.service 2>/dev/null || true
sudo systemctl disable slurmctld.service 2>/dev/null || true
sudo systemctl mask slurmctld.service 2>/dev/null || true
sudo systemctl disable slurmd.service 2>/dev/null || true
sudo systemctl mask slurmd.service 2>/dev/null || true

# ------------------------------------------------------------------------------
# Install Tailscale
# ------------------------------------------------------------------------------
echo "[4/6] Installing Tailscale..."

# Install Tailscale
curl -fsSL https://tailscale.com/install.sh | sh

# Ensure Tailscale directories exist with correct permissions
sudo mkdir -p /var/lib/tailscale /var/run/tailscale
sudo chmod 700 /var/lib/tailscale
sudo chown root:root /var/lib/tailscale /var/run/tailscale

# Enable tailscaled but don't start (will auth on first boot)
sudo systemctl enable tailscaled

# Install Tailscale auth config if provided
if [ -f /tmp/clusteros-files/.env ]; then
    echo "  Creating Tailscale configuration from build environment..."
    
    # Source the build environment
    set -a  # auto-export variables
    source /tmp/clusteros-files/.env
    set +a  # stop auto-export
    
    echo "  Loaded environment variables:"
    echo "    TAILSCALE_OAUTH_CLIENT_ID: ${TAILSCALE_OAUTH_CLIENT_ID:-NOT SET}"
    echo "    TAILSCALE_OAUTH_CLIENT_SECRET: ${TAILSCALE_OAUTH_CLIENT_SECRET:+SET (length ${#TAILSCALE_OAUTH_CLIENT_SECRET})} ${TAILSCALE_OAUTH_CLIENT_SECRET:-NOT SET}"
    echo "    TAILSCALE_AUTHKEY: ${TAILSCALE_AUTHKEY:+SET (length ${#TAILSCALE_AUTHKEY})} ${TAILSCALE_AUTHKEY:-NOT SET}"
    
    sudo mkdir -p /etc/clusteros
    
    # Create the final tailscale.env file directly
    sudo tee /etc/clusteros/tailscale.env > /dev/null <<TSENV
# Tailscale Configuration for ClusterOS
# Generated during image build

# OAuth Client credentials (recommended - never expire)
TAILSCALE_OAUTH_CLIENT_ID=${TAILSCALE_OAUTH_CLIENT_ID:-your_oauth_client_id_here}
TAILSCALE_OAUTH_CLIENT_SECRET=${TAILSCALE_OAUTH_CLIENT_SECRET:-your_oauth_client_secret_here}

# Tailscale Tags (required for OAuth in most orgs)
TAILSCALE_TAGS=clusteros

# Auth Key (alternative method)
TAILSCALE_AUTHKEY=${TAILSCALE_AUTHKEY:-your_auth_key_here}

# Optional: Custom hostname
# TAILSCALE_HOSTNAME=cluster-node
TSENV
    
    sudo chmod 600 /etc/clusteros/tailscale.env
    
    echo "  Tailscale configuration created at: /etc/clusteros/tailscale.env"
    echo "  Final file content:"
    sudo cat /etc/clusteros/tailscale.env | head -10

elif [ -f /tmp/clusteros-files/tailscale/tailscale.env ]; then
    echo "  Installing Tailscale template configuration..."
    sudo mkdir -p /etc/clusteros
    sudo cp /tmp/clusteros-files/tailscale/tailscale.env /etc/clusteros/
    sudo chmod 600 /etc/clusteros/tailscale.env
    echo "  Template file copied (manual configuration required)"
fi

if [ -f /etc/clusteros/tailscale.env ]; then
    
    # Check if valid credentials are configured (after creation)
    echo "  Checking for Tailscale credentials..."
    HAS_OAUTH_ID=$(sudo grep -v '^#\|^$' /etc/clusteros/tailscale.env | grep -q 'TAILSCALE_OAUTH_CLIENT_ID=.*[^r]$' && echo "yes" || echo "no")
    HAS_OAUTH_SECRET=$(sudo grep -v '^#\|^$' /etc/clusteros/tailscale.env | grep -q 'TAILSCALE_OAUTH_CLIENT_SECRET=.*[^r]$' && echo "yes" || echo "no")
    HAS_AUTHKEY=$(sudo grep -v '^#\|^$' /etc/clusteros/tailscale.env | grep -q 'TAILSCALE_AUTHKEY=.*[^r]$' && echo "yes" || echo "no")
    
    # OAuth requires BOTH credentials
    if [ "$HAS_OAUTH_ID" = "yes" ] && [ "$HAS_OAUTH_SECRET" = "yes" ]; then
        HAS_OAUTH="yes"
    else
        HAS_OAUTH="no"
    fi
    
    echo "  OAuth Client ID detected: $HAS_OAUTH_ID"
    echo "  OAuth Client Secret detected: $HAS_OAUTH_SECRET"
    echo "  OAuth (complete) detected: $HAS_OAUTH"
    echo "  AuthKey detected: $HAS_AUTHKEY"
    
    # Debug: Show what we found
    echo "  OAuth ID line: $(sudo grep -v '^#\|^$' /etc/clusteros/tailscale.env | grep 'TAILSCALE_OAUTH_CLIENT_ID' || echo 'NOT FOUND')"
    echo "  OAuth Secret line: $(sudo grep -v '^#\|^$' /etc/clusteros/tailscale.env | grep 'TAILSCALE_OAUTH_CLIENT_SECRET' | sed 's/=.*/=***REDACTED***/' || echo 'NOT FOUND')"
    echo "  AuthKey line: $(sudo grep -v '^#\|^$' /etc/clusteros/tailscale.env | grep 'TAILSCALE_AUTHKEY' | sed 's/=.*/=***REDACTED***/' || echo 'NOT FOUND')"
    
    # Install auto-auth service
    if [ -f /tmp/clusteros-files/systemd/tailscale-auth.service ]; then
        sudo cp /tmp/clusteros-files/systemd/tailscale-auth.service /etc/systemd/system/
        sudo systemctl daemon-reload
        
        # Always enable the service - it will handle missing credentials gracefully
        sudo systemctl enable tailscale-auth.service
        echo "  Tailscale auto-auth service enabled"
        echo "  Service will attempt authentication on first boot"
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

# Install tailscaled service override to wait for WiFi before starting
if [ -d /tmp/clusteros-files/systemd/tailscaled.service.d ]; then
    sudo mkdir -p /etc/systemd/system/tailscaled.service.d
    sudo cp /tmp/clusteros-files/systemd/tailscaled.service.d/*.conf \
        /etc/systemd/system/tailscaled.service.d/
    sudo systemctl daemon-reload
    echo "  tailscaled configured to wait for WiFi"
fi

# DISABLE the main wpa_supplicant.service (D-Bus service for NetworkManager)
# Netplan manages its own wpa_supplicant processes for WiFi
sudo systemctl unmask wpa_supplicant.service 2>/dev/null || true
sudo systemctl disable wpa_supplicant.service 2>/dev/null || true

# Disable NetworkManager - we use systemd-networkd + netplan + cluster-wifi
sudo systemctl disable NetworkManager.service 2>/dev/null || true

# Disable systemd-rfkill - it can restore a "blocked" state from previous boot
# Our cluster-wifi.service will handle rfkill unblocking
sudo systemctl disable systemd-rfkill.service 2>/dev/null || true
sudo systemctl mask systemd-rfkill.service 2>/dev/null || true
sudo systemctl disable systemd-rfkill.socket 2>/dev/null || true
sudo systemctl mask systemd-rfkill.socket 2>/dev/null || true
# Pre-unblock rfkill during image build (create a clean state)
sudo mkdir -p /var/lib/systemd/rfkill
echo "0" | sudo tee /var/lib/systemd/rfkill/platform-*:wlan 2>/dev/null || true
echo "  Disabled systemd-rfkill (prevents restoring blocked state)"

# Install and enable WiFi setup service
if [ -f /tmp/clusteros-files/systemd/cluster-wifi.service ]; then
    sudo cp /tmp/clusteros-files/systemd/cluster-wifi.service /etc/systemd/system/
    sudo systemctl daemon-reload
    sudo systemctl enable cluster-wifi.service
    echo "  WiFi auto-connect service enabled (uses netplan)"
fi

# Install rfkill unblock service - keeps WiFi unblocked after cloud-init
if [ -f /tmp/clusteros-files/systemd/wifi-rfkill-unblock.service ]; then
    sudo cp /tmp/clusteros-files/systemd/wifi-rfkill-unblock.service /etc/systemd/system/
    sudo systemctl daemon-reload
    sudo systemctl enable wifi-rfkill-unblock.service
    echo "  WiFi rfkill unblock service enabled"
fi

# WireGuard is replaced by Tailscale — no /etc/wireguard directory needed.

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
    # cluster-autostart no longer exists — bootstrapping is in node-agent.service ExecStartPre.
    sudo chmod +x /usr/local/bin/cluster-wifi-setup 2>/dev/null || true
    sudo chmod +x /usr/local/bin/tailscale-auth 2>/dev/null || true
    sudo chmod +x /usr/local/bin/debug-tailscale 2>/dev/null || true
fi

# cluster-autostart.service was a legacy shim that has been replaced by
# node-agent.service ExecStartPre (which runs apply-patch.sh on first boot).
# No separate service file needed — node-agent.service handles bootstrapping.

# SSH config - disable root login
sudo sed -i 's/^#*PermitRootLogin.*/PermitRootLogin no/' /etc/ssh/sshd_config

# Apply sysctl settings
sudo sysctl --system

# Verify installation
echo ""
echo "============================================"
echo "Cleanup for Image Distribution"
echo "============================================"

# CRITICAL: Remove any identity file created during build.
# Each node MUST generate its own unique identity on first boot.
# Without this, all nodes cloned from this image share the same NodeID
# and Serf treats them as a single node.
sudo rm -f /var/lib/cluster-os/identity.json /var/lib/cluster-os/identity.json.tmp
echo "  Removed identity file (will regenerate on first boot)"

# Remove any K3s state from build (etcd DB has wrong member entries if cloned)
sudo rm -rf /var/lib/rancher/k3s/server/db /var/lib/rancher/k3s/agent
echo "  Removed K3s runtime state (will initialize fresh on first boot)"

# Remove any SLURM config (controller generates it after cluster assembly)
sudo rm -f /etc/slurm/slurm.conf
echo "  Removed slurm.conf (will be generated by controller)"

# Remove any Raft state
sudo rm -rf /var/lib/cluster-os/raft
echo "  Removed Raft state (will bootstrap fresh)"

# Also clean up any machine-id that cloud-init may have cached
# (some distros bake this into images, causing duplicate DHCP leases)
sudo truncate -s 0 /etc/machine-id 2>/dev/null || true
echo "  Reset machine-id (will regenerate on first boot)"

echo ""
echo "============================================"
echo "Installation Summary"
echo "============================================"
echo "node-agent:  $(node-agent --version 2>&1 || echo 'installed')"
echo "k3s:         $(k3s --version 2>&1 | head -1 || echo 'installed')"
echo "slurm:       $(sinfo --version 2>&1 || echo 'installed')"
echo "tailscale:   $(tailscale version 2>&1 | head -1 || echo 'installed')"
# WireGuard replaced by Tailscale — wg binary not installed.
echo ""
echo "ClusterOS commands installed:"
ls -1 /usr/local/bin/cluster-* 2>/dev/null || echo "  (none)"
echo ""
echo "Enabled services:"
systemctl list-unit-files | grep -E "node-agent|k3s|slurm|munge|tailscale|cluster-autostart|cluster-wifi" | head -20
echo ""
echo "WiFi configured: TALKTALK665317 (see images/ubuntu/files/netplan/99-clusteros.yaml)"
echo ""
echo "============================================"
echo "Provisioning Complete"
echo "============================================"
