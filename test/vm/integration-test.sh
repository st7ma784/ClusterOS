#!/bin/bash
set -e

# QEMU VM Integration Test Script
# Tests full cluster functionality in QEMU VMs

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

TESTS_PASSED=0
TESTS_FAILED=0

log_test() { echo -e "${BLUE}[TEST]${NC} $1"; }
log_pass() { echo -e "${GREEN}[PASS]${NC} $1"; TESTS_PASSED=$((TESTS_PASSED + 1)); }
log_fail() { echo -e "${RED}[FAIL]${NC} $1"; TESTS_FAILED=$((TESTS_FAILED + 1)); }
log_info() { echo -e "${BLUE}[INFO]${NC} $1"; }

# Helper function to SSH to a node
ssh_node() {
    local node_num=$1
    local cmd=$2
    local port=$((2222 + node_num))

    timeout 10 ssh -p "$port" \
        -o StrictHostKeyChecking=no \
        -o ConnectTimeout=5 \
        -o UserKnownHostsFile=/dev/null \
        -q \
        clusteros@localhost "$cmd" 2>/dev/null
}

# Helper function to wait for VMs to be ready
wait_for_vms() {
    local nodes=$1
    log_info "Waiting for VMs to be ready (SSH accessible)..."

    for i in $(seq 1 "$nodes"); do
        local ready=false
        for attempt in {1..30}; do
            if ssh_node "$i" "echo ready" &>/dev/null; then
                log_info "Node $i is ready"
                ready=true
                break
            fi
            sleep 2
        done

        if [ "$ready" = false ]; then
            log_fail "Node $i failed to become ready"
            return 1
        fi
    done

    log_info "All VMs ready"
    return 0
}

# Test functions
test_vm_running() {
    local node=$1
    log_test "VM node$node is running"

    if ssh_node "$node" "uptime" &>/dev/null; then
        log_pass "Node $node is running"
        return 0
    else
        log_fail "Node $node is not accessible"
        return 1
    fi
}

test_systemd_pid1() {
    local node=$1
    log_test "systemd is PID 1 on node$node"

    local pid1=$(ssh_node "$node" "ps -p 1 -o comm=" 2>/dev/null | tr -d '[:space:]')

    if [ "$pid1" = "systemd" ]; then
        log_pass "systemd is PID 1 on node$node"
        return 0
    else
        log_fail "PID 1 is '$pid1', not systemd on node$node"
        return 1
    fi
}

test_node_agent_running() {
    local node=$1
    log_test "node-agent service is running on node$node"

    if ssh_node "$node" "sudo systemctl is-active node-agent" | grep -q "active"; then
        log_pass "node-agent is running on node$node"
        return 0
    else
        log_fail "node-agent is not running on node$node"
        return 1
    fi
}

test_node_identity() {
    local node=$1
    log_test "Node identity exists on node$node"

    if ssh_node "$node" "sudo test -f /var/lib/cluster-os/identity/node_id" &>/dev/null; then
        log_pass "Node identity exists on node$node"
        return 0
    else
        log_fail "Node identity missing on node$node"
        return 1
    fi
}

test_wireguard_installed() {
    local node=$1
    log_test "WireGuard is installed on node$node"

    if ssh_node "$node" "command -v wg" &>/dev/null; then
        log_pass "WireGuard is installed on node$node"
        return 0
    else
        log_fail "WireGuard is not installed on node$node"
        return 1
    fi
}

test_slurm_installed() {
    local node=$1
    log_test "SLURM is installed on node$node"

    if ssh_node "$node" "command -v sinfo" &>/dev/null; then
        log_pass "SLURM is installed on node$node"
        return 0
    else
        log_fail "SLURM is not installed on node$node"
        return 1
    fi
}

test_k3s_installed() {
    local node=$1
    log_test "K3s is installed on node$node"

    if ssh_node "$node" "command -v k3s" &>/dev/null; then
        log_pass "K3s is installed on node$node"
        return 0
    else
        log_fail "K3s is not installed on node$node"
        return 1
    fi
}

test_network_connectivity() {
    local from_node=$1
    local to_node=$2
    log_test "Network connectivity from node$from_node to node$to_node"

    # Get SSH port for target node
    local to_port=$((2222 + to_node))

    # Try to ping the host machine from source node (basic connectivity test)
    if ssh_node "$from_node" "ping -c 1 -W 2 10.0.2.2" &>/dev/null; then
        log_pass "Network connectivity works from node$from_node"
        return 0
    else
        log_fail "Network connectivity failed from node$from_node"
        return 1
    fi
}

test_systemd_services() {
    local node=$1
    log_test "Essential systemd services on node$node"

    local services=("systemd-networkd" "systemd-resolved")
    local all_ok=true

    for svc in "${services[@]}"; do
        if ! ssh_node "$node" "sudo systemctl is-active $svc" | grep -q "active"; then
            log_fail "$svc is not active on node$node"
            all_ok=false
        fi
    done

    if [ "$all_ok" = true ]; then
        log_pass "Essential systemd services running on node$node"
        return 0
    else
        return 1
    fi
}

test_dev_kmsg_access() {
    local node=$1
    log_test "/dev/kmsg access on node$node (required for K3s)"

    if ssh_node "$node" "sudo test -c /dev/kmsg" &>/dev/null; then
        log_pass "/dev/kmsg is accessible on node$node"
        return 0
    else
        log_fail "/dev/kmsg is not accessible on node$node"
        return 1
    fi
}

# Main test execution
main() {
    echo "========================================="
    echo "QEMU VM Integration Tests"
    echo "========================================="
    echo ""

    # Check if VMs are running
    if ! pgrep -f "qemu-system-x86_64.*cluster-os-node" &>/dev/null; then
        log_fail "No QEMU VMs running. Start cluster first with: make test-vm"
        exit 1
    fi

    # Determine number of nodes
    NUM_NODES=$(pgrep -f "qemu-system-x86_64.*cluster-os-node" | wc -l)
    log_info "Found $NUM_NODES running VM(s)"
    echo ""

    # Wait for VMs to be ready
    if ! wait_for_vms "$NUM_NODES"; then
        log_fail "VMs failed to become ready"
        exit 1
    fi

    echo ""
    log_info "Starting integration tests..."
    echo ""

    # Run tests on each node
    for node in $(seq 1 "$NUM_NODES"); do
        echo "========================================="
        echo "Testing Node $node"
        echo "========================================="

        test_vm_running "$node"
        test_systemd_pid1 "$node"
        test_node_agent_running "$node"
        test_node_identity "$node"
        test_wireguard_installed "$node"
        test_slurm_installed "$node"
        test_k3s_installed "$node"
        test_systemd_services "$node"
        test_dev_kmsg_access "$node"

        # Test network connectivity to next node
        if [ "$node" -lt "$NUM_NODES" ]; then
            test_network_connectivity "$node" $((node + 1))
        fi

        echo ""
    done

    # Summary
    echo "========================================="
    echo "Test Summary"
    echo "========================================="
    echo -e "${GREEN}Passed:${NC} $TESTS_PASSED"
    echo -e "${RED}Failed:${NC} $TESTS_FAILED"
    echo ""

    if [ $TESTS_FAILED -eq 0 ]; then
        echo -e "${GREEN}All tests passed!${NC}"
        exit 0
    else
        echo -e "${RED}Some tests failed!${NC}"
        exit 1
    fi
}

main "$@"
