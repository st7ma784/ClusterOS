# Operations Dashboard - Comprehensive Monitoring Setup

This guide shows how to set up a complete monitoring solution for Cluster-OS that includes SLURM, Kubernetes, and cluster health in a unified dashboard.

## Overview

The operations dashboard provides:
- **Unified View** - All cluster components in one place
- **Real-time Metrics** - Live data from Prometheus
- **Historical Data** - Long-term trend analysis
- **Alerting** - Proactive issue detection
- **Custom Dashboards** - Role-specific views

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    Cluster-OS Nodes                          │
│                                                              │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐   │
│  │  Node 1  │  │  Node 2  │  │  Node 3  │  │  Node N  │   │
│  │          │  │          │  │          │  │          │   │
│  │ Exporters│  │ Exporters│  │ Exporters│  │ Exporters│   │
│  │  - Node  │  │  - Node  │  │  - Node  │  │  - Node  │   │
│  │  - SLURM │  │  - SLURM │  │  - SLURM │  │  - SLURM │   │
│  │  - K8s   │  │  - K8s   │  │  - K8s   │  │  - K8s   │   │
│  │  - WG    │  │  - WG    │  │  - WG    │  │  - WG    │   │
│  └─────┬────┘  └─────┬────┘  └─────┬────┘  └─────┬────┘   │
│        │             │             │             │         │
└────────┼─────────────┼─────────────┼─────────────┼─────────┘
         │             │             │             │
         └─────────────┴─────────────┴─────────────┘
                       │
         ┌─────────────▼─────────────┐
         │      Prometheus            │
         │  (Metrics Collection)      │
         │  - Scrape exporters        │
         │  - Store time-series       │
         │  - Evaluate alerts         │
         └─────────────┬──────────────┘
                       │
         ┌─────────────▼─────────────┐
         │       Grafana              │
         │  (Visualization)           │
         │  - Dashboards              │
         │  - Graphs & charts         │
         │  - Alerting UI             │
         └────────────────────────────┘
                       │
         ┌─────────────▼─────────────┐
         │   AlertManager             │
         │  (Alert Routing)           │
         │  - Email                   │
         │  - Slack                   │
         │  - PagerDuty               │
         └────────────────────────────┘
```

## Complete Installation Guide

### Prerequisites

```bash
# Install Docker and Docker Compose
sudo apt-get update
sudo apt-get install -y docker.io docker-compose

# Add user to docker group
sudo usermod -aG docker $USER

# Log out and back in
```

### Step 1: Deploy Monitoring Stack

Create monitoring stack directory:

```bash
mkdir -p ~/monitoring
cd ~/monitoring
```

Create `docker-compose.yml`:

```yaml
version: '3.8'

networks:
  monitoring:
    driver: bridge

volumes:
  prometheus_data: {}
  grafana_data: {}
  alertmanager_data: {}

services:
  # Prometheus - Metrics collection
  prometheus:
    image: prom/prometheus:latest
    container_name: prometheus
    restart: unless-stopped
    volumes:
      - ./prometheus:/etc/prometheus
      - prometheus_data:/prometheus
    command:
      - '--config.file=/etc/prometheus/prometheus.yml'
      - '--storage.tsdb.path=/prometheus'
      - '--storage.tsdb.retention.time=30d'
      - '--web.console.libraries=/etc/prometheus/console_libraries'
      - '--web.console.templates=/etc/prometheus/consoles'
      - '--web.enable-lifecycle'
    ports:
      - "9090:9090"
    networks:
      - monitoring

  # Grafana - Visualization
  grafana:
    image: grafana/grafana:latest
    container_name: grafana
    restart: unless-stopped
    volumes:
      - grafana_data:/var/lib/grafana
      - ./grafana/provisioning:/etc/grafana/provisioning
    environment:
      - GF_SECURITY_ADMIN_USER=admin
      - GF_SECURITY_ADMIN_PASSWORD=admin
      - GF_INSTALL_PLUGINS=grafana-piechart-panel
    ports:
      - "3000:3000"
    networks:
      - monitoring
    depends_on:
      - prometheus

  # AlertManager - Alert routing
  alertmanager:
    image: prom/alertmanager:latest
    container_name: alertmanager
    restart: unless-stopped
    volumes:
      - ./alertmanager:/etc/alertmanager
      - alertmanager_data:/alertmanager
    command:
      - '--config.file=/etc/alertmanager/alertmanager.yml'
      - '--storage.path=/alertmanager'
    ports:
      - "9093:9093"
    networks:
      - monitoring

  # cAdvisor - Container metrics
  cadvisor:
    image: gcr.io/cadvisor/cadvisor:latest
    container_name: cadvisor
    restart: unless-stopped
    privileged: true
    volumes:
      - /:/rootfs:ro
      - /var/run:/var/run:ro
      - /sys:/sys:ro
      - /var/lib/docker:/var/lib/docker:ro
      - /dev/disk:/dev/disk:ro
    ports:
      - "8080:8080"
    networks:
      - monitoring
