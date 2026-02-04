#!/bin/bash
#SBATCH --job-name=job_array_test
#SBATCH --array=1-10
#SBATCH --nodes=1
#SBATCH --ntasks=1
#SBATCH --cpus-per-task=1
#SBATCH --time=45:00
#SBATCH --output=/tmp/job_array_test_%A_%a.out
#SBATCH --error=/tmp/job_array_test_%A_%a.err

# SLURM Job Array Test Script
# Runs multiple independent tasks as a job array

echo "========================================="
echo "SLURM Job Array Test"
echo "========================================="
echo "Array Job ID: $SLURM_ARRAY_JOB_ID"
echo "Task ID: $SLURM_ARRAY_TASK_ID"
echo "Node: $SLURMD_NODENAME"
echo "Started at: $(date)"
echo ""

# Run the job array task
python3 /home/user/ClusterOS/test/slurm/test_job_array.py

exit_code=$?
echo ""
echo "Task $SLURM_ARRAY_TASK_ID completed with exit code: $exit_code"
echo "Finished at: $(date)"
exit $exit_code