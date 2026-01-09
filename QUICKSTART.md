# Cluster-OS Quick Start Guide

## Prerequisites

- Docker with systemd support
- At least 4GB RAM
- Linux host (for cgroup support)
- Git

## Installation

```bash
# Clone the repository
git clone https://github.com/your-org/cluster-os
cd cluster-os
```

## Build and Test

### Step 1: Build the Node Agent

```bash
make node
```

This compiles the node-agent binary that runs on each cluster node.

### Step 2: Start the Test Cluster

```bash
make test-cluster
```

This will:
1. Build Docker images with systemd
2. Start 5 containers (node1-node5)
3. Node1 bootstraps the cluster
4. Nodes 2-5 automatically join node1
5. All nodes authenticate using the cluster key

**Expected output:**
```
✓ Cluster started successfully!

Cluster nodes:
NAMES              STATUS          NETWORKS
cluster-os-node5   Up 2 seconds    docker_cluster_net
cluster-os-node4   Up 4 seconds    docker_cluster_net
cluster-os-node3   Up 6 seconds    docker_cluster_net
cluster-os-node2   Up 9 seconds    docker_cluster_net
cluster-os-node1   Up 20 seconds   docker_cluster_net
```

### Step 3: Check Cluster Status

```bash
./test/docker/cluster-ctl.sh status
```

**Expected output:**
```
Cluster Status:
✓ node1 - alive (bootstrap controller)
✓ node2 - alive (worker)
✓ node3 - alive (worker)
✓ node4 - alive (worker)
✓ node5 - alive (worker)
```

### Step 4: View Logs

```bash
# View logs from node1
./test/docker/cluster-ctl.sh logs node1

# Follow logs in real-time
./test/docker/cluster-ctl.sh logs node1 -f

# Look for authentication messages
docker logs cluster-os-node1 2>&1 | grep auth
```

**Expected authentication logs:**
```
Cluster Auth: 7RB0TPs+d/VuD3rL...  (truncated for security)
Node node2 authenticated successfully
Node node3 authenticated successfully
Node node4 authenticated successfully
Node node5 authenticated successfully
```

### Step 5: Interact with Nodes

```bash
# Open shell on node1
./test/docker/cluster-ctl.sh shell node1

# Inside the container, check services
systemctl status node-agent
journalctl -u node-agent -f

# Check WireGuard mesh
wg show wg0

# Exit the container
exit
```

### Step 6: Stop the Cluster

```bash
# Stop containers (preserves data)
./test/docker/stop-cluster.sh

# Or stop and remove all data
./test/docker/clean-cluster.sh
```

## Common Issues

### Issue: "Container name already in use"

**Solution:** The script now automatically cleans up old containers. If you still get this error:

```bash
# Manually stop and remove old containers
./test/docker/stop-cluster.sh

# Then try again
make test-cluster
```

### Issue: "Cannot connect to Docker daemon"

**Solution:** Make sure Docker is running:

```bash
sudo systemctl start docker
docker ps  # Should work without errors
```

### Issue: Nodes aren't joining the cluster

**Check:**
1. Verify all nodes have the same auth key:
   ```bash
   docker exec cluster-os-node1 grep auth_key /etc/cluster-os/node.yaml
   docker exec cluster-os-node2 grep auth_key /etc/cluster-os/node.yaml
   ```

2. Check if nodes can reach each other:
   ```bash
   docker exec cluster-os-node2 ping -c 3 node1
   ```

3. Check node-agent logs:
   ```bash
   docker logs cluster-os-node1
   ```

### Issue: "Permission denied" errors

**Solution:** Make sure scripts are executable:

```bash
chmod +x test/docker/*.sh
chmod +x scripts/*.sh
```

## Next Steps

### For Development

After validating in Docker, you can:

1. **Deploy to VMs** - Use the same Docker image
2. **Create Bare-Metal OS Image** - Build with Packer (Phase 5)
3. **Boot Physical Machines** - Nodes auto-join on first boot

