# SLURM Monitoring Guide

This guide covers comprehensive monitoring of the SLURM workload manager in your Cluster-OS deployment.

## Overview

SLURM (Simple Linux Utility for Resource Management) is a highly scalable cluster management and job scheduling system. Proper monitoring ensures optimal resource utilization and quick issue detection.

## Quick Status Commands

### Cluster Overview

```bash
# View all nodes and their status
sinfo

# Detailed node information
sinfo -Nel

# View partition information
sinfo -s

# Show only specific partitions
sinfo -p compute,gpu
```

**Expected Output**:
```
PARTITION AVAIL  TIMELIMIT  NODES  STATE NODELIST
compute*     up   infinite      3   idle node[1-3]
gpu          up   infinite      1   idle node4
```

**Node States**:
- `idle` - Ready for jobs
- `alloc` - Fully allocated
- `mix` - Partially allocated
- `down` - Unavailable
- `drain` - Being drained

### Job Queue

```bash
# Show all jobs
squeue

# Show only running jobs
squeue -t RUNNING

# Show only pending jobs
squeue -t PENDING

# Show jobs for specific user
squeue -u username

# Show detailed job info
squeue -o "%.18i %.9P %.8j %.8u %.2t %.10M %.6D %R"
```

**Job States**:
- `PD` - Pending (waiting)
- `R` - Running
- `CA` - Cancelled
- `CG` - Completing
- `CD` - Completed
- `F` - Failed

### Node Details

```bash
# Show detailed node status
scontrol show nodes

# Show specific node
scontrol show node node1

# Show node features
scontrol show node node1 | grep Features

# Show node configuration
scontrol show config
```

## Monitoring Resource Usage

### Per-Node Resource Monitoring

```bash
# Show allocated CPUs and memory per node
sinfo -N -o "%N %.6D %.6t %.14C %.8m %.8e"

# Explanation of columns:
# %N - Node name
# %D - Number of nodes
# %t - State
# %C - CPUs (allocated/idle/other/total)
# %m - Memory size
# %e - Free memory
```

### Job Resource Usage

```bash
# Show resource usage for running jobs
sstat -j <job_id> --format=JobID,MaxRSS,MaxVMSize,AveCPU

# Show accounting information for completed jobs
sacct -j <job_id> --format=JobID,JobName,MaxRSS,Elapsed,State

# Show all jobs from last 24 hours
sacct -S $(date -d '1 day ago' +%Y-%m-%d) --format=JobID,JobName,User,Elapsed,State,ExitCode

# Show jobs with high memory usage
sacct --format=JobID,JobName,MaxRSS,State -S $(date -d '1 day ago' +%Y-%m-%d) | sort -k3 -h
```

### Real-Time Job Monitoring

```bash
# Watch job queue in real-time
watch -n 5 squeue

# Monitor specific job
watch -n 2 "scontrol show job <job_id>"

# Monitor node utilization
watch -n 5 "sinfo -Nel"
```

## SLURM Controller Monitoring

### Service Status

```bash
# Check slurmctld (controller) status
sudo systemctl status slurmctld

# Check slurmd (worker) status on each node
sudo systemctl status slurmd

# Check munge (authentication) status
sudo systemctl status munge

# View logs
sudo journalctl -u slurmctld -f
sudo journalctl -u slurmd -f
```

### Controller Health

```bash
# Check controller responsiveness
scontrol ping

# Show controller configuration
scontrol show config | grep ControlMachine

# Check for controller failover (if HA configured)
scontrol show config | grep -E "ControlMachine|BackupController"

# View controller load
sdiag
```

### Database Accounting

```bash
# Check database connection
sacctmgr show configuration

# List all clusters
sacctmgr show cluster

# Show accounting storage status
sacctmgr show problem
```

## Advanced Monitoring

### Job Priority and Fairshare

```bash
# Show job priority factors
sprio

# Show fairshare information
sshare -a

# Show detailed priority calculation
sprio -l -j <job_id>

# Show user fairshare usage
sshare -u <username>
```

### Reservation Monitoring

```bash
# Show all reservations
scontrol show reservation

# Show specific reservation
scontrol show reservation=<name>

# Show nodes in reservation
scontrol show reservation=<name> | grep Nodes
```

### Partition Analysis

```bash
# Show partition limits and defaults
scontrol show partition

# Show partition-specific job counts
squeue --partition=compute --format="%P %.5D"

# Show partition utilization
sinfo -p compute -O PartitionName,Nodes,AllocNodes,IdleNodes
```

## Prometheus Exporter (Recommended)

### Install SLURM Exporter

