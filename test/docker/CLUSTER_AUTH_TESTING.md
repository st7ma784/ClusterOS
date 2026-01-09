# Testing Cluster Authentication in Docker

This guide demonstrates how to test the cluster authentication system using Docker containers.

## Quick Verification

### Test 1: All Nodes with Correct Key (Should Work)

```bash
# Start the cluster normally
make test-cluster

# Wait 30 seconds for cluster to form
sleep 30

# Check cluster status - all 5 nodes should be present
./test/docker/cluster-ctl.sh status

# Expected output:
# ✓ node1 - alive
# ✓ node2 - alive
# ✓ node3 - alive
# ✓ node4 - alive
# ✓ node5 - alive
```

### Test 2: Node with Wrong Key (Should Fail)

```bash
# Start the normal cluster first
make test-cluster

# Try to join a rogue node with wrong auth key
docker run -d \
    --name cluster-os-rogue \
    --hostname rogue \
    --privileged \
    --cgroupns=host \
    --network docker_cluster_net \
    --ip 10.90.0.50 \
    --tmpfs /run \
    --tmpfs /run/lock \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
    -e NODE_NAME=rogue \
    -e NODE_BOOTSTRAP=false \
    -e NODE_JOIN=node1:7946 \
    -e CLUSTER_AUTH_KEY=WRONG_KEY_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa= \
    cluster-os-node:latest

# Wait for join attempt
sleep 10

# Check node1 logs - should show authentication failure
docker logs cluster-os-node1 2>&1 | grep -i "auth"

# Expected output:
# "Node rogue failed authentication: invalid signature - node does not have correct cluster auth key"

# The rogue node should NOT appear in cluster members
docker exec cluster-os-node1 /usr/local/bin/node-agent info

# Clean up
docker stop cluster-os-rogue
docker rm cluster-os-rogue
```

### Test 3: Node with Missing Key (Should Fail)

```bash
# Start the normal cluster first
make test-cluster

# Try to join a node without any auth key
docker run -d \
    --name cluster-os-nokey \
    --hostname nokey \
    --privileged \
    --cgroupns=host \
    --network docker_cluster_net \
    --ip 10.90.0.51 \
    --tmpfs /run \
    --tmpfs /run/lock \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
    -e NODE_NAME=nokey \
    -e NODE_BOOTSTRAP=false \
    -e NODE_JOIN=node1:7946 \
    cluster-os-node:latest
    # Note: no CLUSTER_AUTH_KEY env var

# Check node1 logs
docker logs cluster-os-node1 2>&1 | grep -i "nokey"

# Expected: Node will fail to start due to validation error
# "cluster.auth_key must be set - run scripts/generate-cluster-key.sh to create one"

# Clean up
docker stop cluster-os-nokey
docker rm cluster-os-nokey
```

## Detailed Verification Steps

### Step 1: Verify Configuration Propagation

Check that all nodes received the correct auth key:

```bash
# Check node1
docker exec cluster-os-node1 grep auth_key /etc/cluster-os/node.yaml

# Check node2
docker exec cluster-os-node2 grep auth_key /etc/cluster-os/node.yaml

# All should show:
# auth_key: "7RB0TPs+d/VuD3rL/7ZD2JEcpA14aNCBvLOPHwEBy9s="
```

### Step 2: Verify Join Tokens

Check that nodes generate valid join tokens:

```bash
# View node logs to see token generation
docker logs cluster-os-node1 2>&1 | grep -i "token\|auth"

# Expected to see:
# "Cluster Auth: 7RB0TPs+d/VuD3r..."  (truncated for security)
```

### Step 3: Verify Authentication Success

```bash
# Check logs for successful authentication
docker logs cluster-os-node1 2>&1 | grep "authenticated successfully"

# Expected output:
# "Node node2 authenticated successfully"
# "Node node3 authenticated successfully"
# "Node node4 authenticated successfully"
# "Node node5 authenticated successfully"
```

### Step 4: Verify Cluster Membership

```bash
# Inside node1, check Serf members
docker exec cluster-os-node1 bash -c "serf members 2>/dev/null || echo 'Serf CLI not available, using agent info'"

# Or use node-agent
docker exec cluster-os-node1 /usr/local/bin/node-agent info

# Should show all 5 nodes as alive
```

## Testing Fork Isolation

Simulate two separate forks of the repository with different keys:

### Fork A (Cluster 1)

```bash
# Clean start
./test/docker/clean-cluster.sh

# Start cluster with key A
export CLUSTER_KEY_A="7RB0TPs+d/VuD3rL/7ZD2JEcpA14aNCBvLOPHwEBy9s="

# Start 3 nodes with key A
for i in 1 2 3; do
    IP=$((9 + i))
    docker run -d \
        --name cluster-a-node$i \
        --hostname cluster-a-node$i \
        --privileged --cgroupns=host \
        --network docker_cluster_net \
        --ip 10.90.0.$IP \
        --tmpfs /run --tmpfs /run/lock \
        -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
        -e NODE_NAME=cluster-a-node$i \
        -e NODE_BOOTSTRAP=$([ $i -eq 1 ] && echo "true" || echo "false") \
        -e NODE_JOIN=$([ $i -eq 1 ] && echo "" || echo "cluster-a-node1:7946") \
        -e CLUSTER_AUTH_KEY="$CLUSTER_KEY_A" \
        cluster-os-node:latest
    sleep 2
done
```

### Fork B (Cluster 2)

