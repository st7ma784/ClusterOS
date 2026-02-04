#!/bin/bash
# ClusterOS SLURM Test Suite Runner
# Runs all SLURM tests to verify multi-node functionality

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOG_FILE="/tmp/slurm_test_suite_$(date +%Y%m%d_%H%M%S).log"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() { echo -e "${BLUE}[INFO]${NC} $1" | tee -a "$LOG_FILE"; }
log_success() { echo -e "${GREEN}[SUCCESS]${NC} $1" | tee -a "$LOG_FILE"; }
log_warning() { echo -e "${YELLOW}[WARNING]${NC} $1" | tee -a "$LOG_FILE"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1" | tee -a "$LOG_FILE"; }

# Function to submit and wait for a job
submit_and_wait() {
    local script_path="$1"
    local job_name="$2"
    local timeout="${3:-300}"  # Default 5 minutes

    log_info "Submitting $job_name test..."

    # Submit the job
    local submit_output
    submit_output=$(sbatch "$script_path" 2>&1)
    local submit_exit=$?

    if [ $submit_exit -ne 0 ]; then
        log_error "Failed to submit $job_name job: $submit_output"
        return 1
    fi

    # Extract job ID
    local job_id
    job_id=$(echo "$submit_output" | grep -oP 'Submitted batch job \K\d+')
    if [ -z "$job_id" ]; then
        log_error "Could not extract job ID from: $submit_output"
        return 1
    fi

    log_info "$job_name job submitted with ID: $job_id"

    # Wait for job completion
    local start_time=$(date +%s)
    while true; do
        local current_time=$(date +%s)
        local elapsed=$((current_time - start_time))

        if [ $elapsed -gt $timeout ]; then
            log_error "$job_name job timed out after ${timeout}s"
            scancel "$job_id" 2>/dev/null || true
            return 1
        fi

        # Check if job is still running
        if ! squeue -h -j "$job_id" >/dev/null 2>&1; then
            # Job finished, check exit status
            local job_status
            job_status=$(sacct -j "$job_id" --format=State,ExitCode --noheader --parsable2 | head -1)

            if echo "$job_status" | grep -q "COMPLETED.*0:0"; then
                log_success "$job_name test passed"
                return 0
            else
                log_error "$job_name test failed with status: $job_status"
                return 1
            fi
        fi

        sleep 5
    done
}

# Pre-flight checks
preflight_checks() {
    log_info "Running pre-flight checks..."

    # Check if SLURM is available
    if ! command -v sbatch >/dev/null 2>&1; then
        log_error "SLURM sbatch command not found"
        return 1
    fi

    if ! command -v squeue >/dev/null 2>&1; then
        log_error "SLURM squeue command not found"
        return 1
    fi

    # Check if we have nodes available
    local node_count
    node_count=$(sinfo -h -o "%D" | awk '{sum += $1} END {print sum}')
    if [ "$node_count" -lt 2 ]; then
        log_warning "Only $node_count nodes available - some tests may be limited"
    else
        log_info "Found $node_count nodes available"
    fi

    # Check if MPI is available
    if ! command -v mpirun >/dev/null 2>&1; then
        log_warning "MPI (mpirun) not found - MPI tests will likely fail"
    fi

    # Check if Python MPI support is available
    if ! python3 -c "import mpi4py" >/dev/null 2>&1; then
        log_warning "mpi4py not available - MPI tests will fail"
    fi

    log_success "Pre-flight checks completed"
    return 0
}

# Main test execution
main() {
    echo "=========================================" | tee "$LOG_FILE"
    echo "ClusterOS SLURM Test Suite" | tee -a "$LOG_FILE"
    echo "=========================================" | tee -a "$LOG_FILE"
    echo "Started at: $(date)" | tee -a "$LOG_FILE"
    echo "Log file: $LOG_FILE" | tee -a "$LOG_FILE"
    echo "" | tee -a "$LOG_FILE"

    local passed=0
    local total=0

    # Pre-flight checks
    if ! preflight_checks; then
        log_error "Pre-flight checks failed"
        exit 1
    fi

    # Test 1: Multi-node functionality
    total=$((total + 1))
    if submit_and_wait "$SCRIPT_DIR/run_multi_node_test.sh" "Multi-Node" 600; then
        passed=$((passed + 1))
    fi

    # Test 2: Basic MPI
    total=$((total + 1))
    if submit_and_wait "$SCRIPT_DIR/run_mpi_test.sh" "MPI Basic" 900; then
        passed=$((passed + 1))
    fi

    # Test 3: MPI Scaling
    total=$((total + 1))
    if submit_and_wait "$SCRIPT_DIR/run_mpi_scaling_test.sh" "MPI Scaling" 1200; then
        passed=$((passed + 1))
    fi

    # Test 4: Job Arrays
    total=$((total + 1))
    if submit_and_wait "$SCRIPT_DIR/run_job_array_test.sh" "Job Array" 1800; then
        passed=$((passed + 1))
    fi

    # Test 5: Job Dependencies
    total=$((total + 1))
    if submit_and_wait "$SCRIPT_DIR/run_job_dependencies_test.sh" "Job Dependencies" 900; then
        passed=$((passed + 1))
    fi

    # Summary
    echo "" | tee -a "$LOG_FILE"
    echo "=========================================" | tee -a "$LOG_FILE"
    echo "TEST SUITE SUMMARY" | tee -a "$LOG_FILE"
    echo "=========================================" | tee -a "$LOG_FILE"
    echo "Tests passed: $passed/$total" | tee -a "$LOG_FILE"

    if [ $passed -eq $total ]; then
        log_success "🎉 ALL TESTS PASSED!"
        echo "Your SLURM cluster is working correctly!" | tee -a "$LOG_FILE"
        exit 0
    else
        log_error "❌ SOME TESTS FAILED"
        echo "Check the log file for details: $LOG_FILE" | tee -a "$LOG_FILE"
        exit 1
    fi
}

# Run main function
main "$@"