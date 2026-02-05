# ClusterOS Dashboard and Cluster Management Implementation

## Overview

This document describes the enhanced dashboard functionality and addresses the requirements for zero-touch cluster setup, leader handoff, and cluster merging.

## Completed: Dashboard API (Commit 4680c21)

### HTTP API Endpoints

The dashboard API is available on port 9090 with the following endpoints:

#### GET /api/v1/status
Comprehensive cluster status including:
- Node information (ID, name, cluster, region)
- Network status (Tailscale connectivity and IP)
- Discovery status (Serf members)
- Leadership information (current leader, is_leader status)
- Role statuses (running, healthy, leader status per role)
- System service status (SLURM, k3s, Apache, ttyd)
- Resource usage (CPU, memory, disk)

#### GET /api/v1/nodes
All known nodes in the cluster with:
- Node ID, name, roles, status
- Network addresses (Tailscale IP, WireGuard IP)
- Capabilities (CPU, RAM, GPU, architecture)
- Tags and last_seen timestamps
- Join time

#### GET /api/v1/leaders
Current leader for each role:
- slurm-controller
- k3s-server
- raft

Shows leader node ID, name, address, and Tailscale IP for each role.

#### GET /api/v1/services
Detailed service connectivity status:

**Tailscale:**
- Connected status
- Local IP address
- Peer count

**SLURM:**
- Controller status (running/stopped/error)
- Worker status (running/stopped/error)
- Nodes in partition
- Queued jobs count

**K3s:**
- Server status (running/stopped/error)
- Agent status (running/stopped/error)
- Kubernetes node count
- Pod count across all namespaces

#### GET /api/v1/cluster
Cluster-wide information:
- Cluster name, region, datacenter
- Total nodes and alive nodes
- Capacity metrics (CPU cores, RAM, GPUs)
- Role distribution (nodes per role)

#### GET /api/v1/jobs
Running and queued jobs:

**SLURM Jobs:**
- Job ID, name, state, user, runtime
- Total job count

**K3s Pods:**
- Pod name, namespace, phase
- Total pod count

### Enhanced CLI

The `node-agent status` command now queries the API and displays:
- Node information
- Network connectivity (Tailscale status)
- Leadership information (leader ID, is_leader flag)
- Role statuses (running, healthy, leader per role)
- Discovery information (cluster member count)

## SLURM/k3s Tailscale Network Review

### Current Implementation ✅

Both SLURM and k3s are properly configured to use Tailscale networking:

**K3s Server Configuration:**
```go
// In daemon.go line 643:
if d.tailscale != nil && (roleName == "k3s-server" || roleName == "k3s-agent") {
    roleConfig.Config["node_ip"] = d.tailscale.GetLocalIP().String()
}
```

**K3s Server Start:**
```go
// Sets --node-ip and --advertise-address to Tailscale IP
args = append(args, "--node-ip", ks.config.NodeIP)
args = append(args, "--advertise-address", ks.config.NodeIP)
```

**SLURM Controller:**
```go
// Uses Tailscale IP for node lookups
if node.TailscaleIP != "" {
    return node.TailscaleIP
}
```

### Findings ✅

1. **K3s properly uses Tailscale IPs** for node communication
2. **SLURM properly resolves nodes via Tailscale** 
3. **Flannel is disabled** - Tailscale handles node-to-node connectivity
4. **Cluster join URLs use Tailscale IPs** for k3s server joining

### No Critical Issues Found

The implementation correctly:
- Waits for Tailscale to be ready before starting services
- Uses Tailscale IPs for all inter-node communication
- Handles node discovery via Tailscale peer list
- Configures services to advertise Tailscale addresses

## Zero-Touch Cluster Setup ✅

### Current Flow

1. **Boot from live image**
   - cloud-init installs WiFi packages and configures network
   - netplan connects to WiFi (SSID: TALKTALK665317)
   - Tailscale daemon starts automatically
   
2. **Tailscale authentication**
   - `tailscale-auth.service` runs on first boot
   - Uses OAuth to generate ephemeral auth key
   - Authenticates with Tailscale automatically
   - Falls back to static auth key if OAuth not configured

3. **Node agent startup**
   - `wait-for-tailscale` polls until Tailscale is connected
   - `node-agent.service` starts
   - Discovers other nodes via Tailscale peer discovery
   - Joins Serf gossip cluster
   - Participates in leader election

4. **Role assignment**
   - Node evaluates enabled roles from config
   - Starts SLURM worker, k3s agent by default
   - Competes for controller/server roles via leader election
   - Automatically configures based on elected role

### Validation Checklist

- [x] WiFi auto-connects on boot
- [x] Tailscale auto-authenticates
- [x] Node agent auto-starts
- [x] Peer discovery works
- [x] Leader election occurs automatically
- [x] Services start based on role
- [x] No user input required

