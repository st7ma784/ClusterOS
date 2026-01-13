# Getting Started with Cluster-OS

This guide will get you from zero to a running cluster in under 30 minutes.

## Step 1: Install Prerequisites (5 minutes)

Cluster-OS requires Packer, QEMU, and some supporting tools.

### Quick Install

```bash
# Run the automated installer
./scripts/install-prereqs.sh

# Verify installation
./scripts/verify-prereqs.sh
```

**IMPORTANT**: Log out and back in after installation for group changes to take effect!

### Manual Install

If you prefer to install manually:

```bash
# Install QEMU and virtualization
sudo apt-get install -y qemu-system-x86 qemu-utils qemu-kvm cloud-image-utils genisoimage

# Install Packer
wget -O- https://apt.releases.hashicorp.com/gpg | sudo gpg --dearmor -o /usr/share/keyrings/hashicorp-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/hashicorp-archive-keyring.gpg] https://apt.releases.hashicorp.com focal main" | sudo tee /etc/apt/sources.list.d/hashicorp.list
sudo apt update && sudo apt install packer

# Configure user groups
sudo usermod -aG kvm $USER
sudo usermod -aG libvirt $USER
```

## Step 2: Build Node Agent (1 minute)

```bash
make node
```

This builds the `node-agent` binary that will run on each cluster node.

## Step 3: Build OS Image (10-20 minutes)

```bash
make image
```

This uses Packer to:
1. Download Ubuntu 24.04 ISO (~2.5GB)
2. Install and configure the system
3. Install WireGuard, SLURM, K3s
4. Create bootable disk images

**Build time depends on your system:**
- **With KVM acceleration**: 10-20 minutes (requires `/dev/kvm`)
- **Without KVM (TCG fallback)**: 1-2 hours (no special hardware needed)

Packer will automatically use KVM if available, otherwise fall back to TCG.

### What Gets Created

```
/data/packer-output/cluster-os-node/
├── cluster-os-node.qcow2      # QEMU VM image
├── cluster-os-node.raw        # Raw disk image
└── cluster-os-node.raw.gz     # Compressed installer
```

**Note**: Packer uses `/data/packer-output/` as the build output directory (not the source tree). This keeps the repository clean and is configured in [images/ubuntu/packer.pkr.hcl](images/ubuntu/packer.pkr.hcl).

## Step 4: Launch Test Cluster (2 minutes)

```bash
# Launch 3-node cluster
make test-vm

# Or launch 5-node cluster
make test-vm-5
```

Wait about 60 seconds for VMs to boot and initialize.

## Step 5: Verify Cluster

### Check Status

```bash
make vm-status
```

Expected output:
```
=========================================
Cluster OS VM Status
=========================================
✓ node1 - RUNNING (PID: 12345, SSH port: 2223)
✓ node2 - RUNNING (PID: 12346, SSH port: 2224)
✓ node3 - RUNNING (PID: 12347, SSH port: 2225)
```

### SSH to a Node

```bash
# Using control script
./test/vm/qemu/cluster-ctl.sh shell 1

# Or directly
ssh -p 2223 clusteros@localhost
# Password: clusteros
```

### Run Integration Tests

```bash
./test/vm/integration-test.sh
```

This tests:
- VM boot and SSH access
- systemd as PID 1
- node-agent service
- Node identity generation
- WireGuard installation
- SLURM installation
- K3s installation
- Network connectivity

## Step 6: Explore the Cluster

### Check Node Agent

```bash
# SSH to node 1
./test/vm/qemu/cluster-ctl.sh shell 1

# Inside the VM:
sudo systemctl status node-agent
sudo journalctl -u node-agent -f
```

### Check WireGuard

```bash
# View WireGuard interface
sudo wg show

# Check WireGuard IP
ip addr show wg0

# Test connectivity to peers
ping 10.42.92.120  # Example peer IP
```

### Check SLURM

```bash
# Check SLURM services
sudo systemctl status munge
sudo systemctl status slurmctld  # On controller
sudo systemctl status slurmd      # On worker

# View cluster info
sinfo
scontrol show nodes

# Submit test job
sbatch -N 2 --wrap="hostname && sleep 30"
squeue
```

### Check K3s

```bash
# Check K3s service
sudo systemctl status k3s        # On server
sudo systemctl status k3s-agent  # On agent

# View cluster
sudo k3s kubectl get nodes
sudo k3s kubectl get pods -A

# Deploy test app
sudo k3s kubectl run nginx --image=nginx
sudo k3s kubectl get pods
```

## Step 7: Manage the Cluster

### Useful Commands

```bash
# Show cluster status
make vm-status

# Show detailed info
make vm-info

# Execute command on a node
./test/vm/qemu/cluster-ctl.sh exec 1 "uptime"

# View console logs
./test/vm/qemu/cluster-ctl.sh logs 1

# Stop cluster
make vm-stop

# Clean all data and start fresh
make vm-clean
```

### Access All Nodes

