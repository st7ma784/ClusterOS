#!/usr/bin/env python3
"""
Test MPI on SLURM cluster using mpi4py
This tests that MPI works correctly across multiple nodes
"""

import sys

try:
    from mpi4py import MPI
except ImportError:
    print("ERROR: mpi4py not installed")
    print("Install with: pip3 install mpi4py")
    sys.exit(1)

import socket
import os


def main():
    # Initialize MPI
    comm = MPI.COMM_WORLD
    rank = comm.Get_rank()
    size = comm.Get_size()
    processor_name = MPI.Get_processor_name()

    # Get host information
    hostname = socket.gethostname()
    pid = os.getpid()

    # Print from each rank
    print(f"Rank {rank}/{size}: Running on {processor_name} (hostname: {hostname}, PID: {pid})")

    # Barrier to ensure all ranks have printed
    comm.Barrier()

    # Root process summarizes
    if rank == 0:
        print("=" * 60)
        print(f"MPI Test Complete!")
        print(f"Total MPI processes: {size}")
        print("=" * 60)

        # Test send/receive
        if size > 1:
            print("\nTesting MPI send/receive...")
            data_to_send = {"message": "Hello from rank 0", "value": 42}
            comm.send(data_to_send, dest=1, tag=11)
            print(f"Rank 0: Sent data to rank 1: {data_to_send}")

    elif rank == 1:
        # Receive data from rank 0
        data_received = comm.recv(source=0, tag=11)
        print(f"Rank 1: Received data from rank 0: {data_received}")

    # Barrier before collective operations
    comm.Barrier()

    # Test collective operations (gather)
    if rank == 0:
        print("\nTesting MPI collective operations (gather)...")

    # Each rank contributes its rank number
    local_data = rank * rank
    gathered_data = comm.gather(local_data, root=0)

    if rank == 0:
        print(f"Rank 0: Gathered data from all ranks: {gathered_data}")
        print("=" * 60)
        print("✓ MPI test successful!")
        print("=" * 60)

    return 0


if __name__ == '__main__':
    sys.exit(main())
