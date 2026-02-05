# ClusterOS Boot and Connectivity Fix Summary

## Issues Addressed

### 1. Live Image Not Connecting to Cluster
**Problem:** Node agent wasn't starting properly to form a cluster on first boot.

**Root Cause:** 
- Missing `tailscale-auth` script to authenticate with Tailscale
- Tailscale service dependency issue - node-agent started before Tailscale was ready
- Incorrect systemd service condition prevented authentication

**Solution:**
- Created `/usr/local/bin/tailscale-auth` script with OAuth support
- Created `/usr/local/bin/wait-for-tailscale` polling script
- Updated `tailscale-auth.service` to remove blocking condition
- Updated `node-agent.service` to wait for Tailscale properly

### 2. EFI Boot Failure After Installation
**Problem:** When installing to bare metal with `cluster-os-install`, the system wouldn't boot when installation media was removed.

**Root Cause:**
- No proper bootloader installation in the installer script
- Missing EFI partition handling
- No GRUB installation for EFI systems

**Solution:**
- Created comprehensive `/usr/local/bin/cluster-os-install` script with:
  - EFI partition detection and mounting
  - GRUB EFI bootloader installation (`grub-install --target=x86_64-efi`)
  - Fallback to BIOS/MBR boot if EFI fails
  - Proper fstab configuration with UUIDs
  - Cluster credential and Tailscale config copying
  - Chroot environment for proper bootloader installation

### 3. Missing Node-Agent Entry Point
**Problem:** The `node-agent` binary couldn't be built - missing `cmd/node-agent/main.go`.

**Root Cause:**
- Repository structure was incomplete
- No CLI entry point for the node-agent daemon

**Solution:**
- Created `node/cmd/node-agent/main.go` with full CLI:
  - `start` - Start the daemon (with --foreground)
  - `init` - Initialize node identity
  - `info` - Show node information
  - `status` - Show node status
  - `version` - Show version
- Fixed Makefile to use system Go installation

### 4. WiFi Connectivity
**Problem:** Nodes needed to connect via WiFi to your network automatically.

**Solution:**
- Added WiFi packages to cloud-init (wpasupplicant, wireless-tools, iw, rfkill)
- Configured default WiFi in netplan:
  - SSID: TALKTALK665317
  - Password: NXJP7U39
- Added documentation noting these are cluster-specific and should be customized

## Files Created

### Scripts in `/usr/local/bin/`
1. **tailscale-auth** - Authenticates with Tailscale using OAuth or static keys
2. **cluster-os-install** - Installs ClusterOS to bare metal with EFI boot support  
3. **wait-for-tailscale** - Polls Tailscale status before starting node-agent
4. **node-agent** - Main ClusterOS node agent binary

### Configuration Files
1. **images/ubuntu/files/systemd/node-agent.service** - Updated with wait-for-tailscale
2. **images/ubuntu/files/systemd/tailscale-auth.service** - Fixed condition
3. **images/ubuntu/files/netplan/99-clusteros.yaml** - Added WiFi configuration
4. **images/ubuntu/cloud-init/user-data** - Added WiFi packages

### Source Code
1. **node/cmd/node-agent/main.go** - CLI entry point for node-agent
2. **Makefile** - Fixed Go path detection

## Service Startup Sequence

The proper startup order is now:

```
network-online.target
    ↓
tailscaled.service
    ↓
tailscale-auth.service (runs once on first boot)
    ↓
wait-for-tailscale (polls until ready)
    ↓
node-agent.service
```

## Testing Instructions

### 1. Build the Image

```bash
# Generate cluster key if not exists
make cluster-key

# Build the OS image (requires Packer and QEMU)
make image
```

This will create:
- `/data/packer-output/cluster-os-node/cluster-os-node.qcow2` (VM image)
- `/data/packer-output/cluster-os-node/cluster-os-node.raw` (bare metal image)
- `/data/packer-output/cluster-os-node/cluster-os-node.raw.gz` (compressed)

### 2. Test in VM

```bash
# Test with QEMU
qemu-system-x86_64 -enable-kvm -m 2048 \
  -hda /data/packer-output/cluster-os-node/cluster-os-node.qcow2
```

### 3. Create Bootable USB

