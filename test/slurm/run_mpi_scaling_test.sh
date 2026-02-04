#!/bin/bash
#SBATCH --job-name=mpi_scaling_test
#SBATCH --nodes=2
#SBATCH --ntasks=4
#SBATCH --cpus-per-task=1
#SBATCH --time=20:00
#SBATCH --output=/tmp/mpi_scaling_test_%j.out
#SBATCH --error=/tmp/mpi_scaling_test_%j.err

# SLURM MPI Scaling Test Job Script
# Tests MPI performance and scaling across multiple nodes

echo "========================================="
echo "SLURM MPI Scaling Test Job"
echo "========================================="
echo "Job ID: $SLURM_JOB_ID"
echo "Nodes: $SLURM_NODELIST"
echo "Tasks: $SLURM_NTASKS"
echo "Started at: $(date)"
echo ""

# Load MPI module if available (adjust as needed for your system)
# module load mpi/openmpi-x86_64

# Run the MPI scaling test
mpirun -np $SLURM_NTASKS python3 /home/user/ClusterOS/test/slurm/test_mpi_scaling.py

exit_code=$?
echo ""
echo "MPI scaling test completed with exit code: $exit_code"
echo "Finished at: $(date)"
exit $exit_code