# Cluster-OS

**A reproducible, self-assembling operating system for heterogeneous bare-metal clusters**

Cluster-OS automatically forms secure distributed compute clusters with zero-touch node joining, encrypted mesh networking, and integrated SLURM, Kubernetes, and Jupyter.

## 🚀 Quick Start

### Prerequisites

Install required tools (Packer, QEMU, cloud-image-utils):

```bash
# Quick install script
./scripts/install-prereqs.sh

# Or verify existing installation
./scripts/verify-prereqs.sh
```

**IMPORTANT**: Log out and back in after installation for group changes to take effect.

### Build and Test

```bash
# 1. Build node-agent binary
make node

# 2. Build bootable OS image (10-20 minutes first time)
make image

# 3. Launch 3-node QEMU VM cluster
make test-vm

# 4. Check cluster status
make vm-status

# 5. SSH to a node
./test/vm/qemu/cluster-ctl.sh shell 1

# Inside the VM:
sudo systemctl status node-agent
sudo wg show
sinfo  # SLURM
sudo k3s kubectl get nodes  # K3s
```

### Create USB Installer

```bash
# Build bootable USB/ISO installer
make usb

# Write to USB drive (WARNING: destructive!)
lsblk  # Find your USB device
gunzip -c dist/cluster-os-usb.img.gz | sudo dd of=/dev/sdX bs=4M status=progress
```

## 📖 Documentation

- **[PACKER_QEMU_QUICKSTART.md](PACKER_QEMU_QUICKSTART.md)** - Quick reference guide
- **[docs/VM_TESTING.md](docs/VM_TESTING.md)** - Comprehensive VM testing guide
- **[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)** - Production deployment guide
- **[docs/INSTALL_TOOLS.md](docs/INSTALL_TOOLS.md)** - Tool installation details
- **[PACKER_IMPLEMENTATION_SUMMARY.md](PACKER_IMPLEMENTATION_SUMMARY.md)** - Architecture overview

## 🏗️ Architecture

Cluster-OS uses a **Packer + QEMU** build pipeline for full systemd testing:

```
[Packer Build]
     ↓
[Ubuntu 24.04 + node-agent + WireGuard + SLURM + K3s]
     ↓
[Bootable Images: qcow2, raw, iso]
     ↓
   ┌─────────────────┬─────────────────┐
   ↓                 ↓                 ↓
[QEMU Testing]  [USB Deploy]  [Bare Metal]
```

### Key Components

1. **Node Agent** - Core control plane daemon
   - Cryptographic node identity
   - Cluster discovery and joining
   - Role assignment and orchestration
   - Service lifecycle management

2. **WireGuard Mesh** - Encrypted overlay network
   - Automatic peer discovery
   - Zero-config secure tunnels
   - Stable virtual IPs

3. **Distributed State** - Gossip-based membership
   - Serf for cluster membership
   - Leader election per role
   - Partition tolerant

4. **Workload Services**
   - **SLURM** - HPC job scheduling
   - **K3s** - Lightweight Kubernetes
   - **JupyterHub** - Interactive notebooks

## 🔧 Development Workflow

### Docker Testing (Quick Iteration)

Docker is useful for rapid development but has **limitations**:
- ❌ No systemd as PID 1
- ❌ SLURM cgroup issues
- ❌ K3s kubelet blocked (/dev/kmsg)

```bash
# Quick Docker tests (limited functionality)
make test-cluster
```

### QEMU Testing (Full Validation)

QEMU VMs provide **complete testing**:
- ✅ Full systemd support
- ✅ SLURM with cgroups
- ✅ K3s with kubelet
- ✅ Accurate to bare-metal

```bash
# Full VM testing
make test-vm       # 3 nodes
make test-vm-5     # 5 nodes

# Management
make vm-status     # Check status
make vm-info       # Detailed info
make vm-stop       # Stop VMs
make vm-clean      # Clean all data

# Integration tests
./test/vm/integration-test.sh
```

### Build Targets

```bash
# Building
make node          # Build node-agent binary
make image         # Build OS image with Packer
make usb           # Create USB/ISO installer
make release       # Create release artifacts

# Testing - Docker (Limited)
make test          # Unit tests
make test-cluster  # Docker multi-node
make test-slurm    # SLURM only (partial)
make test-k3s      # K3s only (blocked)

# Testing - QEMU VMs (Full)
make test-vm       # 3-node cluster
make test-vm-5     # 5-node cluster
make vm-status     # Cluster status
make vm-stop       # Stop cluster
make vm-clean      # Clean all data

# Development
make fmt           # Format code
make lint          # Lint code
make deps          # Download dependencies
make clean         # Clean artifacts
```

## 🎯 Features

### Core Capabilities

- **Zero-Touch Joining** - Nodes discover and join automatically
- **Encrypted Mesh** - WireGuard-based secure overlay
- **Distributed Control** - No single point of failure
- **Cryptographic Identity** - Ed25519 node authentication
- **Dynamic Roles** - Automatic role assignment and migration
- **Systemd Integration** - Native service management

### Workload Support

- **SLURM** - HPC job scheduling with MPI
- **Kubernetes** - K3s for container orchestration
- **Jupyter** - Multi-user notebook server
- **OpenMPI** - Distributed computing framework

### Network Features