```bash
# SSH to each node
./test/vm/qemu/cluster-ctl.sh shell 1  # Node 1 (port 2223)
./test/vm/qemu/cluster-ctl.sh shell 2  # Node 2 (port 2224)
./test/vm/qemu/cluster-ctl.sh shell 3  # Node 3 (port 2225)

# Or direct SSH
ssh -p 2223 clusteros@localhost  # Node 1
ssh -p 2224 clusteros@localhost  # Node 2
ssh -p 2225 clusteros@localhost  # Node 3
```

### VNC Access (Graphical)

```bash
# Install VNC viewer
sudo apt-get install tigervnc-viewer

# Connect to node 1
vncviewer localhost:5900

# Node 2: localhost:5901
# Node 3: localhost:5902
```

## Step 8: Create USB Installer (Optional)

To deploy to physical hardware:

```bash
# Create USB/ISO installer
make usb
```

This creates:
- `dist/cluster-os-installer.iso` - Bootable ISO
- `dist/cluster-os-usb.img.gz` - Compressed USB image

### Write to USB Drive

```bash
# Find your USB device
lsblk

# Write image (WARNING: This erases the USB drive!)
gunzip -c dist/cluster-os-usb.img.gz | sudo dd of=/dev/sdX bs=4M status=progress

# Replace /dev/sdX with your actual USB device
```

### Boot from USB

1. Insert USB into target machine
2. Enter BIOS/UEFI (F2, F12, or Del)
3. Set USB as first boot device
4. Disable Secure Boot if required
5. Save and reboot

The machine will boot Cluster-OS and automatically join the cluster!

## Common Workflows

### Development Iteration

```bash
# 1. Make changes to node-agent
vim node/cmd/node-agent/main.go

# 2. Rebuild binary
make node

# 3. Rebuild image
make clean
make image

# 4. Test
make vm-clean
make test-vm
./test/vm/integration-test.sh
```

### Testing Options

**For Full Cluster Testing (Recommended):**

Use QEMU VMs (tested above) - full systemd support, SLURM, K3s all functional.

**For Node Agent Development Only:**

Docker is available for rapid iteration on node-agent logic, but has significant limitations:

```bash
# NOT recommended for system testing
make test-cluster
```

⚠️ **Docker Limitations:**
- No systemd (containers run single process)
- SLURM doesn't work properly in containers
- K3s doesn't work properly in containers
- No WireGuard networking support
- Limited to testing node-agent code changes only

**Recommendation:** Always use QEMU VMs (`make test-vm`) for validating the complete system.

### Debugging

```bash
# View VM console logs
./test/vm/qemu/cluster-ctl.sh logs 1

# Check Packer build
cd images/ubuntu
PACKER_LOG=1 packer build packer.pkr.hcl

# Check VM process
ps aux | grep qemu

# Kill stuck VMs
make vm-clean
```

## Next Steps

### Learn More

- **[PACKER_QEMU_QUICKSTART.md](PACKER_QEMU_QUICKSTART.md)** - Quick reference
- **[docs/VM_TESTING.md](docs/VM_TESTING.md)** - Detailed testing guide
- **[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)** - Production deployment
- **[CLAUDE.md](CLAUDE.md)** - Full specification

### Customize Your Cluster

1. **Pre-seed cluster key**:
   ```bash
   # Generate shared secret
   openssl rand -hex 32 > cluster.key

   # Rebuild image (will include key)
   make image
   ```

2. **Adjust VM resources**:
   ```bash
   NUM_NODES=5 MEMORY=4096 CPUS=4 make test-vm
   ```

3. **Configure WiFi**:
   Edit `images/ubuntu/netplan/01-clusteros-network.yaml`

4. **Modify roles**:
   Edit node-agent logic in `node/internal/roles/`

### Deploy to Production

See [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) for:
- PXE boot setup
- Cloud provider deployment
- Large-scale cluster management
- Security hardening

## Troubleshooting

### "Permission denied" on /dev/kvm

```bash
# Add yourself to kvm group
sudo usermod -aG kvm $USER

# Log out and back in
```

### VM won't boot

```bash
# Check logs
cat test/vm/qemu/vms/node1/serial.log

# Try without KVM
# Edit test/vm/qemu/start-cluster.sh and remove -accel kvm
```

### Can't SSH to VM

```bash
# Wait longer (60 seconds for cloud-init)
sleep 60

# Check if SSH port is open
netstat -tlnp | grep 2223
```

### Packer build fails

```bash
# Enable debug logging
cd images/ubuntu
PACKER_LOG=1 packer build packer.pkr.hcl

# Check disk space
df -h .
```

### Out of disk space

```bash
# Clean everything
make clean
make vm-clean

# Remove old images
rm -rf images/ubuntu/output-*
```

## Getting Help

- **Documentation**: [docs/](docs/)
- **Issues**: File a GitHub issue
- **Verification**: Run `./scripts/verify-prereqs.sh`

## Success! 🎉

You now have:
- ✅ A working node-agent
- ✅ Bootable OS images
- ✅ Running QEMU VM cluster
- ✅ Full systemd support
- ✅ SLURM and K3s capabilities
- ✅ USB installer for hardware

**Total time**: ~30 minutes
**What you tested**: Everything except bare-metal deployment

Next: Deploy to actual hardware or scale up your QEMU cluster!
