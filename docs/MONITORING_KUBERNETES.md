# Kubernetes (K3s) Monitoring Guide

This guide covers comprehensive monitoring of K3s Kubernetes clusters in your Cluster-OS deployment.

## Overview

K3s is a lightweight Kubernetes distribution. This guide shows how to monitor the K3s control plane, worker nodes, pods, and workloads.

## Quick Status Commands

### Cluster Health

```bash
# Check cluster info
sudo k3s kubectl cluster-info

# View node status
sudo k3s kubectl get nodes

# View node details
sudo k3s kubectl get nodes -o wide

# Describe specific node
sudo k3s kubectl describe node <node-name>

# Check component status
sudo k3s kubectl get componentstatuses
```

**Expected Output**:
```
NAME     STATUS   ROLES                  AGE   VERSION
node1    Ready    control-plane,master   10d   v1.28.5+k3s1
node2    Ready    <none>                 10d   v1.28.5+k3s1
node3    Ready    <none>                 10d   v1.28.5+k3s1
```

**Node Conditions** to monitor:
- `Ready` - Node is healthy
- `MemoryPressure` - Node has memory issues
- `DiskPressure` - Node has disk issues
- `PIDPressure` - Too many processes
- `NetworkUnavailable` - Network problems

### Pod Status

```bash
# View all pods across all namespaces
sudo k3s kubectl get pods -A

# View pods in specific namespace
sudo k3s kubectl get pods -n kube-system

# View pods with more details
sudo k3s kubectl get pods -A -o wide

# Show pods not running
sudo k3s kubectl get pods -A --field-selector status.phase!=Running

# Watch pods in real-time
watch -n 2 "sudo k3s kubectl get pods -A"
```

### Resource Usage

```bash
# View node resource usage
sudo k3s kubectl top nodes

# View pod resource usage
sudo k3s kubectl top pods -A

# View pod resource usage sorted by CPU
sudo k3s kubectl top pods -A --sort-by=cpu

# View pod resource usage sorted by memory
sudo k3s kubectl top pods -A --sort-by=memory
```

## K3s Service Monitoring

### Service Status

```bash
# Check K3s server status (on control plane)
sudo systemctl status k3s

# Check K3s agent status (on workers)
sudo systemctl status k3s-agent

# View K3s server logs
sudo journalctl -u k3s -f

# View K3s agent logs
sudo journalctl -u k3s-agent -f

# Check for errors in logs
sudo journalctl -u k3s --since "1 hour ago" | grep -i error
```

### K3s Process Monitoring

```bash
# Check K3s processes
ps aux | grep k3s

# Check K3s resource usage
top -p $(pgrep k3s | tr '\n' ',' | sed 's/,$//')

# Check K3s network connections
sudo ss -tulpn | grep k3s
```

## Kubernetes API Server Monitoring

### API Server Health

```bash
# Check API server health
sudo k3s kubectl get --raw='/healthz'

# Check API server readiness
sudo k3s kubectl get --raw='/readyz'

# Check API server livez
sudo k3s kubectl get --raw='/livez'

# View API server metrics
sudo k3s kubectl get --raw='/metrics'
```

### API Request Metrics

```bash
# Check API server requests
curl -k https://localhost:6443/metrics | grep apiserver_request_total

# Check API latency
curl -k https://localhost:6443/metrics | grep apiserver_request_duration_seconds
```

## Workload Monitoring

### Deployments

```bash
# View all deployments
sudo k3s kubectl get deployments -A

# View deployment status
sudo k3s kubectl rollout status deployment/<name> -n <namespace>

# View deployment history
sudo k3s kubectl rollout history deployment/<name> -n <namespace>

# Describe deployment
sudo k3s kubectl describe deployment/<name> -n <namespace>
```

### StatefulSets

```bash
# View StatefulSets
sudo k3s kubectl get statefulsets -A

# Describe StatefulSet
sudo k3s kubectl describe statefulset/<name> -n <namespace>

# Check StatefulSet status
sudo k3s kubectl get statefulset/<name> -n <namespace> -o wide
```

### DaemonSets

```bash
# View DaemonSets
sudo k3s kubectl get daemonsets -A

# Check DaemonSet coverage
sudo k3s kubectl get daemonset/<name> -n <namespace>

# Describe DaemonSet
sudo k3s kubectl describe daemonset/<name> -n <namespace>
```

### Jobs and CronJobs

```bash
# View jobs
sudo k3s kubectl get jobs -A

# View CronJobs
sudo k3s kubectl get cronjobs -A

# View job logs
sudo k3s kubectl logs job/<name> -n <namespace>
```

