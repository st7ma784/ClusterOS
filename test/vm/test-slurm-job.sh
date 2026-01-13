#!/bin/bash
set -e

# Test SLURM job submission and execution
# Run this script from a node in the cluster

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_test() { echo -e "${BLUE}[TEST]${NC} $1"; }
log_pass() { echo -e "${GREEN}[PASS]${NC} $1"; }
log_fail() { echo -e "${RED}[FAIL]${NC} $1"; }
log_info() { echo -e "${BLUE}[INFO]${NC} $1"; }

# Helper for SSH to node
ssh_node() {
    local node_num=$1
    shift
    local port=$((2222 + node_num))
    ssh -p "$port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -q clusteros@localhost "$@"
}

echo "========================================="
echo "SLURM Job Submission Tests"
echo "========================================="
echo ""

# Check SLURM controller status
log_test "Checking SLURM controller status..."
if ssh_node 1 "sudo sinfo" 2>/dev/null; then
    log_pass "SLURM controller is responding"
else
    log_fail "SLURM controller is not responding"
    echo ""
    echo "Checking slurmctld status..."
    ssh_node 1 "sudo systemctl status slurmctld 2>/dev/null || sudo journalctl -u slurmctld -n 20" || true
    echo ""
    echo "Checking slurmd status..."
    ssh_node 1 "sudo systemctl status slurmd 2>/dev/null || sudo journalctl -u slurmd -n 20" || true
    exit 1
fi

echo ""

# Check node status
log_test "Checking SLURM node status..."
ssh_node 1 "sudo sinfo -N -l" 2>/dev/null || log_fail "Could not get node info"

echo ""

# Submit a simple test job
log_test "Submitting simple test job..."

JOB_SCRIPT=$(cat <<'EOF'
#!/bin/bash
#SBATCH --job-name=test-job
#SBATCH --output=/tmp/test-job-%j.out
#SBATCH --ntasks=1
#SBATCH --time=00:01:00

echo "Hello from SLURM job!"
echo "Running on host: $(hostname)"
echo "Job ID: $SLURM_JOB_ID"
echo "Task ID: $SLURM_PROCID"
echo "Date: $(date)"
echo ""
echo "System info:"
uname -a
echo ""
echo "CPU info:"
nproc
echo ""
echo "Test completed successfully!"
EOF
)

# Create job script on node1
ssh_node 1 "echo '$JOB_SCRIPT' > /tmp/test-job.sh && chmod +x /tmp/test-job.sh"

# Submit job
JOB_ID=$(ssh_node 1 "sudo sbatch /tmp/test-job.sh 2>/dev/null | grep -oP 'Submitted batch job \K\d+'")

if [ -n "$JOB_ID" ]; then
    log_pass "Job submitted with ID: $JOB_ID"
else
    log_fail "Failed to submit job"
    exit 1
fi

echo ""

# Wait for job to complete
log_test "Waiting for job to complete..."
MAX_WAIT=60
WAIT_TIME=0
while [ $WAIT_TIME -lt $MAX_WAIT ]; do
    JOB_STATE=$(ssh_node 1 "sudo squeue -j $JOB_ID -h -o %T 2>/dev/null" || echo "COMPLETED")
    
    if [ "$JOB_STATE" = "" ] || [ "$JOB_STATE" = "COMPLETED" ]; then
        break
    fi
    
    echo "  Job state: $JOB_STATE (waiting...)"
    sleep 2
    WAIT_TIME=$((WAIT_TIME + 2))
done

# Check job output
log_test "Checking job output..."
OUTPUT_FILE="/tmp/test-job-${JOB_ID}.out"

if ssh_node 1 "sudo test -f $OUTPUT_FILE" 2>/dev/null; then
    log_pass "Job output file exists"
    echo ""
    echo "Job output:"
    echo "-------------------------------------------"
    ssh_node 1 "sudo cat $OUTPUT_FILE" 2>/dev/null
    echo "-------------------------------------------"
    
    if ssh_node 1 "sudo grep -q 'Test completed successfully' $OUTPUT_FILE" 2>/dev/null; then
        log_pass "Job completed successfully!"
    else
        log_fail "Job did not complete successfully"
    fi
else
    log_fail "Job output file not found"
fi

echo ""

# Submit a multi-node test job
log_test "Submitting multi-node test job..."

MULTI_JOB_SCRIPT=$(cat <<'EOF'
#!/bin/bash
#SBATCH --job-name=multi-node-test
#SBATCH --output=/tmp/multi-node-test-%j.out
#SBATCH --nodes=2
#SBATCH --ntasks-per-node=1
#SBATCH --time=00:01:00

echo "Multi-node SLURM test"
echo "====================="
echo "Running on host: $(hostname)"
echo "SLURM_JOB_NODELIST: $SLURM_JOB_NODELIST"
echo "SLURM_NNODES: $SLURM_NNODES"
echo ""

# Use srun to run on all nodes
srun hostname

echo ""
echo "Multi-node test completed!"
EOF
)

ssh_node 1 "echo '$MULTI_JOB_SCRIPT' > /tmp/multi-node-test.sh && chmod +x /tmp/multi-node-test.sh"

MULTI_JOB_ID=$(ssh_node 1 "sudo sbatch /tmp/multi-node-test.sh 2>/dev/null | grep -oP 'Submitted batch job \K\d+'" || echo "")

if [ -n "$MULTI_JOB_ID" ]; then
    log_pass "Multi-node job submitted with ID: $MULTI_JOB_ID"
    
    # Wait for completion
    sleep 5
    
    MULTI_OUTPUT="/tmp/multi-node-test-${MULTI_JOB_ID}.out"
    if ssh_node 1 "sudo test -f $MULTI_OUTPUT" 2>/dev/null; then
        echo ""
        echo "Multi-node job output:"
        echo "-------------------------------------------"
        ssh_node 1 "sudo cat $MULTI_OUTPUT" 2>/dev/null
        echo "-------------------------------------------"
    fi
else
    log_info "Multi-node job not submitted (might not have enough nodes)"
fi

echo ""
echo "========================================="
echo "SLURM Tests Complete!"
echo "========================================="
