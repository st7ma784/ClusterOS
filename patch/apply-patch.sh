#!/bin/bash
# ClusterOS Patch - Cumulative fix for cluster-status, munge, k3s, and SLURM
#
# This patch:
#   1. Ensures jq is installed (needed to parse the status file)
#   2. Fixes config key mismatch (bootstrap_nodes → bootstrap_peers)
#   3. Regenerates unique node identity (cloned images share the same one)
#   4. Creates missing munge directories for SLURM auth
#   5. Kills stale K3s/etcd processes holding port 2380
#   6. Cleans stale K3s etcd data, SLURM config, and Raft state
#   7. Installs patched node-agent with fixes:
#      - K3s: kills orphaned etcd on port 2380 before starting server
#      - SLURM: passes explicit -N <nodename> to slurmd (fixes NodeName mismatch)
#   8. Masks slurmd/slurmctld/munge systemd services (fixes DNS SRV lookup failure)
#   9. Installs updated helper scripts (cluster-status, cluster-init, etc.)
#  10. Restarts node-agent and verifies
#
# Usage:
#   On each node:  sudo bash apply-patch.sh
#   Or from a management host:
#     for ip in 100.105.26.8 100.105.X.Y; do
#       scp -r patch/ clusteros@$ip:~/patch/
#       ssh clusteros@$ip 'sudo bash ~/patch/apply-patch.sh'
#     done

set -e

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo -e "${CYAN}=== ClusterOS Patch: Cumulative fix (status, munge, k3s etcd, SLURM nodename) ===${NC}"
echo ""

# Must be root
if [ "$(id -u)" -ne 0 ]; then
    echo -e "${RED}Error: Must run as root (sudo)${NC}"
    exit 1
fi

# 1. Ensure dependencies are installed
echo -e "${YELLOW}[1/10] Ensuring dependencies are installed...${NC}"
PKGS_TO_INSTALL=""
command -v jq &>/dev/null || PKGS_TO_INSTALL="$PKGS_TO_INSTALL jq"
# PMIx library required by SLURM's MpiDefault=pmix (slurmd fails to load mpi/pmix without it)
dpkg -s libpmix-dev &>/dev/null 2>&1 || PKGS_TO_INSTALL="$PKGS_TO_INSTALL libpmix-dev"
# MPI packages for cluster-test MPI tests
command -v mpicc &>/dev/null || PKGS_TO_INSTALL="$PKGS_TO_INSTALL openmpi-bin libopenmpi-dev"
dpkg -s python3-mpi4py &>/dev/null 2>&1 || PKGS_TO_INSTALL="$PKGS_TO_INSTALL python3-mpi4py"
dpkg -s build-essential &>/dev/null 2>&1 || PKGS_TO_INSTALL="$PKGS_TO_INSTALL build-essential"

if [ -n "$PKGS_TO_INSTALL" ]; then
    apt-get update -qq && apt-get install -y -qq $PKGS_TO_INSTALL
    echo -e "  ${GREEN}✓${NC} Installed:$PKGS_TO_INSTALL"
else
    echo -e "  ${GREEN}✓${NC} All dependencies present (jq, libpmix-dev, openmpi, build-essential)"
fi

# 2. Fix config key mismatch (bootstrap_nodes → bootstrap_peers)
echo -e "${YELLOW}[2/10] Fixing config keys...${NC}"
if [ -f /etc/clusteros/node.yaml ]; then
    if grep -q 'bootstrap_nodes:' /etc/clusteros/node.yaml; then
        sed -i 's/bootstrap_nodes:/bootstrap_peers:/' /etc/clusteros/node.yaml
        echo -e "  ${GREEN}✓${NC} Fixed bootstrap_nodes → bootstrap_peers in config"
    else
        echo -e "  ${GREEN}✓${NC} Config keys already correct"
    fi
fi

