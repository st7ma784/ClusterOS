#!/bin/bash
# Stop the Cluster-OS test cluster

set -e

GREEN='\033[0;32m'
NC='\033[0m'

echo -e "${GREEN}Stopping Cluster-OS test cluster...${NC}"

# Stop all containers
docker stop $(docker ps -q --filter "name=cluster-os-") 2>/dev/null || true

# Remove containers
docker rm $(docker ps -aq --filter "name=cluster-os-") 2>/dev/null || true

echo -e "${GREEN}✓ Cluster stopped${NC}"
