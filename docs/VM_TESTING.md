# VM Testing with QEMU

This guide explains how to test Cluster OS using QEMU virtual machines instead of Docker containers.

## Why QEMU Instead of Docker?

Docker containers have fundamental limitations that prevent full testing of Cluster OS:

1. **No systemd as PID 1**: Docker doesn't support systemd as the init system
2. **No /dev/kmsg access**: K3s kubelet requires kernel message buffer access
3. **Limited cgroup support**: SLURM needs full cgroup integration
4. **No D-Bus**: Required for systemd service management

QEMU VMs solve these issues by providing:
- Full systemd support
- Complete kernel access
- Native cgroup management
- Real init system

## Prerequisites

### Install QEMU and Packer

```bash
# Install QEMU
sudo apt-get update
sudo apt-get install -y qemu-system-x86 qemu-utils cloud-image-utils genisoimage

# Install Packer
wget -O- https://apt.releases.hashicorp.com/gpg | sudo gpg --dearmor -o /usr/share/keyrings/hashicorp-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/hashicorp-archive-keyring.gpg] https://apt.releases.hashicorp.com focal main" | sudo tee /etc/apt/sources.list.d/hashicorp.list
sudo apt update && sudo apt install packer

# Verify installation
packer --version
qemu-system-x86_64 --version
```

### Enable KVM (Optional but Recommended)

KVM acceleration makes VMs run much faster:

```bash
# Check if KVM is available
egrep -c '(vmx|svm)' /proc/cpuinfo  # Should be > 0

# Install KVM
sudo apt-get install -y qemu-kvm libvirt-daemon-system

# Add your user to kvm group
sudo usermod -aG kvm $USER

# Log out and back in for group changes to take effect
```

## Quick Start

### 1. Build the OS Image

Build the base OS image using Packer (takes 10-20 minutes):

```bash
make image
```

This creates:
- `images/ubuntu/output-cluster-os-node/cluster-os-node.qcow2` - VM disk image
- `images/ubuntu/output-cluster-os-node/cluster-os-node.raw` - Raw disk image
- `images/ubuntu/output-cluster-os-node/cluster-os-node.raw.gz` - Compressed installer

### 2. Start VM Cluster

Launch a 3-node cluster:

```bash
make test-vm
```

Or a 5-node cluster:

```bash
make test-vm-5
```

Or customize:

```bash
NUM_NODES=7 MEMORY=4096 CPUS=4 make test-vm
```

### 3. Check Cluster Status

```bash
make vm-status
```

Output example:
```
=========================================
Cluster OS VM Status
=========================================
✓ node1 - RUNNING (PID: 12345, SSH port: 2223)
✓ node2 - RUNNING (PID: 12346, SSH port: 2224)
✓ node3 - RUNNING (PID: 12347, SSH port: 2225)
```

### 4. Access VMs

SSH to a node:

```bash
./test/vm/qemu/cluster-ctl.sh shell 1
# Or directly:
ssh -p 2223 clusteros@localhost
# Password: clusteros
```

Execute commands:

```bash
./test/vm/qemu/cluster-ctl.sh exec 1 "sudo systemctl status node-agent"
```

View logs:

```bash
./test/vm/qemu/cluster-ctl.sh logs 1
```

### 5. Stop Cluster

```bash
make vm-stop
```

Or clean everything:

```bash
make vm-clean
```

## VM Cluster Management

### Cluster Control Script

The `cluster-ctl.sh` script provides comprehensive VM management:

```bash
# Show status of all VMs
./test/vm/qemu/cluster-ctl.sh status

# Show detailed info
./test/vm/qemu/cluster-ctl.sh info
./test/vm/qemu/cluster-ctl.sh info 1  # Single node

# SSH to a node
./test/vm/qemu/cluster-ctl.sh shell 1

# View serial console logs
./test/vm/qemu/cluster-ctl.sh logs 1

# Execute command on a node
./test/vm/qemu/cluster-ctl.sh exec 1 "sudo systemctl status node-agent"

# Stop all VMs
./test/vm/qemu/cluster-ctl.sh stop

# Clean all VM data
./test/vm/qemu/cluster-ctl.sh clean
```

### Manual VM Control

Start cluster manually with custom settings:

```bash
cd test/vm/qemu

# Start 5-node cluster with 4GB RAM and 4 CPUs each
NUM_NODES=5 MEMORY=4096 CPUS=4 ./start-cluster.sh

# Stop cluster
./stop-cluster.sh
```

## Testing SLURM and K3s

Unlike Docker, QEMU VMs have full systemd support, enabling complete testing:

### Test SLURM

```bash
# SSH to node 1
./test/vm/qemu/cluster-ctl.sh shell 1

# Inside the VM:
sudo systemctl status munge
sudo systemctl status slurmctld  # On controller
sudo systemctl status slurmd      # On workers

# Check SLURM cluster
sinfo
squeue

# Submit a test job
sbatch -N 2 --wrap="hostname && sleep 30"
```

### Test K3s