## Leader Handoff Without Disruption

### Current State

**SLURM Controller:**
- HA mode enabled by default
- Supports backup controller configuration
- Munge keys replicated via Raft
- Needs: Graceful slurmctld handoff

**K3s Server:**
- Multi-server HA mode enabled
- Embedded etcd with quorum (3+ servers)
- API server requests go to all servers
- Needs: Ensure smooth etcd member changes

### Recommendations for Seamless Handoff

#### SLURM Controller Handoff

1. **Pre-handoff state sync:**
   ```
   - New controller joins as backup
   - Syncs slurm.conf from primary
   - Receives Munge key via Raft
   - Starts slurmctld in standby mode
   ```

2. **Graceful transition:**
   ```
   - Primary announces intent to step down
   - Drains new job submissions (send to backup)
   - Allows running jobs to continue
   - Backup promotes to primary
   - Primary demotes to backup or stops
   ```

3. **Implementation needed:**
   - Add `PrepareHandoff()` method to SLURMController
   - Implement job submission redirection
   - Add backup controller sync mechanism
   - Use Raft for coordinated transition

#### K3s Server Handoff

1. **Current HA setup:**
   - Multiple servers share etcd cluster
   - Kube-apiserver runs on all servers
   - LoadBalancer distributes requests
   - Embedded etcd handles leader election

2. **Node departure handling:**
   - Before node leaves, remove from etcd cluster
   - Drain pods from node
   - Remove node from Kubernetes
   - Update Serf membership

3. **Implementation needed:**
   - Add `PreShutdown()` hook in K3sServer role
   - Implement node drain before departure
   - Add etcd member remove before shutdown
   - Coordinate with cluster state

### Proposed Implementation

```go
// In roles/role.go - add to Role interface:
type Role interface {
    // ... existing methods ...
    
    // PrepareHandoff prepares the role for leader transition
    PrepareHandoff(ctx context.Context, newLeader string) error
    
    // CompleteHandoff completes the handoff after new leader is ready
    CompleteHandoff(ctx context.Context) error
}

// In daemon - coordinate handoff:
func (d *Daemon) CoordinatedLeaderTransition(role string, newLeaderID string) error {
    // 1. Get current leader role instance
    currentRole := d.roleManager.GetRole(role)
    
    // 2. Prepare current leader for handoff
    if err := currentRole.PrepareHandoff(ctx, newLeaderID); err != nil {
        return err
    }
    
    // 3. Wait for new leader to be ready
    d.waitForLeaderReady(role, newLeaderID)
    
    // 4. Complete handoff on old leader
    if err := currentRole.CompleteHandoff(ctx); err != nil {
        return err
    }
    
    // 5. Update Raft/Serf with new leader
    d.updateLeaderState(role, newLeaderID)
    
    return nil
}
```

## Cluster Merging

### Scenario

Two separate ClusterOS clusters, each with multiple nodes, discover each other when a new node with connectivity to both joins.

### Challenges

1. **Conflicting leaders:** Both clusters have their own SLURM controller and k3s server
2. **Different Munge keys:** SLURM clusters use different authentication
3. **Separate k3s tokens:** K8s clusters have different join tokens
4. **Job preservation:** Running jobs should not be disrupted
5. **State reconciliation:** Raft logs may conflict

### Proposed Solution

#### Detection Phase

```
Node joins both networks (e.g., connects via shared Tailscale tailnet)
├── Discovers nodes from Cluster A via Serf
├── Discovers nodes from Cluster B via Serf
└── Detects cluster split: different cluster.auth_key values
```

#### Decision Phase

```
Determine merge strategy based on cluster sizes:
├── Larger cluster becomes primary (more nodes = more resources)
├── Smaller cluster migrates to primary
└── If equal size, older cluster (earliest JoinedAt) becomes primary
```

#### Merge Execution

**Phase 1: Coordination (5 minutes)**
```
1. Bridge node announces merge proposal to both clusters
2. Leaders from both clusters coordinate via bridge node
3. Primary cluster designated based on decision criteria
4. Secondary cluster prepares for migration
```

**Phase 2: Service Migration (10-15 minutes)**
```
SLURM:
1. Secondary controller drains job submissions
2. Running jobs allowed to complete or migrated
3. Secondary workers reconfigure to primary controller
4. Primary controller adopts secondary's Munge key (backward compat)
5. Unified slurm.conf generated with all nodes

K3s:
1. Secondary server demotes (stops accepting new workloads)
2. Secondary workers rejoin primary server
3. Pods on secondary server drained to other nodes
4. Secondary server joins primary as agent (optional)
```

**Phase 3: State Unification (5 minutes)**
```
1. Raft logs merged (primary keeps log, secondary discards)
2. Cluster state unified (all nodes report to primary)
3. Serf clusters merge (gossip now includes all nodes)
4. Service endpoints updated (dashboard shows unified cluster)
```

