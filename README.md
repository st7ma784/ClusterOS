# Cluster-OS

**A reproducible, self-assembling operating system for heterogeneous bare-metal clusters**

Cluster-OS automatically forms secure distributed compute clusters with zero-touch node joining, encrypted mesh networking via Tailscale, and integrated SLURM, Kubernetes, and Jupyter.

## Quick Start

### Prerequisites

```bash
./scripts/install-prereqs.sh   # Install Packer, QEMU, cloud-image-utils
./scripts/verify-prereqs.sh    # Verify existing installation
```

Log out and back in after installation for group changes to take effect.

### Build and Test

```bash
make node        # Build node-agent binary
make image       # Build bootable OS image (10-20 min first time)
make test-vm     # Launch 3-node QEMU VM cluster
make vm-status   # Check cluster status

# SSH to a node
./test/vm/qemu/cluster-ctl.sh shell 1

# Inside the VM:
sudo systemctl status node-agent
sinfo                          # SLURM
sudo k3s kubectl get nodes     # K3s
```

### Create USB Installer

```bash
make usb   # Build bootable USB/ISO
lsblk      # Find your USB device
gunzip -c dist/cluster-os-usb.img.gz | sudo dd of=/dev/sdX bs=4M status=progress
```

## Architecture

```
[Packer Build]
     ↓
[Ubuntu 24.04 + node-agent + Tailscale + SLURM + K3s]
     ↓
[Bootable Images: qcow2, raw, iso]
     ↓
   ┌─────────────────┬─────────────────┐
   ↓                 ↓                 ↓
[QEMU Testing]  [USB Deploy]  [Bare Metal]
```

### Key Components

| Component | Description |
|-----------|-------------|
| **Node Agent** | Core control-plane daemon: cryptographic identity, discovery, role orchestration |
| **Tailscale Mesh** | Encrypted overlay network with automatic peer discovery |
| **Serf** | Gossip-based cluster membership and leader election |
| **SLURM** | HPC job scheduling with MPI and multiprocessing |
| **K3s** | Lightweight Kubernetes with multi-control-plane HA |
| **JupyterHub** | Multi-user notebooks via KubeSpawner + SLURMSpawner |

## Build Targets

```bash
# Build
make node          # node-agent binary
make image         # OS image via Packer
make usb           # USB/ISO installer
make release       # Release artifacts

# Test — QEMU (full systemd, recommended)
make test-vm       # 3-node cluster
make test-vm-5     # 5-node cluster
make vm-status     # Cluster status
make vm-stop       # Stop cluster
make vm-clean      # Remove all VM data

# Test — Docker (rapid iteration, limited)
make test          # Unit tests
make test-cluster  # Docker multi-node

# Dev
make fmt           # Format code
make lint          # Lint
make deps          # Download Go dependencies
make clean         # Clean artifacts
```

## Deployment

See [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) for bare-metal deployment.  
See [docs/HELM_APPS_GUIDE.md](docs/HELM_APPS_GUIDE.md) for Helm chart deployment.  
See [docs/NETWORKING.md](docs/NETWORKING.md) for network configuration.

## Requirements

**Per Node (recommended):** 4 CPU cores, 4 GB RAM, 20 GB disk, 1 Gbps network  
**Build machine:** 8 GB RAM+, 30 GB free disk, VT-x/AMD-V support

## Security

- Ed25519 cryptographic identity per node
- Tailscale encrypted mesh (replaces WireGuard)
- Munge authentication for SLURM
- Per-node firewall rules via iptables

## Known Limitations

Docker containers lack full systemd, `/dev/kmsg`, and cgroup support needed by K3s and SLURM. Use QEMU VMs for full validation — see [docs/VM_TESTING.md](docs/VM_TESTING.md).

## Contributing

1. Fork, create a branch, test with `make test-vm`
2. Submit pull request

## License

Apache 2.0
