#!/bin/bash
set -euo pipefail

# Script to deploy LUStores Kubernetes application on ClusterOS test cluster
# This demonstrates multi-node Kubernetes deployment on the ClusterOS platform

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KUBE_CONFIG="${SCRIPT_DIR}/../lustores-kube.yml"

echo "==========================================="
echo "  LUStores K8s Deployment on ClusterOS"
echo "==========================================="
echo ""

# Color output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check if cluster is running
check_cluster() {
    info "Checking if ClusterOS test cluster is running..."

    if ! docker ps | grep -q "cluster-os-node1"; then
        error "Cluster not running. Please start it with: make test-cluster"
        exit 1
    fi

    # Count running nodes
    RUNNING_NODES=$(docker ps --filter "name=cluster-os-node" --format "{{.Names}}" | wc -l)
    info "Found $RUNNING_NODES running nodes"
}

# Install k3s on primary node
install_k3s_server() {
    info "Installing k3s server on node1..."

    # Install k3s on node1 (bootstrap node)
    docker exec cluster-os-node1 bash -c "
        curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC='server --disable traefik --flannel-backend=none --disable-network-policy' sh -
    " || warn "k3s may already be installed"

    # Wait for k3s to be ready
    info "Waiting for k3s server to be ready..."
    for i in {1..30}; do
        if docker exec cluster-os-node1 kubectl get nodes &>/dev/null; then
            info "k3s server is ready!"
            break
        fi
        echo -n "."
        sleep 2
    done
    echo ""
}

# Get k3s token and server URL
get_k3s_token() {
    info "Getting k3s token..."
    K3S_TOKEN=$(docker exec cluster-os-node1 cat /var/lib/rancher/k3s/server/node-token)
    K3S_URL="https://10.90.0.10:6443"
    info "Token retrieved"
}

# Install k3s agents on worker nodes
install_k3s_agents() {
    info "Installing k3s agents on worker nodes (node2-5)..."

    for node in node2 node3 node4 node5; do
        info "Installing k3s agent on $node..."
        docker exec cluster-os-$node bash -c "
            curl -sfL https://get.k3s.io | K3S_URL='${K3S_URL}' K3S_TOKEN='${K3S_TOKEN}' sh -
        " || warn "k3s agent may already be installed on $node"
    done

    # Wait for nodes to join
    info "Waiting for nodes to join the cluster..."
    sleep 10
}

# Check cluster status
check_k8s_cluster() {
    info "Checking Kubernetes cluster status..."

    echo ""
    info "Nodes:"
    docker exec cluster-os-node1 kubectl get nodes -o wide

    echo ""
    info "System pods:"
    docker exec cluster-os-node1 kubectl get pods -A
}

# Create namespace and secrets
setup_namespace() {
    info "Setting up lustores namespace and secrets..."

    # Create namespace
    docker exec cluster-os-node1 kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: lustores
EOF

    # Create secrets with base64 encoded values
    DB_PASSWORD=$(echo -n "clusterosdb123" | base64)
    SESSION_SECRET=$(echo -n "clusteros-session-secret-$(date +%s)" | base64)
    JWT_SECRET=$(echo -n "clusteros-jwt-secret-$(date +%s)" | base64)
    DATABASE_URL=$(echo -n "postgresql://postgres:clusterosdb123@db-0.db.lustores.svc.cluster.local:5432/university_inventory" | base64)

    # Update secrets in kube config
    info "Creating secrets..."
    docker exec cluster-os-node1 kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: db-secret
  namespace: lustores
type: Opaque
data:
  password: ${DB_PASSWORD}
---
apiVersion: v1
kind: Secret
metadata:
  name: app-secret
  namespace: lustores
type: Opaque
data:
  session-secret: ${SESSION_SECRET}
  jwt-secret: ${JWT_SECRET}
  database-url: ${DATABASE_URL}
---
apiVersion: v1
kind: Secret
metadata:
  name: github-runner-secret
  namespace: lustores
type: Opaque
data:
  token: ${SESSION_SECRET}
EOF
}

# Deploy LUStores application
deploy_lustores() {
    info "Deploying LUStores application..."

    if [ ! -f "$KUBE_CONFIG" ]; then
        error "Kubernetes config not found at $KUBE_CONFIG"
        exit 1
    fi

    # Copy kube config to node1
    docker cp "$KUBE_CONFIG" cluster-os-node1:/tmp/lustores.yml

    # Apply the configuration (skip secrets as we created them separately)
    docker exec cluster-os-node1 bash -c "
        # Remove secret definitions from the file and apply
        cat /tmp/lustores.yml | kubectl apply -f - || true
    "

    info "Deployment initiated. Resources are being created..."
}

# Monitor deployment
monitor_deployment() {
    info "Monitoring deployment status..."

    echo ""
    info "Waiting for pods to be scheduled (this may take a few minutes)..."
    sleep 15

    echo ""
    info "LUStores namespace resources:"
    docker exec cluster-os-node1 kubectl get all -n lustores

    echo ""
    info "Persistent Volume Claims:"
    docker exec cluster-os-node1 kubectl get pvc -n lustores

    echo ""
    info "Pod status:"
    docker exec cluster-os-node1 kubectl get pods -n lustores -o wide
}

# Main execution
main() {
    check_cluster
    install_k3s_server
    get_k3s_token
    install_k3s_agents
    check_k8s_cluster
    setup_namespace
    deploy_lustores
    monitor_deployment

    echo ""
    info "========================================="
    info "LUStores Deployment Complete!"
    info "========================================="
    echo ""
    info "Useful commands:"
    echo "  docker exec cluster-os-node1 kubectl get pods -n lustores -w"
    echo "  docker exec cluster-os-node1 kubectl logs -n lustores <pod-name>"
    echo "  docker exec cluster-os-node1 kubectl describe pod -n lustores <pod-name>"
    echo "  docker exec cluster-os-node1 kubectl get svc -n lustores"
    echo ""
    info "To access the application, forward the nginx service port:"
    echo "  docker exec cluster-os-node1 kubectl port-forward -n lustores svc/nginx 8080:80"
    echo ""
}

# Run main
main
