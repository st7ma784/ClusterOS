# Cluster Ring and Node Monitoring Guide

This guide covers monitoring the Cluster-OS mesh network, node discovery, and cluster membership (the "ring").

## Overview

Cluster-OS uses a combination of technologies for cluster formation:
- **Serf** - Gossip-based membership and failure detection
- **WireGuard** - Encrypted mesh networking
- **Raft** - Leader election (for roles)
- **Node Agent** - Cluster orchestration

## Quick Status Commands

### Cluster Membership (Serf)

```bash
# View all cluster members
sudo serf members

# View members with detailed status
sudo serf members -detailed

# View only alive members
sudo serf members -status=alive

# View members in specific role
sudo serf members -tag role=slurm-controller
```

**Expected Output**:
```
node1    10.0.0.1:7946    alive    role=slurm-controller,dc=datacenter1
node2    10.0.0.2:7946    alive    role=slurm-worker,dc=datacenter1
node3    10.0.0.3:7946    alive    role=k8s-worker,dc=datacenter1
```

**Member States**:
- `alive` - Node is healthy and responding
- `left` - Node gracefully left the cluster
- `failed` - Node failed to respond

### WireGuard Mesh Status

```bash
# View WireGuard interface status
sudo wg show

# View specific interface
sudo wg show wg0

# View in detail
sudo wg show all dump

# Check WireGuard interface
ip addr show wg0

# Show WireGuard peers
sudo wg show wg0 peers
```

**Expected Output**:
```
interface: wg0
  public key: <key>
  private key: (hidden)
  listening port: 51820

peer: <peer-public-key>
  endpoint: 192.168.1.100:51820
  allowed ips: 10.42.92.0/24
  latest handshake: 45 seconds ago
  transfer: 1.25 MiB received, 892 KiB sent
```

### Node Agent Status

```bash
# Check node-agent service
sudo systemctl status node-agent

# View node identity
sudo cat /var/lib/cluster-os/identity/node_id

# View cluster state
sudo cat /var/lib/cluster-os/cluster-state.json | jq

# View assigned roles
sudo cat /var/lib/cluster-os/roles.json | jq
```

### Network Connectivity

```bash
# Ping all WireGuard peers
for ip in $(sudo wg show wg0 allowed-ips | awk '{print $2}' | cut -d/ -f1); do
    echo "Pinging $ip..."
    ping -c 3 $ip
done

# Test connectivity to specific node
ping -c 5 <wireguard-ip>

# Trace route through mesh
traceroute <wireguard-ip>

# Check listening ports
sudo ss -tulpn | grep -E '7946|51820|6443'
```

## Detailed Monitoring

### Serf Cluster Health

```bash
# Check Serf agent status
sudo serf agent -rpc-addr=127.0.0.1:7373 members

# Query cluster events
sudo serf event list

# Monitor Serf logs
sudo journalctl -u serf -f

# Check gossip protocol stats
sudo serf monitor -log-level=debug
```

### WireGuard Performance

```bash
# Monitor WireGuard traffic
watch -n 1 'sudo wg show wg0 transfer'

# Check WireGuard handshakes
watch -n 5 'sudo wg show wg0 latest-handshakes'

# Monitor WireGuard interface stats
watch -n 2 'ip -s link show wg0'

# Check for packet loss
ping -c 100 <peer-ip> | grep loss
```

### Node Discovery Timeline

```bash
# View node-agent logs for discovery events
sudo journalctl -u node-agent | grep -i discovery

# View join events
sudo journalctl -u node-agent | grep -i "joined cluster"

# View peer discovery
sudo journalctl -u node-agent | grep -i "discovered peer"

# Timeline of cluster formation
sudo journalctl -u node-agent --since today | grep -E "discovery|join|peer"
```

## Prometheus Metrics

### Install Node Exporter

```bash
# Install Prometheus node-exporter
sudo apt-get install -y prometheus-node-exporter

# Or run in Docker
docker run -d \
  --name node-exporter \
  --net="host" \
  --pid="host" \
  -v "/:/host:ro,rslave" \
  quay.io/prometheus/node-exporter:latest \
  --path.rootfs=/host

# Verify
curl http://localhost:9100/metrics
```