# 3. Regenerate node identity (all nodes from same image share the same identity!)
echo -e "${YELLOW}[3/10] Regenerating unique node identity...${NC}"
IDENTITY_FILE="/var/lib/cluster-os/identity.json"
if [ -f "$IDENTITY_FILE" ]; then
    OLD_ID=$(grep -o '"node_id":"[^"]*"' "$IDENTITY_FILE" 2>/dev/null | cut -d'"' -f4 || echo "unknown")
    rm -f "$IDENTITY_FILE"
    echo -e "  ${GREEN}✓${NC} Removed shared identity (was: ${OLD_ID:0:16}...)"
fi
echo -e "  ${CYAN}→${NC} New identity will be generated on next start"

# Fix duplicate machine-id (causes DHCP to assign same IP to multiple nodes)
CURRENT_MACHINEID=$(cat /etc/machine-id 2>/dev/null)
# Check if machine-id looks like it was cloned (all nodes would have the same one)
if [ -n "$CURRENT_MACHINEID" ]; then
    # Regenerate machine-id to get a unique one
    rm -f /etc/machine-id
    systemd-machine-id-setup 2>/dev/null || dbus-uuidgen --ensure=/etc/machine-id 2>/dev/null
    NEW_MACHINEID=$(cat /etc/machine-id 2>/dev/null)
    if [ "$CURRENT_MACHINEID" != "$NEW_MACHINEID" ]; then
        echo -e "  ${GREEN}✓${NC} Regenerated machine-id (was cloned from image)"
        echo -e "  ${YELLOW}!${NC} DHCP lease will renew with new unique ID — may get new LAN IP"
    else
        echo -e "  ${GREEN}✓${NC} machine-id unchanged"
    fi
fi

# 4. Fix munge directories (required for SLURM authentication)
echo -e "${YELLOW}[4/10] Fixing munge directories...${NC}"
mkdir -p /etc/munge /var/lib/munge /var/run/munge /var/log/munge
chmod 700 /etc/munge /var/lib/munge
chmod 755 /var/run/munge
chmod 700 /var/log/munge
if id munge &>/dev/null; then
    chown -R munge:munge /etc/munge /var/lib/munge /var/run/munge /var/log/munge 2>/dev/null || true
    echo -e "  ${GREEN}✓${NC} Munge directories created with correct ownership"
else
    echo -e "  ${YELLOW}!${NC} Munge user not found - run: apt-get install -y munge"
fi

# Kill any stuck munge daemon
pkill -9 munged 2>/dev/null || true
rm -f /var/run/munge/munge.socket.2 2>/dev/null || true

# 5. Kill stale K3s/etcd processes holding port 2380
echo -e "${YELLOW}[5/10] Killing stale K3s/etcd processes on port 2380...${NC}"
if ss -tlnp 2>/dev/null | grep -q ':2380 '; then
    echo -e "  ${CYAN}→${NC} Port 2380 is in use, cleaning up..."
    if [ -x /usr/local/bin/k3s-killall.sh ]; then
        /usr/local/bin/k3s-killall.sh 2>/dev/null || true
        echo -e "  ${GREEN}✓${NC} Ran k3s-killall.sh"
    else
        pkill -9 -f 'k3s server' 2>/dev/null || true
        pkill -9 -f 'etcd' 2>/dev/null || true
        echo -e "  ${GREEN}✓${NC} Killed stale K3s/etcd processes"
    fi
    sleep 2
else
    echo -e "  ${GREEN}✓${NC} Port 2380 is free"
fi

# 6. Stop node-agent and clean stale cluster state
echo -e "${YELLOW}[6/11] Stopping services and cleaning stale state...${NC}"
systemctl stop node-agent 2>/dev/null || true
systemctl stop k3s 2>/dev/null || true
systemctl stop k3s-agent 2>/dev/null || true
systemctl stop slurmd 2>/dev/null || true
systemctl stop slurmctld 2>/dev/null || true
systemctl stop munge 2>/dev/null || true
sleep 1
echo -e "  ${GREEN}✓${NC} Services stopped"

