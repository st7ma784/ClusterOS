# Cluster-OS Networking Architecture

This document describes the three-layer networking model used by Cluster-OS, the
iptables chain ordering that governs traffic flows, and how to diagnose common
networking problems.

---

## Three-Layer Model

```
┌─────────────────────────────────────────────────────────────────┐
│  Layer 1: Physical LAN / WiFi (192.168.x.x or wired)           │
│  ├─ DHCP-assigned host IPs                                      │
│  ├─ Internet access: outbound 80/443 MUST be unrestricted       │
│  └─ Firewall: ufw ACCEPT SSH + cluster ports on INPUT           │
│                                                                  │
│  Layer 2: Tailscale Mesh (100.64.0.0/10 CGNAT)                  │
│  ├─ All cluster-internal comms (Serf, k3s API, SLURM, etcd)     │
│  ├─ Stable virtual IP per node, survives LAN topology changes   │
│  ├─ Firewall: trust tailscale0/ts0 entirely (already encrypted) │
│  └─ Flannel VXLAN runs inside Tailscale tunnels (8472/udp)      │
│                                                                  │
│  Layer 3: k3s Pod Network (Flannel)                              │
│  ├─ Pod IPs:     10.42.0.0/16                                   │
│  ├─ Service IPs: 10.43.0.0/16                                   │
│  └─ FORWARD chain must ACCEPT these CIDRs (pods can't route     │
│     without this — kube-proxy DNAT depends on it)               │
└─────────────────────────────────────────────────────────────────┘
```

---

## Traffic Flows

### Outbound during bootstrap (image pulls)

```
containerd → docker.io:443
  → OUTPUT chain (no REDIRECT rules!) → LAN gateway → internet
```

This is the most critical flow to protect.  Any `OUTPUT REDIRECT` rule on port 443
will intercept containerd's TLS handshake and redirect it to a local port
(typically 30443), causing an immediate connection reset.  Image pulls, `helm`,
and `apt` all fail silently or with misleading TLS errors.

### Ingress (external → cluster services)

```
Browser → node:80 or node:443
  → nginx-ingress pod (hostNetwork=true, binds directly to host port)
  → kube-proxy DNAT → backend pod IP
```

nginx-ingress is deployed as a DaemonSet with `hostNetwork: true`.  It binds
directly to the host's port 80/443 — **no REDIRECT or PREROUTING rules are
needed**.  UFW must ACCEPT INPUT on 80/443, and the FORWARD chain must ACCEPT
pod CIDRs so kube-proxy DNAT works.

### Inter-node (Serf gossip, k3s API, SLURM)

```
node-A:tailscale0 (100.x.x.1) → node-B:tailscale0 (100.x.x.2)
  → Tailscale WireGuard tunnel → decrypted on node-B → local port
```

All cluster-internal traffic flows over Tailscale.  UFW trusts `tailscale0` and
`ts0` interfaces entirely, so no per-port rules are needed for this layer.

### Inter-pod (Flannel VXLAN)

```
pod-A (10.42.1.5) → pod-B (10.42.2.7)
  → FORWARD chain → Flannel VXLAN encapsulation
  → tailscale0 (VXLAN over Tailscale) → remote node → decapsulate → pod-B
```

The FORWARD chain must ACCEPT source/destination 10.42.0.0/16 and 10.43.0.0/16,
and the `flannel.1`/`cni0` interfaces must also be trusted.

---

## Why hostNetwork nginx-ingress (no REDIRECT)

Older approaches used iptables PREROUTING REDIRECT rules to redirect ports
80/443 to NodePort ports 30080/30443.  This was abandoned because:

1. **OUTPUT REDIRECT breaks node-local outbound connections** — any process on
   the node (containerd, helm, apt, curl) connecting to port 443 externally gets
   redirected to the local port 30443 instead.  Image pulls fail.
2. **iptables-persistent restores rules on reboot** — even if removed manually,
   the rules return after a reboot if `/etc/iptables/rules.v4` is stale.
3. **hostNetwork nginx is simpler and faster** — the nginx pod owns the port
   directly; kube-proxy handles backend routing via Service DNAT.  No NAT chain
   manipulation needed.

---

## iptables Chain Order (relevant chains)

```
Inbound packet:
  PREROUTING (nat) → FORWARD or INPUT → POSTROUTING (nat)

Outbound packet (from local process):
  OUTPUT (nat) → OUTPUT (filter) → POSTROUTING (nat)
```

Key rules and their purpose:

