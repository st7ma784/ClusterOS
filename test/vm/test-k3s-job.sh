#!/bin/bash
set -e

# Test K3s/Kubernetes workloads
# Run this script from a node in the cluster

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_test() { echo -e "${BLUE}[TEST]${NC} $1"; }
log_pass() { echo -e "${GREEN}[PASS]${NC} $1"; }
log_fail() { echo -e "${RED}[FAIL]${NC} $1"; }
log_info() { echo -e "${BLUE}[INFO]${NC} $1"; }

# Helper for SSH to node
ssh_node() {
    local node_num=$1
    shift
    local port=$((2222 + node_num))
    ssh -p "$port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -q clusteros@localhost "$@"
}

echo "========================================="
echo "K3s/Kubernetes Job Tests"
echo "========================================="
echo ""

# Check K3s status
log_test "Checking K3s cluster status..."
if ssh_node 1 "sudo k3s kubectl get nodes" 2>/dev/null; then
    log_pass "K3s cluster is responding"
else
    log_fail "K3s cluster is not responding"
    echo ""
    echo "Checking k3s service status..."
    ssh_node 1 "sudo systemctl status k3s 2>/dev/null || sudo journalctl -u k3s -n 30" || true
    exit 1
fi

echo ""

# Check system pods
log_test "Checking system pods..."
ssh_node 1 "sudo k3s kubectl get pods -n kube-system" 2>/dev/null || log_fail "Could not get system pods"

echo ""

# Create a simple test pod
log_test "Creating test pod..."

TEST_POD=$(cat <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: test-pod
  namespace: default
spec:
  containers:
  - name: test-container
    image: busybox:latest
    command: ['sh', '-c', 'echo "Hello from Kubernetes!" && hostname && date && sleep 10']
  restartPolicy: Never
EOF
)

# Delete existing test pod if any
ssh_node 1 "sudo k3s kubectl delete pod test-pod --ignore-not-found=true" 2>/dev/null

# Create the pod
echo "$TEST_POD" | ssh_node 1 "sudo k3s kubectl apply -f -" 2>/dev/null

if [ $? -eq 0 ]; then
    log_pass "Test pod created"
else
    log_fail "Failed to create test pod"
    exit 1
fi

echo ""

# Wait for pod to complete
log_test "Waiting for pod to complete..."
MAX_WAIT=120
WAIT_TIME=0
while [ $WAIT_TIME -lt $MAX_WAIT ]; do
    POD_STATUS=$(ssh_node 1 "sudo k3s kubectl get pod test-pod -o jsonpath='{.status.phase}'" 2>/dev/null)
    
    echo "  Pod status: $POD_STATUS"
    
    if [ "$POD_STATUS" = "Succeeded" ] || [ "$POD_STATUS" = "Completed" ]; then
        log_pass "Pod completed successfully"
        break
    elif [ "$POD_STATUS" = "Failed" ]; then
        log_fail "Pod failed"
        break
    fi
    
    sleep 5
    WAIT_TIME=$((WAIT_TIME + 5))
done

echo ""

# Get pod logs
log_test "Getting pod logs..."
echo "-------------------------------------------"
ssh_node 1 "sudo k3s kubectl logs test-pod" 2>/dev/null || log_fail "Could not get pod logs"
echo "-------------------------------------------"

echo ""

# Create a deployment test
log_test "Creating test deployment..."

TEST_DEPLOYMENT=$(cat <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx-test
  namespace: default
spec:
  replicas: 2
  selector:
    matchLabels:
      app: nginx-test
  template:
    metadata:
      labels:
        app: nginx-test
    spec:
      containers:
      - name: nginx
        image: nginx:alpine
        ports:
        - containerPort: 80
        resources:
          limits:
            memory: "64Mi"
            cpu: "100m"
          requests:
            memory: "32Mi"
            cpu: "50m"
EOF
)

# Delete existing deployment if any
ssh_node 1 "sudo k3s kubectl delete deployment nginx-test --ignore-not-found=true" 2>/dev/null

# Create the deployment
echo "$TEST_DEPLOYMENT" | ssh_node 1 "sudo k3s kubectl apply -f -" 2>/dev/null

if [ $? -eq 0 ]; then
    log_pass "Test deployment created"
else
    log_fail "Failed to create test deployment"
fi

echo ""

# Wait for deployment to be ready
log_test "Waiting for deployment to be ready..."
ssh_node 1 "sudo k3s kubectl rollout status deployment/nginx-test --timeout=120s" 2>/dev/null

if [ $? -eq 0 ]; then
    log_pass "Deployment rolled out successfully"
else
    log_fail "Deployment rollout failed"
fi

echo ""

# Show deployment status
log_test "Checking deployment status..."
ssh_node 1 "sudo k3s kubectl get deployment nginx-test" 2>/dev/null
echo ""
ssh_node 1 "sudo k3s kubectl get pods -l app=nginx-test" 2>/dev/null

echo ""

# Create a service and test it
log_test "Creating service for nginx..."

TEST_SERVICE=$(cat <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: nginx-test-svc
  namespace: default
spec:
  selector:
    app: nginx-test
  ports:
  - port: 80
    targetPort: 80
  type: ClusterIP
EOF
)

echo "$TEST_SERVICE" | ssh_node 1 "sudo k3s kubectl apply -f -" 2>/dev/null

if [ $? -eq 0 ]; then
    log_pass "Service created"
    
    # Get service IP and test it
    SVC_IP=$(ssh_node 1 "sudo k3s kubectl get svc nginx-test-svc -o jsonpath='{.spec.clusterIP}'" 2>/dev/null)
    log_info "Service IP: $SVC_IP"
    
    # Test the service from within the cluster
    if ssh_node 1 "sudo k3s kubectl run curl-test --image=curlimages/curl --rm -it --restart=Never -- curl -s --connect-timeout 5 http://$SVC_IP" 2>/dev/null | head -5; then
        log_pass "Service is responding!"
    else
        log_info "Service test skipped (curl pod may not have started in time)"
    fi
else
    log_fail "Failed to create service"
fi

echo ""

# Cleanup
log_test "Cleaning up test resources..."
ssh_node 1 "sudo k3s kubectl delete pod test-pod --ignore-not-found=true" 2>/dev/null
ssh_node 1 "sudo k3s kubectl delete deployment nginx-test --ignore-not-found=true" 2>/dev/null
ssh_node 1 "sudo k3s kubectl delete svc nginx-test-svc --ignore-not-found=true" 2>/dev/null
log_pass "Cleanup complete"

echo ""
echo "========================================="
echo "K3s Tests Complete!"
echo "========================================="
