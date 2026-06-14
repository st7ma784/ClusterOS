# Perimeter Firewall Rules — ClusterOS Home Lab

Threat model: a compromised cluster node (even at root) must not be able to
reach home LAN devices, arbitrary internet hosts, or non-cluster Tailscale
nodes.  The perimeter firewall enforces this at the network boundary; the
node-level iptables rules (see `setupFirewallRules()` in daemon.go) enforce it
inside the node itself.

---

## Network Topology

```
Internet
    │
Home Router  (e.g. 192.168.1.0/24, performs NAT)
    │
    ├─ Home LAN devices (laptops, IoT, etc.)  192.168.1.0/24
    │
SonicWall 2600
    ├─ X1 / WAN     ←→  home router LAN  (gets a 192.168.1.x address)
    ├─ X2 / CLUSTER     10.10.0.0/24     (cluster nodes live here)
    └─ X3 / MGMT        10.20.0.0/24     (your workstation / jump host)

Cluster nodes:  10.10.0.2 – 10.10.0.254
Tailscale CGNAT overlay:  100.64.0.0/10  (encrypted, rides on UDP 41641)
Pod CIDR (k3s Flannel):   10.42.0.0/16
Service CIDR (k3s):       10.43.0.0/16
```

If you only have two physical interfaces, move your management workstation to
the home LAN and adjust the MGMT rules to source from 192.168.1.x instead.

---

## Address / Service Objects

Create these objects in the SonicWall UI before building the policy.

### Address Objects

| Name              | Value              | Notes                              |
|-------------------|--------------------|------------------------------------|
| `cluster_nodes`   | 10.10.0.0/24       | All cluster nodes                  |
| `mgmt_station`    | 10.20.0.1          | Your management workstation        |
| `home_lan`        | 192.168.1.0/24     | Home router LAN (WAN-side)         |
| `tailscale_cgnat` | 100.64.0.0/10      | Tailscale overlay addresses        |
| `pod_cidr`        | 10.42.0.0/16       | k3s Flannel pod network            |
| `svc_cidr`        | 10.43.0.0/16       | k3s service network                |

### Service Objects / Groups

| Name               | Proto / Ports          | Purpose                              |
|--------------------|------------------------|--------------------------------------|
| `tailscale_direct` | UDP 41641              | Tailscale WireGuard direct path      |
| `tailscale_stun`   | UDP 3478               | STUN NAT traversal                   |
| `tailscale_derp`   | TCP 443                | Tailscale DERP relay (also HTTPS)    |
| `serf_gossip`      | TCP+UDP 7946           | Serf cluster membership              |
| `cluster_http`     | TCP 30080              | ingress-nginx NodePort HTTP          |
| `cluster_https`    | TCP 30443              | ingress-nginx NodePort HTTPS         |
| `rancher_ui`       | TCP 30444              | Rancher management UI                |
| `slurm_rest`       | TCP 30819              | SLURM REST API                       |
| `internet_egress`  | TCP 80, TCP 443        | Package repos, container registries  |
| `dns_ntp`          | UDP 53, TCP 53, UDP 123| DNS + NTP                            |

---

## Zone Policy — Rule Tables

Rules are evaluated top-down.  **The final rule in every direction is an
implicit DENY ALL** — the SonicWall default; do not remove it.

---

### 1. CLUSTER → WAN  (nodes egress to internet / home LAN)

| # | Action | Source        | Destination   | Service            | Comment                                     |
|---|--------|---------------|---------------|--------------------|---------------------------------------------|
| 1 | DENY   | cluster_nodes | home_lan      | ANY                | Block node→home lateral movement (CRITICAL) |
| 2 | ALLOW  | cluster_nodes | ANY           | tailscale_direct   | Tailscale WireGuard peer connections        |
| 3 | ALLOW  | cluster_nodes | ANY           | tailscale_stun     | STUN NAT traversal                          |
| 4 | ALLOW  | cluster_nodes | ANY           | tailscale_derp     | DERP relay + HTTPS (packages, registries)   |
| 5 | ALLOW  | cluster_nodes | ANY           | TCP 80             | apt, k3s installer, plain HTTP mirrors      |
| 6 | ALLOW  | cluster_nodes | ANY           | dns_ntp            | DNS resolution, NTP clock sync              |
| 7 | ALLOW  | cluster_nodes | ANY           | TCP 22             | SSH outbound (SCP for node-agent updates)   |
| 8 | DENY   | cluster_nodes | ANY           | ANY                | Explicit default deny — enable logging here |

**Rule 1 is the most important.** It prevents a rooted node from port-scanning
or connecting to home devices (NAS, router admin, other laptops) by dropping
all traffic toward the home LAN before any ALLOW rule matches.

---

### 2. WAN → CLUSTER  (home LAN devices accessing the cluster)

| # | Action | Source       | Destination   | Service          | Comment                                          |
|---|--------|--------------|---------------|------------------|--------------------------------------------------|
| 1 | ALLOW  | mgmt_station | cluster_nodes | TCP 22           | SSH / SCP for deployment                         |
| 2 | ALLOW  | mgmt_station | cluster_nodes | cluster_http     | Cluster landing page / services (HTTP NodePort)  |
| 3 | ALLOW  | mgmt_station | cluster_nodes | cluster_https    | Services over HTTPS NodePort                     |
| 4 | ALLOW  | mgmt_station | cluster_nodes | rancher_ui       | Rancher Kubernetes management                    |
| 5 | ALLOW  | mgmt_station | cluster_nodes | slurm_rest       | SLURM REST API                                   |
| 6 | ALLOW  | mgmt_station | cluster_nodes | tailscale_direct | Tailscale from management station                |
| 7 | DENY   | home_lan     | cluster_nodes | ANY              | All other home devices blocked from cluster      |
| 8 | DENY   | ANY          | cluster_nodes | ANY              | Explicit default deny — enable logging here      |

