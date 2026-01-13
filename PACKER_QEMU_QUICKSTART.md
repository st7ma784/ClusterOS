# Packer + QEMU Quick Start Guide

**TL;DR**: Docker testing hits systemd/cgroup limits. Use QEMU VMs for full SLURM and K3s testing.

## Why This Matters

Docker containers **cannot** fully test:
- ❌ SLURM (needs cgroups + systemd)
- ❌ K3s (needs /dev/kmsg access)
- ❌ Full systemd integration

QEMU VMs **can** test everything:
- ✅ Complete systemd support
- ✅ Full kernel access
- ✅ Real init system
- ✅ SLURM with cgroups
- ✅ K3s with kubelet

## Prerequisites Install

```bash
# Install QEMU
sudo apt-get install -y qemu-system-x86 qemu-utils cloud-image-utils genisoimage

# Install Packer
wget -O- https://apt.releases.hashicorp.com/gpg | sudo gpg --dearmor -o /usr/share/keyrings/hashicorp-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/hashicorp-archive-keyring.gpg] https://apt.releases.hashicorp.com $(lsb_release -cs) main" | sudo tee /etc/apt/sources.list.d/hashicorp.list
sudo apt update && sudo apt install packer

# Enable KVM (optional, makes VMs 10x faster)
sudo apt-get install -y qemu-kvm
sudo usermod -aG kvm $USER
# Log out and back in for group change
```

## 5-Minute Quick Start

```bash
# 1. Build the OS image (takes 10-20 minutes first time)
make image

# 2. Start 3-node QEMU cluster
make test-vm

# 3. Check status
make vm-status

# 4. SSH to node 1
./test/vm/qemu/cluster-ctl.sh shell 1

# Inside the VM, check services:
sudo systemctl status node-agent
sudo systemctl status wireguard@wg0
sudo wg show

# 5. Test SLURM (if controller role assigned)
sinfo
squeue

# 6. Test K3s (if k8s role assigned)
sudo k3s kubectl get nodes

# 7. Stop cluster
make vm-stop
```

## Common Commands

```bash
# Build OS image with Packer
make image                    # Takes 10-20 min first time

# Launch clusters
make test-vm                  # 3 nodes
make test-vm-5                # 5 nodes
NUM_NODES=7 make test-vm      # Custom count

# Cluster management
make vm-status                # Show status
make vm-info                  # Detailed info
make vm-stop                  # Stop VMs
make vm-clean                 # Stop and delete all data

# VM control script
./test/vm/qemu/cluster-ctl.sh status       # Status
./test/vm/qemu/cluster-ctl.sh shell 1      # SSH to node 1
./test/vm/qemu/cluster-ctl.sh logs 1       # View console logs
./test/vm/qemu/cluster-ctl.sh exec 1 "cmd" # Execute command

# Create USB installer
make usb                      # Creates ISO and USB image
```

## Access VMs

Each node has SSH and VNC access:

### SSH Access
```bash
# Node 1
ssh -p 2223 clusteros@localhost

# Node 2
ssh -p 2224 clusteros@localhost

# Node 3
ssh -p 2225 clusteros@localhost

# Password: clusteros
```

### VNC Access
```bash
# Install VNC viewer
sudo apt-get install tigervnc-viewer

# Connect to node 1
vncviewer localhost:5900

# Node 2: vnc://localhost:5901
# Node 3: vnc://localhost:5902
```

## Testing Workflows

### Test WireGuard Mesh

```bash
# Check WireGuard on all nodes
for i in {1..3}; do
  echo "=== Node $i ==="
  ./test/vm/qemu/cluster-ctl.sh exec $i "sudo wg show"
done

# Test connectivity
./test/vm/qemu/cluster-ctl.sh exec 1 "ping -c 3 10.42.92.120"
```

### Test SLURM

```bash
# SSH to node 1
./test/vm/qemu/cluster-ctl.sh shell 1

# Inside VM:
sudo systemctl status munge
sudo systemctl status slurmctld

# Check cluster
sinfo
scontrol show nodes

# Submit test job
sbatch -N 2 --wrap="hostname && srun hostname"
squeue
```

### Test K3s

```bash
# SSH to server node
./test/vm/qemu/cluster-ctl.sh shell 1

# Inside VM:
sudo systemctl status k3s

# Check cluster
sudo k3s kubectl get nodes
sudo k3s kubectl get pods -A

# Deploy test workload
sudo k3s kubectl run nginx --image=nginx
sudo k3s kubectl get pods
```

## USB/ISO Deployment

Create bootable installer:

```bash
# Build image first
make image

# Create USB installer
make usb

# Outputs:
# dist/cluster-os-installer.iso     - Bootable ISO
# dist/cluster-os-usb.img.gz        - USB image
```

Write to USB:

