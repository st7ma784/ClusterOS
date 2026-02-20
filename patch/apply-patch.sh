#!/bin/bash
# ClusterOS Patch — Phase-Machine Architecture
#
# Installs the rewritten node-agent (Serf-tag state machine) and unified
# cluster CLI.  Safe to re-run; idempotent on all steps.
#
# What changed:
#   OLD: Raft + Serf-event dual state paths caused 45 s+ retry storms before
#        workers received munge keys or K3s tokens.  Leader callbacks fired
#        before dependencies were ready (K3s agents joining before token existed).
#   NEW: Serf member tags are the single KV store.  A 5-phase state machine
#        (DISCOVERING → ELECTING → PROVISIONING|JOINING → READY) sequences
#        startup explicitly.  Workers block until leader publishes phase=ready.
#
# Usage (on each node):
#   sudo bash apply-patch.sh
#
# Automated (from a dev machine with Tailscale):
#   make deploy NODES="100.x.x.1 100.x.x.2 100.x.x.3"
#   make deploy          # auto-detects online Tailscale peers
#
# Rollout order:
#   Any order is fine.  The phase machine converges regardless of which node
#   patches first.  Patch all nodes within a few minutes to avoid the old and
#   new daemons talking to each other.

set -eo pipefail

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

ok()  { echo -e "  ${GREEN}✓${NC} $*"; }
warn(){ echo -e "  ${YELLOW}!${NC} $*"; }
err(){ echo -e "  ${RED}✗${NC} $*"; }
step(){ echo -e "\n${CYAN}${BOLD}[$1] $2${NC}"; }

echo -e "${CYAN}${BOLD}"
echo "  ╔══════════════════════════════════════════════════╗"
echo "  ║  ClusterOS — Phase-Machine Rollout Patch        ║"
echo "  ╚══════════════════════════════════════════════════╝"
echo -e "${NC}"

# ── Root check ─────────────────────────────────────────────────────────────────
if [ "$(id -u)" -ne 0 ]; then
    err "Must run as root: sudo bash apply-patch.sh"
    exit 1
fi

# ── 1. Dependencies ────────────────────────────────────────────────────────────
step "1/10" "Ensuring dependencies"

PKGS=""
command -v jq      &>/dev/null || PKGS="$PKGS jq"
command -v mpicc   &>/dev/null || PKGS="$PKGS openmpi-bin libopenmpi-dev"
dpkg -s libpmix-dev      &>/dev/null 2>&1 || PKGS="$PKGS libpmix-dev"
dpkg -s python3-mpi4py   &>/dev/null 2>&1 || PKGS="$PKGS python3-mpi4py"
dpkg -s build-essential  &>/dev/null 2>&1 || PKGS="$PKGS build-essential"
# SLURM daemons — node-agent manages these directly, but the binaries must be present.
# slurmctld: controller daemon (leader only)   slurmd: compute daemon (workers)
# munge: authentication — munged MUST run before slurmctld/slurmd can start
command -v slurmctld &>/dev/null || PKGS="$PKGS slurmctld"
command -v slurmd    &>/dev/null || PKGS="$PKGS slurmd"
command -v munged    &>/dev/null || PKGS="$PKGS munge"
# SLURM client tools — squeue, sbatch, sinfo, scancel (needed on ALL nodes, not just controller)
command -v squeue  &>/dev/null || PKGS="$PKGS slurm-client"
# Longhorn distributed storage requirements:
dpkg -s open-iscsi       &>/dev/null 2>&1 || PKGS="$PKGS open-iscsi"
dpkg -s nfs-common       &>/dev/null 2>&1 || PKGS="$PKGS nfs-common"
dpkg -s multipath-tools  &>/dev/null 2>&1 || PKGS="$PKGS multipath-tools"

if [ -n "$PKGS" ]; then
    apt-get update -qq
    # shellcheck disable=SC2086
    apt-get install -y -qq $PKGS
    ok "Installed:$PKGS"
else
    ok "All dependencies present"
fi

# Enable and start iscsid (required by Longhorn for iSCSI volume attachment).
if systemctl list-unit-files iscsid.service &>/dev/null; then
    systemctl enable --now iscsid 2>/dev/null || true
    ok "iscsid enabled (Longhorn iSCSI support)"
fi

# Enable multipath (Longhorn recommends blacklisting its devices in multipath).
if [ -f /etc/multipath.conf ]; then
    if ! grep -q 'longhorn' /etc/multipath.conf 2>/dev/null; then
        cat >> /etc/multipath.conf <<'MPEOF'
blacklist {
    devnode "^sd[a-z0-9]+"
}
MPEOF
        ok "multipath.conf updated for Longhorn"
    fi
