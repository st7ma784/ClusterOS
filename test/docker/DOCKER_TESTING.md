# Docker Cluster Testing Guide

## Overview

This directory contains a complete Docker-based test environment for Cluster-OS. It allows you to test the entire cluster system (discovery, leader election, WireGuard mesh, authentication, etc.) on a single machine before deploying to bare metal or VMs.

## Architecture

The test cluster consists of:
- **5 nodes** running in separate Docker containers
- **systemd** for service management (matches production)
- **Shared cluster authentication key** (all nodes must have matching keys)
- **WireGuard mesh networking** (virtual overlay network)
- **Serf** for gossip-based discovery
- **Raft** for leader election
- **SLURM** and **k3s** ready to be enabled

## Prerequisites

```bash
# Required
- Docker (with compose v2)
- Docker with systemd support (privileged containers)
- At least 4GB RAM available
- Linux host (for cgroup support)

# Build the node-agent binary first
make node
```

## Quick Start

### 1. Start the Test Cluster

```bash
# Build and start all 5 nodes
make test-cluster

# Or manually:
cd test/docker
./start-cluster-direct.sh
```

This will:
1. Build the node-agent binary
2. Build Docker images with systemd
3. Start 5 containers (node1-node5)
4. Node1 bootstraps the cluster
5. Nodes 2-5 join via node1

### 2. Check Cluster Status

```bash
# View cluster status
./test/docker/cluster-ctl.sh status

# View detailed info
./test/docker/cluster-ctl.sh info

# View logs from node1
./test/docker/cluster-ctl.sh logs node1

# Follow logs in real-time
./test/docker/cluster-ctl.sh logs node1 -f
```

### 3. Interact with Nodes

```bash
# Open shell on node1
./test/docker/cluster-ctl.sh shell node1

# Run command on node2
docker exec -it cluster-os-node2 /usr/local/bin/node-agent info

# Check Serf members
docker exec -it cluster-os-node1 serf members
```

### 4. Stop the Cluster

```bash
# Stop all containers (preserves data)
./test/docker/stop-cluster.sh

# Stop and remove all data
./test/docker/clean-cluster.sh
```

## Cluster Authentication

All nodes in the test cluster use the **same cluster authentication key** to ensure they can join each other.

### Default Test Key

The docker-compose.yaml is configured with this key:
```
7RB0TPs+d/VuD3rL/7ZD2JEcpA14aNCBvLOPHwEBy9s=
```

This matches the default key in `cluster.key` and `node/config/node.yaml`.

### Testing with Different Keys

To test authentication rejection:

```bash
# Start the cluster normally
make test-cluster

# Try to start a rogue node with a different key
docker run -it --rm \
  --network test_cluster_net \
  -e CLUSTER_AUTH_KEY="WRONG-KEY-WILL-BE-REJECTED" \
  -e NODE_NAME=rogue \
  -e NODE_JOIN=10.90.0.10:7946 \
  cluster-os/node:latest

# Watch node1 logs - you'll see it reject the rogue node
./test/docker/cluster-ctl.sh logs node1 | grep "failed authentication"
```

## Container Details

### Container Structure

Each container runs:
- **Ubuntu 24.04** base
- **systemd** as init (PID 1)
- **node-agent** systemd service
- **WireGuard**, **SLURM**, **k3s** pre-installed

### Node Configuration

Nodes are configured via environment variables:

| Variable | Description | Example |
|----------|-------------|---------|
| `NODE_NAME` | Unique node name | `node1` |
| `NODE_BOOTSTRAP` | Bootstrap mode | `true` or `false` |
| `NODE_JOIN` | Join address | `node1:7946` |
| `CLUSTER_AUTH_KEY` | Cluster auth key | `base64-key` |
| `SERF_BIND_PORT` | Serf gossip port | `7946` |
| `RAFT_BIND_PORT` | Raft consensus port | `7373` |
| `WIREGUARD_PORT` | WireGuard VPN port | `51820` |

### Network Topology

```
Docker Bridge Network: 10.90.0.0/16
├── node1: 10.90.0.10 (bootstrap)
├── node2: 10.90.0.11 (joins node1)
├── node3: 10.90.0.12 (joins node1)
├── node4: 10.90.0.13 (joins node1)
└── node5: 10.90.0.14 (joins node1)

WireGuard Overlay: 10.42.0.0/16
├── node1: 10.42.0.1/16 (allocated via IPAM)
├── node2: 10.42.0.2/16
├── node3: 10.42.0.3/16
├── node4: 10.42.0.4/16
└── node5: 10.42.0.5/16
```

### Exposed Ports

Only node1 exposes ports to the host:
- `7946/tcp` - Serf gossip (TCP)
- `7946/udp` - Serf gossip (UDP)

All other inter-node communication happens within the Docker network.

