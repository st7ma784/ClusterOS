# Cluster-OS Specification

## 1. Purpose

Cluster-OS is a reproducible, self-assembling operating system image for heterogeneous bare-metal machines that automatically form a secure distributed compute cluster.

It MUST support:
- Zero-touch node joining
- Encrypted mesh networking
- Distributed control plane (no fixed head node)
- SLURM, Kubernetes, and Jupyter integration
- Full testing in containers prior to VM or bare-metal deployment
- connect via wired network or wifi by default to SSID: TALKTALK665317 key: NXJP7U39 
- have a USB installer/OS Image for quick deployment to hardware

The OS image is a delivery mechanism; the core product is the **node control plane**.

---

## 2. Non-Goals

- High-availability guarantees beyond best-effort
- Cloud provider integration
- Managed multi-tenant security boundaries
- GUI or desktop environment

---

## 3. Design Principles

1. Nodes are identified cryptographically, not by hostname
2. Every service MUST be runnable in Docker
3. Bare metal installs MUST use identical artifacts to container tests
4. All configuration MUST be declarative
5. Failure and re-election are first-class concerns

---

## 4. High-Level Architecture
+------------------+

Node Agent
Identity
Discovery
Networking
Role Execution
Health Reporting
+------------------+
    |
    v
+------------------------+

Distributed State
Membership
Leader Election
Metadata
+------------------------+
    |
    v
    +------------------------+

Workload Services
WireGuard Mesh
SLURM
Kubernetes (k3s)
JupyterHub
+------------------------+

## 5. Repository Structure (MANDATORY)
cluster-os/
├── SPEC.md # This document
├── Makefile # Unified build/test interface
│
├── node/ # Core node agent
│ ├── cmd/
│ ├── internal/
│ │ ├── identity/
│ │ ├── discovery/
│ │ ├── networking/
│ │ ├── roles/
│ │ └── state/
│ ├── api/
│ │ └── node-agent.proto
│ ├── config/
│ │ └── node.yaml
│ ├── Dockerfile
│ └── systemd/
│ └── node-agent.service
│
├── services/ # Role-specific services
│ ├── wireguard/
│ │ ├── renderer/
│ │ └── templates/
│ ├── slurm/
│ │ ├── controller/
│ │ ├── worker/
│ │ └── templates/
│ ├── kubernetes/
│ │ ├── k3s/
│ │ └── manifests/
│ └── jupyter/
│ └── hub/
│
├── discovery/ # Cluster membership & gossip
│ ├── Dockerfile
│ ├── config/
│ └── events/
│
├── images/ # OS image builds
│ ├── ubuntu/
│ │ ├── packer.pkr.hcl
│ │ ├── cloud-init/
│ │ │ ├── user-data
│ │ │ └── meta-data
│ │ └── provision.sh
│
├── test/
│ ├── docker/
│ │ ├── docker-compose.yaml
│ │ └── chaos/
│ ├── vm/
│ │ └── qemu/
│ └── integration/
│
├── scripts/
│ ├── build-node.sh
│ ├── build-image.sh
│ └── release.sh
│
└── docs/
├── architecture.md
├── onboarding.md
└── failure-modes.md

---

## 6. Node Agent (CORE COMPONENT)

### 6.1 Responsibilities

The node agent MUST:
- Generate and persist a cryptographic identity on first boot
- Join the discovery layer
- Establish WireGuard connectivity
- Participate in leader election
- Execute assigned roles
- Report health and metadata

### 6.2 Lifecycle
init → join → converge → serve → reconfigure


### 6.3 Identity

- Ed25519 keypair
- Stored at `/var/lib/cluster-os/identity`
- Public key is node ID

---

## 7. Discovery & State

### Requirements
- Gossip-based membership
- Eventual consistency
- Leader election per role
- Partition tolerance

### Data Model

```json
{
  "node_id": "ed25519:…",
  "roles": ["slurm-worker", "k8s-node"],
  "capabilities": {
    "cpu": 16,
    "ram": "64GB",
    "gpu": false
  },
  "status": "healthy"
}
```
## 8. Networking
Overlay

WireGuard

Stable virtual IP per node

Encrypted peer-to-peer links

Peer Discovery

Distributed via discovery layer

No static VPN server

## 9. Services
### 9.1 SLURM

Dynamic slurm.conf rendering

Controller elected dynamically

Munge keys distributed securely

### 9.2 Kubernetes

k3s

Multi-control-plane

Auto-join workers

### 9.3 Jupyter

JupyterHub inside Kubernetes

SLURMSpawner + KubeSpawner

## 10. Docker-First Testing (REQUIRED)
Node Simulation

Each Docker container represents a full node:

systemd enabled

node-agent running

networking isolated

Example:

make test-cluster

### Test Scenarios

Node join/leave

Leader failure

Network partition

State recovery

## 11. OS Image Build
Tooling

Packer

cloud-init

Ubuntu Server LTS

### Image Requirements

node-agent preinstalled

No hardcoded cluster config

First-boot auto-join

## 12. Build Targets (Makefile)
make node           # Build node-agent
make test           # Run unit tests
make test-cluster   # Docker multi-node simulation
make image          # Build OS image
make release        # Produce installable artifacts

## 13. Acceptance Criteria
### MVP

Nodes discover each other automatically

Encrypted connectivity

Leader election works

No single point of failure

SLURM jobs execute across nodes with MPI and Multiprocessing features

Kubernetes workloads schedule

Jupyter notebooks function

efficient access and use of libraries from opence envs across nodes

## 14. Failure Handling

Any role MUST be re-electable

Node restarts MUST be idempotent

Partial failures MUST converge without manual intervention

## 15. Licensing

Apache 2.0 (or compatible permissive license)