else
    cat > /etc/multipath.conf <<'MPEOF'
defaults {
    user_friendly_names yes
}
blacklist {
    devnode "^sd[a-z0-9]+"
}
MPEOF
    ok "multipath.conf created"
fi
systemctl enable --now multipathd 2>/dev/null || true

# ── 2. Unique node identity ────────────────────────────────────────────────────
step "2/10" "Unique node identity"

IDENTITY_FILE="/var/lib/cluster-os/identity.json"
if [ -f "$IDENTITY_FILE" ]; then
    OLD_ID=$(python3 -c "import json,sys; d=json.load(open('$IDENTITY_FILE')); print(d.get('node_id','?')[:16])" 2>/dev/null || echo "?")
    rm -f "$IDENTITY_FILE"
    ok "Removed shared identity (was ${OLD_ID}…) — new identity on next start"
fi

CURRENT_MID=$(cat /etc/machine-id 2>/dev/null || true)
rm -f /etc/machine-id
systemd-machine-id-setup 2>/dev/null || dbus-uuidgen --ensure=/etc/machine-id 2>/dev/null || true
NEW_MID=$(cat /etc/machine-id 2>/dev/null || true)
if [ "$CURRENT_MID" != "$NEW_MID" ]; then
    ok "Regenerated machine-id (cloned image detected)"
    warn "DHCP may assign a new LAN IP after reboot"
else
    ok "machine-id unchanged"
fi

# ── 3. Config fixups ───────────────────────────────────────────────────────────
step "3/10" "Config fixups"

NODE_YAML="/etc/clusteros/node.yaml"
if [ -f "$NODE_YAML" ]; then
    # bootstrap_nodes → bootstrap_peers (old key name)
    if grep -q 'bootstrap_nodes:' "$NODE_YAML"; then
        sed -i 's/bootstrap_nodes:/bootstrap_peers:/' "$NODE_YAML"
        ok "Fixed config key: bootstrap_nodes → bootstrap_peers"
    fi
    # Remove election_mode — daemon now always uses the Serf phase machine
    if grep -q 'election_mode:' "$NODE_YAML"; then
        sed -i '/election_mode:/d' "$NODE_YAML"
        ok "Removed election_mode (daemon always uses Serf phase machine now)"
    fi
    # Inject cluster auth key if provided alongside this script
    if [ -f "$SCRIPT_DIR/cluster.key" ]; then
        CLUSTER_KEY=$(tr -d '[:space:]' < "$SCRIPT_DIR/cluster.key")
        sed -i "s|auth_key:.*|auth_key: \"$CLUSTER_KEY\"|" "$NODE_YAML"
        ok "Cluster auth key updated"
    fi
    ok "Config at $NODE_YAML looks good"
else
    warn "No config at $NODE_YAML — node-agent will use defaults"
fi

# ── 4. Stop all services ───────────────────────────────────────────────────────
step "4/10" "Stopping services"

for svc in node-agent k3s k3s-agent slurmd slurmctld munge; do
    systemctl stop "$svc" 2>/dev/null || true
done
sleep 1

# Kill anything still holding cluster ports
pkill -9 -f 'k3s server'  2>/dev/null || true
pkill -9 -f 'k3s agent'   2>/dev/null || true
pkill -9 munged            2>/dev/null || true
pkill -9 slurmctld         2>/dev/null || true
pkill -9 slurmd            2>/dev/null || true
sleep 1
ok "All services stopped"

# Mask SLURM + munge systemd units — node-agent manages them directly via exec.
# Without masking, systemd races node-agent and starts slurmd before slurm.conf exists.
for svc in slurmd slurmctld munge; do
    systemctl disable "$svc".service 2>/dev/null || true
    systemctl mask    "$svc".service 2>/dev/null || true
done
ok "Masked slurmd / slurmctld / munge (node-agent manages these directly)"

# ── 5. Wipe stale state ────────────────────────────────────────────────────────
step "5/10" "Wiping stale cluster state"

# Raft state — no longer used, always delete
if [ -d /var/lib/cluster-os/raft ]; then
    rm -rf /var/lib/cluster-os/raft
    ok "Removed stale Raft data"
fi

# K3s etcd — cloned images share the same member IPs; wipe so it reinitialises
if [ -d /var/lib/rancher/k3s/server/db ]; then
    rm -rf /var/lib/rancher/k3s/server/db
    ok "Removed stale K3s etcd database"
fi
if [ -d /var/lib/rancher/k3s/agent ]; then
    rm -rf /var/lib/rancher/k3s/agent
    ok "Removed stale K3s agent state"