## Scheduler Monitoring

### Scheduler Status

```bash
# Check scheduler health
sudo k3s kubectl get pods -n kube-system -l component=kube-scheduler

# View scheduler logs
sudo k3s kubectl logs -n kube-system -l component=kube-scheduler

# Check scheduler metrics
curl -k https://localhost:10259/metrics | grep scheduler
```

### Pod Scheduling

```bash
# View pending pods (not scheduled)
sudo k3s kubectl get pods -A --field-selector status.phase=Pending

# Describe pending pod to see why
sudo k3s kubectl describe pod/<name> -n <namespace>

# Check node affinity and taints
sudo k3s kubectl describe nodes | grep -A 5 Taints
```

### Scheduler Events

```bash
# View scheduling events
sudo k3s kubectl get events -A --sort-by='.lastTimestamp' | grep -i schedule

# Watch events in real-time
sudo k3s kubectl get events -A -w
```

## Metrics Server (Required for kubectl top)

### Install Metrics Server

```bash
# Deploy metrics-server
sudo k3s kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml

# Or use Helm
helm repo add metrics-server https://kubernetes-sigs.github.io/metrics-server/
helm install metrics-server metrics-server/metrics-server -n kube-system

# Verify deployment
sudo k3s kubectl get deployment metrics-server -n kube-system
```

### Configure Metrics Server for K3s

```bash
# Edit deployment to add --kubelet-insecure-tls flag
sudo k3s kubectl edit deployment metrics-server -n kube-system

# Add under containers.args:
# - --kubelet-insecure-tls
# - --kubelet-preferred-address-types=InternalIP
```

## Prometheus Operator (Recommended)

### Install Prometheus Stack

```bash
# Add Prometheus Helm repo
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update

# Install kube-prometheus-stack
helm install prometheus prometheus-community/kube-prometheus-stack \
  --namespace monitoring \
  --create-namespace \
  --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false

# Verify installation
sudo k3s kubectl get pods -n monitoring
```

### Access Prometheus

```bash
# Port-forward Prometheus
sudo k3s kubectl port-forward -n monitoring svc/prometheus-kube-prometheus-prometheus 9090:9090

# Access at http://localhost:9090
```

### Access Grafana

```bash
# Port-forward Grafana
sudo k3s kubectl port-forward -n monitoring svc/prometheus-grafana 3000:80

# Get admin password
sudo k3s kubectl get secret -n monitoring prometheus-grafana \
  -o jsonpath="{.data.admin-password}" | base64 --decode

# Access at http://localhost:3000
# Username: admin
# Password: <from above>
```

## Key Metrics to Monitor

### Node Metrics

- `node_cpu_seconds_total` - CPU usage
- `node_memory_MemAvailable_bytes` - Available memory
- `node_disk_io_time_seconds_total` - Disk I/O
- `node_network_receive_bytes_total` - Network RX
- `node_network_transmit_bytes_total` - Network TX
- `node_filesystem_avail_bytes` - Disk space

### Kubelet Metrics

- `kubelet_running_pods` - Number of running pods
- `kubelet_running_containers` - Number of running containers
- `kubelet_pod_start_duration_seconds` - Pod startup time
- `kubelet_pod_worker_duration_seconds` - Pod worker duration

### API Server Metrics

- `apiserver_request_total` - Total requests
- `apiserver_request_duration_seconds` - Request latency
- `apiserver_current_inflight_requests` - In-flight requests
- `etcd_request_duration_seconds` - ETCD latency

### Scheduler Metrics

- `scheduler_pending_pods` - Pending pods
- `scheduler_schedule_attempts_total` - Schedule attempts
- `scheduler_scheduling_algorithm_duration_seconds` - Scheduling latency
- `scheduler_binding_duration_seconds` - Binding duration

### Pod Metrics

- `kube_pod_status_phase` - Pod phase (Running, Pending, etc.)
- `kube_pod_container_status_restarts_total` - Container restarts
- `kube_pod_container_status_ready` - Container ready status
- `container_cpu_usage_seconds_total` - Container CPU usage
- `container_memory_working_set_bytes` - Container memory usage

## Grafana Dashboards

### Import Pre-built Dashboards

1. **Kubernetes Cluster Monitoring** (Dashboard ID: `7249`)
   - Overall cluster health
   - Node resource usage
   - Pod distribution

2. **Kubernetes Pod Resources** (Dashboard ID: `6417`)
   - CPU and memory by pod
   - Network usage
   - Storage I/O