Rules 2–5 can be tightened to specific node IPs if you always access services
via a fixed node address.  If you expose services through the home router's
port forwarding, add a matching ALLOW here for the forwarded destination port.

---

### 3. MGMT → CLUSTER  (your management workstation has full access)

| # | Action | Source       | Destination   | Service | Comment                  |
|---|--------|--------------|---------------|---------|--------------------------|
| 1 | ALLOW  | mgmt_station | cluster_nodes | ANY     | Full management access   |
| 2 | DENY   | mgmt_net     | cluster_nodes | ANY     | Other MGMT hosts blocked |

---

### 4. CLUSTER → MGMT  (nodes must not initiate connections to management)

| # | Action | Source        | Destination | Service | Comment                        |
|---|--------|---------------|-------------|---------|--------------------------------|
| 1 | DENY   | cluster_nodes | mgmt_net    | ANY     | Nodes never talk to MGMT first |

---

### 5. Intra-CLUSTER  (node-to-node on the cluster LAN, same zone)

All sensitive cluster protocols (k3s, etcd, SLURM, MPI) travel inside the
Tailscale WireGuard overlay — they never appear as plaintext on the LAN.
Only the bootstrap and Tailscale packets need LAN-level rules.

| # | Action | Source        | Destination   | Service          | Comment                                  |
|---|--------|---------------|---------------|------------------|------------------------------------------|
| 1 | ALLOW  | cluster_nodes | cluster_nodes | tailscale_direct | Tailscale peer-to-peer WireGuard         |
| 2 | ALLOW  | cluster_nodes | cluster_nodes | tailscale_stun   | STUN for direct path negotiation         |
| 3 | ALLOW  | cluster_nodes | cluster_nodes | serf_gossip      | Serf cluster membership (bootstrap)      |
| 4 | ALLOW  | cluster_nodes | cluster_nodes | TCP 22           | SSH / SCP for node-agent updates         |
| 5 | ALLOW  | cluster_nodes | cluster_nodes | TCP 8090         | node-agent peer-update HTTP server       |
| 6 | ALLOW  | cluster_nodes | cluster_nodes | ICMP             | Ping for health checks                   |
| 7 | DENY   | cluster_nodes | cluster_nodes | ANY              | Everything else — enable logging here    |

**Why k3s (6443), etcd (2379), SLURM (6817-6819), and MPI ports are absent:**
Those services only bind to the Tailscale interface (100.64.0.0/10).  The
node iptables firewall accepts them when they arrive on `tailscale0` but DROPs
them if they arrive on the LAN interface — opening them at the perimeter would
add attack surface with no benefit.

---

## MPI-Specific Notes

OpenMPI is configured by `/etc/openmpi/openmpi-mca-params.conf` (installed from
`images/ubuntu/files/config/openmpi-mca-params.conf`) to:

1. Use only `tailscale0` (`btl_tcp_if_include = tailscale0`)
2. Rendezvous on TCP 20000–21000 (`btl_tcp_port_min/max_user`)

All MPI traffic therefore travels encrypted inside Tailscale WireGuard and
crosses the LAN as opaque UDP 41641.  The perimeter firewall never sees MPI
connections in plaintext, and the port range is predictable for auditing.

For `mpirun` / `srun`, no extra flags are needed — the system-wide MCA params
file is read automatically by every OpenMPI process.

---

## Tailscale ACLs (Recommended Complement)

The perimeter and node firewalls contain a compromised node within the cluster
network, but a rooted node can still reach *other* Tailscale devices in your
tailnet (personal laptop, phone, etc.).  Lock this down in Tailscale ACLs:

```json
{
  "tagOwners": {
    "tag:clusteros": ["autogroup:admin"]
  },
  "acls": [
    { "action": "accept", "src": ["tag:clusteros"],    "dst": ["tag:clusteros:*"] },
    { "action": "accept", "src": ["autogroup:admin"],  "dst": ["tag:clusteros:*"] },
    { "action": "deny",   "src": ["tag:clusteros"],    "dst": ["*:*"] }
  ]
}
```

Apply in Tailscale Admin → Access Controls.  Nodes are tagged via
`TAILSCALE_TAGS=clusteros` in `/etc/clusteros/tailscale.env`.

---

## Quick Verification Checklist

From a **cluster node**, the following should **fail** (firewall working):

```bash
# Should be blocked — no lateral movement to home LAN
curl --connect-timeout 3 http://192.168.1.1       # home router admin page
ping -c2 -W2 192.168.1.100                         # arbitrary home device

# Should be blocked — no arbitrary internet egress
curl --connect-timeout 3 telnet://1.1.1.1:23      # port 23 not in allowlist
nc -zv 8.8.8.8 8080                                # port 8080 not in allowlist
```

The following should **succeed** (cluster still functional):

```bash
tailscale ping <peer-tailscale-ip>    # Tailscale overlay working
apt-get update                         # internet egress on 80/443 working
curl -sk https://127.0.0.1:6443/healthz            # k3s API healthy
curl -s  http://127.0.0.1:30080/                   # ingress landing page
mpirun -np 4 --host node1,node2 hostname           # MPI across nodes
