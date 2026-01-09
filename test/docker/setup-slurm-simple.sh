#!/bin/bash

# Simplified SLURM Configuration for Docker Testing
# This disables cgroup and systemd dependencies

set -e

NODES="node1 node2 node3 node4 node5"

create_simple_slurm_conf() {
    cat > /tmp/slurm.conf << 'SLURM_CONF'
# SLURM Configuration - Simplified for Docker
ClusterName=cluster-os-test
ControlMachine=node1
ControlAddr=10.90.0.10

# Disable cgroups for Docker compatibility
ProctrackType=proctrack/linuxproc
TaskPlugin=task/none
CgroupPlugin=task/none

# Authentication
AuthType=auth/munge
CryptoType=crypto/munge

# Nodes - Match Docker container specs
NodeName=node1,node2,node3,node4,node5 \
    CPUs=4 \
    RealMemory=8192 \
    State=UNKNOWN

# Partition
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

# Timeouts
SlurmctldTimeout=300
SlurmdTimeout=300
InactiveLimit=0
MinJobAge=300

# Scheduler
SchedulerType=sched/builtin
SelectType=select/linear

# Other
JobAcctGatherType=jobacct_gather/none
SLURM_CONF
}

setup_nodes() {
    echo "Setting up nodes..."
    
    for node in $NODES; do
        docker exec cluster-os-$node bash -c '
            # Create required directories
            mkdir -p /var/spool/slurm
            mkdir -p /var/run/munge
            mkdir -p /var/log
            
            # Kill existing services gracefully
            pkill -f "munged|slurmd|slurmctld" || true
            sleep 1
            
            # Setup munge
            groupadd -r munge 2>/dev/null || true
            useradd -r -g munge munge 2>/dev/null || true
            chown munge:munge /var/run/munge
            chmod 0755 /var/run/munge
        ' || true
    done
}

# Create config and distribute
create_simple_slurm_conf

for node in $NODES; do
    docker cp /tmp/slurm.conf cluster-os-$node:/etc/slurm/slurm.conf
    docker exec cluster-os-$node bash -c '
        chown root:root /etc/slurm/slurm.conf
        chmod 644 /etc/slurm/slurm.conf
    '
done

setup_nodes

# Start munge on all nodes
echo "Starting Munge daemon..."
for node in $NODES; do
    docker exec cluster-os-$node bash -c 'nohup munged &' 2>/dev/null || true
done
sleep 2

# Start slurmctld on node1
echo "Starting slurmctld on node1..."
docker exec cluster-os-node1 bash -c 'nohup slurmctld -L /var/log/slurmctld.log &' 2>/dev/null
sleep 3

# Start slurmd on all nodes  
echo "Starting slurmd on all nodes..."
for node in $NODES; do
    docker exec cluster-os-$node bash -c 'nohup slurmd -L /var/log/slurmd.log &' 2>/dev/null
done
sleep 3

echo ""
echo "=========================================="
echo "SLURM Status - Waiting for nodes to register..."
echo "=========================================="
sleep 3

# Check status
docker exec cluster-os-node1 sinfo 2>&1 || echo "sinfo not yet ready"

echo ""
echo "Waiting for nodes to register (30 seconds)..."
for i in {1..10}; do
    sleep 3
    echo -n "."
    status=$(docker exec cluster-os-node1 sinfo 2>/dev/null | tail -6 || echo "")
    if echo "$status" | grep -q "idle\|allocated"; then
        echo " ✓ Nodes registered!"
        break
    fi
done

echo ""
echo "=========================================="
echo "Final SLURM Status"
echo "=========================================="
docker exec cluster-os-node1 sinfo -N -l

echo ""
echo "Process status on nodes:"
docker exec cluster-os-node1 ps aux | grep -E "slurm|munge" | grep -v grep | head -5

echo ""
echo "Done!"