```bash
# SSH to node 1
./test/vm/qemu/cluster-ctl.sh shell 1

# Inside the VM:
sudo systemctl status k3s         # On server
sudo systemctl status k3s-agent   # On agents

# Check Kubernetes cluster
sudo k3s kubectl get nodes
sudo k3s kubectl get pods -A
```

### Test WireGuard Mesh

```bash
# On node 1
./test/vm/qemu/cluster-ctl.sh exec 1 "sudo wg show"

# Test connectivity between nodes
# Node 1 -> Node 2
./test/vm/qemu/cluster-ctl.sh exec 1 "ping -c 3 10.42.92.120"
```

## Network Configuration

### Port Forwarding

Each VM has SSH forwarded to the host:

- Node 1: localhost:2223
- Node 2: localhost:2224
- Node 3: localhost:2225
- Node N: localhost:222(N+2)

### VNC Access

VMs are accessible via VNC for graphical debugging:

- Node 1: vnc://localhost:5900
- Node 2: vnc://localhost:5901
- Node 3: vnc://localhost:5902

Use any VNC client:

```bash
# Install VNC viewer
sudo apt-get install tigervnc-viewer

# Connect to node 1
vncviewer localhost:5900
```

### VM Networking

VMs use QEMU user networking (SLIRP) by default. For advanced networking:

1. **Bridge networking** (requires setup):
   ```bash
   # Edit start-cluster.sh to use bridge
   -netdev bridge,id=net0,br=br0 \
   -device virtio-net-pci,netdev=net0
   ```

2. **TAP interfaces** (requires privileges):
   ```bash
   # Create TAP interfaces
   sudo ip tuntap add dev tap0 mode tap
   sudo ip link set tap0 up
   ```

## Creating USB Installers

### Build Bootable Images

```bash
# Build both ISO and USB image
make usb

# Or manually:
./scripts/create-usb-installer.sh --both
```

This creates:
- `dist/cluster-os-installer.iso` - Bootable ISO
- `dist/cluster-os-usb.img.gz` - Compressed USB image

### Write to USB Drive

**Warning**: This will erase all data on the USB drive!

```bash
# Find your USB device
lsblk

# Write the image (replace /dev/sdX with your device)
gunzip -c dist/cluster-os-usb.img.gz | sudo dd of=/dev/sdX bs=4M status=progress oflag=sync

# Or write the ISO
sudo dd if=dist/cluster-os-installer.iso of=/dev/sdX bs=4M status=progress oflag=sync
```

## Troubleshooting

### VM Won't Start

Check KVM access:
```bash
ls -la /dev/kvm
# Should be readable by your user or kvm group
```

Check logs:
```bash
cat test/vm/qemu/vms/node1/serial.log
```

### Can't SSH to VM

Wait for cloud-init to complete (~60 seconds after boot):
```bash
./test/vm/qemu/cluster-ctl.sh logs 1 | grep "Cloud-init"
```

Check SSH port:
```bash
netstat -tlnp | grep 2223
```

### Image Build Fails

Check Packer logs:
```bash
cd images/ubuntu
PACKER_LOG=1 packer build packer.pkr.hcl
```

Verify prerequisites:
```bash
packer --version
qemu-system-x86_64 --version
```

### Out of Disk Space

Clean old builds:
```bash
make clean

# Remove old VM disks
make vm-clean
```

### Slow Performance

Enable KVM acceleration (see Prerequisites)

Increase VM resources:
```bash
NUM_NODES=3 MEMORY=4096 CPUS=4 make test-vm
```

## Advanced Usage

### Custom Packer Builds

Edit `images/ubuntu/packer.pkr.hcl` to customize:

```hcl
variable "memory" {
  default = "4096"  # Increase memory
}

variable "disk_size" {
  default = "40G"   # Increase disk
}
```

Rebuild:
```bash
cd images/ubuntu
packer build packer.pkr.hcl
```

### Pre-seed Cluster Key

Create a cluster key file before building:

```bash
# Generate cluster key
openssl rand -hex 32 > cluster.key

# Build image with key
make image
```

### Snapshot VMs

Save VM state:
```bash
qemu-img snapshot -c snapshot1 test/vm/qemu/vms/node1/disk.qcow2
```

Restore snapshot:
```bash
qemu-img snapshot -a snapshot1 test/vm/qemu/vms/node1/disk.qcow2
```

## Comparison: Docker vs QEMU

| Feature | Docker | QEMU VM |
|---------|--------|---------|
| systemd support | ❌ Limited | ✅ Full |
| /dev/kmsg access | ❌ No | ✅ Yes |
| cgroup management | ❌ Limited | ✅ Full |
| SLURM | ⚠️ Partial | ✅ Full |
| K3s | ❌ Blocked | ✅ Full |
| Boot time | ~2s | ~30s |
| Resource usage | Low | Medium |
| Isolation | Process | Hardware |
| Best for | Quick tests | Full validation |

## Summary

QEMU VM testing provides complete validation of Cluster OS with full systemd, SLURM, and K3s support. While slower than Docker, it accurately represents bare-metal deployment.

**Recommended workflow**:
1. Develop with Docker (fast iteration)
2. Validate with QEMU (full feature testing)
3. Deploy to bare metal (production)