```

### Step 2: Configure Prometheus

Create `prometheus/prometheus.yml`:

```yaml
global:
  scrape_interval: 15s
  evaluation_interval: 15s
  external_labels:
    cluster: 'cluster-os'
    monitor: 'cluster-os-monitor'

# Alertmanager configuration
alerting:
  alertmanagers:
    - static_configs:
        - targets:
            - alertmanager:9093

# Load rules
rule_files:
  - "alerts/*.yml"

# Scrape configurations
scrape_configs:
  # Prometheus itself
  - job_name: 'prometheus'
    static_configs:
      - targets: ['localhost:9090']

  # Node exporters on cluster nodes
  - job_name: 'node'
    static_configs:
      - targets:
          - 'node1:9100'
          - 'node2:9100'
          - 'node3:9100'
    relabel_configs:
      - source_labels: [__address__]
        target_label: instance
        regex: '([^:]+):.*'
        replacement: '${1}'

  # WireGuard exporters
  - job_name: 'wireguard'
    static_configs:
      - targets:
          - 'node1:9586'
          - 'node2:9586'
          - 'node3:9586'

  # SLURM exporter
  - job_name: 'slurm'
    static_configs:
      - targets:
          - 'node1:8080'  # SLURM controller node
    scrape_interval: 30s

  # Kubernetes metrics
  - job_name: 'kubernetes-apiservers'
    kubernetes_sd_configs:
      - role: endpoints
    scheme: https
    tls_config:
      ca_file: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
    bearer_token_file: /var/run/secrets/kubernetes.io/serviceaccount/token
    relabel_configs:
      - source_labels: [__meta_kubernetes_namespace, __meta_kubernetes_service_name, __meta_kubernetes_endpoint_port_name]
        action: keep
        regex: default;kubernetes;https

  # Kubernetes nodes
  - job_name: 'kubernetes-nodes'
    kubernetes_sd_configs:
      - role: node
    scheme: https
    tls_config:
      ca_file: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
    bearer_token_file: /var/run/secrets/kubernetes.io/serviceaccount/token
    relabel_configs:
      - action: labelmap
        regex: __meta_kubernetes_node_label_(.+)

  # Kubernetes pods
  - job_name: 'kubernetes-pods'
    kubernetes_sd_configs:
      - role: pod
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_scrape]
        action: keep
        regex: true
      - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_path]
        action: replace
        target_label: __metrics_path__
        regex: (.+)
      - source_labels: [__address__, __meta_kubernetes_pod_annotation_prometheus_io_port]
        action: replace
        regex: ([^:]+)(?::\d+)?;(\d+)
        replacement: $1:$2
        target_label: __address__

  # cAdvisor
  - job_name: 'cadvisor'
    static_configs:
      - targets:
          - 'cadvisor:8080'
```

### Step 3: Create Alert Rules

Create `prometheus/alerts/cluster_alerts.yml`:

```yaml
groups:
  - name: cluster_health
    interval: 30s
    rules:
      # Node down
      - alert: NodeDown
        expr: up{job="node"} == 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "Node {{ $labels.instance }} is down"
          description: "Node has been down for more than 5 minutes"

      # SLURM node down
      - alert: SLURMNodeDown
        expr: slurm_nodes_down > 0
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "SLURM nodes are down"
          description: "{{ $value }} SLURM nodes are in DOWN state"

      # Kubernetes node not ready
      - alert: KubernetesNodeNotReady
        expr: kube_node_status_condition{condition="Ready",status="true"} == 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "Kubernetes node {{ $labels.node }} not ready"
          description: "Node has been not ready for >5 minutes"

      # WireGuard peer down
      - alert: WireGuardPeerDown
        expr: wireguard_peers < 2
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "WireGuard peer count low"
          description: "Only {{ $value }} peers connected"
```

### Step 4: Configure AlertManager

Create `alertmanager/alertmanager.yml`:

```yaml
global:
  resolve_timeout: 5m

# Route alerts
route:
  group_by: ['alertname', 'cluster']
  group_wait: 10s
  group_interval: 10s
  repeat_interval: 12h
  receiver: 'default'
  routes:
    - match:
        severity: critical
      receiver: 'critical'
    - match:
        severity: warning
      receiver: 'warning'

