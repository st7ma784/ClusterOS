#!/bin/bash
# Direct Docker container startup script for Cluster-OS test cluster
# This bypasses docker-compose to use cgroupns=host which isn't supported in compose schema validation

set -e

# Colors
GREEN='\033[0;32m'
BLUE='\033[0;34m'
NC='\033[0m'

echo -e "${GREEN}Starting Cluster-OS test cluster with direct Docker commands...${NC}"

# Create network if it doesn't exist
if ! docker network inspect docker_cluster_net >/dev/null 2>&1; then
    echo "Creating cluster network..."
    docker network create --subnet=10.90.0.0/16 docker_cluster_net
fi

# Build the image
echo "Building node image..."
cd /home/user/ClusterOS
docker build -t cluster-os-node:latest -f node/Dockerfile .

# Start node1 (bootstrap with SLURM controller + k3s server)
echo -e "${BLUE}Starting node1 (bootstrap - controller)...${NC}"
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
    --network docker_cluster_net \
    --ip 10.90.0.10 \
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
    -e WIREGUARD_PORT=51820 \
    -p 7946:7946/tcp \
    -p 7946:7946/udp \
    -p 6443:6443 \
    -p 6817:6817 \
    cluster-os-node:latest

# Wait for node1 to be healthy
echo "Waiting for node1 to initialize..."
sleep 10

# Start worker nodes
for i in 2 3 4 5; do
    # Calculate proper IP: node2=.11, node3=.12, node4=.13, node5=.14
    IP_SUFFIX=$((i + 9))
    echo -e "${BLUE}Starting node$i...${NC}"
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
        --network docker_cluster_net \
        --ip 10.90.0.$IP_SUFFIX \
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
        -e WIREGUARD_PORT=51820 \
        cluster-os-node:latest
    sleep 2
done

echo ""
echo -e "${GREEN}✓ Cluster started successfully!${NC}"
echo ""
echo "Cluster nodes:"
docker ps --filter "name=cluster-os-" --format "table {{.Names}}\t{{.Status}}\t{{.Networks}}"