| Chain       | Table  | Rule                                  | Purpose                          |
|-------------|--------|---------------------------------------|----------------------------------|
| INPUT       | filter | -s 100.64.0.0/10 -j ACCEPT           | Trust Tailscale CGNAT range      |
| INPUT       | filter | -i tailscale0 -j ACCEPT              | Trust Tailscale interface        |
| INPUT       | filter | -p tcp --dport 80 -j ACCEPT          | nginx-ingress HTTP               |
| INPUT       | filter | -p tcp --dport 443 -j ACCEPT         | nginx-ingress HTTPS              |
| FORWARD     | filter | -d 10.42.0.0/16 -j ACCEPT           | Pod network inbound routing      |
| FORWARD     | filter | -s 10.42.0.0/16 -j ACCEPT           | Pod network outbound routing     |
| FORWARD     | filter | -d 10.43.0.0/16 -j ACCEPT           | Service CIDR routing             |
| FORWARD     | filter | -s 10.43.0.0/16 -j ACCEPT           | Service CIDR routing             |
| FORWARD     | filter | -i flannel.1 -j ACCEPT              | Flannel VXLAN in                 |
| FORWARD     | filter | -o flannel.1 -j ACCEPT              | Flannel VXLAN out                |
| OUTPUT      | nat    | ~~--dport 443 -j REDIRECT~~          | **MUST NOT EXIST** (breaks pulls)|
| PREROUTING  | nat    | ~~--dport 443 -j REDIRECT~~          | **MUST NOT EXIST** (breaks pods) |

---

## Required sysctl Settings

IP forwarding must be enabled for the node to act as a router between layers:

```bash
net.ipv4.ip_forward = 1
net.ipv6.conf.all.forwarding = 1
```

Cluster-OS writes these to `/etc/sysctl.d/99-clusteros.conf` so they persist
across reboots (applied by `apply-patch.sh` and `setupFirewallRules()`).

---

## Firewall Rules Summary

The following ports must be open on INPUT (all TCP unless noted):

| Port / Range   | Protocol | Service                              |
|----------------|----------|--------------------------------------|
| 22             | tcp      | SSH                                  |
| 80             | tcp      | HTTP ingress (nginx hostNetwork)     |
| 443            | tcp      | HTTPS ingress (nginx hostNetwork)    |
| 7946           | tcp+udp  | Serf gossip                          |
| 6443           | tcp      | k3s API server                       |
| 6817           | tcp      | SLURM slurmctld                      |
| 6818           | tcp      | SLURM slurmd                         |
| 6819           | tcp      | SLURM slurmdbd                       |
| 10250          | tcp      | Kubelet API                          |
| 2379-2380      | tcp      | etcd (embedded in k3s)               |
| 8472           | udp      | Flannel VXLAN (via Tailscale)        |
| 30000-32767    | tcp+udp  | Kubernetes NodePort range            |

---

## Troubleshooting

### Image pulls fail with TLS or connection errors

```bash
# Check for stale REDIRECT rules
iptables -t nat -L OUTPUT -n | grep REDIRECT
iptables -t nat -L PREROUTING -n | grep REDIRECT

# Remove them manually (daemon does this on startup too)
iptables -t nat -D OUTPUT -p tcp --dport 443 -j REDIRECT --to-ports 30443
iptables -t nat -D OUTPUT -p tcp --dport 80  -j REDIRECT --to-ports 30080

# Verify containerd can pull
k3s crictl pull alpine:latest
```

### Pods cannot reach each other or services

```bash
# Check FORWARD chain
iptables -L FORWARD -n | head -20

# Check UFW FORWARD policy
grep DEFAULT_FORWARD_POLICY /etc/default/ufw

# Should be ACCEPT; fix with:
sed -i 's/DEFAULT_FORWARD_POLICY="DROP"/DEFAULT_FORWARD_POLICY="ACCEPT"/' /etc/default/ufw
ufw reload

# Check IP forwarding
sysctl net.ipv4.ip_forward   # should be 1
```

### nginx-ingress not receiving external traffic

```bash
# Verify nginx pod is running with hostNetwork
kubectl -n ingress-nginx get pods -o wide
kubectl -n ingress-nginx get pod <pod-name> -o jsonpath='{.spec.hostNetwork}'

# Check port 80/443 is open on the host
ss -tlnp | grep -E ':80|:443'
iptables -L INPUT -n | grep -E 'dpt:80|dpt:443'

# UFW status
ufw status | grep -E '80|443'
```

### Node cannot join (Serf / k3s agent fails)

```bash
# Confirm Tailscale is up
tailscale status

# Ping another node's Tailscale IP
ping 100.x.x.y

# Check Serf port
nc -zv 100.x.x.y 7946

# Check k3s API
curl -k https://100.x.x.y:6443/healthz
```

### Stale iptables rules survive reboot

```bash
# After cleanup, save clean rules so iptables-persistent won't restore stale ones
iptables-save > /etc/iptables/rules.v4

# Verify the saved file has no REDIRECT lines
grep REDIRECT /etc/iptables/rules.v4   # should return nothing
```

---

## How Cluster-OS Enforces These Rules

Two places apply and clean up networking rules:

1. **`apply-patch.sh`** — runs once during deployment:
   - Emergency NAT cleanup runs immediately after root check (before k3s install)
   - Step 7 opens ports, sets UFW FORWARD policy, enables sysctl, adds pod CIDR
     FORWARD rules, and saves clean iptables state

2. **`daemon.go::setupFirewallRules()`** — runs at daemon startup on every boot:
   - Opens INPUT ports (including 80/443)
   - Trusts Tailscale interfaces
   - Removes any stale REDIRECT rules (catches iptables-persistent restores)
   - Sets UFW FORWARD policy if still DROP
   - Enables ip_forward sysctl
   - Adds pod CIDR FORWARD rules

This dual-layer approach ensures the node is in a clean networking state whether
the patch was recently applied or the node has rebooted multiple times since.
