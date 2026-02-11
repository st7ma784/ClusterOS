#!/bin/bash
# ClusterOS Patch: Fix SLURM DNS SRV lookup failure
#
# Problem:
#   slurmd.service (systemd) starts before node-agent generates slurm.conf.
#   Without a config file, slurmd falls back to SLURM's "configless" mode
#   and tries a DNS SRV lookup (_slurmctld._tcp.<cluster>) to find the
#   controller. No SRV records exist, so it fails with:
#     error: fetch_config: DNS SRV lookup failed
#     error: resolve_ctls_from_dns_srv: res_nsearch error: Unknown host
#     error: _establish_configuration: failed to load configs
#
# Root cause:
#   provision.sh uses "systemctl disable" for slurmd/slurmctld/munge, but
#   disable only removes WantedBy symlinks. The services can still be
#   triggered by package dependencies, dbus activation, or manual start.
#   The node-agent manages these daemons directly via exec.Command, so
#   the systemd services are conflicting duplicates.
#
# Fix:
#   Mask slurmd.service, slurmctld.service, and munge.service so systemd
#   can never start them. The node-agent remains the sole manager of these
#   processes.
#
# Usage:
#   On each node:  sudo bash fix-slurm-dns.sh
#   Or from a management host:
#     for ip in 100.105.26.8 100.105.X.Y; do
#       scp patch/fix-slurm-dns.sh clusteros@$ip:~/patch/
#       ssh clusteros@$ip 'sudo bash ~/patch/fix-slurm-dns.sh'
#     done

set -e

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

echo -e "${CYAN}=== ClusterOS Patch: Fix SLURM DNS SRV lookup failure ===${NC}"
echo ""

# Must be root
if [ "$(id -u)" -ne 0 ]; then
    echo -e "${RED}Error: Must run as root (sudo)${NC}"
    exit 1
fi

# 1. Stop the failing systemd SLURM services
echo -e "${YELLOW}[1/3] Stopping systemd SLURM services...${NC}"
systemctl stop slurmd.service 2>/dev/null || true
systemctl stop slurmctld.service 2>/dev/null || true
systemctl stop munge.service 2>/dev/null || true
echo -e "  ${GREEN}✓${NC} Stopped slurmd, slurmctld, munge systemd services"

# 2. Mask the services (symlink to /dev/null — prevents ALL activation)
echo -e "${YELLOW}[2/3] Masking SLURM systemd services (node-agent manages these directly)...${NC}"
systemctl disable slurmd.service 2>/dev/null || true
systemctl disable slurmctld.service 2>/dev/null || true
systemctl disable munge.service 2>/dev/null || true
systemctl mask slurmd.service 2>/dev/null || true
systemctl mask slurmctld.service 2>/dev/null || true
systemctl mask munge.service 2>/dev/null || true
echo -e "  ${GREEN}✓${NC} Masked slurmd.service (→ /dev/null)"
echo -e "  ${GREEN}✓${NC} Masked slurmctld.service (→ /dev/null)"
echo -e "  ${GREEN}✓${NC} Masked munge.service (→ /dev/null)"

# 3. Verify
echo -e "${YELLOW}[3/3] Verifying...${NC}"
for svc in slurmd.service slurmctld.service munge.service; do
    STATUS=$(systemctl is-enabled "$svc" 2>/dev/null || echo "unknown")
    if [ "$STATUS" = "masked" ]; then
        echo -e "  ${GREEN}✓${NC} $svc is masked"
    else
        echo -e "  ${RED}✗${NC} $svc is $STATUS (expected: masked)"
    fi
done

# Check if node-agent is running and will manage slurm
if systemctl is-active --quiet node-agent; then
    echo -e "  ${GREEN}✓${NC} node-agent is running (will manage slurmd/slurmctld/munged directly)"
else
    echo -e "  ${YELLOW}!${NC} node-agent is not running — start it: systemctl start node-agent"
fi

echo ""
echo -e "${GREEN}Patch applied successfully!${NC}"
echo ""
echo -e "${YELLOW}What this fixed:${NC}"
echo "  - slurmd.service can no longer start via systemd (was causing DNS SRV errors)"
echo "  - slurmctld.service can no longer start via systemd"
echo "  - munge.service can no longer start via systemd"
echo "  - node-agent remains the sole manager of SLURM daemons"
echo ""
echo "Monitor: journalctl -fu node-agent"
