#!/bin/bash
# Direct Docker container startup script for Cluster-OS test cluster
# Uses Docker bridge networking for inter-container Serf/SLURM/k3s communication.
# Containers join Tailscale via baked-in OAuth creds (patch/tailscale.env) and become
# first-class cluster peers alongside bare metal nodes on the Tailscale network.
#
# Note: cgroupns=host is required for k3s and cannot be expressed in compose schema,
# so we use direct docker run here instead of docker compose.

set -e

GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
NC='\033[0m'

BRIDGE_NET=docker_cluster_net
CLUSTER_AUTH_KEY="${CLUSTER_AUTH_KEY:-$(cat cluster.key 2>/dev/null | tr -d '[:space:]')}"
if [ -z "$CLUSTER_AUTH_KEY" ]; then
    echo "Error: cluster.key not found. Run 'make cluster-key' first."
    exit 1
fi

echo -e "${GREEN}Starting Cluster-OS test cluster...${NC}"
echo "Bridge network: $BRIDGE_NET (10.90.0.0/16)"
echo "Tailscale: containers will join via baked OAuth creds (patch/tailscale.env)"
echo ""

# Clean up existing containers
echo -e "${YELLOW}Cleaning up any existing cluster containers...${NC}"
for i in 1 2 3 4 5; do
    if docker ps -a --format '{{.Names}}' | grep -q "^cluster-os-node$i$"; then
        echo "Removing existing container: cluster-os-node$i"
        docker stop cluster-os-node$i >/dev/null 2>&1 || true
        docker rm cluster-os-node$i >/dev/null 2>&1 || true
    fi
done
echo "Cleanup complete"
echo ""

# Create bridge network if it doesn't exist
if ! docker network inspect $BRIDGE_NET >/dev/null 2>&1; then
    echo "Creating bridge network '$BRIDGE_NET'..."
    docker network create --subnet=10.90.0.0/16 $BRIDGE_NET
fi

# Build the image
echo "Building node image..."
cd /home/user/ClusterOS
docker build -t cluster-os-node:latest -f node/Dockerfile .

# Start node1 (bootstrap — SLURM controller + k3s server)
echo -e "${BLUE}Starting node1 (bootstrap)  10.90.0.10...${NC}"
docker run -d \
    --name cluster-os-node1 \
    --hostname node1 \
    --privileged \
    --cgroupns=host \
    --cap-add=NET_ADMIN \
    --cap-add=SYS_MODULE \
    --cap-add=SYS_ADMIN \
    --security-opt seccomp=unconfined \
    --stop-signal=SIGRTMIN+3 \
    --network $BRIDGE_NET \
    --ip 10.90.0.10 \
    --device /dev/net/tun:/dev/net/tun \
    --tmpfs /run \
    --tmpfs /run/lock \
    --tmpfs /tmp \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
    -v cluster-os-node1-data:/var/lib/cluster-os \
    -v cluster-os-node1-log:/var/log/cluster-os \
    -e NODE_NAME=node1 \
    -e NODE_BOOTSTRAP=true \
    -e NODE_ROLES=slurm-controller,k3s-server \
    -e SERF_BIND_PORT=7946 \
    -e RAFT_BIND_PORT=7373 \
    -e CLUSTER_AUTH_KEY="$CLUSTER_AUTH_KEY" \
    -p 7946:7946/tcp \
    -p 7946:7946/udp \
    -p 6443:6443 \
    -p 6817:6817 \
    cluster-os-node:latest

echo "Waiting for node1 to initialize..."
sleep 10

# Start worker nodes
for i in 2 3 4 5; do
    IP_SUFFIX=$((i + 9))
    echo -e "${BLUE}Starting node$i  10.90.0.$IP_SUFFIX...${NC}"
    docker run -d \
        --name cluster-os-node$i \
        --hostname node$i \
        --privileged \
        --cgroupns=host \
        --cap-add=NET_ADMIN \
        --cap-add=SYS_MODULE \
        --cap-add=SYS_ADMIN \
        --security-opt seccomp=unconfined \
        --stop-signal=SIGRTMIN+3 \
        --network $BRIDGE_NET \
        --ip 10.90.0.$IP_SUFFIX \
        --device /dev/net/tun:/dev/net/tun \
        --tmpfs /run \
        --tmpfs /run/lock \
        --tmpfs /tmp \
        -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
        -v cluster-os-node$i-data:/var/lib/cluster-os \
        -v cluster-os-node$i-log:/var/log/cluster-os \
        -e NODE_NAME=node$i \
        -e NODE_BOOTSTRAP=false \
        -e NODE_JOIN=node1:7946 \
        -e NODE_ROLES=slurm-worker,k3s-agent \
        -e SERF_BIND_PORT=7946 \
        -e RAFT_BIND_PORT=7373 \
        -e CLUSTER_AUTH_KEY="$CLUSTER_AUTH_KEY" \
        cluster-os-node:latest
    sleep 2
done

echo ""
echo -e "${GREEN}Cluster started:${NC}"
docker ps --filter "name=cluster-os-" --format "table {{.Names}}\t{{.Status}}\t{{.Networks}}"
echo ""
echo "Once Tailscale connects, containers appear on the tailnet as:"
echo "  cluster-os-node1, cluster-os-node2, ... cluster-os-node5"
echo ""
echo "Useful commands:"
echo "  ./test/docker/cluster-ctl.sh status      # Show cluster status"
echo "  ./test/docker/cluster-ctl.sh logs node1  # View logs"
echo "  ./test/docker/cluster-ctl.sh shell node1 # Open shell"
echo "  ./test/docker/stop-cluster.sh            # Stop cluster"