## Test Scenarios

### 1. Node Join/Leave

```bash
# Start cluster
make test-cluster

# Stop node3
docker stop cluster-os-node3

# Watch cluster detect the failure
./test/docker/cluster-ctl.sh logs node1 | grep node3

# Restart node3
docker start cluster-os-node3

# Watch it rejoin
./test/docker/cluster-ctl.sh logs node1 | grep "Node joined"
```

### 2. Leader Election

```bash
# Check who is the Raft leader
docker exec cluster-os-node1 /usr/local/bin/node-agent info | grep leader

# Kill the leader
docker stop cluster-os-node1  # if node1 was leader

# Watch new leader election in logs
./test/docker/cluster-ctl.sh logs node2
```

### 3. Network Partition

```bash
# Disconnect node2 from the network
docker network disconnect test_cluster_net cluster-os-node2

# Watch cluster detect partition
./test/docker/cluster-ctl.sh logs node1

# Reconnect node2
docker network connect test_cluster_net cluster-os-node2 --ip 10.90.0.11
```

### 4. WireGuard Mesh

```bash
# Shell into node1
./test/docker/cluster-ctl.sh shell node1

# Check WireGuard interface
wg show wg0

# Ping another node via WireGuard
ping 10.42.0.2  # node2's WireGuard IP
```

## Debugging

### View All Logs

```bash
# All containers
docker compose -f test/docker/docker-compose.yaml logs

# Specific container
docker logs cluster-os-node1

# Follow logs
docker logs -f cluster-os-node1
```

### Inspect Containers

```bash
# List all cluster containers
docker ps -a | grep cluster-os

# Inspect node1
docker inspect cluster-os-node1

# View resource usage
docker stats
```

### Check Services

```bash
# Inside a container
docker exec -it cluster-os-node1 bash

# Check systemd services
systemctl status node-agent
journalctl -u node-agent -f

# Check Serf membership
serf members

# Check network interfaces
ip addr show
wg show
```

### Common Issues

#### Containers won't start

```bash
# Check if systemd is working
docker run --rm --privileged ubuntu:24.04 /lib/systemd/systemd --version

# Check cgroup mount
ls -la /sys/fs/cgroup

# Try with more privileges
# Edit docker-compose.yaml and ensure privileged: true
```

#### Nodes can't join cluster

```bash
# Check if nodes can reach each other
docker exec cluster-os-node2 ping node1

# Check Serf port
docker exec cluster-os-node1 netstat -tulpn | grep 7946

# Check authentication
docker exec cluster-os-node1 grep auth_key /etc/cluster-os/node.yaml
docker exec cluster-os-node2 grep auth_key /etc/cluster-os/node.yaml
# Both should have the same key
```

#### WireGuard not working

```bash
# Check if WireGuard kernel module is loaded
lsmod | grep wireguard

# Inside container
docker exec -it cluster-os-node1 bash
modprobe wireguard  # May fail in container
wg show  # Should show interface even without kernel module
```

## Cleanup

### Soft Cleanup (stop but keep data)

```bash
./test/docker/stop-cluster.sh
```

### Hard Cleanup (remove everything)

```bash
./test/docker/clean-cluster.sh

# Or manually:
docker compose -f test/docker/docker-compose.yaml down -v
docker volume prune -f
```

## Performance Testing

### Measure Join Time

```bash
# Time how long it takes for all nodes to join
time make test-cluster
```

### Measure Consensus Time

```bash
# Time leader election
docker stop cluster-os-node1
time until docker exec cluster-os-node2 /usr/local/bin/node-agent info | grep -q "leader"; do sleep 1; done
```

### Load Testing

```bash
# Add more nodes by copying node5 in docker-compose.yaml
# Change name, hostname, IP address, etc.

# Then restart
./test/docker/clean-cluster.sh
make test-cluster
```

## CI/CD Integration

Example GitHub Actions workflow:

```yaml
name: Test Cluster
on: [push]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - name: Build binary
        run: make node
      - name: Start test cluster
        run: make test-cluster
      - name: Wait for cluster
        run: sleep 30
      - name: Check cluster status
        run: ./test/docker/cluster-ctl.sh status
      - name: Run integration tests
        run: make test-integration
      - name: Cleanup
        run: ./test/docker/clean-cluster.sh
```

## Next Steps

After validating in Docker:
1. Deploy to VMs using the same Docker image
2. Create bare-metal OS image with Packer
3. Boot physical machines from the OS image
4. Nodes auto-join the cluster on first boot

## References

- [Main README](../../README.md)
- [Architecture Documentation](../../docs/architecture.md)
- [Cluster Authentication](../../docs/cluster-authentication.md)
- [Docker Systemd Best Practices](https://developers.redhat.com/blog/2019/04/24/how-to-run-systemd-in-a-container)
