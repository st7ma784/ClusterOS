# Cluster-OS Docker Container Guide

This guide covers running Cluster-OS as a Docker container with support for Tailscale networking and GPU acceleration.

## Quick Start

### Interactive Mode with GPU

```bash
docker run -it --rm \
  --cap-add=NET_ADMIN \
  --device=/dev/net/tun \
  --gpus 1 \
  st7ma784/clusteros:latest
```

This launches an interactive container with:
- Single GPU mounted
- Tailscale networking enabled
- Auto-cleanup on exit

## Command Reference

### Core Flags

| Flag | Purpose |
|------|---------|
| `-it` | Interactive terminal mode |
| `--rm` | Auto-cleanup container on exit |
| `--cap-add=NET_ADMIN` | Required for Tailscale network management |
| `--device=/dev/net/tun` | Exposes TUN device for Tailscale VPN |
| `--gpus 1` | Mount a single GPU |
| `--gpus all` | Mount all available GPUs |

## Usage Scenarios

### 1. Development with Interactive Shell

```bash
docker run -it --rm \
  --cap-add=NET_ADMIN \
  --device=/dev/net/tun \
  --gpus 1 \
  st7ma784/clusteros:latest
```

Provides an interactive bash shell where you can:
- Inspect logs: `journalctl -xeu node-agent`
- Check status: `tailscale status`
- Test services: `systemctl status`

### 2. Background Daemon Mode

For production or long-running deployments:

```bash
docker run -d \
  --name cluster-node \
  --cap-add=NET_ADMIN \
  --device=/dev/net/tun \
  --gpus all \
  --restart unless-stopped \
  st7ma784/clusteros:latest
```

Monitor with:
```bash
docker logs -f cluster-node
docker exec -it cluster-node bash
```

### 3. Custom Node Configuration

Mount your own `node.yaml`:

```bash
docker run -it --rm \
  --cap-add=NET_ADMIN \
  --device=/dev/net/tun \
  --gpus 1 \
  -v /path/to/node.yaml:/etc/cluster-os/node.yaml \
  st7ma784/clusteros:latest
```

### 4. Bootstrap Mode (Start a New Cluster)

```bash
docker run -it --rm \
  --cap-add=NET_ADMIN \
  --device=/dev/net/tun \
  --gpus 1 \
  -e NODE_BOOTSTRAP=true \
  -e NODE_NAME=cluster-bootstrap \
  st7ma784/clusteros:latest
```

### 5. Join an Existing Cluster

```bash
docker run -it --rm \
  --cap-add=NET_ADMIN \
  --device=/dev/net/tun \
  --gpus 1 \
  -e NODE_JOIN=<bootstrap-peer-ip>:7946 \
  -e NODE_NAME=cluster-worker-1 \
  st7ma784/clusteros:latest
```

Replace `<bootstrap-peer-ip>` with the IP of the bootstrap node.

## Environment Variables

Configure container behavior via environment variables:

| Variable | Default | Example | Purpose |
|----------|---------|---------|---------|
| `NODE_NAME` | Container hostname | `my-worker-1` | Node identifier in cluster |
| `NODE_BOOTSTRAP` | `false` | `true` | Start new cluster (first node) |
| `NODE_JOIN` | None | `10.0.0.5:7946` | Join existing cluster at address |
| `NODE_ROLES` | Auto-detected | `slurm-worker,k3s-agent` | Comma-separated roles |
| `SERF_BIND_PORT` | `7946` | `7946` | Gossip protocol port |
| `RAFT_BIND_PORT` | `7373` | `7373` | Consensus protocol port |

### Example with Multiple Variables

```bash
docker run -it --rm \
  --cap-add=NET_ADMIN \
  --device=/dev/net/tun \
  --gpus 1 \
  -e NODE_NAME=gpu-worker-01 \
  -e NODE_BOOTSTRAP=false \
  -e NODE_ROLES=slurm-worker,k3s-agent \
  st7ma784/clusteros:latest
```

## GPU Support

### Prerequisites

Ensure your system has:
1. NVIDIA GPU with drivers installed
2. Docker version 19.03+ or `nvidia-docker`
3. NVIDIA Container Toolkit

Verify setup:
```bash
docker run --rm --gpus all ubuntu nvidia-smi
```

### GPU Usage Options

**Single GPU:**
```bash
--gpus 1
```

**All GPUs:**
```bash
--gpus all
```

**Specific GPU by ID:**
```bash
--gpus '"device=0,1"'
```

## Volume Mounts

Persist data across container restarts:

```bash
docker run -d \
  --cap-add=NET_ADMIN \
  --device=/dev/net/tun \
  --gpus 1 \
  -v cluster-os-identity:/var/lib/cluster-os \
  -v cluster-os-logs:/var/log/cluster-os \
  --restart unless-stopped \
  st7ma784/clusteros:latest
```

Create volumes first (optional):
```bash
docker volume create cluster-os-identity
docker volume create cluster-os-logs
```

List volumes:
```bash
docker volume ls | grep cluster-os
```

## Networking

### Port Mapping

Expose Cluster-OS ports to host:

