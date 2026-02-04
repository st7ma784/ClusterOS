#!/bin/bash
#SBATCH --job-name=job_dependencies_test
#SBATCH --nodes=1
#SBATCH --ntasks=1
#SBATCH --cpus-per-task=1
#SBATCH --time=30:00
#SBATCH --output=/tmp/job_dependencies_test_%j.out
#SBATCH --error=/tmp/job_dependencies_test_%j.err

# SLURM Job Dependencies Test Script
# Tests job dependency functionality

echo "========================================="
echo "SLURM Job Dependencies Test"
echo "========================================="
echo "Job ID: $SLURM_JOB_ID"
echo "Node: $SLURMD_NODENAME"
echo "Started at: $(date)"
echo ""

# Run the job dependencies test (this will submit additional jobs)
python3 /home/user/ClusterOS/test/slurm/test_job_dependencies.py

exit_code=$?
echo ""
echo "Job dependencies test completed with exit code: $exit_code"
echo "Finished at: $(date)"
exit $exit_code