3. **Node Exporter Full** (Dashboard ID: `1860`)
   - Detailed node metrics
   - System resources
   - Network statistics

4. **K3s Cluster** (Dashboard ID: `11074`)
   - K3s-specific metrics
   - Control plane health
   - Agent status

### Import in Grafana

1. Go to Dashboards → Import
2. Enter Dashboard ID
3. Select Prometheus data source
4. Click Import

## Alerting Rules

### Prometheus Alert Rules

```yaml
# kubernetes_alerts.yml
groups:
  - name: kubernetes
    interval: 30s
    rules:
      # Node down
      - alert: KubernetesNodeNotReady
        expr: kube_node_status_condition{condition="Ready",status="true"} == 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "Kubernetes Node not ready"
          description: "Node {{ $labels.node }} has been not ready for >5 minutes"

      # High CPU usage
      - alert: KubernetesNodeHighCPU
        expr: (100 - (avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100)) > 80
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Kubernetes Node high CPU usage"
          description: "Node {{ $labels.instance }} CPU usage is {{ $value }}%"

      # High memory usage
      - alert: KubernetesNodeHighMemory
        expr: (1 - (node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)) * 100 > 80
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Kubernetes Node high memory usage"
          description: "Node {{ $labels.instance }} memory usage is {{ $value }}%"

      # Pod not running
      - alert: KubernetesPodNotRunning
        expr: kube_pod_status_phase{phase!="Running",phase!="Succeeded"} == 1
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "Kubernetes Pod not running"
          description: "Pod {{ $labels.namespace }}/{{ $labels.pod }} is not running"

      # Container restarting
      - alert: KubernetesContainerRestarting
        expr: rate(kube_pod_container_status_restarts_total[15m]) > 0
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Kubernetes container restarting"
          description: "Container {{ $labels.namespace }}/{{ $labels.pod }}/{{ $labels.container }} is restarting"

      # Deployment replicas mismatch
      - alert: KubernetesDeploymentReplicasMismatch
        expr: kube_deployment_status_replicas_available != kube_deployment_spec_replicas
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Kubernetes Deployment replicas mismatch"
          description: "Deployment {{ $labels.namespace }}/{{ $labels.deployment }} has {{ $value }} replicas available vs spec"

      # StatefulSet replicas mismatch
      - alert: KubernetesStatefulSetReplicasMismatch
        expr: kube_statefulset_status_replicas_ready != kube_statefulset_spec_replicas
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Kubernetes StatefulSet replicas mismatch"
          description: "StatefulSet {{ $labels.namespace }}/{{ $labels.statefulset }} has {{ $value }} replicas ready vs spec"

      # PersistentVolume filling up
      - alert: KubernetesPersistentVolumeFillingUp
        expr: (kubelet_volume_stats_available_bytes / kubelet_volume_stats_capacity_bytes) * 100 < 10
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Kubernetes PersistentVolume filling up"
          description: "PV {{ $labels.persistentvolumeclaim }} has {{ $value }}% space remaining"

      # Job failed
      - alert: KubernetesJobFailed
        expr: kube_job_status_failed > 0
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Kubernetes Job failed"
          description: "Job {{ $labels.namespace }}/{{ $labels.job_name }} failed"
```

## Custom Monitoring Scripts

### Cluster Health Check

```bash
#!/bin/bash
# check_k3s_health.sh - Check K3s cluster health

echo "=== K3s Cluster Health Check ==="
echo ""

# Check nodes
echo "Node Status:"
sudo k3s kubectl get nodes

NOT_READY=$(sudo k3s kubectl get nodes --no-headers | grep -v " Ready " | wc -l)
if [ "$NOT_READY" -gt 0 ]; then
    echo "WARNING: $NOT_READY nodes are not ready"
fi

echo ""

# Check system pods
echo "System Pods:"
sudo k3s kubectl get pods -n kube-system

FAILING_PODS=$(sudo k3s kubectl get pods -n kube-system --no-headers | grep -v "Running\|Completed" | wc -l)
if [ "$FAILING_PODS" -gt 0 ]; then
    echo "WARNING: $FAILING_PODS system pods are not running"
fi

echo ""

# Check API server
echo "API Server Health:"
sudo k3s kubectl get --raw='/healthz' && echo "✓ Healthy" || echo "✗ Unhealthy"
```

### Resource Usage Report

