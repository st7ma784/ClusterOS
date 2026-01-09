#!/bin/bash
# Integration test script for Cluster-OS Docker environment

# Don't exit on error - we want to run all tests and report results
set +e

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

# Test 9: Check cluster authentication configuration
test_cluster_authentication() {
    log_info "Test 9: Checking cluster authentication configuration..."

    for node in node1 node2 node3 node4 node5; do
        auth_key=$(docker exec "cluster-os-$node" env | grep CLUSTER_AUTH_KEY | cut -d= -f2)

        if [ -n "$auth_key" ]; then
            test_pass "$node has cluster auth key configured (${auth_key:0:16}...)"
        else
            test_fail "$node does NOT have cluster auth key"
        fi
    done
}

# Test 10: Check authentication logs
test_authentication_logs() {
    log_info "Test 10: Checking for authentication success logs..."

    # Give nodes time to authenticate
    sleep 5

    # Check node1 logs for authentication messages
    auth_logs=$(docker logs cluster-os-node1 2>&1 | grep -i "cluster auth" | head -1)

    if [ -n "$auth_logs" ]; then
        test_pass "Authentication system is active on node1"
    else
        test_warn "Authentication logs not found (may still be starting)"
    fi

    # Check for successful authentication
    auth_success=$(docker logs cluster-os-node1 2>&1 | grep -c "authenticated successfully" || echo "0")

    if [ "$auth_success" -gt 0 ]; then
        test_pass "Found $auth_success successful authentication(s)"
    else
        test_warn "No successful authentications logged yet"
    fi
}

# Test 11: Check WireGuard interface
test_wireguard_interface() {
    log_info "Test 11: Checking WireGuard interface..."

    # Give nodes time to initialize WireGuard (up to 30 seconds)
    sleep 5

    for node in node1 node2 node3 node4 node5; do
        for i in {1..5}; do
            if docker exec "cluster-os-$node" ip link show wg0 > /dev/null 2>&1; then
                test_pass "$node has WireGuard interface wg0"
                # Get additional WireGuard status
                wg_status=$(docker exec "cluster-os-$node" wg show wg0 2>/dev/null | head -1)
                if [ -n "$wg_status" ]; then
                    test_pass "$node WireGuard interface is operational"
                fi
                break
            else
                if [ $i -lt 5 ]; then
                    log_warn "WireGuard not ready on $node yet (attempt $i/5), waiting..."
                    sleep 2
                else
                    test_fail "$node does NOT have wg0 interface after 10 seconds"
                    # Show diagnostics
                    docker exec "cluster-os-$node" ip link show 2>/dev/null || true
                    docker exec "cluster-os-$node" journalctl -u node-agent.service -n 20 2>/dev/null || true
                fi
            fi
        done
    done
}

# Test 12: Deploy lustores on Kubernetes
test_lustores_deployment() {
    log_info "Test 12: Testing lustores-kube deployment..."

    # Check if lustores-kube.yml exists
    if [ ! -f "$PROJECT_ROOT/test/lustores-kube.yml" ]; then
        test_fail "lustores-kube.yml not found"
        return
    fi

    # Wait for k3s to be ready on node1 (it should already be starting)
    log_info "Waiting for k3s API to be ready..."
    for i in {1..60}; do
        if docker exec cluster-os-node1 k3s kubectl get nodes > /dev/null 2>&1; then
            test_pass "k3s is ready on node1"
            break
        fi
        if [ $i -eq 60 ]; then
            test_warn "k3s API did not become ready in time (120 seconds)"
            log_info "Note: k3s was started in the background during container initialization"
            log_info "Check logs: docker logs cluster-os-node1 | grep k3s"
            return
        fi
        if [ $((i % 15)) -eq 0 ]; then
            log_info "Still waiting for k3s... ($i/120 seconds)"
        fi
        sleep 2
    done

    # Apply lustores deployment
    log_info "Applying lustores deployment..."
    if docker exec cluster-os-node1 k3s kubectl apply -f /tmp/lustores-kube.yml > /dev/null 2>&1; then
        test_pass "lustores deployment applied"
    else
        # Copy file and try again
        docker cp "$PROJECT_ROOT/test/lustores-kube.yml" cluster-os-node1:/tmp/lustores-kube.yml
        if docker exec cluster-os-node1 k3s kubectl apply -f /tmp/lustores-kube.yml > /dev/null 2>&1; then
            test_pass "lustores deployment applied"
        else
            test_fail "Failed to apply lustores deployment"
            return
        fi
    fi

    # Wait for pods to be created
    sleep 5

    # Check if lustores pods are running
    log_info "Checking lustores pod status..."
    pod_count=$(docker exec cluster-os-node1 k3s kubectl get pods -l app=lustores -o json 2>/dev/null | grep -c '"phase":"Running"' || echo "0")

    if [ "$pod_count" -gt 0 ]; then
        test_pass "lustores has $pod_count running pod(s)"
    else
        test_warn "lustores pods not yet running (may still be scheduling)"
        # Show pod status for debugging
        docker exec cluster-os-node1 k3s kubectl get pods -l app=lustores 2>/dev/null || true
    fi
}

