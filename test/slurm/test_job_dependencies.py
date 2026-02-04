#!/usr/bin/env python3
"""
SLURM Job Dependencies Test
Tests SLURM job dependency functionality
"""

import sys
import os
import socket
import time
import subprocess
from datetime import datetime


def run_command(cmd, description=""):
    """Run a command and return (success, output, error)"""
    try:
        result = subprocess.run(cmd, shell=True, capture_output=True, text=True, timeout=60)
        return result.returncode == 0, result.stdout.strip(), result.stderr.strip()
    except subprocess.TimeoutExpired:
        return False, "", "Command timed out"
    except Exception as e:
        return False, "", str(e)


def submit_dependent_jobs():
    """Submit a chain of dependent jobs"""
    hostname = socket.gethostname()

    print("Submitting chain of dependent jobs...")

    # Job 1: Simple data preparation
    job1_script = f"""#!/bin/bash
#SBATCH --job-name=dep_test_1
#SBATCH --output=/tmp/dep_test_1_%j.out
#SBATCH --error=/tmp/dep_test_1_%j.err

echo "Job 1: Data preparation started on {hostname}"
date
echo "Creating test data..."
echo "test_data_123" > /tmp/dep_test_data.txt
echo "Job 1 completed successfully"
"""

    # Job 2: Depends on Job 1, processes the data
    job2_script = f"""#!/bin/bash
#SBATCH --job-name=dep_test_2
#SBATCH --output=/tmp/dep_test_2_%j.out
#SBATCH --error=/tmp/dep_test_2_%j.err
#SBATCH --dependency=afterok:$JOB1_ID

echo "Job 2: Data processing started on {hostname}"
date
echo "Reading test data..."
if [ -f /tmp/dep_test_data.txt ]; then
    data=$(cat /tmp/dep_test_data.txt)
    echo "Data read: $data"
    echo "processed_$data" > /tmp/dep_test_processed.txt
    echo "Job 2 completed successfully"
else
    echo "ERROR: Test data not found!"
    exit 1
fi
"""

    # Job 3: Depends on Job 2, analyzes results
    job3_script = f"""#!/bin/bash
#SBATCH --job-name=dep_test_3
#SBATCH --output=/tmp/dep_test_3_%j.out
#SBATCH --error=/tmp/dep_test_3_%j.err
#SBATCH --dependency=afterok:$JOB2_ID

echo "Job 3: Analysis started on {hostname}"
date
echo "Analyzing processed data..."
if [ -f /tmp/dep_test_processed.txt ]; then
    data=$(cat /tmp/dep_test_processed.txt)
    echo "Analysis result: $data"
    echo "Final result: analysis_complete" > /tmp/dep_test_final.txt
    echo "Job 3 completed successfully"
else
    echo "ERROR: Processed data not found!"
    exit 1
fi
"""

    # Submit Job 1
    with open('/tmp/job1.sh', 'w') as f:
        f.write(job1_script)
    os.chmod('/tmp/job1.sh', 0o755)

    success, output, error = run_command("sbatch /tmp/job1.sh")
    if not success:
        print(f"❌ Failed to submit Job 1: {error}")
        return None, None, None

    job1_id = output.split()[-1]  # Extract job ID from "Submitted batch job 12345"
    print(f"✅ Submitted Job 1 with ID: {job1_id}")

    # Submit Job 2 with dependency on Job 1
    job2_script = job2_script.replace('$JOB1_ID', job1_id)
    with open('/tmp/job2.sh', 'w') as f:
        f.write(job2_script)
    os.chmod('/tmp/job2.sh', 0o755)

    success, output, error = run_command("sbatch /tmp/job2.sh")
    if not success:
        print(f"❌ Failed to submit Job 2: {error}")
        return job1_id, None, None

    job2_id = output.split()[-1]
    print(f"✅ Submitted Job 2 with ID: {job2_id} (depends on {job1_id})")

    # Submit Job 3 with dependency on Job 2
    job3_script = job3_script.replace('$JOB2_ID', job2_id)
    with open('/tmp/job3.sh', 'w') as f:
        f.write(job3_script)
    os.chmod('/tmp/job3.sh', 0o755)

    success, output, error = run_command("sbatch /tmp/job3.sh")
    if not success:
        print(f"❌ Failed to submit Job 3: {error}")
        return job1_id, job2_id, None

    job3_id = output.split()[-1]
    print(f"✅ Submitted Job 3 with ID: {job3_id} (depends on {job2_id})")

    return job1_id, job2_id, job3_id