# Receivers
receivers:
  - name: 'default'
    email_configs:
      - to: 'ops@example.com'
        from: 'alertmanager@cluster-os.local'
        smarthost: 'smtp.example.com:587'
        auth_username: 'alertmanager@cluster-os.local'
        auth_password: 'password'

  - name: 'critical'
    email_configs:
      - to: 'oncall@example.com'
        from: 'alertmanager@cluster-os.local'
    slack_configs:
      - api_url: 'https://hooks.slack.com/services/YOUR/SLACK/WEBHOOK'
        channel: '#critical-alerts'
        title: 'Critical Alert'
        text: '{{ range .Alerts }}{{ .Annotations.description }}{{ end }}'

  - name: 'warning'
    slack_configs:
      - api_url: 'https://hooks.slack.com/services/YOUR/SLACK/WEBHOOK'
        channel: '#warnings'
        title: 'Warning'
        text: '{{ range .Alerts }}{{ .Annotations.description }}{{ end }}'

# Inhibit rules
inhibit_rules:
  - source_match:
      severity: 'critical'
    target_match:
      severity: 'warning'
    equal: ['alertname', 'instance']
```

### Step 5: Configure Grafana Data Sources

Create `grafana/provisioning/datasources/prometheus.yml`:

```yaml
apiVersion: 1

datasources:
  - name: Prometheus
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
    editable: true
```

### Step 6: Start Monitoring Stack

```bash
# Create necessary directories
mkdir -p prometheus/alerts
mkdir -p alertmanager
mkdir -p grafana/provisioning/datasources

# Start stack
docker-compose up -d

# Check status
docker-compose ps

# View logs
docker-compose logs -f
```

### Step 7: Install Exporters on Cluster Nodes

**On each cluster node**, install exporters:

```bash
# Install node-exporter
sudo apt-get install -y prometheus-node-exporter
sudo systemctl enable --now prometheus-node-exporter

# Install SLURM exporter (on SLURM nodes)
git clone https://github.com/vpenso/prometheus-slurm-exporter
cd prometheus-slurm-exporter
go build
sudo cp prometheus-slurm-exporter /usr/local/bin/
sudo systemctl enable --now slurm-exporter

# Install WireGuard exporter
# See MONITORING_CLUSTER.md for details
```

## Unified Dashboard Design

### Main Overview Dashboard

Import or create a dashboard with these panels:

**Row 1: Cluster Health**
- Total nodes
- Nodes alive/down
- WireGuard peers connected
- Cluster membership status

**Row 2: Resource Utilization**
- Total CPU usage (gauge)
- Total memory usage (gauge)
- Total disk usage (gauge)
- Network traffic (graph)

**Row 3: SLURM Status**
- SLURM nodes (idle/allocated/down)
- Jobs (running/pending)
- CPU utilization
- Memory utilization

**Row 4: Kubernetes Status**
- K8s nodes (ready/not ready)
- Pods (running/pending/failed)
- Container restarts
- API server requests

**Row 5: Network Health**
- WireGuard handshake age
- Packet loss
- Latency graph
- Bandwidth usage

### Dashboard JSON Template

Save as `grafana/dashboards/cluster-overview.json`:

```json
{
  "dashboard": {
    "title": "Cluster-OS Overview",
    "tags": ["cluster-os", "overview"],
    "timezone": "browser",
    "panels": [
      {
        "id": 1,
        "title": "Total Nodes",
        "type": "singlestat",
        "targets": [
          {
            "expr": "count(up{job=\"node\"})",
            "legendFormat": "Nodes"
          }
        ]
      },
      {
        "id": 2,
        "title": "Nodes Alive",
        "type": "singlestat",
        "targets": [
          {
            "expr": "count(up{job=\"node\"} == 1)",
            "legendFormat": "Alive"
          }
        ]
      },
      {
        "id": 3,
        "title": "CPU Usage",
        "type": "graph",
        "targets": [
          {
            "expr": "100 - (avg(rate(node_cpu_seconds_total{mode=\"idle\"}[5m])) * 100)",
            "legendFormat": "CPU %"
          }
        ]
      },
      {
        "id": 4,
        "title": "SLURM Jobs",
        "type": "graph",
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
      },
      {
        "id": 5,
        "title": "K8s Pods",
        "type": "graph",
        "targets": [
          {
            "expr": "sum(kube_pod_status_phase{phase=\"Running\"})",
            "legendFormat": "Running"
          },
          {
            "expr": "sum(kube_pod_status_phase{phase=\"Pending\"})",
            "legendFormat": "Pending"
          }
        ]
      }
    ]
  }
}
```

## Access Monitoring Stack

### Web Interfaces

```bash
# Prometheus
http://localhost:9090