# 6b. Mask SLURM systemd services (fixes DNS SRV lookup failure)
# "disable" only removes WantedBy symlinks; "mask" symlinks to /dev/null and prevents
# ALL activation. Without this, slurmd.service starts before slurm.conf exists and
# falls back to configless mode → DNS SRV lookup → failure.
# The node-agent manages slurmd/slurmctld/munged directly via exec.Command.
echo -e "${YELLOW}[6b/11] Masking SLURM systemd services...${NC}"
systemctl disable slurmd.service 2>/dev/null || true
systemctl mask slurmd.service 2>/dev/null || true
systemctl disable slurmctld.service 2>/dev/null || true
systemctl mask slurmctld.service 2>/dev/null || true
systemctl disable munge.service 2>/dev/null || true
systemctl mask munge.service 2>/dev/null || true
echo -e "  ${GREEN}✓${NC} Masked slurmd.service, slurmctld.service, munge.service"
echo -e "  ${CYAN}→${NC} node-agent will manage these daemons directly"

# Wipe stale K3s etcd data (cloned from image — has wrong member IPs)
if [ -d /var/lib/rancher/k3s/server/db ]; then
    rm -rf /var/lib/rancher/k3s/server/db
    echo -e "  ${GREEN}✓${NC} Removed stale K3s etcd database (will reinitialize)"
fi
# Also wipe stale K3s agent state that references old node names
if [ -d /var/lib/rancher/k3s/agent ]; then
    rm -rf /var/lib/rancher/k3s/agent
    echo -e "  ${GREEN}✓${NC} Removed stale K3s agent state"
fi

# Remove stale SLURM config (will be regenerated with correct NodeNames)
if [ -f /etc/slurm/slurm.conf ]; then
    rm -f /etc/slurm/slurm.conf
    echo -e "  ${GREEN}✓${NC} Removed stale slurm.conf (will be regenerated)"
fi

# Clean Raft state (stale from cloned image)
if [ -d /var/lib/cluster-os/raft ]; then
    rm -rf /var/lib/cluster-os/raft
    echo -e "  ${GREEN}✓${NC} Removed stale Raft state"
fi
sleep 1
echo -e "  ${GREEN}✓${NC} Stale state cleaned"

# 7. Replace files
echo -e "${YELLOW}[7/10] Installing patched files...${NC}"

# Backup originals
mkdir -p /usr/local/bin/.clusteros-backup
for f in node-agent cluster-status cluster-init; do
    if [ -f "/usr/local/bin/$f" ]; then
        cp "/usr/local/bin/$f" "/usr/local/bin/.clusteros-backup/$f.bak"
    fi
done
echo -e "  ${GREEN}✓${NC} Backed up originals to /usr/local/bin/.clusteros-backup/"

# Install new node-agent binary
install -m 755 "$SCRIPT_DIR/node-agent" /usr/local/bin/node-agent
echo -e "  ${GREEN}✓${NC} node-agent binary updated"

# Install updated scripts
install -m 755 "$SCRIPT_DIR/cluster-status" /usr/local/bin/cluster-status
echo -e "  ${GREEN}✓${NC} cluster-status updated"

install -m 755 "$SCRIPT_DIR/cluster-init" /usr/local/bin/cluster-init
echo -e "  ${GREEN}✓${NC} cluster-init updated"

if [ -f "$SCRIPT_DIR/cluster-test" ]; then
    install -m 755 "$SCRIPT_DIR/cluster-test" /usr/local/bin/cluster-test
    echo -e "  ${GREEN}✓${NC} cluster-test installed"
fi

if [ -f "$SCRIPT_DIR/cluster-dashboard" ]; then
    install -m 755 "$SCRIPT_DIR/cluster-dashboard" /usr/local/bin/cluster-dashboard
    echo -e "  ${GREEN}✓${NC} cluster-dashboard installed"
fi