```bash
# Find USB device
lsblk

# Write (WARNING: erases USB!)
gunzip -c dist/cluster-os-usb.img.gz | sudo dd of=/dev/sdX bs=4M status=progress
```

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                    Packer Build Process                  │
│                                                          │
│  Ubuntu 24.04 ISO → Autoinstall → Provision → Image     │
│                                                          │
│  Provisions:                                             │
│    • node-agent binary                                   │
│    • WireGuard                                           │
│    • SLURM                                               │
│    • K3s                                                 │
│    • systemd services                                    │
│                                                          │
│  Output:                                                 │
│    • cluster-os-node.qcow2  (VM image)                  │
│    • cluster-os-node.raw    (Raw disk)                  │
│    • cluster-os-node.raw.gz (Compressed)                │
└─────────────────────────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────┐
│                  QEMU VM Test Cluster                    │
│                                                          │
│  ┌─────────┐  ┌─────────┐  ┌─────────┐                 │
│  │  Node 1 │  │  Node 2 │  │  Node 3 │                 │
│  │         │  │         │  │         │                 │
│  │ systemd │  │ systemd │  │ systemd │                 │
│  │  wg0    │──│  wg0    │──│  wg0    │  WireGuard      │
│  │ slurm   │  │ slurm   │  │ slurm   │  SLURM          │
│  │  k3s    │  │  k3s    │  │  k3s    │  Kubernetes     │
│  └─────────┘  └─────────┘  └─────────┘                 │
│                                                          │
│  Each node:                                              │
│    • Full systemd as PID 1                               │
│    • Complete kernel access                              │
│    • Native cgroup support                               │
│    • SSH: localhost:222X                                 │
│    • VNC: localhost:590X                                 │
└─────────────────────────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────┐
│                   USB/ISO Installer                      │
│                                                          │
│  cluster-os-installer.iso → Boot → Install to hardware  │
│  cluster-os-usb.img.gz   → dd → Bootable USB            │
└─────────────────────────────────────────────────────────┘
```

## File Structure

```
images/ubuntu/
├── packer.pkr.hcl                 # Packer configuration
├── http/
│   ├── user-data                   # Cloud-init autoinstall
│   └── meta-data                   # Instance metadata
├── systemd/
│   └── node-agent.service          # Systemd service
├── netplan/
│   └── 01-clusteros-network.yaml   # Network config (WiFi)
└── provision.sh                    # Provisioning script

test/vm/qemu/
├── start-cluster.sh                # Launch QEMU cluster
├── stop-cluster.sh                 # Stop cluster
├── cluster-ctl.sh                  # Cluster control
└── vms/                            # VM data (generated)
    ├── node1/
    │   ├── disk.qcow2              # VM disk
    │   ├── cloud-init.iso          # Cloud-init config
    │   └── serial.log              # Console output
    └── node2/...

scripts/
└── create-usb-installer.sh         # USB/ISO creator

dist/                               # Build outputs
├── cluster-os-installer.iso        # Bootable ISO
└── cluster-os-usb.img.gz           # USB installer
```

## Troubleshooting

### VM won't start
```bash
# Check KVM access
ls -la /dev/kvm

# Add yourself to kvm group
sudo usermod -aG kvm $USER
# Log out and back in

# Check logs
cat test/vm/qemu/vms/node1/serial.log
```

### Can't SSH to VM
```bash
# Wait for boot (60 seconds)
./test/vm/qemu/cluster-ctl.sh logs 1 | grep "login:"

# Check SSH port
netstat -tlnp | grep 2223
```

### Packer build fails
```bash
# Clean and retry
make clean
make image

# Debug mode
cd images/ubuntu
PACKER_LOG=1 packer build packer.pkr.hcl
```

### Out of disk space
```bash
# Clean everything
make clean
make vm-clean

# Remove old images
rm -rf images/ubuntu/output-*
```

## Performance Tips

1. **Enable KVM**: Makes VMs 10x faster
   ```bash
   sudo apt-get install qemu-kvm
   sudo usermod -aG kvm $USER
   ```

2. **Increase VM resources**:
   ```bash
   NUM_NODES=3 MEMORY=4096 CPUS=4 make test-vm
   ```

3. **Use SSD**: Store VMs on SSD if possible
   ```bash
   # Check disk type
   lsblk -d -o name,rota
   # 0 = SSD, 1 = HDD
   ```

4. **Parallel builds**: Packer can build multiple images
   ```bash
   cd images/ubuntu
   packer build -parallel-builds=4 packer.pkr.hcl
   ```

## Docker vs QEMU Comparison

| Feature | Docker | QEMU |
|---------|--------|------|
| systemd | ❌ | ✅ |
| SLURM | ⚠️ Partial | ✅ Full |
| K3s | ❌ | ✅ |
| Boot time | 2s | 30s |
| Resource usage | Low | Medium |
| Isolation | Process | Hardware |
| Best for | Quick tests | Full validation |

**Recommendation**: Use Docker for rapid iteration, QEMU for validation.

## Next Steps

1. Read full docs: [docs/VM_TESTING.md](docs/VM_TESTING.md)
2. Deployment guide: [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)
3. Original spec: [CLAUDE.md](CLAUDE.md)

## Summary

You now have:
- ✅ Packer configuration for building OS images
- ✅ QEMU VM test harness with full systemd
- ✅ USB/ISO installer creation
- ✅ Complete testing for SLURM and K3s
- ✅ Path to bare-metal deployment

The Docker limitations are **bypassed** by using QEMU VMs with full kernel access.