def wait_for_jobs(job_ids):
    """Wait for all jobs to complete and check their status"""
    print(f"\nWaiting for jobs to complete: {', '.join(job_ids)}")

    max_wait_time = 300  # 5 minutes
    start_time = time.time()

    while time.time() - start_time < max_wait_time:
        all_completed = True
        completed_jobs = []
        failed_jobs = []

        for job_id in job_ids:
            success, output, error = run_command(f"squeue -h -j {job_id}")
            if output.strip():  # Job still in queue
                all_completed = False
            else:
                # Check job status
                success, output, error = run_command(f"sacct -j {job_id} --format=State --noheader --parsable2")
                if success and output.strip():
                    state = output.strip().split('\n')[0]
                    if 'COMPLETED' in state:
                        completed_jobs.append(job_id)
                    elif 'FAILED' in state or 'CANCELLED' in state:
                        failed_jobs.append(job_id)
                    else:
                        all_completed = False  # Still running or other state
                else:
                    all_completed = False

        if all_completed:
            break

        print(f"Waiting... ({int(time.time() - start_time)}s elapsed)")
        time.sleep(5)

    return completed_jobs, failed_jobs


def verify_results():
    """Verify that the dependent jobs produced the expected results"""
    print("\nVerifying job results...")

    # Check for expected output files
    expected_files = [
        '/tmp/dep_test_data.txt',
        '/tmp/dep_test_processed.txt',
        '/tmp/dep_test_final.txt'
    ]

    for file_path in expected_files:
        if os.path.exists(file_path):
            with open(file_path, 'r') as f:
                content = f.read().strip()
            print(f"✅ {file_path}: {content}")
        else:
            print(f"❌ {file_path}: missing")
            return False

    # Verify content
    try:
        with open('/tmp/dep_test_data.txt', 'r') as f:
            if 'test_data_123' not in f.read():
                print("❌ Data file content incorrect")
                return False

        with open('/tmp/dep_test_processed.txt', 'r') as f:
            if 'processed_test_data_123' not in f.read():
                print("❌ Processed file content incorrect")
                return False

        with open('/tmp/dep_test_final.txt', 'r') as f:
            if 'analysis_complete' not in f.read():
                print("❌ Final file content incorrect")
                return False

    except Exception as e:
        print(f"❌ Error reading result files: {e}")
        return False

    return True


def main():
    print("ClusterOS SLURM Job Dependencies Test")
    print(f"Started at: {datetime.now()}")
    print(f"Running on: {socket.gethostname()}")

    # Submit dependent jobs
    job1_id, job2_id, job3_id = submit_dependent_jobs()

    if not all([job1_id, job2_id, job3_id]):
        print("❌ Failed to submit all dependent jobs")
        return 1

    # Wait for completion
    job_ids = [job1_id, job2_id, job3_id]
    completed, failed = wait_for_jobs(job_ids)

    print("
Job completion status:")
    print(f"Completed: {', '.join(completed)}")
    print(f"Failed: {', '.join(failed)}")

    if len(completed) != 3 or failed:
        print("❌ Not all jobs completed successfully")
        return 1

    # Verify results
    if not verify_results():
        print("❌ Job results verification failed")
        return 1

    print("\n" + "=" * 60)
    print("🎉 JOB DEPENDENCIES TEST PASSED!")
    print("All dependent jobs executed in correct order")
    print("=" * 60)

    return 0


if __name__ == '__main__':
    sys.exit(main())