fi

# K3s IP-marker file used by the new server.go to detect node IP changes
rm -f /var/lib/rancher/k3s/.cluster-os-ip 2>/dev/null || true

# Stale slurm.conf — will be regenerated with the correct controller IP
rm -f /etc/slurm/slurm.conf 2>/dev/null || true
ok "Removed stale slurm.conf"

# Serf status file — stale phase/leader values would confuse the new daemon
rm -f /run/clusteros/status.json 2>/dev/null || true

# Munge socket left behind by a crashed munged
rm -f /var/run/munge/munge.socket.2 2>/dev/null || true

# Stale Rancher Helm release (re-deployed by node-agent with correct auth config)
KUBECONFIG=/etc/rancher/k3s/k3s.yaml
export KUBECONFIG
if command -v helm &>/dev/null && [ -f "$KUBECONFIG" ]; then
    if helm list -n cattle-system --kubeconfig "$KUBECONFIG" 2>/dev/null | grep -q rancher; then
        helm uninstall rancher -n cattle-system --kubeconfig "$KUBECONFIG" 2>/dev/null || true
        k3s kubectl delete ingress rancher -n cattle-system 2>/dev/null || true
        ok "Removed stale Rancher Helm release (will re-deploy with correct config)"
    fi
fi

ok "Stale state cleared"

# ── 6. Munge directories ───────────────────────────────────────────────────────
step "6/10" "Munge directories"

mkdir -p /etc/munge /var/lib/munge /var/run/munge /var/log/munge
chmod 700 /etc/munge /var/lib/munge /var/log/munge
chmod 755 /var/run/munge
if id munge &>/dev/null; then
    chown -R munge:munge /etc/munge /var/lib/munge /var/run/munge /var/log/munge 2>/dev/null || true
    ok "Munge directories ready (owned by munge:munge)"
else
    warn "munge user not found — install: apt-get install -y munge"
fi

# ── 7. Firewall ────────────────────────────────────────────────────────────────
step "7/10" "Firewall rules"

open_port() {
    local port="$1" proto="${2:-tcp}"
    if command -v ufw &>/dev/null; then
        ufw allow "${port}/${proto}" &>/dev/null 2>&1 || true
    fi
    iptables -C INPUT -p "$proto" --dport "$port" -j ACCEPT 2>/dev/null || \
        iptables -A INPUT -p "$proto" --dport "$port" -j ACCEPT 2>/dev/null || true
}

open_port 22    tcp   # SSH
open_port 7946  tcp   # Serf gossip
open_port 7946  udp
open_port 6443  tcp   # K3s API
open_port 6817  tcp   # slurmctld
open_port 6818  tcp   # slurmd
open_port 6819  tcp   # slurmdbd
open_port 10250 tcp   # Kubelet
open_port "2379:2380" tcp   # etcd
open_port "30000:32767" tcp  # K8s NodePort range
open_port "30000:32767" udp

# Trust the Tailscale interface entirely — all traffic arriving on tailscale0
# has already been authenticated and encrypted by Tailscale.  This covers
# Serf gossip, K3s API, SLURM (6817/6818/6819), and K8s NodePorts without
# needing per-port ufw rules for the overlay network.
if command -v ufw &>/dev/null; then
    ufw allow in on tailscale0 comment 'Tailscale overlay — all trusted' &>/dev/null 2>&1 || true
    ufw allow in on ts0 comment 'Tailscale overlay (ts0 interface)' &>/dev/null 2>&1 || true
fi
# Also allow the Tailscale CGNAT range (100.64.0.0/10) in iptables for
# kernels where the tailscale0 interface rule doesn't match.
iptables -C INPUT -s 100.64.0.0/10 -j ACCEPT 2>/dev/null || \
    iptables -I INPUT -s 100.64.0.0/10 -j ACCEPT 2>/dev/null || true

ok "Firewall rules applied (including Tailscale interface trust)"

# ── 8. Install files ───────────────────────────────────────────────────────────
step "8/10" "Installing files"

mkdir -p /run/clusteros
chmod 755 /run/clusteros

# Backup old binary
mkdir -p /usr/local/bin/.clusteros-backup
for f in node-agent cluster cluster-status cluster-test cluster-dashboard; do
    [ -f "/usr/local/bin/$f" ] && cp "/usr/local/bin/$f" "/usr/local/bin/.clusteros-backup/${f}.bak" 2>/dev/null || true
done
ok "Backed up previous binaries"