# Grafana
http://localhost:3000
# Default credentials: admin / admin

# AlertManager
http://localhost:9093
```

### Import Dashboards

1. **Login to Grafana** (http://localhost:3000)

2. **Import recommended dashboards**:
   - Node Exporter Full (ID: 1860)
   - SLURM Dashboard (ID: 4323)
   - Kubernetes Cluster Monitoring (ID: 7249)
   - Kubernetes Pod Resources (ID: 6417)

3. **Import custom dashboard**:
   - Create JSON from template above
   - Import via UI

## Monitoring Workflows

### Daily Health Check

```bash
# Quick cluster overview
curl -s http://localhost:9090/api/v1/query?query=up | jq

# Check alerts
curl -s http://localhost:9093/api/v1/alerts | jq

# View active alerts in Grafana
# Navigate to Alerting → Alert Rules
```

### Weekly Review

1. Review CPU/memory trends
2. Check for capacity issues
3. Review alert frequency
4. Update thresholds if needed

### Monthly Report

Generate monthly report:

```bash
#!/bin/bash
# generate_monthly_report.sh

MONTH=$(date +%Y-%m)

echo "=== Cluster-OS Monthly Report ($MONTH) ==="
echo ""

# Average CPU usage
echo "Average CPU Usage:"
curl -s "http://localhost:9090/api/v1/query?query=avg_over_time((100-avg(rate(node_cpu_seconds_total{mode=\"idle\"}[5m])))[30d:])" | jq -r '.data.result[0].value[1]'

# Average memory usage
echo "Average Memory Usage:"
curl -s "http://localhost:9090/api/v1/query?query=avg_over_time((1-(node_memory_MemAvailable_bytes/node_memory_MemTotal_bytes))[30d:])" | jq -r '.data.result[0].value[1]'

# Total jobs run (SLURM)
echo "Total SLURM Jobs:"
curl -s "http://localhost:9090/api/v1/query?query=sum(increase(slurm_jobs_completed[30d]))" | jq -r '.data.result[0].value[1]'

# Uptime
echo "Node Uptime:"
curl -s "http://localhost:9090/api/v1/query?query=node_time_seconds-node_boot_time_seconds" | jq -r '.data.result[] | "\(.metric.instance): \(.value[1]/86400) days"'
```

## Troubleshooting

### Prometheus Not Scraping

```bash
# Check Prometheus targets
curl http://localhost:9090/api/v1/targets

# Check connectivity
docker exec prometheus wget -qO- http://node1:9100/metrics

# View Prometheus logs
docker logs prometheus
```

### Grafana Dashboard Empty

```bash
# Check data source
curl http://localhost:3000/api/datasources

# Test Prometheus query
curl "http://localhost:3000/api/datasources/proxy/1/api/v1/query?query=up"

# Check Grafana logs
docker logs grafana
```

### Alerts Not Firing

```bash
# Check alert rules
curl http://localhost:9090/api/v1/rules

# Check AlertManager
curl http://localhost:9093/api/v1/alerts

# View AlertManager logs
docker logs alertmanager
```

## Best Practices

1. **Retention Policy**:
   - Keep 30 days in Prometheus
   - Archive older data to long-term storage

2. **Alert Tuning**:
   - Adjust thresholds based on baseline
   - Reduce alert fatigue
   - Use inhibition rules

3. **Dashboard Organization**:
   - Create role-specific dashboards
   - Use folders for organization
   - Tag dashboards appropriately

4. **Security**:
   - Change default Grafana password
   - Use HTTPS in production
   - Restrict access with firewall

5. **Backup**:
   - Backup Grafana dashboards
   - Export Prometheus data
   - Save AlertManager configuration

## Summary

Complete monitoring setup provides:
- ✅ Unified view of all cluster components
- ✅ Real-time and historical metrics
- ✅ Automated alerting
- ✅ Custom dashboards
- ✅ Integration with SLURM and Kubernetes

**Next Steps**:
1. Deploy monitoring stack
2. Install exporters on nodes
3. Import dashboards
4. Configure alerts
5. Set up notifications

For detailed component monitoring:
- [SLURM Monitoring](MONITORING_SLURM.md)
- [Kubernetes Monitoring](MONITORING_KUBERNETES.md)
- [Cluster Monitoring](MONITORING_CLUSTER.md)