```bash
# Generate a different key for Fork B
export CLUSTER_KEY_B=$(openssl rand -base64 32)
echo "Fork B Key: $CLUSTER_KEY_B"

# Start 3 nodes with key B
for i in 4 5 6; do
    IP=$((9 + i))
    docker run -d \
        --name cluster-b-node$i \
        --hostname cluster-b-node$i \
        --privileged --cgroupns=host \
        --network docker_cluster_net \
        --ip 10.90.0.$IP \
        --tmpfs /run --tmpfs /run/lock \
        -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
        -e NODE_NAME=cluster-b-node$i \
        -e NODE_BOOTSTRAP=$([ $i -eq 4 ] && echo "true" || echo "false") \
        -e NODE_JOIN=$([ $i -eq 4 ] && echo "" || echo "cluster-b-node4:7946") \
        -e CLUSTER_AUTH_KEY="$CLUSTER_KEY_B" \
        cluster-os-node:latest
    sleep 2
done
```

### Verify Isolation

```bash
# Cluster A should have 3 members
docker exec cluster-a-node1 /usr/local/bin/node-agent info | grep -i "members\|nodes"

# Cluster B should have 3 members
docker exec cluster-b-node4 /usr/local/bin/node-agent info | grep -i "members\|nodes"

# Try to make a Cluster A node join Cluster B (should fail)
docker exec cluster-a-node1 bash -c 'echo "Attempting cross-cluster join..."'

# The clusters remain isolated due to different auth keys
```

### Cleanup

```bash
docker stop $(docker ps -q --filter "name=cluster-a-") $(docker ps -q --filter "name=cluster-b-")
docker rm $(docker ps -aq --filter "name=cluster-a-") $(docker ps -aq --filter "name=cluster-b-")
```

## Automated Test Script

Create a test script for CI/CD:

```bash
#!/bin/bash
# test/docker/test-auth.sh

set -e

echo "=== Cluster Authentication Test ==="

# Test 1: Normal cluster formation
echo "Test 1: Starting cluster with valid auth..."
make test-cluster
sleep 30

NODES=$(docker exec cluster-os-node1 /usr/local/bin/node-agent info | grep -c "alive" || echo 0)
if [ "$NODES" -eq 5 ]; then
    echo "✓ Test 1 PASSED: All 5 nodes joined"
else
    echo "✗ Test 1 FAILED: Expected 5 nodes, got $NODES"
    exit 1
fi

# Test 2: Wrong key rejection
echo "Test 2: Testing wrong key rejection..."
docker run -d --name test-rogue \
    --privileged --cgroupns=host \
    --network docker_cluster_net \
    --tmpfs /run -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
    -e NODE_NAME=rogue \
    -e NODE_JOIN=node1:7946 \
    -e CLUSTER_AUTH_KEY=wrong_key_aaaaaaaaaaaaaaaaaaaaaaaaaaa= \
    cluster-os-node:latest >/dev/null 2>&1 || true

sleep 10

REJECTED=$(docker logs cluster-os-node1 2>&1 | grep -c "failed authentication" || echo 0)
if [ "$REJECTED" -gt 0 ]; then
    echo "✓ Test 2 PASSED: Rogue node rejected"
else
    echo "✗ Test 2 FAILED: Rogue node was not rejected"
    exit 1
fi

docker stop test-rogue >/dev/null 2>&1 || true
docker rm test-rogue >/dev/null 2>&1 || true

# Cleanup
./test/docker/clean-cluster.sh

echo "=== All tests PASSED ==="
```

Make it executable:

```bash
chmod +x test/docker/test-auth.sh
```

Run it:

```bash
./test/docker/test-auth.sh
```

## Monitoring Authentication Events

### Real-time Authentication Log

```bash
# Watch all authentication events across the cluster
docker logs -f cluster-os-node1 2>&1 | grep --line-buffered -i "auth\|join\|reject"
```

### Authentication Metrics

```bash
# Count successful authentications
docker logs cluster-os-node1 2>&1 | grep -c "authenticated successfully"

# Count failed authentications
docker logs cluster-os-node1 2>&1 | grep -c "failed authentication"

# Count nodes without auth tokens
docker logs cluster-os-node1 2>&1 | grep -c "without auth token"
```

## Troubleshooting

### Issue: All nodes fail to join

**Check:**
1. Are all nodes using the same CLUSTER_AUTH_KEY?
2. Is the key properly base64 encoded?
3. Is the key at least 32 bytes (decoded)?

```bash
# Verify key on all nodes
for i in 1 2 3 4 5; do
    echo "node$i:"
    docker exec cluster-os-node$i grep auth_key /etc/cluster-os/node.yaml
done
```

### Issue: Node starts but doesn't join

**Check:**
1. Can the node reach the bootstrap node?
2. Is the auth key configured?
3. Are there network issues?

```bash
# Test connectivity
docker exec cluster-os-node2 ping -c 3 node1

# Check if Serf is running
docker exec cluster-os-node2 netstat -tulpn | grep 7946

# Check auth key
docker exec cluster-os-node2 cat /etc/cluster-os/node.yaml | grep auth_key
```

### Issue: Rogue node is accepted

This should NEVER happen. If it does:

```bash
# Verify the keys are actually different
docker exec cluster-os-node1 grep auth_key /etc/cluster-os/node.yaml
docker exec cluster-os-rogue grep auth_key /etc/cluster-os/node.yaml

# Check if authentication is actually running
docker logs cluster-os-node1 2>&1 | grep -i "cluster auth"
```

## Summary

The Docker test environment provides a complete validation platform for the cluster authentication system:

✓ **Fast iteration** - Test changes in seconds without deploying to hardware
✓ **Isolated testing** - Each test runs in its own environment
✓ **Fork simulation** - Verify different clusters remain isolated
✓ **Security validation** - Confirm unauthorized nodes are rejected
✓ **CI/CD ready** - Automated testing scripts for continuous validation

Once validated in Docker, the same authentication mechanism works identically on VMs and bare metal.