```bash
docker run -d \
  --cap-add=NET_ADMIN \
  --device=/dev/net/tun \
  --gpus 1 \
  -p 7946:7946/tcp \
  -p 7946:7946/udp \
  -p 7373:7373/tcp \
  st7ma784/clusteros:latest
```

| Port | Protocol | Purpose |
|------|----------|---------|
| 7946 | TCP/UDP | Serf gossip protocol |
| 7373 | TCP | Raft consensus |
| 51820 | UDP | WireGuard (legacy, Tailscale now used) |

### Network Modes

**Default (bridge):**
```bash
docker run -it --rm ... st7ma784/clusteros:latest
```

**Host networking (not recommended for Tailscale):**
```bash
docker run -it --rm --network host ... st7ma784/clusteros:latest
```

**Custom network:**
```bash
# Create network
docker network create cluster-os-net

# Run container on network
docker run -it --rm \
  --network cluster-os-net \
  --cap-add=NET_ADMIN \
  --device=/dev/net/tun \
  --gpus 1 \
  st7ma784/clusteros:latest
```

## Debugging

### View Logs

Container startup logs:
```bash
docker logs <container-id>
```

Node-agent service logs:
```bash
docker exec -it <container-id> journalctl -xeu node-agent
```

Tailscale logs:
```bash
docker exec -it <container-id> tailscale status
docker exec -it <container-id> tailscale netcheck
```

### Interactive Shell

```bash
docker exec -it <container-id> bash
```

### Container Inspection

```bash
# View container details
docker inspect <container-id>

# Check resource usage
docker stats <container-id>

# Check network connections
docker exec -it <container-id> netstat -tlnp
```

## Tailscale Integration

The container automatically authenticates with Tailscale using baked-in OAuth credentials (same as bare-metal deployments).

### Check Tailscale Status

```bash
docker exec -it <container-id> tailscale status
```

Output shows:
- VPN connection status
- Assigned Tailscale IP (e.g., `100.90.106.18`)
- Connected peers
- Hostname

### Network Testing

```bash
# Ping another Tailscale node
docker exec -it <container-id> ping 100.x.x.x

# Check Tailscale network
docker exec -it <container-id> tailscale netcheck
```

## Resource Management

### Memory Limits

Restrict memory usage:
```bash
docker run -it --rm \
  --cap-add=NET_ADMIN \
  --device=/dev/net/tun \
  --gpus 1 \
  -m 4g \
  st7ma784/clusteros:latest
```

### CPU Limits

Limit CPU cores:
```bash
docker run -it --rm \
  --cap-add=NET_ADMIN \
  --device=/dev/net/tun \
  --gpus 1 \
  --cpus 4 \
  st7ma784/clusteros:latest
```

### Combined Resource Limits

```bash
docker run -it --rm \
  --cap-add=NET_ADMIN \
  --device=/dev/net/tun \
  --gpus 1 \
  -m 8g \
  --cpus 8 \
  st7ma784/clusteros:latest
```

## Container Lifecycle

### Start

```bash
docker run -d --name my-node \
  --cap-add=NET_ADMIN \
  --device=/dev/net/tun \
  --gpus 1 \
  st7ma784/clusteros:latest
```

### Pause/Resume

```bash
docker pause my-node
docker unpause my-node
```

### Stop/Start

```bash
docker stop my-node
docker start my-node
```

### Remove

```bash
docker rm -f my-node
```

## Building Custom Images

Modify the Dockerfile for custom configurations:

```bash
# From repository root
cd /home/user/ClusterOS
docker build -f node/Dockerfile -t my-clusteros:latest .
```

## Troubleshooting

### Tailscale fails to start

**Error**: `System has not been booted with systemd as init system`

**Solution**: Add required systemd flags:
```bash
docker run -it --rm \
  --cap-add=SYS_ADMIN \
  --cap-add=NET_ADMIN \
  --device=/dev/net/tun \
  --gpus 1 \
  st7ma784/clusteros:latest
```

### GPU not detected

**Error**: `could not select device driver`

**Solution**: Verify NVIDIA Docker setup:
```bash
docker run --rm --gpus all ubuntu nvidia-smi
```

### Network connectivity issues

**Debug**:
```bash
# Check Tailscale connection
docker exec -it <container-id> tailscale status

# Verify network interface
docker exec -it <container-id> ip addr show

# Test DNS resolution
docker exec -it <container-id> nslookup cluster-os.local
```

## Best Practices

1. **Always use `--cap-add=NET_ADMIN`** for Tailscale
2. **Use `--restart unless-stopped`** for production deployments
3. **Mount volumes** to persist cluster identity and logs
4. **Use specific image tags** (e.g., `v1.0.0`) instead of `latest` in production
5. **Monitor logs** regularly for errors and warnings
6. **Set resource limits** to prevent host system exhaustion
7. **Use unique `NODE_NAME`** values to avoid conflicts

## See Also

- [Cluster Authentication Guide](cluster-authentication.md)
- [Deployment Guide](DEPLOYMENT.md)
- [Docker Testing Documentation](../test/docker/README.md)
- [Networking Architecture](NETWORKING.md)
