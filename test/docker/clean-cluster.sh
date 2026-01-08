#!/bin/bash
# Clean up the Cluster-OS test cluster (removes all data)

set -e

YELLOW='\033[1;33m'
GREEN='\033[0;32m'
NC='\033[0m'

echo -e "${YELLOW}WARNING: This will delete all cluster data!${NC}"
read -p "Are you sure? (y/N) " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Aborted"
    exit 1
fi

echo -e "${GREEN}Stopping containers...${NC}"
docker stop $(docker ps -q --filter "name=cluster-os-") 2>/dev/null || true

echo -e "${GREEN}Removing containers...${NC}"
docker rm $(docker ps -aq --filter "name=cluster-os-") 2>/dev/null || true

echo -e "${GREEN}Removing volumes...${NC}"
docker volume rm $(docker volume ls -q | grep "cluster-os-node") 2>/dev/null || true

echo -e "${GREEN}Removing network...${NC}"
docker network rm docker_cluster_net 2>/dev/null || true

echo -e "${GREEN}✓ Cluster cleaned up${NC}"
