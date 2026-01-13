# Cluster OS Deployment Guide

This guide covers deploying Cluster OS to physical hardware or production VMs.

## Deployment Methods

1. **USB/ISO Installer** - Boot from USB and install to disk
2. **PXE Boot** - Network boot for datacenter deployment
3. **Cloud Image** - Deploy to cloud providers (AWS, GCP, Azure)
4. **Direct Disk Write** - Write image directly to hardware

## Method 1: USB/ISO Installer

### Build the Installer

```bash
# Build the OS image
make image

# Create USB installer
make usb
```

This creates:
- `dist/cluster-os-installer.iso` - Bootable ISO
- `dist/cluster-os-usb.img.gz` - USB image

### Write to USB Drive

**Warning**: This erases all data on the USB drive!

```bash
# Find your USB device
lsblk

# Write compressed image (recommended)
gunzip -c dist/cluster-os-usb.img.gz | sudo dd of=/dev/sdX bs=4M status=progress oflag=sync

# Or write ISO
sudo dd if=dist/cluster-os-installer.iso of=/dev/sdX bs=4M status=progress oflag=sync

# Sync and eject
sync
sudo eject /dev/sdX
```

### Boot and Install

1. **Prepare target machine**:
   - Insert USB drive
   - Enter BIOS/UEFI (usually F2, F12, or Del during boot)
   - Set boot order: USB first
   - Disable Secure Boot (if required)

2. **Boot from USB**:
   - Machine will boot into Cluster OS installer
   - Wait for boot process (~30 seconds)

3. **Automatic installation**:
   - The image will boot and run Cluster OS automatically
   - For permanent installation, run the installer script:
   ```bash
   # From the USB boot environment
   lsblk  # Identify target disk

   # Install to target disk
   sudo dd if=/dev/sda of=/dev/nvme0n1 bs=4M status=progress
   # Replace /dev/sda (USB) and /dev/nvme0n1 (target) as needed
   ```

4. **Remove USB and reboot**:
   ```bash
   sudo reboot
   ```

### First Boot Configuration

On first boot, the node will:
1. Generate unique cryptographic identity
2. Look for cluster join information
3. Start node-agent service
4. Attempt to discover other nodes

## Method 2: PXE Network Boot

PXE boot is ideal for deploying many machines quickly in a datacenter.

### Setup PXE Server

```bash
# Install PXE server components
sudo apt-get install -y dnsmasq nginx

# Create PXE directory structure
sudo mkdir -p /srv/tftp
sudo mkdir -p /var/www/html/clusteros

# Extract boot files from image
mkdir /tmp/mount
sudo mount -o loop,offset=$((2048*512)) images/ubuntu/output-cluster-os-node/cluster-os-node.raw /tmp/mount

# Copy kernel and initrd
sudo cp /tmp/mount/boot/vmlinuz-* /srv/tftp/vmlinuz
sudo cp /tmp/mount/boot/initrd.img-* /srv/tftp/initrd.img

# Copy full image for installation
sudo cp images/ubuntu/output-cluster-os-node/cluster-os-node.raw /var/www/html/clusteros/

# Cleanup
sudo umount /tmp/mount
```

### Configure dnsmasq

```bash
# Edit /etc/dnsmasq.conf
sudo tee /etc/dnsmasq.conf <<EOF
# DHCP configuration
dhcp-range=192.168.1.100,192.168.1.200,12h
dhcp-boot=pxelinux.0

# TFTP configuration
enable-tftp
tftp-root=/srv/tftp

# PXE boot file
dhcp-match=set:efi-x86_64,option:client-arch,7
dhcp-boot=tag:efi-x86_64,bootx64.efi
EOF

# Restart dnsmasq
sudo systemctl restart dnsmasq
```

### Configure PXE Boot Menu

```bash
# Create pxelinux config
sudo mkdir -p /srv/tftp/pxelinux.cfg

sudo tee /srv/tftp/pxelinux.cfg/default <<'EOF'
DEFAULT clusteros
LABEL clusteros
  KERNEL vmlinuz
  APPEND initrd=initrd.img boot=live fetch=http://192.168.1.1/clusteros/cluster-os-node.raw
  TEXT HELP
    Boot Cluster OS
  ENDTEXT
EOF
```

### Boot Nodes

1. Configure target machines for PXE boot in BIOS
2. Connect to same network as PXE server
3. Power on
4. Machines will automatically download and boot Cluster OS

## Method 3: Cloud Provider Deployment

### AWS EC2

```bash
# Convert qcow2 to raw
qemu-img convert -f qcow2 -O raw \
  images/ubuntu/output-cluster-os-node/cluster-os-node.qcow2 \
  cluster-os.raw

# Create AMI from raw image
aws ec2 import-snapshot \
  --description "Cluster OS" \
  --disk-container file://container.json

# container.json:
{
  "Description": "Cluster OS Disk",
  "Format": "raw",
  "UserBucket": {
    "S3Bucket": "my-bucket",
    "S3Key": "cluster-os.raw"
  }
}

# Launch instances
aws ec2 run-instances \
  --image-id ami-xxxxx \
  --instance-type t3.medium \
  --count 3 \
  --security-group-ids sg-xxxxx \
  --subnet-id subnet-xxxxx
```

### Google Cloud Platform