- **Auto WiFi** - Pre-configured WiFi (TALKTALK665317)
- **Wired Fallback** - Automatic Ethernet DHCP
- **Mesh Routing** - WireGuard overlay with encryption
- **Service Discovery** - Serf-based gossip protocol

## 📁 Repository Structure

```
cluster-os/
├── node/                      # Node agent (Go)
│   ├── cmd/node-agent/        # Main binary
│   ├── internal/              # Core logic
│   │   ├── identity/          # Cryptographic identity
│   │   ├── discovery/         # Cluster discovery
│   │   ├── networking/        # WireGuard management
│   │   └── roles/             # Role execution
│   └── Dockerfile             # Docker image (for testing)
│
├── images/ubuntu/             # Packer build
│   ├── packer.pkr.hcl         # Main configuration
│   ├── http/                  # Autoinstall files
│   ├── systemd/               # Service units
│   └── netplan/               # Network config
│
├── test/
│   ├── docker/                # Docker testing (limited)
│   ├── vm/                    # QEMU VM testing (full)
│   │   ├── qemu/              # VM launcher scripts
│   │   └── integration-test.sh # Test suite
│   └── integration/           # Integration tests
│
├── scripts/
│   ├── install-prereqs.sh     # Tool installation
│   ├── verify-prereqs.sh      # Prerequisites check
│   └── create-usb-installer.sh # USB/ISO creator
│
├── docs/
│   ├── VM_TESTING.md          # VM testing guide
│   ├── DEPLOYMENT.md          # Deployment guide
│   └── INSTALL_TOOLS.md       # Tool installation
│
└── dist/                      # Build outputs
    ├── cluster-os-installer.iso
    └── cluster-os-usb.img.gz
```

## 🐛 Troubleshooting

### Prerequisites Not Installed

```bash
# Check what's missing
./scripts/verify-prereqs.sh

# Install everything
./scripts/install-prereqs.sh

# Log out and back in (for KVM group)
```

### VM Won't Start

```bash
# Check KVM access
ls -la /dev/kvm

# View VM logs
cat test/vm/qemu/vms/node1/serial.log

# Check QEMU processes
ps aux | grep qemu
```

### Can't SSH to VM

```bash
# Wait for boot (takes 60 seconds)
./test/vm/qemu/cluster-ctl.sh logs 1 | grep "login:"

# Check SSH port
netstat -tlnp | grep 2223
```

### Build Fails

```bash
# Clean and retry
make clean
make node
make image

# Debug Packer
cd images/ubuntu
PACKER_LOG=1 packer build packer.pkr.hcl
```

## 🚧 Known Limitations

### Docker Testing

Docker containers have fundamental limitations:
- No systemd as PID 1 (blocks proper service management)
- No /dev/kmsg access (blocks K3s kubelet)
- Limited cgroup support (breaks SLURM resource management)

**Solution**: Use QEMU VMs for full testing (see [docs/VM_TESTING.md](docs/VM_TESTING.md))

### Current Status

- ✅ **WireGuard Mesh** - Fully functional
- ✅ **Node Discovery** - Serf-based, working
- ✅ **Identity System** - Ed25519, operational
- ⚠️  **SLURM** - Works in QEMU VMs, limited in Docker
- ⚠️  **K3s** - Works in QEMU VMs, blocked in Docker
- 🚧 **JupyterHub** - Planned

## 🔐 Security

### Node Authentication

- Ed25519 cryptographic identity per node
- Shared cluster key for initial joining
- Munge for SLURM authentication
- WireGuard for encrypted transport

### Network Security

- All cluster traffic encrypted via WireGuard
- No plaintext communication
- Automatic key rotation support
- Per-node firewall rules

## 📊 Performance

### Build Times

- First Packer build: 10-20 minutes
- Subsequent builds: 5-10 minutes (cached)
- VM boot time: 30-60 seconds
- Cluster formation: 2-3 minutes

### Resource Requirements

**Per Node (Recommended)**:
- RAM: 4GB
- CPUs: 4 cores
- Disk: 20GB
- Network: 1Gbps+

**Build Machine**:
- RAM: 8GB+
- Disk: 30GB free
- CPU: Virtualization support (VT-x/AMD-V)

## 🤝 Contributing

1. Fork the repository
2. Create a feature branch
3. Make changes and test with `make test-vm`
4. Submit pull request

### Development Setup

```bash
# Clone repository
git clone https://github.com/cluster-os/cluster-os
cd cluster-os

# Install prerequisites
./scripts/install-prereqs.sh

# Build and test
make node
make image
make test-vm
```

## 📄 License

Apache 2.0 - See [LICENSE](LICENSE) for details

## 🙏 Acknowledgments

Built with:
- [Ubuntu 24.04 LTS](https://ubuntu.com/)
- [WireGuard](https://www.wireguard.com/)
- [SLURM](https://slurm.schedmd.com/)
- [K3s](https://k3s.io/)
- [Serf](https://www.serf.io/)
- [Packer](https://www.packer.io/)
- [QEMU](https://www.qemu.org/)

## 📞 Support

- Documentation: [docs/](docs/)
- Issues: [GitHub Issues](https://github.com/cluster-os/cluster-os/issues)
- Discussions: [GitHub Discussions](https://github.com/cluster-os/cluster-os/discussions)

---

**Status**: Active Development
**Version**: 0.1.0-dev
**Last Updated**: January 2026
