# K3s Testing Status in ClusterOS

## Current State

### ✅ K3s Components That Work:
- k3s server starts and initializes
- etcd database initialized successfully
- API server attempts to start
- Certificate generation working
- Configuration files created
- Network configuration applied

### ❌ K3s Component That Fails:
- **kubelet**: Cannot start - requires `/dev/kmsg` access
  - Error: `open /dev/kmsg: permission denied`
  - This is a fundamental Linux kernel device that Docker containers cannot access

### 📊 K3s Startup Flow (from logs):

```
✓ Acquiring lock file
✓ Preparing data dir
✓ Starting k3s v1.34.3+k3s1
✓ Database table schema and indexes up to date
✓ Kine available at unix://kine.sock
✓ Certificates generated
✓ ETCD server now running
✓ API server configured
✗ Kubelet failed - cannot access /dev/kmsg
✓ Graceful shutdown initiated
```

## Why K3s Cannot Run Fully in Docker

The k3s kubelet component requires:
1. `/dev/kmsg` - Kernel message buffer (for logging)
2. Full cgroup v2 delegation (for container resource management)
3. D-Bus system socket (for systemd integration)

Docker containers, even in privileged mode, cannot provide these because:
- Kernel devices (/dev/kmsg) are filtered at the kernel level
- Cgroup delegation requires host systemd configuration
- D-Bus is not available in containerized environments

## Possible Workarounds

### 1. Use k3d (Recommended for Local Testing)
```bash
# Install k3d: https://k3d.io
k3d cluster create test-cluster
```
- Runs k3s in optimized containers with proper support
- Full Kubernetes support
- Easy node addition and cluster management

### 2. Run on Native Linux (No Docker)
If running on Linux directly (not Docker Desktop/WSL2):
- Install k3s with: `curl -sfL https://get.k3s.io | sh -`
- Full kernel device access
- No container restrictions

### 3. Minimal API-only Mode
- Could modify to use k3s API server components only
- No kubelet/node functionality
- Not a full k8s cluster

### 4. Use Kind or Minikube
Alternative Kubernetes distributions that handle containerization better:
- Kind: `kind create cluster`
- Minikube: `minikube start`

## Testing K3s API Server

Even though full k3s cannot run, we can test if k3s API would be accessible:

```bash
# Check if k3s processes started
docker exec cluster-os-node1 ps aux | grep k3s

# Check startup logs
docker exec cluster-os-node1 tail -100 /var/log/cluster-os/k3s-server.log

# Try kubectl (will fail due to API server not running)
docker exec cluster-os-node1 k3s kubectl get nodes 2>&1
```

## ClusterOS + WireGuard Status

✅ **WireGuard is fully operational**
- All 5 nodes connected
- Network verified
- Ready for distributed testing

**Recommendation**: Use the working ClusterOS + WireGuard infrastructure for testing distributed systems that don't require full Kubernetes. For Kubernetes testing, use k3d or native Linux.

## Files Related to K3s

- Implementation: [test/docker/entrypoint.sh](../test/docker/entrypoint.sh) (lines 115-150)
- Configuration: [test/docker/docker-compose.yaml](../test/docker/docker-compose.yaml)
- Logs: `docker exec cluster-os-node1 tail -f /var/log/cluster-os/k3s-server.log`

## Next Steps

1. If Kubernetes is required: Use k3d instead
2. If testing distributed networking: Use current ClusterOS setup with WireGuard
3. If testing HPC workloads: Install SLURM properly with systemd-enabled containers