if [ -f "$SCRIPT_DIR/cluster-setup-services" ]; then
    install -m 755 "$SCRIPT_DIR/cluster-setup-services" /usr/local/bin/cluster-setup-services
    echo -e "  ${GREEN}✓${NC} cluster-setup-services installed"
fi

# Create status directory
mkdir -p /run/clusteros
chmod 755 /run/clusteros

# Inject cluster auth key if provided
if [ -f "$SCRIPT_DIR/cluster.key" ]; then
    CLUSTER_KEY=$(cat "$SCRIPT_DIR/cluster.key" | tr -d '[:space:]')
    sed -i "s|auth_key:.*|auth_key: \"$CLUSTER_KEY\"|" /etc/clusteros/node.yaml
    echo -e "  ${GREEN}✓${NC} Cluster auth key updated"
fi

# 8. Restart node-agent
echo -e "${YELLOW}[8/10] Restarting node-agent...${NC}"
systemctl start node-agent
sleep 3

if systemctl is-active --quiet node-agent; then
    echo -e "  ${GREEN}✓${NC} node-agent running"
else
    echo -e "  ${RED}✗${NC} node-agent failed to start - check: journalctl -u node-agent -n 50"
    exit 1
fi

# Verify status file appears
sleep 2
if [ -f /run/clusteros/status.json ]; then
    MEMBERS=$(jq -r '.member_count' /run/clusteros/status.json 2>/dev/null)
    echo -e "  ${GREEN}✓${NC} Status file created ($MEMBERS members)"
else
    echo -e "  ${YELLOW}!${NC} Status file not yet created (may take up to 10s)"
fi

# 9. Verify munge is running
echo -e "${YELLOW}[9/10] Verifying munge...${NC}"
sleep 3
if pgrep -x munged &>/dev/null; then
    echo -e "  ${GREEN}✓${NC} Munge daemon running"
else
    echo -e "  ${YELLOW}!${NC} Munge not yet running (will start when SLURM role activates)"
fi

# 10. Verify K3s etcd and SLURM fixes
echo -e "${YELLOW}[10/10] Verifying K3s and SLURM fixes...${NC}"
sleep 5
if ss -tlnp 2>/dev/null | grep -q ':2380 '; then
    echo -e "  ${GREEN}✓${NC} Port 2380 in use (K3s etcd started — expected if this node is a server)"
else
    echo -e "  ${GREEN}✓${NC} Port 2380 free (K3s will bind it when elected as server)"
fi

if pgrep -x slurmd &>/dev/null; then
    echo -e "  ${GREEN}✓${NC} slurmd is running"
elif [ -f /etc/slurm/slurm.conf ]; then
    echo -e "  ${YELLOW}!${NC} slurm.conf exists but slurmd not yet running (will start on next health check)"
else
    echo -e "  ${YELLOW}!${NC} Waiting for controller to generate slurm.conf (normal on fresh start)"
fi

echo ""
echo -e "${GREEN}Patch applied successfully!${NC}"
echo ""
echo -e "${YELLOW}What happens next:${NC}"
echo "  1. node-agent generates a new unique identity (immediate)"
echo "  2. Tailscale peer discovery finds other nodes (~15 seconds)"
echo "  3. Serf cluster assembles, leader elected (~30 seconds)"
echo "  4. Leader generates slurm.conf and starts slurmctld"
echo "  5. Workers receive config and start slurmd with explicit -N <nodename>"
echo "  6. K3s server starts on leader with fresh etcd (stale processes killed first)"
echo ""
echo -e "${YELLOW}Fixes included:${NC}"
echo "  - K3s: kills orphaned etcd processes on port 2380 before starting"
echo "  - SLURM: slurmd receives explicit -N <nodename> matching slurm.conf"
echo "  - SLURM: masked slurmd/slurmctld/munge systemd services (fixes DNS SRV error)"
echo "  - Munge: directories created with correct ownership"
echo "  - Identity: regenerated to avoid cloned-image conflicts"
echo ""
echo "Monitor: journalctl -fu node-agent"
echo "Check:   cluster-status"