test_warn() {
    echo -e "${YELLOW}⚠${NC} $1"
    # Don't increment counters for warnings
}

# Test 13: Check SLURM controller election
test_slurm_controller_election() {
    log_info "Test 13: Testing SLURM controller leader election..."

    # Give SLURM time to elect controller (Raft leader)
    sleep 10

    controller_count=0
    controller_node=""

    for node in node1 node2 node3 node4 node5; do
        if docker exec "cluster-os-$node" pgrep slurmctld > /dev/null 2>&1; then
            ((controller_count++))
            controller_node=$node
        fi
    done

    if [ $controller_count -eq 1 ]; then
        test_pass "SLURM controller elected on $controller_node"

        # Verify controller is running
        if docker exec "cluster-os-$controller_node" systemctl is-active slurmctld > /dev/null 2>&1; then
            test_pass "slurmctld service is active on $controller_node"
        else
            test_fail "slurmctld service is not active on $controller_node"
        fi
    elif [ $controller_count -eq 0 ]; then
        test_fail "No SLURM controller elected (expected 1, found 0)"
    else
        test_fail "Multiple SLURM controllers running (expected 1, found $controller_count)"
    fi
}

# Test 14: Check SLURM workers registered
test_slurm_workers() {
    log_info "Test 14: Testing SLURM worker registration..."

    # Give workers time to register
    sleep 5

    # Find the controller node
    controller_node=""
    for node in node1 node2 node3 node4 node5; do
        if docker exec "cluster-os-$node" pgrep slurmctld > /dev/null 2>&1; then
            controller_node=$node
            break
        fi
    done

    if [ -z "$controller_node" ]; then
        test_fail "Cannot test workers: no controller found"
        return
    fi

    # Check how many workers are registered
    worker_count=$(docker exec "cluster-os-$controller_node" sinfo -h 2>/dev/null | wc -l || echo "0")

    if [ "$worker_count" -ge 5 ]; then
        test_pass "All SLURM workers registered ($worker_count nodes)"
    elif [ "$worker_count" -gt 0 ]; then
        test_warn "Some SLURM workers registered ($worker_count/5 nodes)"
    else
        test_fail "No SLURM workers registered"
    fi
}

# Test 15: Check munge authentication
test_munge_authentication() {
    log_info "Test 15: Testing munge authentication across nodes..."

    # Give munge time to start
    sleep 5

    # Check if munge key exists and has correct permissions
    for node in node1 node2 node3 node4 node5; do
        if docker exec "cluster-os-$node" test -f /etc/munge/munge.key; then
            # Check permissions (should be 0400)
            perms=$(docker exec "cluster-os-$node" stat -c "%a" /etc/munge/munge.key 2>/dev/null || echo "000")
            if [ "$perms" = "400" ]; then
                test_pass "$node has munge key with correct permissions"
            else
                test_warn "$node has munge key but permissions are $perms (expected 400)"
            fi
        else
            test_fail "$node does NOT have munge key"
        fi
    done

    # Verify all nodes have identical munge keys
    declare -A munge_hashes
    for node in node1 node2 node3 node4 node5; do
        hash=$(docker exec "cluster-os-$node" md5sum /etc/munge/munge.key 2>/dev/null | awk '{print $1}' || echo "")
        if [ -n "$hash" ]; then
            munge_hashes[$node]=$hash
        fi
    done

    # Compare hashes
    first_hash=""
    all_match=true
    for node in node1 node2 node3 node4 node5; do
        hash=${munge_hashes[$node]}
        if [ -z "$first_hash" ]; then
            first_hash=$hash
        elif [ "$hash" != "$first_hash" ]; then
            all_match=false
            test_fail "Munge key mismatch: $node has different key"
        fi
    done

    if $all_match && [ -n "$first_hash" ]; then
        test_pass "All nodes have identical munge keys"
    fi
}