```bash
# Install prometheus-slurm-exporter
sudo apt-get install -y prometheus-slurm-exporter

# Or build from source
git clone https://github.com/vpenso/prometheus-slurm-exporter.git
cd prometheus-slurm-exporter
go build

# Configure systemd service
sudo tee /etc/systemd/system/slurm-exporter.service << EOF
[Unit]
Description=SLURM Prometheus Exporter
After=network.target

[Service]
Type=simple
User=slurm
ExecStart=/usr/local/bin/prometheus-slurm-exporter
Restart=always

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now slurm-exporter
```

### Metrics Exposed

The exporter provides these metrics:

- `slurm_nodes_total` - Total number of nodes
- `slurm_nodes_idle` - Idle nodes
- `slurm_nodes_allocated` - Allocated nodes
- `slurm_nodes_down` - Down nodes
- `slurm_jobs_running` - Running jobs
- `slurm_jobs_pending` - Pending jobs
- `slurm_cpus_total` - Total CPUs
- `slurm_cpus_allocated` - Allocated CPUs
- `slurm_memory_total` - Total memory
- `slurm_memory_allocated` - Allocated memory

### Prometheus Configuration

```yaml
# prometheus.yml
scrape_configs:
  - job_name: 'slurm'
    static_configs:
      - targets: ['localhost:8080']
    scrape_interval: 30s
```

## Grafana Dashboard

### Import SLURM Dashboard

1. **Import pre-built dashboard**:
   - Dashboard ID: `4323` (SLURM Dashboard)
   - Or use custom JSON from below

2. **Key Panels**:
   - Node status distribution
   - CPU utilization over time
   - Memory utilization over time
   - Job queue length
   - Jobs by state
   - User fairshare usage
   - Partition utilization

### Sample Dashboard JSON

```json
{
  "dashboard": {
    "title": "SLURM Cluster Monitoring",
    "panels": [
      {
        "title": "Node Status",
        "targets": [
          {
            "expr": "slurm_nodes_idle",
            "legendFormat": "Idle"
          },
          {
            "expr": "slurm_nodes_allocated",
            "legendFormat": "Allocated"
          },
          {
            "expr": "slurm_nodes_down",
            "legendFormat": "Down"
          }
        ]
      },
      {
        "title": "CPU Utilization",
        "targets": [
          {
            "expr": "(slurm_cpus_allocated / slurm_cpus_total) * 100",
            "legendFormat": "CPU Usage %"
          }
        ]
      },
      {
        "title": "Job Queue",
        "targets": [
          {
            "expr": "slurm_jobs_running",
            "legendFormat": "Running"
          },
          {
            "expr": "slurm_jobs_pending",
            "legendFormat": "Pending"
          }
        ]
      }
    ]
  }
}
```

## Alerting Rules

### Prometheus Alert Rules

```yaml
# slurm_alerts.yml
groups:
  - name: slurm
    interval: 30s
    rules:
      # Node down alert
      - alert: SlurmNodeDown
        expr: slurm_nodes_down > 0
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "SLURM nodes are down"
          description: "{{ $value }} SLURM nodes are in DOWN state"

      # High CPU utilization
      - alert: SlurmHighCPUUsage
        expr: (slurm_cpus_allocated / slurm_cpus_total) > 0.9
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "SLURM cluster CPU usage is high"
          description: "CPU utilization is {{ $value | humanizePercentage }}"

      # High memory utilization
      - alert: SlurmHighMemoryUsage
        expr: (slurm_memory_allocated / slurm_memory_total) > 0.9
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "SLURM cluster memory usage is high"
          description: "Memory utilization is {{ $value | humanizePercentage }}"

      # Long pending queue
      - alert: SlurmLongPendingQueue
        expr: slurm_jobs_pending > 20
        for: 30m
        labels:
          severity: info
        annotations:
          summary: "SLURM has many pending jobs"
          description: "{{ $value }} jobs have been pending for >30 minutes"

      # Controller not responding
      - alert: SlurmControllerDown
        expr: up{job="slurm"} == 0
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "SLURM controller is down"
          description: "Cannot scrape SLURM metrics"
```

## Custom Monitoring Scripts

### Node Health Check

```bash
#!/bin/bash
# check_slurm_nodes.sh - Check node health

NODES_DOWN=$(sinfo -h -o "%t" | grep -c "down")
NODES_DRAIN=$(sinfo -h -o "%t" | grep -c "drain")

if [ "$NODES_DOWN" -gt 0 ]; then
    echo "WARNING: $NODES_DOWN nodes are DOWN"
    sinfo -t down -Nel
fi

if [ "$NODES_DRAIN" -gt 0 ]; then
    echo "WARNING: $NODES_DRAIN nodes are DRAINING"
    sinfo -t drain -Nel
fi
```

