#!/usr/bin/env python3
"""
Test Python multiprocessing on SLURM cluster
This tests that Python's multiprocessing library works correctly across SLURM
"""

from multiprocessing import Pool, cpu_count
import socket
import os
import sys


def worker_task(x):
    """
    Simple worker task that returns host and computation result
    """
    hostname = socket.gethostname()
    pid = os.getpid()
    result = x * x
    return f"Host: {hostname}, PID: {pid}, Input: {x}, Result: {result}"


def main():
    print("=" * 60)
    print("Python Multiprocessing Test on SLURM")
    print("=" * 60)

    # Get available CPUs (SLURM sets this appropriately)
    num_cpus = cpu_count()
    print(f"Available CPUs: {num_cpus}")
    print(f"Running on host: {socket.gethostname()}")
    print(f"Main process PID: {os.getpid()}")

    # Create input data
    data = list(range(1, 17))  # 16 tasks
    print(f"\nProcessing {len(data)} tasks with {num_cpus} processes...")

    # Process with multiprocessing pool
    with Pool(processes=num_cpus) as pool:
        results = pool.map(worker_task, data)

    # Display results
    print("\nResults:")
    print("-" * 60)
    for result in results:
        print(result)

    print("-" * 60)
    print(f"✓ Successfully processed {len(results)} tasks")
    print("=" * 60)

    return 0


if __name__ == '__main__':
    sys.exit(main())