# Test 16: Test SLURM job submission
test_slurm_job_submission() {
    log_info "Test 16: Testing SLURM job submission..."

    # Find the controller node
    controller_node=""
    for node in node1 node2 node3 node4 node5; do
        if docker exec "cluster-os-$node" pgrep slurmctld > /dev/null 2>&1; then
            controller_node=$node
            break
        fi
    done

    if [ -z "$controller_node" ]; then
        test_fail "Cannot test job submission: no controller found"
        return
    fi

    # Submit a simple test job
    job_output=$(docker exec "cluster-os-$controller_node" sbatch --wrap="hostname" 2>&1 || echo "")
    job_id=$(echo "$job_output" | grep -oP 'Submitted batch job \K\d+' || echo "")

    if [ -n "$job_id" ]; then
        test_pass "SLURM job submitted successfully (job ID: $job_id)"

        # Wait for job to complete (max 30 seconds)
        for i in {1..15}; do
            state=$(docker exec "cluster-os-$controller_node" sacct -j "$job_id" -n -o state 2>/dev/null | head -1 | xargs || echo "")
            if [ "$state" = "COMPLETED" ]; then
                test_pass "SLURM job completed successfully"
                break
            elif [ "$state" = "FAILED" ]; then
                test_fail "SLURM job failed"
                break
            fi
            sleep 2
        done

        if [ "$state" != "COMPLETED" ] && [ "$state" != "FAILED" ]; then
            test_warn "SLURM job still in state: $state (may take longer)"
        fi
    else
        test_fail "Failed to submit SLURM job: $job_output"
    fi
}

# Test 17: Test MPI support
test_mpi_support() {
    log_info "Test 17: Testing MPI support in SLURM..."

    # Copy MPI test script to controller
    controller_node=""
    for node in node1 node2 node3 node4 node5; do
        if docker exec "cluster-os-$node" pgrep slurmctld > /dev/null 2>&1; then
            controller_node=$node
            break
        fi
    done

    if [ -z "$controller_node" ]; then
        test_fail "Cannot test MPI: no controller found"
        return
    fi

    # Check if MPI is configured in slurm.conf
    mpi_config=$(docker exec "cluster-os-$controller_node" grep "MpiDefault" /etc/slurm/slurm.conf 2>/dev/null || echo "")
    if echo "$mpi_config" | grep -q "pmix"; then
        test_pass "SLURM configured with MPI support (PMIx)"
    else
        test_warn "SLURM MPI configuration: $mpi_config"
    fi

    # Check if mpi4py is available
    if docker exec "cluster-os-$controller_node" python3 -c "import mpi4py" 2>/dev/null; then
        test_pass "Python mpi4py module is available"
    else
        test_warn "Python mpi4py module not available"
    fi
}

# Test 18: Test SLURM controller failover
test_slurm_failover() {
    log_info "Test 18: Testing SLURM controller failover..."

    # Find current controller
    original_controller=""
    for node in node1 node2 node3 node4 node5; do
        if docker exec "cluster-os-$node" pgrep slurmctld > /dev/null 2>&1; then
            original_controller=$node
            break
        fi
    done

    if [ -z "$original_controller" ]; then
        test_fail "Cannot test failover: no controller found"
        return
    fi

    log_info "Original controller: $original_controller"

    # Stop the controller
    log_info "Stopping slurmctld on $original_controller..."
    docker exec "cluster-os-$original_controller" pkill -9 slurmctld 2>/dev/null || true

    # Wait for re-election (Raft should elect new leader)
    sleep 15

    # Check if a new controller was elected
    new_controller=""
    for node in node1 node2 node3 node4 node5; do
        if docker exec "cluster-os-$node" pgrep slurmctld > /dev/null 2>&1; then
            new_controller=$node
            break
        fi
    done

    if [ -n "$new_controller" ]; then
        test_pass "New SLURM controller elected: $new_controller"

        if [ "$new_controller" != "$original_controller" ]; then
            test_pass "Failover successful: controller moved from $original_controller to $new_controller"
        else
            test_pass "Original controller restarted (also valid)"
        fi
    else
        test_fail "No SLURM controller elected after failover"
    fi
}