### Job Queue Monitor

```bash
#!/bin/bash
# monitor_queue.sh - Monitor job queue

PENDING=$(squeue -h -t PENDING | wc -l)
RUNNING=$(squeue -h -t RUNNING | wc -l)

echo "Queue Status:"
echo "  Running: $RUNNING"
echo "  Pending: $PENDING"

if [ "$PENDING" -gt 50 ]; then
    echo "WARNING: High pending job count!"
fi
```

### Resource Utilization Report

```bash
#!/bin/bash
# resource_report.sh - Generate resource utilization report

echo "=== SLURM Resource Utilization Report ==="
echo "Date: $(date)"
echo ""

echo "Node Status:"
sinfo -N -o "%N %.6t %.14C %.8m %.8e" | column -t
echo ""

echo "CPU Utilization:"
TOTAL_CPUS=$(sinfo -h -o "%C" | awk -F/ '{print $4}')
ALLOC_CPUS=$(sinfo -h -o "%C" | awk -F/ '{print $1}')
IDLE_CPUS=$(sinfo -h -o "%C" | awk -F/ '{print $2}')

echo "  Total: $TOTAL_CPUS"
echo "  Allocated: $ALLOC_CPUS"
echo "  Idle: $IDLE_CPUS"
echo "  Utilization: $(awk "BEGIN {printf \"%.1f%%\", ($ALLOC_CPUS/$TOTAL_CPUS)*100}")"
echo ""

echo "Top 10 Users by Job Count:"
squeue -h -o "%u" | sort | uniq -c | sort -rn | head -10
```

## Log Analysis

### Important Log Locations

```bash
# Controller logs
/var/log/slurm/slurmctld.log

# Worker logs
/var/log/slurm/slurmd.log

# Accounting logs
/var/log/slurm/slurm_jobacct.log
```

### Log Monitoring Commands

```bash
# Watch controller logs for errors
sudo tail -f /var/log/slurm/slurmctld.log | grep -i error

# Find recent job failures
sudo grep "error" /var/log/slurm/slurmctld.log | tail -50

# Monitor job submissions
sudo tail -f /var/log/slurm/slurmctld.log | grep "submit"

# Check for node failures
sudo grep -i "node.*down" /var/log/slurm/slurmctld.log | tail -20
```

## Performance Tuning Monitoring

### Scheduler Performance

```bash
# View scheduler statistics
sdiag

# Key metrics to watch:
# - Server thread count
# - Agent queue size
# - DBD Agent queue size
# - Jobs submitted
# - Jobs started
# - Jobs completed
# - Main scheduler cycles

# Reset statistics
sudo scontrol reconfigure
```

### Database Performance

```bash
# Check accounting storage performance
sacctmgr show stats

# Monitor database connections
sudo ss -tn | grep :6819 | wc -l  # MariaDB default port
```

## Best Practices

1. **Regular Monitoring**:
   - Check `sinfo` daily
   - Review `squeue` for stuck jobs
   - Monitor logs for errors

2. **Automated Alerts**:
   - Set up Prometheus alerts
   - Configure email notifications
   - Use Slack/Teams webhooks

3. **Capacity Planning**:
   - Track historical usage trends
   - Monitor peak usage times
   - Plan for growth

4. **Maintenance Windows**:
   - Drain nodes before maintenance
   - Monitor job completion
   - Verify node return to service

## Troubleshooting Common Issues

### Nodes Stuck in Down State

```bash
# Check node reason
scontrol show node <node> | grep Reason

# Try to return node to service
sudo scontrol update nodename=<node> state=resume

# If node won't resume, check slurmd
ssh <node> "sudo systemctl status slurmd"
```

### Jobs Not Starting

```bash
# Check job details
scontrol show job <job_id>

# Check node availability
sinfo -Nel

# Check partition limits
scontrol show partition

# Check user limits
sacctmgr show user <username> withassoc
```

### Controller Not Responding

```bash
# Check controller status
sudo systemctl status slurmctld

# Check logs
sudo journalctl -u slurmctld -n 100

# Restart if needed
sudo systemctl restart slurmctld
```

## Summary

Effective SLURM monitoring involves:
- ✅ Regular status checks with `sinfo` and `squeue`
- ✅ Prometheus + Grafana for metrics visualization
- ✅ Automated alerting for issues
- ✅ Log analysis for troubleshooting
- ✅ Performance tuning based on metrics

For more information:
- [SLURM Documentation](https://slurm.schedmd.com/)
- [SLURM Accounting](https://slurm.schedmd.com/accounting.html)
- [SLURM Troubleshooting](https://slurm.schedmd.com/troubleshoot.html)
