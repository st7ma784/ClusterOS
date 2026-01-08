# Cluster-OS Docker Testing Environment

This directory contains the Docker-based testing environment for Cluster-OS. It allows you to simulate a multi-node cluster on a single machine for development and testing.

## Overview

The Docker test environment creates a 5-node cluster with:
- **node1**: Bootstrap node (starts the cluster)
- **node2-5**: Worker nodes (join via node1)

Each container runs:
- Ubuntu 24.04 with systemd
- Cluster-OS node-agent service
- Full networking stack (WireGuard-ready)

## Prerequisites

- Docker Engine 20.10+
- Docker Compose 2.0+
- At least 4GB RAM available
- Linux kernel with cgroup v2 support (for systemd)

## Quick Start

### 1. Build and Start the Cluster

```bash
# From the project root
make test-cluster

# Or using the control script
./test/docker/cluster-ctl.sh start
```

This will:
1. Build the node container image
2. Start all 5 nodes
3. Initialize identities on each node
4. Start node-agent services
5. Begin cluster formation

### 2. Check Cluster Status

```bash
./test/docker/cluster-ctl.sh status
```

### 3. View Cluster Information

```bash
./test/docker/cluster-ctl.sh info
```

Shows detailed information about each node including:
- Container status
- IP addresses
- Node IDs

### 4. Run Integration Tests

```bash
./test/docker/cluster-ctl.sh test
```

Runs automated tests to verify:
- Containers are running
- node-agent is installed
- Identities are generated and unique
- Configuration files exist
- Services are active
- Network connectivity
- Logs are being generated

## Cluster Control Script

The `cluster-ctl.sh` script provides convenient commands for managing the test cluster:

### Basic Commands

```bash
# Start the cluster
./test/docker/cluster-ctl.sh start

# Stop the cluster
./test/docker/cluster-ctl.sh stop

# Restart the cluster
./test/docker/cluster-ctl.sh restart

# Clean up (removes all data)
./test/docker/cluster-ctl.sh clean
```

### Debugging Commands

```bash
# View container logs
./test/docker/cluster-ctl.sh logs node1

# View node-agent service logs (journald)
./test/docker/cluster-ctl.sh agent-logs node1

# Open interactive shell on a node
./test/docker/cluster-ctl.sh shell node2

# Execute a command on a node
./test/docker/cluster-ctl.sh exec node3 ps aux

# View node identity
./test/docker/cluster-ctl.sh identity node1

# View node configuration
./test/docker/cluster-ctl.sh config node1
```

## Manual Docker Commands

If you prefer to use Docker Compose directly:

```bash
cd test/docker

# Start cluster
docker-compose up -d --build

# Stop cluster
docker-compose down

# View logs
docker-compose logs -f node1

# Execute command
docker exec -it cluster-os-node1 /bin/bash
```

## Network Architecture

The cluster uses a custom Docker network:

```
Network: cluster_net (172.21.0.0/16)
├── node1: 172.21.0.10 (bootstrap)
├── node2: 172.21.0.11
├── node3: 172.21.0.12
├── node4: 172.21.0.13
└── node5: 172.21.0.14
```

**Exposed Ports:**
- `7946/tcp`: Serf gossip
- `7946/udp`: Serf gossip
- `7373/tcp`: Raft consensus
- `51820/udp`: WireGuard VPN

## Data Persistence

Each node has persistent volumes:
- `/var/lib/cluster-os`: Identity, Raft data
- `/var/log/cluster-os`: Application logs

Volumes are preserved across restarts but deleted with `cluster-ctl.sh clean`.

## Troubleshooting

### Containers won't start

**Issue**: Systemd requires privileged mode and cgroup v2

**Solution**: Ensure Docker is running with cgroup v2:
```bash
docker info | grep -i cgroup
```

### Services not starting

**Issue**: node-agent service fails to start

**Debug**:
```bash
# Check service status
./test/docker/cluster-ctl.sh exec node1 systemctl status node-agent

# View detailed logs
./test/docker/cluster-ctl.sh agent-logs node1

# Check for errors
./test/docker/cluster-ctl.sh exec node1 journalctl -xeu node-agent
```

### Network connectivity issues

**Issue**: Nodes can't communicate

**Debug**:
```bash
# Check network
docker network inspect test_cluster_net

# Ping between nodes
./test/docker/cluster-ctl.sh exec node1 ping node2

# Check firewall rules
./test/docker/cluster-ctl.sh exec node1 iptables -L
```

### Identity not generated

**Issue**: `/var/lib/cluster-os/identity.json` missing

**Solution**:
```bash
# Manually initialize identity
./test/docker/cluster-ctl.sh exec node1 /usr/local/bin/node-agent init

# Restart service
./test/docker/cluster-ctl.sh exec node1 systemctl restart node-agent
```

## Testing Workflows

### Basic Functionality Test

```bash
# 1. Start cluster
./test/docker/cluster-ctl.sh start

# 2. Wait for initialization (30 seconds)
sleep 30

# 3. Run integration tests
./test/docker/cluster-ctl.sh test

# 4. Check logs for errors
./test/docker/cluster-ctl.sh logs node1 | grep -i error

# 5. Verify identities are unique
for node in node{1..5}; do
    echo -n "$node: "
    ./test/docker/cluster-ctl.sh identity $node | grep node_id
done
```

### Node Failure Simulation

```bash
# 1. Start cluster
./test/docker/cluster-ctl.sh start

# 2. Stop node2
docker stop cluster-os-node2

# 3. Check cluster adapts
./test/docker/cluster-ctl.sh agent-logs node1

# 4. Restart node2
docker start cluster-os-node2

# 5. Verify node2 rejoins
sleep 10
./test/docker/cluster-ctl.sh status
```

### Performance Testing

```bash
# Monitor resource usage
docker stats

# Check node-agent CPU/memory
./test/docker/cluster-ctl.sh exec node1 ps aux | grep node-agent
```

## Environment Variables

Configure nodes via environment variables in `docker-compose.yaml`:

- `NODE_NAME`: Node hostname
- `NODE_BOOTSTRAP`: Bootstrap mode (true/false)
- `NODE_JOIN`: Bootstrap peer address
- `SERF_BIND_PORT`: Serf gossip port (default: 7946)
- `RAFT_BIND_PORT`: Raft consensus port (default: 7373)
- `WIREGUARD_PORT`: WireGuard VPN port (default: 51820)

## Integration with CI/CD

Example GitHub Actions workflow:

```yaml
- name: Build and test cluster
  run: |
    make node
    ./test/docker/cluster-ctl.sh start
    sleep 30
    ./test/docker/cluster-ctl.sh test
    ./test/docker/cluster-ctl.sh clean
```

## Known Limitations

1. **WireGuard in Docker**: May not work fully without kernel module loaded
2. **Systemd in Docker**: Requires privileged mode
3. **Resource Usage**: 5 nodes require ~2-4GB RAM
4. **Network Isolation**: Containers share host kernel

## Next Steps

After testing foundation:
1. Add Serf discovery verification
2. Test Raft leader election
3. Validate WireGuard mesh formation
4. Test service role assignment
5. Implement chaos testing

## Support

For issues specific to Docker testing:
- Check logs: `./test/docker/cluster-ctl.sh logs`
- Review systemd services: `./test/docker/cluster-ctl.sh exec node1 systemctl status`
- Inspect containers: `docker inspect cluster-os-node1`

For general Cluster-OS issues, see main [README.md](../../README.md)
