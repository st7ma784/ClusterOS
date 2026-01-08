#!/bin/bash
# Integration test script for Cluster-OS Docker environment

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
TEST_PASSED=0
TEST_FAILED=0

# Colors for output
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

test_pass() {
    echo -e "${GREEN}✓${NC} $1"
    ((TEST_PASSED++))
}

test_fail() {
    echo -e "${RED}✗${NC} $1"
    ((TEST_FAILED++))
}

# Test 1: Check if containers are running
test_containers_running() {
    log_info "Test 1: Checking if all containers are running..."

    for node in node1 node2 node3 node4 node5; do
        if docker ps | grep -q "cluster-os-$node"; then
            test_pass "Container $node is running"
        else
            test_fail "Container $node is NOT running"
        fi
    done
}

# Test 2: Check if node-agent is installed
test_node_agent_installed() {
    log_info "Test 2: Checking if node-agent is installed on all nodes..."

    for node in node1 node2 node3 node4 node5; do
        if docker exec "cluster-os-$node" /usr/local/bin/node-agent --version > /dev/null 2>&1; then
            test_pass "node-agent installed on $node"
        else
            test_fail "node-agent NOT installed on $node"
        fi
    done
}

# Test 3: Check if identities were generated
test_identities_generated() {
    log_info "Test 3: Checking if node identities were generated..."

    for node in node1 node2 node3 node4 node5; do
        if docker exec "cluster-os-$node" test -f /var/lib/cluster-os/identity.json; then
            test_pass "Identity generated for $node"
        else
            test_fail "Identity NOT generated for $node"
        fi
    done
}

# Test 4: Check if identities are unique
test_identities_unique() {
    log_info "Test 4: Checking if node identities are unique..."

    declare -A node_ids

    for node in node1 node2 node3 node4 node5; do
        node_id=$(docker exec "cluster-os-$node" cat /var/lib/cluster-os/identity.json 2>/dev/null | grep -o '"node_id":"[^"]*"' | cut -d'"' -f4 || echo "")

        if [ -z "$node_id" ]; then
            test_fail "Could not read node ID for $node"
            continue
        fi

        if [ -n "${node_ids[$node_id]}" ]; then
            test_fail "Duplicate node ID detected: $node_id (nodes: ${node_ids[$node_id]} and $node)"
        else
            node_ids[$node_id]=$node
            test_pass "Node $node has unique ID: ${node_id:0:16}..."
        fi
    done
}

# Test 5: Check if configuration files exist
test_config_files() {
    log_info "Test 5: Checking if configuration files exist..."

    for node in node1 node2 node3 node4 node5; do
        if docker exec "cluster-os-$node" test -f /etc/cluster-os/node.yaml; then
            test_pass "Config file exists for $node"
        else
            test_fail "Config file NOT found for $node"
        fi
    done
}

# Test 6: Check systemd service status
test_systemd_service() {
    log_info "Test 6: Checking node-agent systemd service status..."

    for node in node1 node2 node3 node4 node5; do
        status=$(docker exec "cluster-os-$node" systemctl is-active node-agent.service 2>&1 || echo "inactive")

        if [ "$status" = "active" ]; then
            test_pass "node-agent service active on $node"
        else
            test_fail "node-agent service NOT active on $node (status: $status)"
        fi
    done
}

# Test 7: Check network connectivity between nodes
test_network_connectivity() {
    log_info "Test 7: Checking network connectivity between nodes..."

    # Ping from node1 to all other nodes
    for target in node2 node3 node4 node5; do
        if docker exec cluster-os-node1 ping -c 1 -W 2 "$target" > /dev/null 2>&1; then
            test_pass "node1 can ping $target"
        else
            test_fail "node1 CANNOT ping $target"
        fi
    done
}

# Test 8: Check if logs are being generated
test_logs_generated() {
    log_info "Test 8: Checking if logs are being generated..."

    for node in node1 node2 node3 node4 node5; do
        log_lines=$(docker exec "cluster-os-$node" journalctl -u node-agent.service --no-pager -n 10 2>/dev/null | wc -l || echo "0")

        if [ "$log_lines" -gt 0 ]; then
            test_pass "Logs generated for $node ($log_lines lines)"
        else
            test_fail "No logs found for $node"
        fi
    done
}

# Print test summary
print_summary() {
    echo ""
    echo "=========================================="
    echo "Test Summary"
    echo "=========================================="
    echo -e "Tests Passed: ${GREEN}$TEST_PASSED${NC}"
    echo -e "Tests Failed: ${RED}$TEST_FAILED${NC}"
    echo "Total Tests:  $((TEST_PASSED + TEST_FAILED))"
    echo "=========================================="

    if [ $TEST_FAILED -eq 0 ]; then
        echo -e "${GREEN}All tests passed!${NC}"
        return 0
    else
        echo -e "${RED}Some tests failed!${NC}"
        return 1
    fi
}

# Main execution
main() {
    echo "=========================================="
    echo "Cluster-OS Integration Tests"
    echo "=========================================="
    echo ""

    # Run all tests
    test_containers_running
    test_node_agent_installed
    test_identities_generated
    test_identities_unique
    test_config_files
    test_systemd_service
    test_network_connectivity
    test_logs_generated

    # Print summary and exit
    print_summary
    exit $?
}

main "$@"