**Phase 4: Verification (2 minutes)**
```
1. All nodes see unified member list
2. Single SLURM controller manages all workers
3. Single k3s control plane manages all agents
4. Jobs continue running without disruption
```

### Implementation Requirements

```go
// In discovery/serf.go - detect cluster split:
func (sd *SerfDiscovery) DetectClusterSplit() (*ClusterSplitInfo, error) {
    members := sd.serf.Members()
    
    authKeys := make(map[string][]string) // authKey -> []nodeIDs
    for _, member := range members {
        if authKey, ok := member.Tags["auth_key"]; ok {
            authKeys[authKey] = append(authKeys[authKey], member.Name)
        }
    }
    
    if len(authKeys) > 1 {
        return &ClusterSplitInfo{
            DetectedAt: time.Now(),
            Clusters:   authKeys,
        }, nil
    }
    
    return nil, nil
}

// In daemon.go - handle cluster merge:
func (d *Daemon) HandleClusterMerge(splitInfo *ClusterSplitInfo) error {
    // 1. Determine primary cluster (largest by node count)
    primaryAuthKey := d.selectPrimaryCluster(splitInfo)
    
    // 2. Check if we're in primary cluster
    if d.config.Cluster.AuthKey == primaryAuthKey {
        // We're primary - prepare to absorb secondary
        return d.absorbSecondaryCluster(splitInfo)
    } else {
        // We're secondary - prepare to join primary
        return d.joinPrimaryCluster(primaryAuthKey, splitInfo)
    }
}
```

### Safety Mechanisms

1. **Merge approval required:** Manual flag in config to enable merging
2. **Job preservation:** SLURM jobs must complete or be explicitly migrated
3. **Rollback capability:** Keep secondary cluster state for 24h in case rollback needed
4. **Gradual migration:** Move workers incrementally, not all at once
5. **Health checks:** Verify primary cluster stable before migrating

## Services Configuration

### Jupyter (via Open OnDemand)

Located in `node/internal/services/ondemand/ondemand.go`:
- Runs on controller node (leader-elected)
- Port 8080 by default
- Provides web interface for:
  - JupyterHub integration
  - SLURM job submission
  - File browser
  - Terminal access (ttyd)

### K3s / Rancher

Located in `node/internal/services/kubernetes/k3s/`:
- Server mode: HA with multiple servers (3+ for quorum)
- Agent mode: Workers connect to any server
- Rancher UI deployable via manifest
- Manages containerized workloads

### SLURM

Located in `node/internal/services/slurm/`:
- Controller: Leader-elected, HA with backup
- Worker: Runs on all nodes
- Submits batch jobs, MPI workloads
- Integrates with k3s via slurmdbd

## Testing the Dashboard

### Start the Node Agent

```bash
# In foreground for testing
sudo ./bin/node-agent start --foreground --log-level debug
```

### Query the API

```bash
# Comprehensive status
curl http://localhost:9090/api/v1/status | jq

# All nodes
curl http://localhost:9090/api/v1/nodes | jq

# Leader information
curl http://localhost:9090/api/v1/leaders | jq

# Service status
curl http://localhost:9090/api/v1/services | jq

# Cluster capacity
curl http://localhost:9090/api/v1/cluster | jq

# Job information
curl http://localhost:9090/api/v1/jobs | jq
```

### Use the CLI

```bash
# Display formatted status
./bin/node-agent status

# Show node info
./bin/node-agent info
```

## Next Steps

### Immediate (This PR)
- [x] Dashboard API implemented
- [x] Status endpoints working
- [x] CLI enhanced
- [x] SLURM/k3s Tailscale review complete

### Short Term (Follow-up PRs)
- [ ] Implement PrepareHandoff/CompleteHandoff for roles
- [ ] Add graceful SLURM job migration
- [ ] Implement k3s node drain before departure
- [ ] Add coordinated leader transition mechanism

### Medium Term
- [ ] Implement cluster split detection
- [ ] Add cluster merge proposal/coordination
- [ ] Implement service migration for merge
- [ ] Add safety mechanisms and rollback
- [ ] Test cluster merge scenarios

### Long Term
- [ ] Build web dashboard UI consuming API
- [ ] Add metrics and alerting integration
- [ ] Implement advanced job scheduling policies
- [ ] Add multi-cluster federation support

## Conclusion

The dashboard API provides comprehensive visibility into cluster state, node connectivity, leadership, and workload status. The SLURM and k3s implementations are correctly configured to use Tailscale for inter-node communication. Zero-touch setup works from live image to cluster join.

The remaining work focuses on:
1. Graceful leader handoff without job disruption
2. Cluster merging when split clusters discover each other
3. Enhanced monitoring and alerting capabilities

All core infrastructure is in place to support these features.
