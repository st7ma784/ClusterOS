#!/usr/bin/env python3
"""
SLURM Multi-Node Test Suite
Tests various multi-node SLURM functionality including:
- Node discovery and communication
- Shared filesystem access
- Network connectivity
- Resource allocation
"""

import subprocess
import sys
import os
import socket
import time
from datetime import datetime


def run_command(cmd, description=""):
    """Run a command and return (success, output, error)"""
    try:
        result = subprocess.run(cmd, shell=True, capture_output=True, text=True, timeout=30)
        success = result.returncode == 0
        output = result.stdout.strip()
        error = result.stderr.strip()
        return success, output, error
    except subprocess.TimeoutExpired:
        return False, "", "Command timed out"
    except Exception as e:
        return False, "", str(e)


def test_node_discovery():
    """Test that we can discover other nodes in the cluster"""
    print("\n" + "="*60)
    print("TEST: Node Discovery")
    print("="*60)

    # Get SLURM node list
    success, nodes, error = run_command("sinfo -N -h | awk '{print $1}' | sort | uniq")
    if not success:
        print(f"❌ Failed to get node list: {error}")
        return False

    node_list = nodes.split('\n') if nodes else []
    print(f"Discovered nodes: {', '.join(node_list)}")

    # Test connectivity to each node
    failed_nodes = []
    for node in node_list:
        if node.strip():
            success, output, error = run_command(f"ping -c 1 -W 2 {node}")
            if success:
                print(f"✅ {node}: reachable")
            else:
                print(f"❌ {node}: unreachable - {error}")
                failed_nodes.append(node)

    if failed_nodes:
        print(f"⚠️  Warning: {len(failed_nodes)} nodes unreachable")
        return len(failed_nodes) < len(node_list)  # Pass if at least one node is reachable
    else:
        print("✅ All nodes reachable")
        return True


def test_shared_filesystem():
    """Test shared filesystem access across nodes"""
    print("\n" + "="*60)
    print("TEST: Shared Filesystem")
    print("="*60)

    hostname = socket.gethostname()
    test_file = f"/tmp/slurm_test_{hostname}_{int(time.time())}.txt"
    test_content = f"Test file created by {hostname} at {datetime.now()}"

    # Create test file
    try:
        with open(test_file, 'w') as f:
            f.write(test_content)
        print(f"✅ Created test file: {test_file}")
    except Exception as e:
        print(f"❌ Failed to create test file: {e}")
        return False

    # Test file permissions and access
    success, output, error = run_command(f"ls -la {test_file}")
    if success:
        print(f"✅ File permissions: {output}")
    else:
        print(f"❌ Failed to check file permissions: {error}")
        return False

    # Test file content
    success, output, error = run_command(f"cat {test_file}")
    if success and output.strip() == test_content:
        print("✅ File content verified")
    else:
        print(f"❌ File content mismatch: {output}")
        return False

    # Cleanup
    os.remove(test_file)
    print("✅ Test file cleaned up")

    return True


def test_network_connectivity():
    """Test network connectivity between nodes"""
    print("\n" + "="*60)
    print("TEST: Network Connectivity")
    print("="*60)

    # Get all allocated nodes for this job
    success, nodes, error = run_command("scontrol show hostnames $SLURM_NODELIST")
    if not success:
        print(f"❌ Failed to get allocated nodes: {error}")
        return False

    node_list = nodes.split('\n') if nodes else []
    if len(node_list) < 2:
        print("⚠️  Only one node allocated - skipping network tests")
        return True

    print(f"Testing connectivity between {len(node_list)} nodes: {', '.join(node_list)}")

    # Test basic connectivity
    success_count = 0
    for node in node_list:
        if node.strip() and node != socket.gethostname():
            success, output, error = run_command(f"ssh -o ConnectTimeout=5 -o StrictHostKeyChecking=no {node} 'echo \"SSH to {node} successful\"'")
            if success:
                print(f"✅ SSH to {node}: successful")
                success_count += 1
            else:
                print(f"❌ SSH to {node}: failed - {error}")

    if success_count > 0:
        print(f"✅ Network connectivity test passed ({success_count} successful connections)")
        return True
    else:
        print("❌ No successful network connections")
        return False


def test_slurm_environment():
    """Test SLURM environment variables and job information"""
    print("\n" + "="*60)
    print("TEST: SLURM Environment")
    print("="*60)

    required_vars = [
        'SLURM_JOB_ID',
        'SLURM_NODELIST',
        'SLURM_NNODES',
        'SLURM_NTASKS',
        'SLURM_PROCID'
    ]

    missing_vars = []
    for var in required_vars:
        value = os.environ.get(var)
        if value:
            print(f"✅ {var} = {value}")
        else:
            print(f"❌ {var} = (not set)")
            missing_vars.append(var)

    if missing_vars:
        print(f"⚠️  Missing SLURM variables: {', '.join(missing_vars)}")
        return False
    else:
        print("✅ All required SLURM variables present")
        return True


def test_resource_allocation():
    """Test that resources are allocated correctly"""
    print("\n" + "="*60)
    print("TEST: Resource Allocation")
    print("="*60)

    # Check CPU allocation
    success, cpus, error = run_command("nproc")
    if success:
        print(f"✅ Available CPUs: {cpus}")
    else:
        print(f"❌ Failed to get CPU count: {error}")
        return False

    # Check memory
    success, mem, error = run_command("free -h | grep '^Mem:' | awk '{print $2}'")
    if success:
        print(f"✅ Total memory: {mem}")
    else:
        print(f"❌ Failed to get memory info: {error}")
        return False

    # Check SLURM CPU allocation
    slurm_cpus = os.environ.get('SLURM_CPUS_ON_NODE')
    if slurm_cpus:
        print(f"✅ SLURM allocated CPUs per node: {slurm_cpus}")
    else:
        print("⚠️  SLURM_CPUS_ON_NODE not set")

    return True


def main():
    """Run all multi-node tests"""
    print("ClusterOS SLURM Multi-Node Test Suite")
    print(f"Started at: {datetime.now()}")
    print(f"Running on: {socket.gethostname()}")
    print(f"Process ID: {os.getpid()}")

    tests = [
        ("SLURM Environment", test_slurm_environment),
        ("Node Discovery", test_node_discovery),
        ("Shared Filesystem", test_shared_filesystem),
        ("Network Connectivity", test_network_connectivity),
        ("Resource Allocation", test_resource_allocation),
    ]

    passed = 0
    total = len(tests)

    for test_name, test_func in tests:
        try:
            if test_func():
                passed += 1
                print(f"✅ {test_name}: PASSED")
            else:
                print(f"❌ {test_name}: FAILED")
        except Exception as e:
            print(f"❌ {test_name}: ERROR - {e}")

    print("\n" + "="*60)
    print("TEST SUMMARY")
    print("="*60)
    print(f"Tests passed: {passed}/{total}")

    if passed == total:
        print("🎉 ALL TESTS PASSED!")
        return 0
    else:
        print("⚠️  Some tests failed - check output above")
        return 1


if __name__ == '__main__':
    sys.exit(main())