### WireGuard Exporter

```bash
# Install WireGuard exporter
git clone https://github.com/MindFlavor/prometheus_wireguard_exporter
cd prometheus_wireguard_exporter
cargo build --release

# Create systemd service
sudo tee /etc/systemd/system/wireguard-exporter.service << EOF
[Unit]
Description=WireGuard Prometheus Exporter
After=network.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/prometheus_wireguard_exporter -a true -n /etc/wireguard/wg0.conf
Restart=always

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now wireguard-exporter
```

### Key Metrics to Monitor

**Node Metrics**:
- `node_uname_info` - Node information
- `node_load1` - 1-minute load average
- `node_cpu_seconds_total` - CPU usage
- `node_memory_MemAvailable_bytes` - Available memory
- `node_disk_io_time_seconds_total` - Disk I/O
- `node_network_receive_bytes_total` - Network RX
- `node_network_transmit_bytes_total` - Network TX

**WireGuard Metrics**:
- `wireguard_latest_handshake_seconds` - Last handshake time
- `wireguard_sent_bytes_total` - Bytes sent
- `wireguard_received_bytes_total` - Bytes received
- `wireguard_peers` - Number of peers

**Custom Node-Agent Metrics** (if implemented):
- `cluster_os_nodes_total` - Total nodes in cluster
- `cluster_os_nodes_alive` - Alive nodes
- `cluster_os_wireguard_peers` - WireGuard peers
- `cluster_os_roles_assigned` - Roles assigned

## Monitoring Scripts

### Cluster Health Check

```bash
#!/bin/bash
# check_cluster_health.sh - Comprehensive cluster health check

echo "=== Cluster-OS Health Check ==="
echo "Date: $(date)"
echo ""

# Check Serf membership
echo "Cluster Membership:"
TOTAL_MEMBERS=$(sudo serf members | grep -c "alive")
echo "  Total members: $TOTAL_MEMBERS"

FAILED_MEMBERS=$(sudo serf members | grep -c "failed")
if [ "$FAILED_MEMBERS" -gt 0 ]; then
    echo "  ⚠ WARNING: $FAILED_MEMBERS failed members"
    sudo serf members | grep "failed"
fi

echo ""

# Check WireGuard peers
echo "WireGuard Mesh:"
WG_PEERS=$(sudo wg show wg0 peers | wc -l)
echo "  Connected peers: $WG_PEERS"

# Check for stale handshakes (>3 minutes)
echo "  Checking handshakes..."
sudo wg show wg0 latest-handshakes | while read peer timestamp; do
    AGE=$(($(date +%s) - timestamp))
    if [ $AGE -gt 180 ]; then
        echo "  ⚠ Peer $peer: last handshake ${AGE}s ago"
    fi
done

echo ""

# Check network connectivity
echo "Network Connectivity:"
for ip in $(sudo wg show wg0 allowed-ips | awk '{print $2}' | cut -d/ -f1); do
    if ping -c 1 -W 2 $ip &>/dev/null; then
        echo "  ✓ $ip reachable"
    else
        echo "  ✗ $ip unreachable"
    fi
done

echo ""

# Check node-agent status
echo "Node Agent:"
if systemctl is-active --quiet node-agent; then
    echo "  ✓ Running"
else
    echo "  ✗ Not running"
fi
```

### WireGuard Tunnel Monitor

```bash
#!/bin/bash
# monitor_wireguard.sh - Monitor WireGuard tunnel health

echo "=== WireGuard Tunnel Monitor ==="
echo ""

sudo wg show wg0 | awk '
    /^peer:/ { peer=$2 }
    /endpoint:/ { endpoint=$2 }
    /latest handshake:/ {
        # Parse handshake time
        if ($0 ~ /seconds ago/) {
            handshake = $3
        } else if ($0 ~ /minute/) {
            handshake = $3 * 60
        } else {
            handshake = 9999
        }

        status = (handshake < 180) ? "✓ OK" : "⚠ STALE"
        printf "Peer: %s\n", substr(peer, 1, 16)
        printf "  Endpoint: %s\n", endpoint
        printf "  Handshake: %s (%ds ago)\n", status, handshake
        print ""
    }
'
```

