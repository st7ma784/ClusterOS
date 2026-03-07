# Cluster-OS

A reproducible, self-assembling operating system image for heterogeneous bare-metal machines that automatically form a secure distributed compute cluster.

## Status: Phase 3 (Services) Complete ✓

**Phase 1 (Foundation) - COMPLETE:**
- ✅ Complete project structure
- ✅ Build system (Makefile with all targets)
- ✅ Cryptographic identity system (Ed25519)
- ✅ Configuration management
- ✅ Node agent CLI (init, start, status, join, info commands)
- ✅ Comprehensive test suite (65.1% coverage)

**Phase 2 (Networking) - COMPLETE:**
- ✅ Serf discovery layer (gossip-based membership)
- ✅ Member state management (ClusterState)
- ✅ Raft-based leader election (per-role leadership)
- ✅ WireGuard IP allocation (IPAM) - deterministic IPs
- ✅ WireGuard mesh networking manager
- ✅ Configuration templates and renderer

**Phase 3 (Services) - COMPLETE:**
- ✅ Role framework and manager
- ✅ SLURM controller role (leader-elected)
- ✅ SLURM worker role
- ✅ k3s server role (HA control plane)
- ✅ k3s agent role
- ✅ JupyterHub deployment manifests
- ✅ Dual spawner configuration (Kube + SLURM)

**Phase 4 (Docker Testing) - COMPLETE:**
- ✅ Systemd-enabled Docker containers
- ✅ 5-node multi-container test cluster
- ✅ Docker Compose orchestration
- ✅ Integration test framework
- ✅ Cluster control utility (cluster-ctl.sh)
- ✅ Comprehensive test suite
- ✅ Cluster authentication system (HMAC-SHA256)
- ✅ Fork isolation (unique cluster keys per fork)

**Next Phases:**
- ⏳ Phase 5: OS image build (Packer)

## Quick Start

### Build

```bash
# Install dependencies
make deps

# Build node-agent binary
make node

# Run tests
make test
```

### Docker Testing (Recommended)

Test a complete 5-node cluster locally:

```bash
# Start the cluster
make test-cluster

# Check cluster status
./test/docker/cluster-ctl.sh status

# Run integration tests
./test/docker/cluster-ctl.sh test

# View logs
./test/docker/cluster-ctl.sh logs node1

# Open shell on a node
./test/docker/cluster-ctl.sh shell node1

# Stop cluster
./test/docker/cluster-ctl.sh stop
```

See [test/docker/README.md](test/docker/README.md) for comprehensive testing documentation.

### Initialize a Node (Standalone)

```bash
# Initialize node identity
./bin/node-agent init

# View node info
./bin/node-agent info

# Check status
./bin/node-agent status
```

## Security & Cluster Authentication

### 🔒 IMPORTANT: Regenerate Cluster Key When Forking

This repository includes a **cluster authentication key** that prevents unauthorized nodes from joining your cluster. When you fork or clone this repo, you **MUST** generate a new key:

```bash
# Generate your unique cluster key
./scripts/generate-cluster-key.sh

# The key will be saved to cluster.key and displayed
# Copy it to your configuration:
# - node/config/node.yaml (cluster.auth_key field)
# - OR set environment variable: CLUSTEROS_CLUSTER_AUTH_KEY
```

**Why this matters:**
- All nodes with the same key can join each other's clusters
- The default key in this repo is PUBLIC - only for testing
- Different keys = isolated clusters (prevents accidental cross-joining)
- Forks with different keys form separate, independent clusters

### Authentication Architecture

- **HMAC-SHA256** challenge-response authentication
- **Time-based tokens** (5-minute expiry) prevent replay attacks
- **No key transmission** - only cryptographic signatures are sent
- **Automatic rejection** of nodes with wrong/missing keys

See [SECURITY.md](SECURITY.md) and [docs/cluster-authentication.md](docs/cluster-authentication.md) for details.

## Architecture

### Core Components

1. **Node Agent** - Core daemon running on each node
   - Cryptographic identity (Ed25519 keypair)
   - Zero-touch cluster joining
   - Role management
   - Health monitoring

2. **Discovery Layer** (Serf) - ✓ Implemented
   - Gossip-based membership
   - Automatic node discovery (LAN + Tailscale API)
   - Event propagation
   - Tag-based metadata

3. **Leader Election** (Raft) - ✓ Implemented
   - Per-role leader election
   - Strong consistency guarantees
   - Automatic failover
   - BoltDB-backed persistence

4. **Networking** (WireGuard) - ✓ Implemented
   - Encrypted mesh networking
   - Deterministic IP allocation (IPAM)
   - Curve25519 key derivation
   - NAT traversal (PersistentKeepalive)

5. **Workload Services** - ✓ Implemented
   - **Role Framework**: Pluggable service management
   - **SLURM**: Dynamic controller election, worker nodes, MPI support
   - **Kubernetes (k3s)**: HA control plane, auto-joining agents
   - **JupyterHub**: Dual spawners (Kubernetes + SLURM), OpenCE integration

### Design Principles

1. **Cryptographic Identity** - Nodes identified by Ed25519 public keys, not hostnames
2. **Docker-First Testing** - Every service must run in containers before bare-metal
3. **Declarative Configuration** - All settings in YAML, environment variable overrides
4. **Failure as First-Class** - Automatic re-election, partition tolerance
5. **No Single Point of Failure** - Fully distributed control plane

## Networking

Cluster-OS uses a three-layer networking model:

- **Layer 1: Physical LAN / WiFi** — DHCP host IPs, unrestricted outbound 80/443 (required for image pulls)
- **Layer 2: Tailscale Mesh (100.64.0.0/10)** — all cluster-internal traffic (Serf, k3s API, SLURM, etcd); fully encrypted; trusted on INPUT
- **Layer 3: k3s Pod Network (Flannel)** — pod IPs 10.42.0.0/16, service IPs 10.43.0.0/16; FORWARD chain must ACCEPT these CIDRs