```bash
# Upload to Google Cloud Storage
gsutil cp images/ubuntu/output-cluster-os-node/cluster-os-node.raw.gz \
  gs://my-bucket/cluster-os.raw.gz

# Create image
gcloud compute images create cluster-os \
  --source-uri gs://my-bucket/cluster-os.raw.gz

# Create instances
gcloud compute instances create cluster-node-{1..3} \
  --image cluster-os \
  --machine-type n1-standard-2 \
  --zone us-central1-a
```

## Method 4: Direct Disk Write

For single machines or small deployments:

```bash
# Boot from Ubuntu Live USB

# Find target disk
lsblk

# Write image directly
wget http://your-server/cluster-os-node.raw.gz
gunzip -c cluster-os-node.raw.gz | sudo dd of=/dev/sda bs=4M status=progress

# Sync and reboot
sync
sudo reboot
```

## Cluster Configuration

### Pre-seeding Cluster Key

To have nodes automatically join a cluster, pre-seed the cluster key:

```bash
# Generate shared cluster key
openssl rand -hex 32 > cluster.key

# During Packer build, the key is automatically included
make image

# Or manually add to existing installation:
# SSH to node and run:
echo "YOUR-CLUSTER-KEY" | sudo tee /etc/cluster-os/cluster.key
sudo chmod 600 /etc/cluster-os/cluster.key
sudo systemctl restart node-agent
```

### Bootstrap Node

The first node becomes the bootstrap node automatically:

```bash
# Check node identity
sudo cat /var/lib/cluster-os/identity/node_id

# Check discovery status
sudo journalctl -u node-agent -f
```

### Joining Additional Nodes

Additional nodes automatically discover and join the cluster:

```bash
# Check cluster membership
sudo cat /var/lib/cluster-os/cluster-state.json

# View discovered peers
sudo serf members
```

## Network Configuration

### WiFi Setup

Edit `/etc/netplan/01-clusteros-network.yaml`:

```yaml
network:
  version: 2
  wifis:
    wlan0:
      dhcp4: true
      access-points:
        "YOUR-SSID":
          password: "YOUR-PASSWORD"
```

Apply:
```bash
sudo netplan apply
```

### Static IP

```yaml
network:
  version: 2
  ethernets:
    eth0:
      addresses:
        - 192.168.1.10/24
      gateway4: 192.168.1.1
      nameservers:
        addresses:
          - 8.8.8.8
          - 8.8.4.4
```

## Monitoring Deployment

### Check System Status

```bash
# System services
sudo systemctl status node-agent
sudo systemctl status wireguard@wg0

# Network
ip addr show wg0
wg show

# Cluster state
sudo journalctl -u node-agent -n 100
```

### Verify Cluster Formation

```bash
# Check Serf membership
sudo serf members

# Check WireGuard connections
sudo wg show all

# Test inter-node connectivity
ping <peer-wireguard-ip>
```

### Check Role Assignment

```bash
# View assigned roles
cat /var/lib/cluster-os/roles.json

# SLURM status (if role assigned)
sudo systemctl status slurmctld  # Controller
sudo systemctl status slurmd      # Worker

# K3s status (if role assigned)
sudo systemctl status k3s         # Server
sudo systemctl status k3s-agent   # Agent
```

## Troubleshooting

### Node Won't Boot

1. Check boot order in BIOS
2. Verify USB/disk is bootable: `sudo fdisk -l /dev/sdX`
3. Check serial console for errors
4. Disable Secure Boot if enabled

### Node Won't Join Cluster

1. Check network connectivity:
   ```bash
   ping -c 3 8.8.8.8
   ```

2. Verify cluster key:
   ```bash
   sudo cat /etc/cluster-os/cluster.key
   ```

3. Check discovery port (7946):
   ```bash
   sudo ss -tuln | grep 7946
   ```

4. View node-agent logs:
   ```bash
   sudo journalctl -u node-agent -f
   ```

### WireGuard Not Starting

1. Check kernel module:
   ```bash
   sudo modprobe wireguard
   lsmod | grep wireguard
   ```

2. Check interface:
   ```bash
   sudo wg show
   sudo ip link show wg0
   ```

3. Verify configuration:
   ```bash
   sudo cat /etc/wireguard/wg0.conf
   ```

### SLURM/K3s Not Starting

1. Check service status:
   ```bash
   sudo systemctl status slurmd
   sudo systemctl status k3s
   ```

2. Check role assignment:
   ```bash
   cat /var/lib/cluster-os/roles.json
   ```

3. Manual start:
   ```bash
   sudo systemctl start slurmd
   sudo journalctl -u slurmd -f
   ```

## Security Considerations

### Firewall Configuration

```bash
# Allow cluster ports
sudo ufw allow 7946/tcp   # Serf
sudo ufw allow 7946/udp
sudo ufw allow 51820/udp  # WireGuard
sudo ufw allow 6443/tcp   # K3s API
sudo ufw allow 6817/tcp   # SLURM
```

### SSH Hardening

Edit `/etc/ssh/sshd_config`:

```
PermitRootLogin no
PasswordAuthentication no
PubkeyAuthentication yes
```

### Update Management

```bash
# Update node-agent
sudo systemctl stop node-agent
sudo cp /path/to/new/node-agent /usr/local/bin/
sudo systemctl start node-agent

# System updates
sudo apt-get update
sudo apt-get upgrade
```

## Next Steps

After deployment:

1. [Configure SLURM workloads](SLURM.md)
2. [Deploy Kubernetes applications](KUBERNETES.md)
3. [Setup JupyterHub](JUPYTER.md)
4. [Monitor cluster health](MONITORING.md)