### For Production

**🔒 IMPORTANT: Generate Your Own Cluster Key**

The default key in this repo is for TESTING ONLY. For production:

```bash
# Generate a new unique key
./scripts/generate-cluster-key.sh

# Update your configuration
# Edit node/config/node.yaml and set cluster.auth_key
# Or use environment variable: CLUSTEROS_CLUSTER_AUTH_KEY

# Rebuild and test
make test-cluster
```

See [SECURITY.md](SECURITY.md) for details.

### For Forking

If you fork this repository:

1. **Immediately regenerate the cluster key:**
   ```bash
   ./scripts/generate-cluster-key.sh
   ```

2. **Update configuration:**
   ```bash
   # Edit node/config/node.yaml
   # Set cluster.auth_key to your new key
   ```

3. **Commit your key (optional):**
   ```bash
   git add cluster.key node/config/node.yaml
   git commit -m "Set unique cluster authentication key"
   ```

This ensures your cluster is isolated from the original repository's cluster.

## Testing Scenarios

### Test Authentication

```bash
# Start cluster normally
make test-cluster

# Try to join with wrong key (should fail)
docker run -d \
  --name rogue-node \
  --network docker_cluster_net \
  --privileged --cgroupns=host \
  --tmpfs /run -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
  -e NODE_NAME=rogue \
  -e NODE_JOIN=node1:7946 \
  -e CLUSTER_AUTH_KEY=WRONG_KEY_aaaaaaaaaaaaaaaaaaaa= \
  cluster-os-node:latest

# Check node1 logs - should show rejection
docker logs cluster-os-node1 | grep "failed authentication"

# Clean up
docker stop rogue-node && docker rm rogue-node
```

### Test Node Failure and Recovery

```bash
# Start cluster
make test-cluster

# Kill node3
docker stop cluster-os-node3

# Watch cluster detect failure
./test/docker/cluster-ctl.sh logs node1 | grep node3

# Restart node3
docker start cluster-os-node3

# Watch it rejoin
./test/docker/cluster-ctl.sh logs node1 | grep "Node joined"
```

## Useful Commands Reference

```bash
# Build
make node               # Build node-agent binary
make test               # Run unit tests
make test-cluster       # Start Docker cluster

# Cluster Control
./test/docker/cluster-ctl.sh status      # Show cluster status
./test/docker/cluster-ctl.sh info        # Detailed cluster info
./test/docker/cluster-ctl.sh logs <node> # View node logs
./test/docker/cluster-ctl.sh shell <node> # Open shell on node

# Cleanup
./test/docker/stop-cluster.sh            # Stop cluster (keep data)
./test/docker/clean-cluster.sh           # Stop and remove all data

# Docker
docker ps --filter "name=cluster-os-"    # List cluster containers
docker logs -f cluster-os-node1          # Follow node1 logs
docker exec -it cluster-os-node1 bash    # Shell into node1
```

## Resources

- [Full Documentation](README.md)
- [Architecture](docs/architecture.md)
- [Cluster Authentication](docs/cluster-authentication.md)
- [Docker Testing Guide](test/docker/DOCKER_TESTING.md)
- [Security Guide](SECURITY.md)

## Getting Help

- Check logs: `./test/docker/cluster-ctl.sh logs node1`
- View issues: https://github.com/your-org/cluster-os/issues
- Read docs: [test/docker/DOCKER_TESTING.md](test/docker/DOCKER_TESTING.md)

## Success Indicators

Your cluster is working correctly when:

✅ All 5 containers start successfully
✅ Logs show "authenticated successfully" for all nodes
✅ `cluster-ctl.sh status` shows all nodes as "alive"
✅ WireGuard mesh is established (check with `wg show wg0`)
✅ Nodes can ping each other via WireGuard IPs (10.42.0.x)

Happy clustering! 🚀
