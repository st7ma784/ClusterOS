#!/usr/bin/env python3
"""
Advanced MPI Scaling Test for SLURM
Tests MPI performance and correctness across different numbers of processes and nodes
"""

import sys
import time
import socket
import os

try:
    from mpi4py import MPI
except ImportError:
    print("ERROR: mpi4py not installed")
    print("Install with: pip3 install mpi4py")
    sys.exit(1)

import numpy as np


def benchmark_allreduce(comm, data_size):
    """Benchmark MPI Allreduce operation"""
    rank = comm.Get_rank()
    size = comm.Get_size()

    # Create test data
    if rank == 0:
        data = np.random.random(data_size).astype(np.float64)
    else:
        data = np.zeros(data_size, dtype=np.float64)

    # Broadcast initial data
    comm.Bcast(data, root=0)

    # Time the allreduce operation
    start_time = time.time()
    result = np.zeros_like(data)
    comm.Allreduce(data, result, op=MPI.SUM)
    end_time = time.time()

    # Verify correctness (sum should be size * original_sum)
    expected_sum = np.sum(data) * size
    actual_sum = np.sum(result)

    is_correct = abs(actual_sum - expected_sum) < 1e-10

    return end_time - start_time, is_correct, actual_sum, expected_sum


def benchmark_send_recv(comm, message_size):
    """Benchmark point-to-point communication"""
    rank = comm.Get_rank()
    size = comm.Get_size()

    if size < 2:
        return 0.0, True  # Skip if only one process

    # Create test message
    send_data = np.random.random(message_size).astype(np.float64)

    total_time = 0.0
    num_rounds = 10

    for round_num in range(num_rounds):
        if rank == 0:
            # Send to rank 1, receive from rank 1
            start_time = time.time()
            comm.Send(send_data, dest=1, tag=round_num)
            recv_data = np.zeros_like(send_data)
            comm.Recv(recv_data, source=1, tag=round_num)
            end_time = time.time()

            total_time += end_time - start_time

            # Verify data integrity
            if not np.allclose(send_data, recv_data):
                return total_time / num_rounds, False

        elif rank == 1:
            # Receive from rank 0, send back to rank 0
            recv_data = np.zeros_like(send_data)
            comm.Recv(recv_data, source=0, tag=round_num)
            comm.Send(recv_data, dest=0, tag=round_num)

    return total_time / num_rounds, True


def test_process_mapping(comm):
    """Test that processes are correctly mapped to nodes"""
    rank = comm.Get_rank()
    size = comm.Get_size()

    hostname = socket.gethostname()
    processor_name = MPI.Get_processor_name()

    # Gather hostname information from all processes
    hostnames = comm.gather(hostname, root=0)
    processor_names = comm.gather(processor_name, root=0)

    if rank == 0:
        print(f"\nProcess Mapping (Total processes: {size}):")
        print("-" * 60)

        # Count processes per node
        node_counts = {}
        for i, host in enumerate(hostnames):
            node_counts[host] = node_counts.get(host, 0) + 1

        for host, count in sorted(node_counts.items()):
            print(f"Node {host}: {count} processes")

        # Check for unique processor names (indicates different physical cores)
        unique_processors = len(set(processor_names))
        print(f"Unique processor names: {unique_processors}")

        return len(node_counts) > 0  # At least one node has processes

    return True


def run_scaling_tests(comm):
    """Run tests with different data sizes to check scaling"""
    rank = comm.Get_rank()
    size = comm.Get_size()

    if rank == 0:
        print(f"\nMPI Scaling Tests ({size} processes)")
        print("=" * 60)

    # Test different message sizes for allreduce
    data_sizes = [1, 100, 1000, 10000, 100000]

    if rank == 0:
        print("
Allreduce Benchmark:")
        print("Size\t\tTime(s)\t\tCorrectness")
        print("-" * 40)

    for data_size in data_sizes:
        time_taken, is_correct, actual, expected = benchmark_allreduce(comm, data_size)

        if rank == 0:
            status = "✓" if is_correct else "✗"
            print("8d")

        # Check that all processes agree on correctness
        all_correct = comm.allreduce(1 if is_correct else 0, op=MPI.MIN)
        if all_correct != 1:
            if rank == 0:
                print(f"❌ Correctness check failed for size {data_size}")
            return False

    # Test point-to-point communication
    if size >= 2:
        if rank == 0:
            print("
Point-to-Point Benchmark:")
            print("Size\t\tTime(s)\t\tCorrectness")
            print("-" * 40)

        message_sizes = [1, 100, 1000, 10000]

        for msg_size in message_sizes:
            time_taken, is_correct = benchmark_send_recv(comm, msg_size)

            if rank == 0:
                status = "✓" if is_correct else "✗"
                print("8d")

            # Check that root process reports success
            all_correct = comm.allreduce(1 if is_correct else 0, op=MPI.MIN)
            if all_correct != 1:
                if rank == 0:
                    print(f"❌ P2P test failed for size {msg_size}")
                return False

    return True


def main():
    comm = MPI.COMM_WORLD
    rank = comm.Get_rank()
    size = comm.Get_size()

    if rank == 0:
        print("ClusterOS Advanced MPI Scaling Test")
        print(f"Started at: {time.ctime()}")
        print(f"Total MPI processes: {size}")
        print(f"Local hostname: {socket.gethostname()}")

    # Test process mapping
    mapping_ok = test_process_mapping(comm)

    # Run scaling tests
    scaling_ok = run_scaling_tests(comm)

    # Final summary
    if rank == 0:
        print("\n" + "=" * 60)
        print("TEST SUMMARY")
        print("=" * 60)

        all_passed = mapping_ok and scaling_ok

        if all_passed:
            print("🎉 ALL MPI TESTS PASSED!")
            print(f"Successfully tested {size} MPI processes")
        else:
            print("❌ SOME TESTS FAILED")
            if not mapping_ok:
                print("- Process mapping test failed")
            if not scaling_ok:
                print("- Scaling tests failed")

        return 0 if all_passed else 1

    return 0


if __name__ == '__main__':
    sys.exit(main())