### Network Latency Check

```bash
#!/bin/bash
# check_mesh_latency.sh - Check latency to all mesh peers

echo "=== Mesh Network Latency ==="
echo ""

for ip in $(sudo wg show wg0 allowed-ips | awk '{print $2}' | cut -d/ -f1); do
    if [ "$ip" != "10.42.92.0" ]; then  # Skip network address
        echo "Peer: $ip"
        ping -c 5 -q $ip | tail -2
        echo ""
    fi
done
```

### Cluster Topology Map

```bash
#!/bin/bash
# show_topology.sh - Show cluster topology

echo "=== Cluster Topology ==="
echo ""

echo "Nodes:"
sudo serf members -format=json | jq -r '.members[] | "\(.name) - \(.status) - \(.addr) - \(.tags.role)"'

echo ""
echo "WireGuard Mesh:"
sudo wg show wg0 dump | awk '{
    if (NR==1) {
        print "Local public key:", $2
        print "Listen port:", $3
    } else {
        print "Peer:", $1
        print "  Endpoint:", $3
        print "  Allowed IPs:", $4
        print ""
    }
}'
```

## Grafana Dashboards

### Node Exporter Dashboard

Import **Node Exporter Full** (Dashboard ID: `1860`):
- CPU usage
- Memory usage
- Disk I/O
- Network traffic
- System load

### WireGuard Dashboard

Create custom dashboard with panels for:

```json
{
  "panels": [
    {
      "title": "WireGuard Peers",
      "targets": [{
        "expr": "wireguard_peers",
        "legendFormat": "Peers"
      }]
    },
    {
      "title": "Handshake Age",
      "targets": [{
        "expr": "time() - wireguard_latest_handshake_seconds",
        "legendFormat": "{{public_key}}"
      }]
    },
    {
      "title": "Traffic",
      "targets": [
        {
          "expr": "rate(wireguard_sent_bytes_total[5m])",
          "legendFormat": "Sent - {{public_key}}"
        },
        {
          "expr": "rate(wireguard_received_bytes_total[5m])",
          "legendFormat": "Received - {{public_key}}"
        }
      ]
    }
  ]
}
```

### Cluster Overview Dashboard

Custom dashboard showing:
- Total cluster members
- Member status distribution
- WireGuard peer count
- Network latency heatmap
- Role distribution

## Alerting Rules

### Prometheus Alert Rules

```yaml
# cluster_alerts.yml
groups:
  - name: cluster
    interval: 30s
    rules:
      # Node unreachable
      - alert: ClusterNodeUnreachable
        expr: up{job="node"} == 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "Cluster node unreachable"
          description: "Node {{ $labels.instance }} has been unreachable for >5 minutes"

      # WireGuard peer down
      - alert: WireGuardPeerDown
        expr: wireguard_peers < 2
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "WireGuard peer count low"
          description: "Only {{ $value }} WireGuard peers connected"

      # Stale WireGuard handshake
      - alert: WireGuardStaleHandshake
        expr: (time() - wireguard_latest_handshake_seconds) > 300
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "WireGuard handshake is stale"
          description: "Peer {{ $labels.public_key }} handshake is {{ $value }}s old"

      # High network latency
      - alert: HighNetworkLatency
        expr: node_network_latency_seconds > 0.1
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "High network latency detected"
          description: "Latency to {{ $labels.peer }} is {{ $value }}s"

      # Node high CPU
      - alert: NodeHighCPU
        expr: (100 - (avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100)) > 80
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "Node high CPU usage"
          description: "Node {{ $labels.instance }} CPU is {{ $value }}%"

      # Node high memory
      - alert: NodeHighMemory
        expr: (1 - (node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)) * 100 > 90
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Node high memory usage"
          description: "Node {{ $labels.instance }} memory is {{ $value }}%"

      # Cluster size changed
      - alert: ClusterSizeChanged
        expr: changes(cluster_os_nodes_total[10m]) > 0
        for: 1m
        labels:
          severity: info
        annotations:
          summary: "Cluster size changed"
          description: "Cluster size changed to {{ $value }} nodes"
```

