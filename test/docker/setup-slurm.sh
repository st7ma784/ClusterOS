#!/bin/bash

# SLURM Setup Script for Testing
# This script configures and starts SLURM services on all nodes

set -e

NODES="node1 node2 node3 node4 node5"
NODE_IPS=(
    "10.90.0.10"  # node1
    "10.90.0.11"  # node2
    "10.90.0.12"  # node3
    "10.90.0.13"  # node4
    "10.90.0.14"  # node5
)

# Create slurm.conf
create_slurm_conf() {
    local node=$1
    local node_list=""
    local node_count=0
    
    for n in $NODES; do
        node_list="$node_list$n,"
        node_count=$((node_count + 1))
    done
    node_list="${node_list%,}"  # Remove trailing comma
    
    cat > /tmp/slurm.conf << 'SLURM_CONF'
# SLURM Configuration
ClusterName=cluster-os-test
ControlMachine=node1
ControlAddr=10.90.0.10

# Authentication
AuthType=auth/munge
CryptoType=crypto/munge

# Nodes configuration
NodeName=node1,node2,node3,node4,node5 \
    CPUs=2 \
    RealMemory=1024 \
    Sockets=1 \
    CoresPerSocket=2 \
    ThreadsPerCore=1 \
    State=UNKNOWN

# Partition configuration
PartitionName=debug \
    Nodes=node1,node2,node3,node4,node5 \
    Default=YES \
    MaxTime=INFINITE \
    State=UP

# Paths
SlurmdLogFile=/var/log/slurmd.log
SlurmctldLogFile=/var/log/slurmctld.log
SlurmdSpoolDir=/var/spool/slurm/d
StateSaveLocation=/var/spool/slurm

# Timeouts and limits
SlurmctldTimeout=300
SlurmdTimeout=300
InactiveLimit=0
MinJobAge=300

# Scheduler
SchedulerType=sched/builtin
SelectType=select/cons_tres
SelectTypeParameters=CR_Core

# Health check
HealthCheckProgram=/usr/lib/slurm/health_check

# Misc
ProctrackType=proctrack/linuxproc
TaskPlugin=task/none
SLURM_CONF
}

# Setup Munge on all nodes
setup_munge() {
    echo "Setting up Munge..."
    
    # Get munge key from node1
    docker exec cluster-os-node1 bash -c 'mkdir -p /var/run/munge && chmod 0755 /var/run/munge' || true
    
    # Create munge user/group if needed
    for node in $NODES; do
        docker exec cluster-os-$node bash -c '
            groupadd -r munge 2>/dev/null || true
            useradd -r -g munge munge 2>/dev/null || true
            mkdir -p /var/run/munge
            chown munge:munge /var/run/munge
            chmod 0755 /var/run/munge
            mkdir -p /var/log
        ' || true
    done
}

# Setup SLURM on all nodes
setup_slurm() {
    echo "Setting up SLURM..."
    
    create_slurm_conf
    
    # Copy config to all nodes
    for node in $NODES; do
        docker cp /tmp/slurm.conf cluster-os-$node:/etc/slurm/slurm.conf
        docker exec cluster-os-$node bash -c '
            chown root:root /etc/slurm/slurm.conf
            chmod 644 /etc/slurm/slurm.conf
            mkdir -p /var/spool/slurm
            mkdir -p /var/log
        '
    done
}

# Start services
start_services() {
    echo "Starting SLURM services..."
    
    # Start munge on all nodes
    echo "Starting Munge daemon..."
    for node in $NODES; do
        docker exec cluster-os-$node bash -c 'munged -f &' 2>/dev/null || true
    done
    sleep 2
    
    # Start slurmctld on node1
    echo "Starting slurmctld on node1..."
    docker exec cluster-os-node1 bash -c 'slurmctld -D -vvv > /var/log/slurmctld.log 2>&1 &' || true
    sleep 3
    
    # Start slurmd on all nodes
    echo "Starting slurmd on all nodes..."
    for node in $NODES; do
        docker exec cluster-os-$node bash -c 'slurmd -D -vvv > /var/log/slurmd.log 2>&1 &' || true
    done
    sleep 2
}

# Test SLURM
test_slurm() {
    echo ""
    echo "=========================================="
    echo "Testing SLURM"
    echo "=========================================="
    
    echo ""
    echo "1. Checking node status (sinfo):"
    docker exec cluster-os-node1 sinfo
    
    echo ""
    echo "2. Checking detailed node info:"
    docker exec cluster-os-node1 sinfo -N -l
    
    echo ""
    echo "3. Submitting test job..."
    local job_id=$(docker exec cluster-os-node1 bash -c 'sbatch --wrap="echo Hello from SLURM" 2>&1' | grep "Submitted batch" | awk '{print $4}' || echo "0")
    
    if [ "$job_id" != "0" ]; then
        echo "Job submitted with ID: $job_id"
        sleep 3
        echo ""
        echo "4. Job status:"
        docker exec cluster-os-node1 squeue
        
        echo ""
        echo "5. Job details:"
        docker exec cluster-os-node1 scontrol show job $job_id || true
    else
        echo "Failed to submit job"
    fi
}

# Main
main() {
    echo "==========================================="
    echo "SLURM Cluster Setup for Testing"
    echo "==========================================="
    
    setup_munge
    setup_slurm
    start_services
    test_slurm
    
    echo ""
    echo "=========================================="
    echo "Setup Complete!"
    echo "=========================================="
    echo "Useful commands:"
    echo "  sinfo              - Show node information"
    echo "  squeue             - Show job queue"
    echo "  sbatch <script>    - Submit batch job"
    echo "  srun <command>     - Run command across nodes"
    echo "  scancel <job_id>   - Cancel job"
    echo ""
}

main "$@"
