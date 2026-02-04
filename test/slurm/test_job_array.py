#!/usr/bin/env python3
"""
SLURM Job Array Test
Tests SLURM job arrays for running multiple independent tasks
"""

import sys
import os
import socket
import time
import random
from datetime import datetime


def simulate_workload(task_id, duration_seconds):
    """Simulate some computational work"""
    hostname = socket.gethostname()
    start_time = time.time()

    print(f"Task {task_id}: Starting on {hostname} at {datetime.now()}")

    # Simulate work with random CPU usage
    work_iterations = duration_seconds * 1000000
    result = 0

    for i in range(work_iterations):
        result += random.random() * random.random()

    end_time = time.time()
    actual_duration = end_time - start_time

    print(".2f"
    return result, actual_duration


def main():
    # Get SLURM array task ID
    task_id = os.environ.get('SLURM_ARRAY_TASK_ID')
    if not task_id:
        print("ERROR: This script must be run as a SLURM job array")
        print("Use: sbatch --array=1-10 test_job_array.sh")
        sys.exit(1)

    task_id = int(task_id)

    print("=" * 60)
    print(f"SLURM Job Array Test - Task {task_id}")
    print("=" * 60)

    # Get other SLURM environment variables
    job_id = os.environ.get('SLURM_JOB_ID', 'unknown')
    array_job_id = os.environ.get('SLURM_ARRAY_JOB_ID', 'unknown')
    node_list = os.environ.get('SLURM_NODELIST', 'unknown')

    print(f"Job ID: {job_id}")
    print(f"Array Job ID: {array_job_id}")
    print(f"Task ID: {task_id}")
    print(f"Running on node: {socket.gethostname()}")
    print(f"Allocated nodes: {node_list}")

    # Simulate different amounts of work for each task
    # Tasks 1-3: short work, 4-7: medium work, 8-10: long work
    if task_id <= 3:
        duration = 5 + random.uniform(0, 2)  # 5-7 seconds
    elif task_id <= 7:
        duration = 15 + random.uniform(0, 5)  # 15-20 seconds
    else:
        duration = 30 + random.uniform(0, 10)  # 30-40 seconds

    print(f"Planned work duration: {duration:.1f} seconds")

    # Do the work
    result, actual_duration = simulate_workload(task_id, int(duration))

    # Verify we can write to shared filesystem
    output_file = f"/tmp/job_array_task_{task_id}.out"
    try:
        with open(output_file, 'w') as f:
            f.write(f"Task {task_id} completed successfully\n")
            f.write(f"Result: {result:.6f}\n")
            f.write(f"Duration: {actual_duration:.2f} seconds\n")
            f.write(f"Completed at: {datetime.now()}\n")
        print(f"✅ Output written to: {output_file}")
    except Exception as e:
        print(f"❌ Failed to write output file: {e}")
        return 1

    print("=" * 60)
    print(f"✓ Task {task_id} completed successfully!")
    print("=" * 60)

    return 0


if __name__ == '__main__':
    sys.exit(main())