# ClusterOS SLURM Test Suite

This directory contains comprehensive tests for verifying SLURM multi-node functionality, MPI communication, and job management features.

## Test Files

### Python Test Scripts
- `test_mpi.py` - Basic MPI communication test (existing)
- `test_multiprocessing.py` - Python multiprocessing test (existing)
- `test_multi_node.py` - Comprehensive multi-node functionality test
- `test_mpi_scaling.py` - Advanced MPI scaling and performance test
- `test_job_array.py` - SLURM job array functionality test
- `test_job_dependencies.py` - SLURM job dependency chain test

### SLURM Batch Scripts
- `run_multi_node_test.sh` - Submits multi-node functionality test
- `run_mpi_test.sh` - Submits basic MPI test
- `run_mpi_scaling_test.sh` - Submits MPI scaling test
- `run_job_array_test.sh` - Submits job array test (10 tasks)
- `run_job_dependencies_test.sh` - Submits job dependency test
- `run_all_tests.sh` - Runs the complete test suite

## Quick Start

### Run All Tests
```bash
cd /home/user/ClusterOS/test/slurm
./run_all_tests.sh
```

This will automatically submit all tests in sequence and wait for completion.

### Run Individual Tests

#### Multi-Node Test
```bash
sbatch run_multi_node_test.sh
```
Tests node discovery, shared filesystem, network connectivity, and resource allocation across 2 nodes with 4 tasks.

#### MPI Tests
```bash
sbatch run_mpi_test.sh
```
Tests basic MPI communication across multiple nodes.

```bash
sbatch run_mpi_scaling_test.sh
```
Tests MPI performance scaling with different message sizes and collective operations.

#### Job Management Tests
```bash
sbatch run_job_array_test.sh
```
Runs 10 independent tasks as a job array, each with different work durations.

```bash
sbatch run_job_dependencies_test.sh
```
Tests job dependencies by creating a chain of 3 jobs where each depends on the previous one.

## Test Descriptions

### Multi-Node Test (`test_multi_node.py`)
- **SLURM Environment**: Verifies all required SLURM variables are set
- **Node Discovery**: Tests connectivity to all cluster nodes
- **Shared Filesystem**: Verifies file access across nodes
- **Network Connectivity**: Tests SSH connectivity between allocated nodes
- **Resource Allocation**: Checks CPU and memory allocation

### MPI Scaling Test (`test_mpi_scaling.py`)
- **Process Mapping**: Verifies processes are correctly distributed across nodes
- **Allreduce Benchmark**: Tests collective operations with different data sizes
- **Point-to-Point Communication**: Benchmarks send/receive operations
- **Correctness Verification**: Ensures all operations produce correct results

### Job Array Test (`test_job_array.py`)
- **Task Execution**: Runs multiple independent tasks
- **Variable Workloads**: Different tasks have different execution times
- **Output Management**: Each task writes to its own output file
- **Resource Usage**: Tests SLURM's ability to manage multiple concurrent tasks

### Job Dependencies Test (`test_job_dependencies.py`)
- **Dependency Chain**: Creates jobs that depend on each other
- **Data Flow**: Tests passing data between dependent jobs via files
- **Error Handling**: Verifies that failed dependencies prevent execution
- **Result Verification**: Checks that the final result is correct

## Prerequisites

### System Requirements
- SLURM cluster with at least 2 nodes
- Python 3.6+
- MPI implementation (OpenMPI recommended)
- mpi4py Python package

### Installation
```bash
# Install mpi4py
pip3 install mpi4py

# Or on Ubuntu/Debian
sudo apt-get install python3-mpi4py
```

## Output and Logs

All tests write output to `/tmp/` with job IDs in filenames:
- `multi_node_test_<job_id>.out/err`
- `mpi_test_<job_id>.out/err`
- `job_array_test_<array_job_id>_<task_id>.out/err`
- etc.

The full test suite runner creates a comprehensive log file:
- `/tmp/slurm_test_suite_YYYYMMDD_HHMMSS.log`

## Troubleshooting

### Common Issues

1. **"sbatch: command not found"**
   - SLURM is not installed or not in PATH
   - Check: `which sbatch`

2. **"mpirun: command not found"**
   - MPI is not installed
   - Install OpenMPI: `sudo apt-get install openmpi-bin`

3. **"ImportError: No module named 'mpi4py'"**
   - Install mpi4py: `pip3 install mpi4py`

4. **Jobs remain pending**
   - Check cluster status: `sinfo`
   - Check queue: `squeue`
   - May need to start SLURM services

5. **Network connectivity failures**
   - Check SSH keys are set up for passwordless login
   - Verify firewall settings
   - Check DNS resolution between nodes

### Debug Commands
```bash
# Check cluster status
sinfo

# Check queue
squeue

# Check job details
scontrol show job <job_id>

# Check job accounting
sacct -j <job_id>

# Test manual MPI
mpirun -np 2 -H node1,node2 python3 test_mpi.py
```

## Expected Results

When all tests pass, you should see:
- ✅ Multi-node communication working
- ✅ MPI processes communicating across nodes
- ✅ Shared filesystem accessible from all nodes
- ✅ Job arrays executing independently
- ✅ Job dependencies executing in correct order
- ✅ All performance benchmarks within expected ranges

## Integration with ClusterOS

These tests verify that the ClusterOS node-agent properly:
- Configures SLURM across multiple nodes
- Sets up MPI communication
- Manages shared storage
- Handles job scheduling and dependencies
- Provides proper resource allocation

Run these tests after initial cluster setup and after any configuration changes to ensure everything is working correctly.