**Ingress design**: nginx-ingress runs as a DaemonSet with `hostNetwork: true`, binding directly to host ports 80/443. No iptables REDIRECT or PREROUTING rules are used — these would intercept outbound connections from containerd, helm, and apt, breaking image pulls.

See [docs/NETWORKING.md](docs/NETWORKING.md) for the full networking guide including traffic flows, iptables chain order, required sysctl settings, and troubleshooting.

## Project Structure

```
cluster-os/
├── node/                    # Core node agent (Go)
│   ├── cmd/node-agent/      # CLI entry point ✓
│   ├── internal/            # Internal packages
│   │   ├── identity/        # Ed25519 identity system ✓
│   │   ├── auth/            # Cluster authentication ✓
│   │   ├── config/          # Configuration management ✓
│   │   ├── discovery/       # Serf integration ✓
│   │   ├── networking/      # WireGuard mesh + IPAM ✓
│   │   ├── roles/           # Role framework ✓
│   │   └── state/           # Cluster state & Raft ✓
│   ├── config/              # Default configuration
│   └── go.mod               # Go dependencies
│
├── services/                # Role-specific services
│   ├── wireguard/           # WireGuard mesh service ✓
│   │   ├── renderer/        # Config renderer ✓
│   │   └── templates/       # Config templates ✓
│   ├── slurm/               # SLURM integration ✓
│   │   ├── controller/      # Controller role ✓
│   │   ├── worker/          # Worker role ✓
│   │   └── templates/       # Config templates
│   ├── kubernetes/          # k3s integration ✓
│   │   └── k3s/             # Server & agent roles ✓
│   └── jupyter/             # JupyterHub ✓
│       └── hub/             # Deployment manifests ✓
│
├── test/                    # Testing infrastructure ✓
│   ├── docker/              # Docker compose multi-node tests ✓
│   │   ├── docker-compose.yaml  # 5-node cluster ✓
│   │   ├── cluster-ctl.sh       # Cluster control script ✓
│   │   ├── entrypoint.sh        # Container init ✓
│   │   └── README.md            # Testing docs ✓
│   ├── integration/         # Integration tests ✓
│   │   └── test_cluster.sh      # Automated tests ✓
│   └── vm/                  # VM tests (planned)
│
├── images/                  # OS image builds (planned)
│   └── ubuntu/              # Ubuntu-based image
│
├── scripts/                 # Build and utility scripts
├── docs/                    # Documentation
└── Makefile                 # Unified build interface ✓
```

## Development

### Prerequisites

- Go 1.22+ (installed locally in ~/go)
- Docker (for testing)
- Packer (for OS image builds)

### Makefile Targets

```bash
make help          # Show all available targets
make node          # Build node-agent binary
make test          # Run unit tests with coverage
make test-cluster  # Start Docker multi-node cluster (once implemented)
make image         # Build bootable OS image (once implemented)
make release       # Create release artifacts
make clean         # Clean build artifacts
make fmt           # Format Go code
make lint          # Lint Go code
```

### Configuration

Default configuration: `node/config/node.yaml`

Environment variables (prefix: `CLUSTEROS_`):
```bash
export CLUSTEROS_CONFIG=/path/to/config.yaml
export CLUSTEROS_LOG_LEVEL=debug
export CLUSTEROS_DISCOVERY_BIND_PORT=7946
export CLUSTEROS_NETWORKING_LISTEN_PORT=51820
```

### Testing

```bash
# Run all tests
make test

# Run specific package tests
cd node && go test -v ./internal/identity/

# Generate coverage report
make test-coverage
# Opens node/coverage.html
```

## Identity System

Cluster-OS uses Ed25519 cryptographic identities for all nodes:

```bash
# Generate new identity
./bin/node-agent init

# View identity
cat /var/lib/cluster-os/identity.json
```

**Features:**
- Ed25519 keypairs (public key cryptography)
- Node ID derived from public key (base58 encoding)
- Persistent storage with atomic writes
- Deterministic WireGuard key derivation
- Message signing and verification

## Roadmap

### ✅ Phase 1: Foundation (Complete)
- Project structure and build system
- Identity system with Ed25519
- Configuration management
- Node agent CLI
- Test framework

### ✅ Phase 2: Networking (Complete)
- Serf discovery layer integration
- Raft-based leader election
- Member state management
- WireGuard mesh networking
- IP allocation (IPAM)
- Configuration templates and renderer

### ✅ Phase 3: Services (Complete)
- Role framework and manager
- SLURM integration (controller + worker)
- k3s integration (server + agent)
- JupyterHub deployment
- OpenCE library integration
- Munge authentication setup

### ✅ Phase 4: Docker Testing (Complete)
- Docker systemd containers
- 5-node multi-container cluster
- Docker Compose orchestration
- Integration test framework
- Cluster control utility
- Comprehensive test suite

### ⏳ Phase 5: Production (Planned)
- Packer OS image builds
- Cloud-init configuration
- USB installer
- Documentation and tutorials

## Contributing

Cluster-OS follows these development principles:

1. **Test-Driven** - Write tests first, then implementation
2. **Docker-First** - Validate in containers before bare-metal
3. **Incremental** - Small, focused commits
4. **Documented** - Code comments and architecture docs
5. **Secure** - Security-first design (encryption, authentication, least privilege)

## License

Apache 2.0 - See LICENSE file for details

## Authors

Cluster-OS Team

---

**Note:** This is an active development project. The foundation is complete and operational. Networking, services, and deployment tooling are under development. See [CLAUDE.md](./CLAUDE.md) for the complete specification.