# Test 19: Check K3s server election
test_k3s_server_election() {
    log_info "Test 19: Testing K3s server leader election..."

    # Give K3s time to elect server (Raft leader)
    sleep 10

    server_count=0
    server_node=""

    for node in node1 node2 node3 node4 node5; do
        if docker exec "cluster-os-$node" pgrep k3s | xargs -I {} docker exec "cluster-os-$node" cat /proc/{}/cmdline 2>/dev/null | grep -q "server"; then
            ((server_count++))
            server_node=$node
        fi
    done

    if [ $server_count -eq 1 ]; then
        test_pass "K3s server elected on $server_node"
    elif [ $server_count -eq 0 ]; then
        test_warn "No K3s server running (may not be enabled yet)"
    else
        test_fail "Multiple K3s servers running (expected 1, found $server_count)"
    fi
}

# Test 20: Check K3s agent registration
test_k3s_agents() {
    log_info "Test 20: Testing K3s agent registration..."

    # Find the server node
    server_node=""
    for node in node1 node2 node3 node4 node5; do
        if docker exec "cluster-os-$node" pgrep k3s | xargs -I {} docker exec "cluster-os-$node" cat /proc/{}/cmdline 2>/dev/null | grep -q "server"; then
            server_node=$node
            break
        fi
    done

    if [ -z "$server_node" ]; then
        test_warn "Cannot test K3s agents: no server found"
        return
    fi

    # Check how many nodes are registered
    node_count=$(docker exec "cluster-os-$server_node" k3s kubectl get nodes --no-headers 2>/dev/null | wc -l || echo "0")

    if [ "$node_count" -ge 5 ]; then
        test_pass "All K3s nodes registered ($node_count nodes)"
    elif [ "$node_count" -gt 0 ]; then
        test_warn "Some K3s nodes registered ($node_count/5 nodes)"
    else
        test_warn "No K3s nodes registered yet"
    fi
}

# Test 21: Test K3s pod deployment
test_k3s_pod_deployment() {
    log_info "Test 21: Testing K3s pod deployment..."

    # Find the server node
    server_node=""
    for node in node1 node2 node3 node4 node5; do
        if docker exec "cluster-os-$node" pgrep k3s | xargs -I {} docker exec "cluster-os-$node" cat /proc/{}/cmdline 2>/dev/null | grep -q "server"; then
            server_node=$node
            break
        fi
    done

    if [ -z "$server_node" ]; then
        test_warn "Cannot test pod deployment: no K3s server found"
        return
    fi

    # Deploy a simple test pod
    docker exec "cluster-os-$server_node" k3s kubectl run test-pod --image=busybox --restart=Never -- sleep 3600 2>/dev/null || true

    # Wait for pod to be scheduled
    sleep 5

    # Check pod status
    pod_status=$(docker exec "cluster-os-$server_node" k3s kubectl get pod test-pod -o jsonpath='{.status.phase}' 2>/dev/null || echo "")

    if [ "$pod_status" = "Running" ]; then
        test_pass "K3s test pod deployed and running"
        # Clean up
        docker exec "cluster-os-$server_node" k3s kubectl delete pod test-pod 2>/dev/null || true
    elif [ "$pod_status" = "Pending" ]; then
        test_warn "K3s test pod is pending (may still be scheduling)"
    elif [ -n "$pod_status" ]; then
        test_warn "K3s test pod in state: $pod_status"
    else
        test_warn "Could not deploy K3s test pod"
    fi
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

    # Infrastructure tests
    test_containers_running
    test_node_agent_installed
    test_identities_generated
    test_identities_unique
    test_config_files
    test_systemd_service
    test_network_connectivity
    test_logs_generated
    test_cluster_authentication
    test_authentication_logs
    test_wireguard_interface

    # SLURM test suite (skip if disabled)
    if [ "${RUN_SLURM_TESTS:-true}" = "true" ]; then
        echo ""
        echo "=========================================="
        echo "SLURM Integration Tests"
        echo "=========================================="
        test_slurm_controller_election
        test_slurm_workers
        test_munge_authentication
        test_slurm_job_submission
        test_mpi_support
        test_slurm_failover
    fi

    # K3s test suite (skip if disabled)
    if [ "${RUN_K3S_TESTS:-true}" = "true" ]; then
        echo ""
        echo "=========================================="
        echo "K3s Integration Tests"
        echo "=========================================="
        test_k3s_server_election
        test_k3s_agents
        test_k3s_pod_deployment
    fi

    # Legacy lustores deployment test
    if [ "${RUN_LUSTORES_TEST:-false}" = "true" ]; then
        test_lustores_deployment
    fi

    # Print summary and exit
    print_summary
    exit $?
}

main "$@"
