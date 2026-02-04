#!/bin/bash
# Quick SLURM Test Example
# Demonstrates basic SLURM functionality

echo "ClusterOS SLURM Quick Test"
echo "=========================="
echo "Testing basic SLURM functionality..."
echo ""

# Test 1: Check SLURM status
echo "1. Checking SLURM cluster status:"
sinfo
echo ""

# Test 2: Check queue status
echo "2. Checking job queue:"
squeue
echo ""

# Test 3: Submit a simple job
echo "3. Submitting a simple test job..."
JOB_SCRIPT=$(cat << 'EOF'
#!/bin/bash
#SBATCH --job-name=quick_test
#SBATCH --output=/tmp/quick_test_%j.out
#SBATCH --error=/tmp/quick_test_%j.err
#SBATCH --time=1:00

echo "Quick SLURM test running on $(hostname)"
echo "Job ID: $SLURM_JOB_ID"
echo "Node list: $SLURM_NODELIST"
echo "Number of tasks: $SLURM_NTASKS"
echo "Test completed successfully at $(date)"
EOF
)

# Write and submit job
echo "$JOB_SCRIPT" > /tmp/quick_test.sh
chmod +x /tmp/quick_test.sh
sbatch /tmp/quick_test.sh

echo ""
echo "Job submitted! Check status with: squeue"
echo "View results in: /tmp/quick_test_<job_id>.out"
echo ""
echo "For comprehensive testing, run:"
echo "  cd /home/user/ClusterOS/test/slurm"
echo "  ./run_all_tests.sh"