```bash
#!/bin/bash
# k3s_resource_report.sh - Generate resource usage report

echo "=== K3s Resource Usage Report ==="
echo "Date: $(date)"
echo ""

echo "Node Resource Usage:"
sudo k3s kubectl top nodes
echo ""

echo "Top 10 Pods by CPU:"
sudo k3s kubectl top pods -A --sort-by=cpu | head -11
echo ""

echo "Top 10 Pods by Memory:"
sudo k3s kubectl top pods -A --sort-by=memory | head -11
```

### Pod Health Check

```bash
#!/bin/bash
# check_pod_health.sh - Check pod health

RESTART_THRESHOLD=5

echo "=== Pod Health Check ==="
echo ""

echo "Pods with high restart count:"
sudo k3s kubectl get pods -A -o json | jq -r \
  '.items[] | select(.status.containerStatuses != null) |
   select(.status.containerStatuses[].restartCount > '$RESTART_THRESHOLD') |
   "\(.metadata.namespace)/\(.metadata.name): \(.status.containerStatuses[].restartCount) restarts"'

echo ""

echo "Pending pods:"
sudo k3s kubectl get pods -A --field-selector status.phase=Pending
```

## Log Analysis

### Important Log Locations

```bash
# K3s server logs
sudo journalctl -u k3s

# K3s agent logs
sudo journalctl -u k3s-agent

# Containerd logs
sudo journalctl -u containerd

# Pod logs
sudo k3s kubectl logs <pod-name> -n <namespace>

# Previous container logs (after restart)
sudo k3s kubectl logs <pod-name> -n <namespace> --previous
```

### Log Monitoring Commands

```bash
# Watch K3s server logs
sudo journalctl -u k3s -f

# Watch for errors
sudo journalctl -u k3s -f | grep -i error

# View logs for specific pod
sudo k3s kubectl logs -f <pod-name> -n <namespace>

# View logs for all containers in pod
sudo k3s kubectl logs <pod-name> -n <namespace> --all-containers=true

# View logs from multiple pods
sudo k3s kubectl logs -l app=myapp -n <namespace>
```

## Performance Tuning

### Kubelet Configuration

```bash
# Edit kubelet config
sudo vim /var/lib/rancher/k3s/agent/kubelet.config

# Key settings:
# - maxPods: Maximum pods per node
# - podPidsLimit: Max PIDs per pod
# - imageGCHighThresholdPercent: Disk usage threshold for GC
# - imageGCLowThresholdPercent: Disk usage target for GC
```

### Resource Limits

```yaml
# Set resource requests and limits for pods
apiVersion: v1
kind: Pod
metadata:
  name: example
spec:
  containers:
  - name: app
    image: myapp
    resources:
      requests:
        memory: "256Mi"
        cpu: "500m"
      limits:
        memory: "512Mi"
        cpu: "1000m"
```

## Best Practices

1. **Regular Monitoring**:
   - Check node status daily
   - Review pod health
   - Monitor resource usage trends

2. **Automated Alerts**:
   - Set up Prometheus alerts
   - Configure notification channels
   - Test alert routing

3. **Capacity Planning**:
   - Track resource trends
   - Plan for peak usage
   - Monitor storage growth

4. **Maintenance**:
   - Drain nodes before updates
   - Monitor rolling updates
   - Verify service availability

## Troubleshooting Common Issues

### Pods Not Starting

```bash
# Check pod events
sudo k3s kubectl describe pod/<name> -n <namespace>

# Check node resources
sudo k3s kubectl top nodes

# Check ImagePullBackOff
sudo k3s kubectl get events -n <namespace> | grep -i imagepull
```

### Node Not Ready

```bash
# Check node conditions
sudo k3s kubectl describe node <name>

# Check kubelet logs
sudo journalctl -u k3s-agent -n 100

# Check network
ping <node-ip>
```

### Service Not Accessible

```bash
# Check service endpoints
sudo k3s kubectl get endpoints <service> -n <namespace>

# Check service selector
sudo k3s kubectl get service <service> -n <namespace> -o yaml

# Check pod labels
sudo k3s kubectl get pods -n <namespace> --show-labels
```

## Summary

Effective K3s monitoring involves:
- ✅ Regular cluster health checks
- ✅ Prometheus + Grafana for visualization
- ✅ Automated alerting
- ✅ Log aggregation and analysis
- ✅ Resource usage tracking

For more information:
- [K3s Documentation](https://docs.k3s.io/)
- [Kubernetes Monitoring](https://kubernetes.io/docs/tasks/debug-application-cluster/resource-usage-monitoring/)
- [Prometheus Operator](https://prometheus-operator.dev/)