```bash
# Create USB installer
make usb

# Write to USB drive (replace /dev/sdX with your USB device)
sudo dd if=dist/cluster-os-usb.img of=/dev/sdX bs=4M status=progress oflag=sync
```

### 4. Install to Bare Metal

Boot from the USB drive, then:

```bash
# View available disks
lsblk

# Install to target disk (e.g., /dev/sda)
sudo cluster-os-install /dev/sda
```

The installer will:
1. Write the OS image to the target disk
2. Install the EFI bootloader
3. Update fstab with proper UUIDs
4. Copy cluster credentials
5. Make the system bootable

### 5. Verify on First Boot

After booting the installed system:

```bash
# Check Tailscale status
tailscale status

# Check node-agent status
systemctl status node-agent

# View node information
node-agent info

# Check cluster connectivity
journalctl -u node-agent -f
```

## Expected Behavior

1. **Network Connectivity:**
   - Wired network: Automatically connects via DHCP
   - WiFi: Automatically connects to TALKTALK665317

2. **Tailscale Authentication:**
   - On first boot, `tailscale-auth` runs automatically
   - Uses OAuth credentials from `/etc/clusteros/tailscale.env`
   - Falls back to static auth key if OAuth not configured
   - Node appears in your Tailnet

3. **Cluster Formation:**
   - Node agent starts after Tailscale is ready
   - Discovers other nodes via Tailscale network
   - Automatically participates in leader election
   - Joins SLURM and Kubernetes clusters

4. **EFI Boot:**
   - System boots directly from disk
   - No need for installation media
   - GRUB menu shows ClusterOS boot option

## Customization

### Change WiFi Credentials

Edit `images/ubuntu/files/netplan/99-clusteros.yaml` before building:

```yaml
access-points:
  "YOUR-SSID":
    password: "YOUR-PASSWORD"
```

Then rebuild the image with `make image`.

### Configure Tailscale OAuth

Create `images/ubuntu/.env` from `.env.example`:

```bash
TAILSCALE_OAUTH_CLIENT_ID=your-client-id
TAILSCALE_OAUTH_CLIENT_SECRET=your-client-secret
```

Get OAuth credentials from: https://login.tailscale.com/admin/settings/oauth

Required scopes: `devices` (create)

## Troubleshooting

### Node doesn't join Tailscale

Check logs:
```bash
journalctl -u tailscale-auth
journalctl -u tailscaled
```

Manually authenticate:
```bash
sudo tailscale up --authkey=YOUR-KEY
```

### Node agent doesn't start

Check if Tailscale is ready:
```bash
tailscale status
```

Check node-agent logs:
```bash
journalctl -u node-agent -xe
```

### EFI boot fails

Check bootloader installation:
```bash
sudo efibootmgr -v
ls -la /boot/efi/EFI/ubuntu/
```

Reinstall bootloader:
```bash
sudo grub-install --target=x86_64-efi --efi-directory=/boot/efi
sudo update-grub
```

### WiFi not connecting

Check WiFi interface:
```bash
ip link
sudo rfkill list
```

Apply netplan:
```bash
sudo netplan apply
```

Check connection:
```bash
sudo wpa_cli status
```

## Security Notes

1. **WiFi Credentials:** The default WiFi credentials are embedded in the image. Anyone with access to the image can extract them. For production use, consider:
   - Using WPA2-Enterprise with per-device credentials
   - Generating per-cluster WiFi credentials
   - Using wired connections only

2. **Cluster Key:** The cluster authentication key in `/etc/clusteros/cluster.key` allows nodes to join your cluster. Keep this key secure.

3. **Tailscale OAuth:** OAuth credentials provide access to your Tailnet. Store them securely and rotate them periodically.

## Next Steps

1. Build and test the image in a VM
2. Verify Tailscale authentication works
3. Test USB creation and bare-metal installation
4. Verify EFI boot on target hardware
5. Confirm cluster formation with multiple nodes
6. Test SLURM and Kubernetes workloads

## Support

For issues or questions:
1. Check the logs: `journalctl -u node-agent -u tailscaled`
2. Review the scripts: `/usr/local/bin/cluster-*`
3. Check configuration: `/etc/clusteros/`

All scripts include comprehensive logging and error messages to help troubleshoot issues.