## Network Debugging

### WireGuard Troubleshooting

```bash
# Check WireGuard configuration
sudo wg showconf wg0

# Verify routing
ip route show table all | grep wg0

# Check firewall rules
sudo iptables -L -n -v | grep wg0
sudo ufw status

# Test UDP connectivity (WireGuard port)
nc -vuz <peer-ip> 51820

# Enable WireGuard debugging
echo module wireguard +p | sudo tee /sys/kernel/debug/dynamic_debug/control
sudo dmesg -w | grep wireguard
```

### Serf Troubleshooting

```bash
# Check Serf configuration
cat /etc/serf/config.json

# Test Serf RPC
sudo serf members -rpc-addr=127.0.0.1:7373

# Force rejoin
sudo serf force-leave <node-name>
sudo serf join <node-ip>:7946

# Enable verbose logging
sudo serf monitor -log-level=debug
```

### Network Path Testing

```bash
# MTU path discovery
ping -M do -s 1472 <peer-ip>

# Bandwidth testing (iperf3)
# On server:
iperf3 -s

# On client:
iperf3 -c <server-ip>

# Packet capture
sudo tcpdump -i wg0 -w /tmp/wg0.pcap
```

## Log Analysis

### Important Log Locations

```bash
# Node agent logs
sudo journalctl -u node-agent

# Serf logs
sudo journalctl -u serf

# WireGuard kernel logs
sudo dmesg | grep wireguard

# System logs
sudo journalctl --since "1 hour ago"
```

### Log Monitoring Commands

```bash
# Watch node-agent logs
sudo journalctl -u node-agent -f

# Watch for errors
sudo journalctl -u node-agent -f | grep -i error

# View cluster join events
sudo journalctl -u node-agent | grep -i "join\|member"

# View WireGuard events
sudo dmesg -w | grep wireguard
```

## Best Practices

1. **Regular Health Checks**:
   - Run cluster health script daily
   - Monitor WireGuard handshakes
   - Check Serf membership

2. **Automated Monitoring**:
   - Deploy Prometheus + Grafana
   - Configure alerting rules
   - Set up notification channels

3. **Network Maintenance**:
   - Monitor bandwidth usage
   - Check for packet loss
   - Verify MTU settings

4. **Documentation**:
   - Document network topology
   - Track node additions/removals
   - Record configuration changes

## Troubleshooting Common Issues

### Node Not Joining Cluster

```bash
# Check Serf connectivity
telnet <bootstrap-node> 7946

# Check cluster key
sudo cat /etc/cluster-os/cluster.key

# Restart node-agent
sudo systemctl restart node-agent

# View join errors
sudo journalctl -u node-agent | grep -i "join\|error"
```

### WireGuard Peer Not Connecting

```bash
# Check WireGuard status
sudo wg show

# Verify endpoint reachability
ping <peer-endpoint-ip>

# Check firewall
sudo ufw status
sudo ufw allow 51820/udp

# Restart WireGuard
sudo wg-quick down wg0
sudo wg-quick up wg0
```

### High Network Latency

```bash
# Check for packet loss
mtr <peer-ip>

# Test bandwidth
iperf3 -c <peer-ip>

# Check interface errors
ip -s link show wg0

# Monitor real-time latency
ping -D <peer-ip>
```

## Summary

Effective cluster monitoring involves:
- ✅ Monitoring Serf membership
- ✅ Tracking WireGuard mesh health
- ✅ Alerting on connectivity issues
- ✅ Regular health checks
- ✅ Network performance monitoring

For more information:
- [Serf Documentation](https://www.serf.io/docs/)
- [WireGuard Documentation](https://www.wireguard.com/)
- [Prometheus Node Exporter](https://github.com/prometheus/node_exporter)