# node-agent binary — pick arch-specific if present, fall back to plain
BINARY="$SCRIPT_DIR/node-agent"
ARCH=$(uname -m)
case "$ARCH" in
    x86_64)         [ -f "$SCRIPT_DIR/node-agent-amd64" ] && BINARY="$SCRIPT_DIR/node-agent-amd64" ;;
    aarch64|arm64)  [ -f "$SCRIPT_DIR/node-agent-arm64" ] && BINARY="$SCRIPT_DIR/node-agent-arm64" ;;
esac

if [ -f "$BINARY" ]; then
    install -m 755 "$BINARY" /usr/local/bin/node-agent
    ok "node-agent installed ($(uname -m))"
else
    err "No node-agent binary found in $SCRIPT_DIR — run 'make patch' first"
    exit 1
fi

# Unified cluster CLI
if [ -f "$SCRIPT_DIR/cluster" ]; then
    install -m 755 "$SCRIPT_DIR/cluster" /usr/local/bin/cluster
    ok "cluster CLI installed  →  'cluster help' to get started"
fi

# Legacy scripts (kept for backwards compat, superseded by 'cluster')
for script in cluster-status cluster-test cluster-dashboard cluster-init cluster-setup-services; do
    if [ -f "$SCRIPT_DIR/$script" ]; then
        install -m 755 "$SCRIPT_DIR/$script" "/usr/local/bin/$script"
    fi
done
ok "Legacy helper scripts installed"

# ── 9. Start and verify ────────────────────────────────────────────────────────
step "9/10" "Starting node-agent and verifying"

systemctl start node-agent
sleep 4

if ! systemctl is-active --quiet node-agent; then
    err "node-agent failed to start"
    echo ""
    journalctl -u node-agent -n 30 --no-pager
    exit 1
fi
ok "node-agent is running"

# Wait up to 15 s for the status file to appear
for i in $(seq 1 15); do
    [ -f /run/clusteros/status.json ] && break
    sleep 1
done

if [ -f /run/clusteros/status.json ] && command -v jq &>/dev/null; then
    PHASE=$(jq -r '.phase // "unknown"' /run/clusteros/status.json)
    ok "Status file created  (phase=$PHASE)"
else
    warn "Status file not yet written — node-agent may still be starting"
fi

# ── Summary ────────────────────────────────────────────────────────────────────
echo ""
echo -e "${GREEN}${BOLD}Patch applied successfully on $(hostname)!${NC}"
echo ""
echo -e "${CYAN}What happens next (automatic):${NC}"
echo "  ~0 s   New node-agent starts, Serf joins Tailscale peers"
echo "  ~5 s   Cluster membership gossip stabilises, leader elected"
echo "         (leader = node with lexicographically lowest hostname)"
echo "  ~10 s  Leader: starts K3s server, generates munge key,"
echo "         publishes k3s-server / k3s-token / munge-key Serf tags"
echo "  ~4 min Leader: K3s API ready, slurmctld started, phase=ready published"
echo "  <30 s  Workers: read tags, start K3s agent + slurmd automatically"
echo "  +5 min Leader: Longhorn, nginx-ingress, Rancher, slurmdbd deployed"
echo ""
echo -e "${CYAN}Monitor progress:${NC}"
echo "  journalctl -fu node-agent          # live daemon log"
echo "  watch -n2 'cluster status'         # cluster state every 2 s"
echo "  cluster dash                        # live dashboard"
echo ""
echo -e "${CYAN}Once phase=ready:${NC}"
echo "  cluster test all                    # SLURM + K3s + MPI integration tests"
echo "  cluster ui                          # print all web UI URLs"
echo ""
echo -e "${YELLOW}Rollout note:${NC}"
echo "  Patch remaining nodes within a few minutes so old and new daemons"
echo "  do not talk to each other for long.  The phase machine handles"
echo "  any join order gracefully once all nodes run the same version."

# ── 10. Reboot ─────────────────────────────────────────────────────────────────
step "10/10" "Scheduling reboot"

# A reboot ensures:
#   • iscsid / multipathd kernel modules are fully loaded (required by Longhorn)
#   • machine-id change is picked up by DHCP / Tailscale
#   • any stale K3s / Serf file locks are cleared
#   • node-agent starts fresh with the new binary under systemd supervision
echo ""
echo -e "${CYAN}${BOLD}This node will reboot in 10 seconds.${NC}"
echo "  Interrupt with Ctrl-C if you need to stay online, then reboot manually."
echo ""

# Give the operator a brief window to cancel.
REBOOT_DELAY=10
for i in $(seq "$REBOOT_DELAY" -1 1); do
    printf "\r  Rebooting in %2d s ...  (Ctrl-C to cancel)" "$i"
    sleep 1
done
echo ""
ok "Rebooting now"
systemctl reboot
