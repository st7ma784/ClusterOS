#!/bin/bash
#SBATCH --job-name=multi_node_test
#SBATCH --nodes=2
#SBATCH --ntasks=4
#SBATCH --cpus-per-task=1
#SBATCH --time=10:00
#SBATCH --output=/tmp/multi_node_test_%j.out
#SBATCH --error=/tmp/multi_node_test_%j.err

# SLURM Multi-Node Test Job Script
# Tests basic multi-node functionality

echo "========================================="
echo "SLURM Multi-Node Test Job"
echo "========================================="
echo "Job ID: $SLURM_JOB_ID"
echo "Nodes: $SLURM_NODELIST"
echo "Tasks: $SLURM_NTASKS"
echo "Started at: $(date)"
echo ""

# Run the multi-node test
python3 /home/user/ClusterOS/test/slurm/test_multi_node.py

exit_code=$?
echo ""
echo "Job completed with exit code: $exit_code"
echo "Finished at: $(date)"
exit $exit_code