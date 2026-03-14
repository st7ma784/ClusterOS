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

set -e

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

# --no-reboot flag: used by USB live boot (node-agent.service ExecStartPre) so that
# apply-patch.sh does first-boot provisioning without rebooting back into the live USB.
NO_REBOOT=0
for _arg in "$@"; do [[ "$_arg" = "--no-reboot" ]] && NO_REBOOT=1; done

# Also auto-detect USB boot: if root is on a USB device, skip the reboot.
_ROOT_DEV=$(findmnt -n -o SOURCE / 2>/dev/null | sed 's/p\?[0-9]*$//' | head -1)
_ROOT_TRAN=$(lsblk -d -n -o TRAN "$_ROOT_DEV" 2>/dev/null | head -1 || true)
[[ "$_ROOT_TRAN" = "usb" ]] && NO_REBOOT=1 && warn "USB boot detected — reboot step will be skipped"

# ── Print bundle version ───────────────────────────────────────────────────────
if [ -f "$SCRIPT_DIR/VERSION" ]; then
    BUNDLE_VERSION=$(grep '^version=' "$SCRIPT_DIR/VERSION" | cut -d= -f2)
    BUNDLE_COMMIT=$(grep '^commit='  "$SCRIPT_DIR/VERSION" | cut -d= -f2)
    ok "Bundle: $BUNDLE_VERSION ($BUNDLE_COMMIT)"
fi

# ── Kill old node-agent immediately (MUST be first) ───────────────────────────
# The old binary calls setupFirewallRules() continuously.  If we flush iptables
# or nftables BEFORE killing it, it re-adds REDIRECT rules within milliseconds —
# causing _internet_ok to return false, the wrong exit-node to be selected, and
# those stale rules to be saved back to /etc/iptables/rules.v4 at the end of
# this script.  Killing first guarantees a clean slate for all network operations.
#
# Note: make deploy also pre-kills the agent via SSH before uploading the patch
# bundle, eliminating the race window where the old binary re-adds REDIRECT rules
# during SCP.  This kill is belt-and-suspenders for manual apply-patch.sh runs.
systemctl stop node-agent 2>/dev/null || true
# Kill by path (covers standard install) AND by executable name (catches
# any copy started from a non-standard path, e.g. /tmp or a USB image path).
pkill -TERM -f '/usr/local/bin/node-agent' 2>/dev/null || true
pkill -TERM -f '/tmp/node-agent'           2>/dev/null || true
pkill -TERM -x 'node-agent'                2>/dev/null || true
sleep 1
pkill -KILL -f '/usr/local/bin/node-agent' 2>/dev/null || true
pkill -KILL -f '/tmp/node-agent'           2>/dev/null || true
pkill -KILL -x 'node-agent'               2>/dev/null || true
ok "Old node-agent killed (cannot re-add iptables/nftables rules)"

# ── Install binaries and helpers immediately (before any step that can fail) ───
# Everything here runs right after the root check and node-agent kill.
# If apt/network/iptables steps later fail, these are already on disk.

echo ""
echo "  Bundle dir: $SCRIPT_DIR"
echo "  Bundle contents:"
ls -1 "$SCRIPT_DIR/" 2>/dev/null | sed 's/^/    /' || echo "    (empty or unreadable)"
echo ""

# node-agent binary
_BINARY="$SCRIPT_DIR/node-agent"
case "$(uname -m)" in
    x86_64)        [ -f "$SCRIPT_DIR/node-agent-amd64" ] && _BINARY="$SCRIPT_DIR/node-agent-amd64" ;;
    aarch64|arm64) [ -f "$SCRIPT_DIR/node-agent-arm64" ] && _BINARY="$SCRIPT_DIR/node-agent-arm64" ;;
esac
if [ -f "$_BINARY" ]; then
    install -m 755 "$_BINARY" /usr/local/bin/node-agent
    _VER=$(/usr/local/bin/node-agent --version 2>/dev/null | head -1 || echo "unknown")
    ok "node-agent installed: $_VER  →  /usr/local/bin/node-agent"
else
    err "node-agent NOT in bundle ($SCRIPT_DIR) — binary will not be updated"
fi

# cluster-make-usb
if [ -f "$SCRIPT_DIR/cluster-make-usb.sh" ]; then
    install -m 755 "$SCRIPT_DIR/cluster-make-usb.sh" /usr/local/bin/cluster-make-usb
    ok "cluster-make-usb installed  →  /usr/local/bin/cluster-make-usb"
else
    err "cluster-make-usb.sh NOT in bundle ($SCRIPT_DIR)"
    err "  This means 'sudo cluster-make-usb' will not work on this node."
    err "  On dev machine: make patch && make deploy"
fi

# cluster CLI
if [ -f "$SCRIPT_DIR/cluster" ]; then
    install -m 755 "$SCRIPT_DIR/cluster" /usr/local/bin/cluster
    ok "cluster CLI installed  →  /usr/local/bin/cluster"
fi

# tailscale-auth → installed as clusteros-tailscale-init (called later for Tailscale auth)
if [ -f "$SCRIPT_DIR/tailscale-auth" ]; then
    install -m 755 "$SCRIPT_DIR/tailscale-auth" /usr/local/bin/clusteros-tailscale-init
    ok "clusteros-tailscale-init installed  →  /usr/local/bin/clusteros-tailscale-init"
fi

# ── Ensure clusteros user exists ────────────────────────────────────────────
# cloud-init creates this user on Packer-built images; USB-installed nodes
# bypass cloud-init so we create it here instead.  Idempotent: no-op if the
# user already exists (e.g. on existing cluster nodes).
if ! id clusteros &>/dev/null; then
    useradd -m -s /bin/bash -G sudo,adm clusteros 2>/dev/null || true
    echo 'clusteros:clusteros' | chpasswd 2>/dev/null || true
    printf 'clusteros ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/clusteros
    chmod 440 /etc/sudoers.d/clusteros
    # Enable SSH password auth (cloud images disable it by default)
    sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config 2>/dev/null || true
    sed -i 's/^#\?KbdInteractiveAuthentication.*/KbdInteractiveAuthentication yes/' /etc/ssh/sshd_config 2>/dev/null || true
    systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || true
    ok "clusteros user created (password: clusteros, sudo NOPASSWD, SSH enabled)"
else
    ok "clusteros user already exists"
fi

# ── Verify all three critical binaries are now on disk ─────────────────────────
echo ""
_INSTALL_OK=true
for _bin in /usr/local/bin/node-agent /usr/local/bin/cluster-make-usb /usr/local/bin/cluster; do
    if [ -x "$_bin" ]; then
        ok "Verified: $_bin"
    else
        err "MISSING after install attempt: $_bin"
        _INSTALL_OK=false
    fi
done
[ "$_INSTALL_OK" = true ] || warn "One or more binaries failed to install — check bundle contents above"
echo ""

# ── Update MOTD ────────────────────────────────────────────────────────────────
# Write a dynamic MOTD script so every SSH login shows the node version,
# available ClusterOS commands, and cluster phase.
mkdir -p /etc/update-motd.d
cat > /etc/update-motd.d/99-clusteros <<'MOTD_SCRIPT'
#!/bin/bash
# ClusterOS dynamic MOTD — runs on every SSH login
CYAN='\033[0;36m'; BOLD='\033[1m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'

HOSTNAME=$(hostname 2>/dev/null || echo "unknown")
NODE_VER=$(/usr/local/bin/node-agent --version 2>/dev/null | head -1 || echo "unknown")
TS_IP=$(tailscale ip -4 2>/dev/null || echo "not connected")

PHASE="unknown"
if [ -f /run/clusteros/status.json ] && command -v jq &>/dev/null; then
    PHASE=$(jq -r '.phase // "unknown"' /run/clusteros/status.json 2>/dev/null)
fi

printf "\n${CYAN}${BOLD}"
printf "  ╔══════════════════════════════════════════════════╗\n"
printf "  ║  ClusterOS                                       ║\n"
printf "  ╚══════════════════════════════════════════════════╝\n"
printf "${NC}"
printf "  Node      : ${BOLD}%s${NC}  (Tailscale: %s)\n" "$HOSTNAME" "$TS_IP"
printf "  Version   : %s\n" "$NODE_VER"
printf "  Phase     : %s\n" "$PHASE"
printf "\n"
printf "  ${BOLD}Commands:${NC}\n"
printf "    cluster status          — cluster health\n"
printf "    cluster dash            — live dashboard\n"
printf "    cluster logs            — node-agent logs\n"
printf "    sudo cluster-make-usb   — build USB installer for new nodes\n"
printf "\n"
MOTD_SCRIPT
chmod +x /etc/update-motd.d/99-clusteros

# Disable the default Ubuntu MOTD scripts that add noise (ads, updates spam)
for _f in /etc/update-motd.d/10-help-text /etc/update-motd.d/50-motd-news \
           /etc/update-motd.d/80-esm-announce /etc/update-motd.d/95-hwe-eol; do
    [ -f "$_f" ] && chmod -x "$_f" 2>/dev/null || true
done
ok "MOTD updated  →  /etc/update-motd.d/99-clusteros"

# Tailscale credentials + auto-auth.
# If the bundle includes tailscale.env (placed by cluster-make-usb or make deploy),
# install it and authenticate now — before any step that might need network.
# This is what gets a fresh node onto the Tailscale overlay so 'make deploy' can
# find it automatically next time without needing the IP to be specified manually.
if [ -f "$SCRIPT_DIR/tailscale.env" ]; then
    mkdir -p /etc/clusteros
    install -m 600 "$SCRIPT_DIR/tailscale.env" /etc/clusteros/tailscale.env
    ok "Tailscale credentials installed → /etc/clusteros/tailscale.env"

    if command -v tailscale >/dev/null 2>&1; then
        if tailscale status --json 2>/dev/null | grep -q '"BackendState":"Running"'; then
            ok "Tailscale already connected ($(tailscale ip -4 2>/dev/null))"
        else
            ok "Running Tailscale auth from bundled credentials..."
            # Run with full output so errors appear in /var/log/clusteros-boot.log
            # tailscale-auth waits up to 2 min for network then tries OAuth → tailscale up
            if /usr/local/bin/clusteros-tailscale-init; then
                ok "Tailscale connected ($(tailscale ip -4 2>/dev/null))"
            else
                warn "Tailscale auth failed — check /var/log/clusteros-boot.log for details"
                warn "Manual fix: sudo tailscale up --authkey=<key>  or  sudo clusteros-tailscale-init"
            fi
        fi
    else
        warn "Tailscale not installed — run 'make deploy' once node has internet to install it"
    fi
fi

# cluster CLI.
if [ -f "$SCRIPT_DIR/cluster" ]; then
    install -m 755 "$SCRIPT_DIR/cluster" /usr/local/bin/cluster
    ok "cluster CLI installed"
fi

# ── Pre-flight: networking sanity ──────────────────────────────────────────────
# Runs before ALL network operations (apt, k3s install, curl, containerd pulls).
step "pre" "Networking pre-flight"

# 0. Write universal netplan so physical hardware gets DHCP + WiFi on first boot.
#    Cloud images default to virtual interface names (ens3); real hardware uses
#    en*/eth*/wl*.  Without this, DHCP never runs → no network → nothing works.
#    The netplan file is bundled from images/ubuntu/files/netplan/99-clusteros.yaml
#    so WiFi credentials only need updating in that one file.
mkdir -p /etc/netplan
if [ -f "$SCRIPT_DIR/99-clusteros.yaml" ]; then
    install -m 600 "$SCRIPT_DIR/99-clusteros.yaml" /etc/netplan/99-clusteros.yaml
    ok "Netplan installed from bundle (wired DHCP + WiFi)"
else
    warn "99-clusteros.yaml not in bundle — netplan not updated (WiFi may not work)"
fi
netplan apply 2>/dev/null || true

# 1. Force apt to IPv4.
#    Cluster nodes typically have no IPv6 internet routing.  Without this, apt
#    resolves archive.ubuntu.com and gets AAAA records first (e.g. 2a06:bc80:...),
#    tries them, hits "Network is unreachable", and only falls back to IPv4 after
#    a long timeout — or fails entirely.  The same applies to pkgs.tailscale.com
#    and any other apt source.  Pinning to IPv4 fixes silent apt failures instantly.
echo 'Acquire::ForceIPv4 "true";' > /etc/apt/apt.conf.d/99clusteros-force-ipv4
ok "apt pinned to IPv4 (skips unreachable IPv6 on bare-metal nodes)"

# 2. Tailscale cooperative exit-node mesh.
#
#    Every cluster node has its own LAN connection.  By advertising each node as an
#    exit node, any online node can serve as an internet gateway for peers whose LAN
#    path is temporarily broken (flaky WiFi, DHCP hiccup, firewall rule, etc.).
#
#    Strategy:
#      a) Test direct internet connectivity (DNS + TCP 443).
#      b) If direct internet works:
#           - Advertise THIS node as an exit node (helps peers).
#           - Clear any previously selected exit node (we don't need one).
#      c) If direct internet is broken:
#           - Find a Tailscale peer that IS advertising an exit node.
#           - Select it so apt/k3s/Docker can route through it.
#           - Also advertise self (so peers can use us once our LAN recovers).
#      d) If nothing works: warn and continue; apt will fail, but that is a
#         physical layer problem, not a Tailscale configuration problem.
#
#    NOTE: On official Tailscale (tailscale.com) exit nodes must be approved in
#    the admin console (Settings → Machines → Edit route settings) or via ACL
#    autoApprovers.  On Headscale add:  "autoApprovers": {"exitNode": ["*"]}
#    to your policy.  Without approval --advertise-exit-node has no effect on peers.

_ts_set() {
    # tailscale set (v1.40+) is non-disruptive; fall back to tailscale up for older clients.
    if tailscale set "$@" 2>/dev/null; then
        return 0
    fi
    tailscale up "$@" 2>/dev/null
}

# Detect local physical subnets (non-loopback, non-Tailscale, /24 or smaller)
# and advertise them as Tailscale subnet routes so peers without direct internet
# can route through this node, and so wired-only nodes can be reached from Tailscale.
_advertise_lan_subnets() {
    local subnets=""
    while IFS= read -r line; do
        # ip addr show gives lines like: inet 192.168.1.10/24 brd ... scope global eth0
        local iface cidr ip prefix
        iface=$(echo "$line" | awk '{print $NF}')
        cidr=$(echo "$line" | awk '{print $2}')
        ip="${cidr%%/*}"
        prefix="${cidr##*/}"
        # Skip Tailscale/loopback/WireGuard/CNI/Flannel interfaces
        case "$iface" in tailscale*|lo|wg*|tun*|cni*|flannel*|veth*) continue ;; esac
        # Only /24 or smaller (prefix >= 24)
        [ "$prefix" -ge 24 ] 2>/dev/null || continue
        # Skip Flannel pod CIDR (10.42.0.0/16) and service CIDR (10.43.0.0/16)
        case "$ip" in 10.42.*|10.43.*) continue ;; esac
        # Compute network address
        local net
        net=$(python3 -c "import ipaddress; print(ipaddress.ip_interface('$cidr').network.with_prefixlen)" 2>/dev/null) || continue
        [ -n "$net" ] || continue
        if [ -z "$subnets" ]; then
            subnets="$net"
        else
            subnets="$subnets,$net"
        fi
    done < <(ip addr show 2>/dev/null | awk '/inet / && !/127\.0\.0\.1/ {print $2, $NF}' | \
             while read cidr iface; do echo "inet $cidr brd . scope global $iface"; done)

    if [ -n "$subnets" ]; then
        if _ts_set --advertise-routes="$subnets" 2>/dev/null; then
            ok "Advertising LAN subnets via Tailscale ($subnets) — wired peers reachable from mesh"
        else
            warn "Could not advertise LAN subnets ($subnets) — needs admin approval in Tailscale console"
        fi
    fi
}

_internet_ok() {
    # Quick dual-check: DNS resolution AND TCP port 443.
    # Both must succeed so a DNS-only or TCP-only outage is caught.
    host -W 3 archive.ubuntu.com 8.8.8.8 &>/dev/null 2>&1 || \
        nslookup -timeout=3 archive.ubuntu.com 8.8.8.8 &>/dev/null 2>&1 || \
        getent hosts archive.ubuntu.com &>/dev/null 2>&1
    DNS_OK=$?
    nc -zw 4 archive.ubuntu.com 443 &>/dev/null 2>&1
    TCP_OK=$?
    [ "$DNS_OK" -eq 0 ] && [ "$TCP_OK" -eq 0 ]
}

if command -v tailscale &>/dev/null; then
    # Pre-flush the nat OUTPUT chain before checking internet reachability.
    # Stale REDIRECT/REJECT rules in nat OUTPUT cause _internet_ok to return
    # false (connection refused / refused on port 443), which makes the script
    # wrongly select a Tailscale exit peer and route all subsequent traffic
    # (apt, containerd image pulls) through that peer — which may itself have
    # issues.  Flushing first means the internet check reflects the actual
    # network state, not an iptables artifact.
    iptables -t nat -F OUTPUT 2>/dev/null || true
    nft flush ruleset 2>/dev/null || true
    if _internet_ok; then
        ok "Direct internet reachable"
        # Advertise this node as a potential exit node for peers that need it.
        if _ts_set --advertise-exit-node=true --accept-routes=true 2>/dev/null; then
            ok "Advertising as Tailscale exit node + accepting peer routes"
        else
            warn "Could not set --advertise-exit-node (may need admin approval on tailscale.com)"
        fi
        # Also advertise local physical LAN subnets so wired-only peers are
        # reachable from the Tailscale mesh and can share internet via this node.
        _advertise_lan_subnets
        # Clear any previously selected exit node — we have direct internet.
        SPLIT_ROUTES=$(ip route show 2>/dev/null | grep -cE '^(0\.0\.0\.0/1|128\.0\.0\.0/1)' || true)
        if [ "${SPLIT_ROUTES:-0}" -gt 0 ]; then
            _ts_set --exit-node= 2>/dev/null || true
            sleep 2
            ok "Cleared stale exit-node selection (direct internet preferred)"
        fi
    else
        warn "Direct internet unreachable — attempting to route via a Tailscale exit-node peer"
        # List peers advertising as exit nodes; pick the first online one.
        EXIT_PEER=""
        if command -v jq &>/dev/null; then
            EXIT_PEER=$(tailscale status --json 2>/dev/null | \
                jq -r '
                  .Peer[]
                  | select(.ExitNodeOption == true and .Online == true)
                  | .TailscaleIPs[0]
                ' 2>/dev/null | head -1)
        fi
        if [ -n "$EXIT_PEER" ]; then
            warn "Trying exit node: $EXIT_PEER"
            if _ts_set --exit-node="$EXIT_PEER" --exit-node-allow-lan-access=true \
                        --accept-routes=true --advertise-exit-node=true 2>/dev/null; then
                sleep 3   # let routing table settle
                if _internet_ok; then
                    ok "Internet restored via exit node $EXIT_PEER"
                else
                    warn "Exit node $EXIT_PEER selected but internet still unreachable"
                    warn "apt / k3s installs may fail — check Tailscale connectivity"
                fi
            else
                warn "Could not set exit node $EXIT_PEER"
            fi
        else
            warn "No online Tailscale exit-node peers found"
            warn "All nodes may have lost internet simultaneously — check LAN/router"
            warn "Proceeding; apt will fail if internet is truly unavailable"
        fi
        # Advertise self regardless so we serve peers once our LAN recovers.
        _ts_set --advertise-exit-node=true --accept-routes=true 2>/dev/null || true
    fi
else
    ok "Tailscale not present — skipping exit node configuration"
fi

# ── Emergency NAT cleanup ───────────────────────────────────────────────────────
# Must run before ANYTHING else (including k3s install) because stale OUTPUT
# REDIRECT rules intercept outbound TCP 80/443 — blocking containerd image pulls,
# helm, and apt.
#
# ROOT CAUSE of why these rules keep coming back:
#   iptables-persistent / netfilter-persistent saves and restores /etc/iptables/rules.v4
#   on every reboot.  If that file was saved while OLD redirect rules were active
#   (by a previous node-agent version that added them), they are restored after
#   every reboot — even after apply-patch.sh and the new daemon have removed them.
#
# Node-agent was already killed at the very top of this script (before
# any flush) to prevent the race condition where old binary re-adds rules.

# 2. Disable ALL persistent firewall services and wipe their rule files.
#    Both iptables-persistent (/etc/iptables/rules.v4) and nftables.service
#    (/etc/nftables.conf) restore stale DROP/REDIRECT rules at every boot.
#    Either can silently block pod networking (CoreDNS ContainerCreating,
#    flannel VXLAN, kube-proxy NodePort) even after an in-memory flush.

for svc in netfilter-persistent iptables-persistent nftables ufw; do
    systemctl stop    "$svc" 2>/dev/null || true
    systemctl disable "$svc" 2>/dev/null || true
done
# Remove network hooks that may restore rules on interface up.
rm -f /etc/network/if-pre-up.d/iptables 2>/dev/null || true
rm -f /etc/network/if-pre-up.d/ip6tables 2>/dev/null || true

# Wipe persisted iptables rule files — nothing should restore them on next boot.
rm -f /etc/iptables/rules.v4 /etc/iptables/rules.v6 2>/dev/null || true
ok "iptables-persistent disabled and stale rules files deleted"

# Ensure the iptables directory exists early so subsequent saves won't fail
mkdir -p /etc/iptables 2>/dev/null || true

# Flush the live nftables ruleset.  On Ubuntu 20.04+ iptables uses the nftables
# backend (iptables-nft), so this also clears any stale iptables rules that were
# stored in nftables tables from previous installs.  We do this BEFORE setting
# our own iptables rules so we start from a clean slate.
nft flush ruleset 2>/dev/null || true

# Write a no-op /etc/nftables.conf so nftables.service cannot re-poison the
# ruleset if it is ever re-enabled (e.g. by an unattended-upgrade).
cat > /etc/nftables.conf << 'NFTEOF'
#!/usr/sbin/nft -f
# Managed by cluster-os apply-patch.sh — do not edit manually.
# All cluster networking is managed by k3s / kube-proxy / node-agent.
flush ruleset
NFTEOF
ok "nftables flushed and /etc/nftables.conf cleared (no-op)"

# 3. Flush ALL OUTPUT nat rules — this removes every REDIRECT regardless of port.
#    We use a full chain flush rather than per-rule removal because an old daemon
#    version might have added rules we don't know about.  The OUTPUT chain only
#    affects locally-originated traffic (apt, containerd, curl); kube-proxy and
#    Tailscale use PREROUTING and POSTROUTING, which are left intact.
iptables -t nat -F OUTPUT 2>/dev/null || true
ok "Flushed iptables nat OUTPUT chain (no REDIRECT rules can block apt/containerd)"

# ── DNS rescue ─────────────────────────────────────────────────────────────────
# k3s rewrites /etc/resolv.conf to point at its CoreDNS service (10.43.0.10).
# If k3s is running but broken (agent can't reach the server, CoreDNS pods haven't
# started), ALL DNS resolution fails — including apt, curl, helm, and any outbound
# connections.  This is the most common reason for "apt update: Could not resolve
# archive.ubuntu.com" on nodes that already have k3s installed.
#
# Fix: run this check BEFORE apt (step 1) so package installs don't fail.
#   If DNS works         → leave /etc/resolv.conf untouched.
#   If DNS is broken     → inject 8.8.8.8/1.1.1.1 as working nameservers.
#   Systemd-resolved     → set FallbackDNS so it survives future k3s restarts.

_dns_working() {
    # Try a real DNS lookup — getent uses the system resolver, so this accurately
    # reflects whether apt will be able to resolve package archive hostnames.
    getent hosts archive.ubuntu.com &>/dev/null 2>&1
}

if ! _dns_working; then
    warn "DNS resolution failing — k3s CoreDNS (10.43.0.10) is probably down"
    if [ -L /etc/resolv.conf ]; then
        # /etc/resolv.conf is a symlink → systemd-resolved is in charge.
        # Configure FallbackDNS so resolved falls back to 8.8.8.8 when primary fails.
        RESOLVED_CONF="/etc/systemd/resolved.conf"
        if [ -f "$RESOLVED_CONF" ] && ! grep -q '^FallbackDNS=' "$RESOLVED_CONF" 2>/dev/null; then
            # Append under [Resolve] section if it exists, otherwise append at end.
            if grep -q '^\[Resolve\]' "$RESOLVED_CONF" 2>/dev/null; then
                sed -i '/^\[Resolve\]/a FallbackDNS=8.8.8.8 1.1.1.1' "$RESOLVED_CONF"
            else
                printf '\n[Resolve]\nFallbackDNS=8.8.8.8 1.1.1.1\n' >> "$RESOLVED_CONF"
            fi
            systemctl restart systemd-resolved 2>/dev/null || true
            sleep 2
        fi
    else
        # Plain resolv.conf file (k3s has written it directly).
        # Prepend public nameservers; keep any existing non-k3s entries below.
        cp /etc/resolv.conf /etc/resolv.conf.pre-patch 2>/dev/null || true
        {
            printf 'nameserver 8.8.8.8\nnameserver 1.1.1.1\n'
            # Keep search/domain lines; drop CoreDNS nameserver (10.x) lines.
            grep -vE '^nameserver 10\.' /etc/resolv.conf.pre-patch 2>/dev/null || true
        } > /etc/resolv.conf
    fi

    if _dns_working; then
        ok "DNS restored with fallback nameservers (8.8.8.8 / 1.1.1.1)"
    else
        warn "DNS still failing after fallback injection — apt installs may fail"
        warn "Check physical network / router connectivity"
    fi
else
    ok "DNS resolution working"
fi

# ── 0. Pause image airgap install ─────────────────────────────────────────────
# The k3s sandbox (pause) image MUST be on disk before k3s starts.
# If it isn't, k3s pulls from Docker Hub/registry.k8s.io — which fails whenever
# the network has issues (stale nftables rules, rate-limits, cold-boot DNS lag).
#
# Strategy — two layers, both always attempted:
#   Layer A: copy the pre-bundled tar from the patch directory into the k3s
#            airgap folder.  k3s auto-imports every *.tar in agent/images/ at
#            startup, so the image is available from disk with zero network I/O.
#            Works even when containerd is not yet running.
#   Layer B: if containerd IS already running (upgrade-in-place), import the
#            tar immediately so running pods recover without a k3s restart.
CONTAINERD_SOCK="/run/k3s/containerd/containerd.sock"
PAUSE_IMG="registry.k8s.io/pause:3.6"
PAUSE_ALIAS="docker.io/rancher/mirrored-pause:3.6"
AIRGAP_DIR="/var/lib/rancher/k3s/agent/images"
AIRGAP_FILE="$AIRGAP_DIR/pause-3.6.tar"
PATCH_DIR="$(cd "$(dirname "$0")" && pwd)"

step "0/10" "Installing pause image airgap bundle"
mkdir -p "$AIRGAP_DIR"

# Layer A — install from the pre-bundled tar shipped with the patch.
# 'make patch' on the dev machine pulls pause:3.6 via docker/skopeo and saves it
# as patch/pause-3.6.tar so every node gets it with no internet dependency.
if [ -f "$PATCH_DIR/pause-3.6.tar" ]; then
    cp -f "$PATCH_DIR/pause-3.6.tar" "$AIRGAP_FILE"
    ok "Installed pause image from patch bundle → $AIRGAP_FILE (network-independent)"
else
    warn "patch/pause-3.6.tar not found — re-run 'make patch' on dev machine with docker/skopeo installed"
    warn "Nodes will attempt to pull pause image from the internet at k3s startup"
fi

# Layer B — hot-inject into running containerd (upgrade-in-place path).
if [ -S "$CONTAINERD_SOCK" ]; then
    if [ -f "$AIRGAP_FILE" ]; then
        # Import the tar we just installed so the running containerd sees it now.
        if k3s ctr images import "$AIRGAP_FILE" 2>/dev/null; then
            k3s ctr images tag "$PAUSE_IMG" "$PAUSE_ALIAS" 2>/dev/null || true
            ok "Hot-imported pause image into running containerd — pods recover without restart"
        fi
    else
        # No bundle — attempt a live pull as best-effort fallback.
        if k3s ctr images pull "$PAUSE_IMG" 2>/dev/null; then
            k3s ctr images tag "$PAUSE_IMG" "$PAUSE_ALIAS" 2>/dev/null || true
            k3s ctr images export "$AIRGAP_FILE" "$PAUSE_IMG" 2>/dev/null || true
            ok "Pulled + cached $PAUSE_IMG from registry.k8s.io (network was available)"
        else
            warn "Live pull of $PAUSE_IMG failed — k3s will retry at startup after rules are cleaned"
        fi
    fi
fi

# ── 1. Dependencies ────────────────────────────────────────────────────────────
step "1/10" "Ensuring dependencies"

# Pre-apt connectivity check: confirm port 80 is reachable before running apt.
# If it still gets connection refused here, an iptables rule is still blocking it.
# Diagnose by dumping the nat OUTPUT chain so we know exactly what rule is at fault.
if ! timeout 3 bash -c 'echo >/dev/tcp/91.189.91.81/80' 2>/dev/null; then
    warn "TCP port 80 still blocked — checking iptables nat OUTPUT chain:"
    iptables -t nat -L OUTPUT -n --line-numbers 2>/dev/null | head -20 || true
    warn "Attempting full nat OUTPUT flush as fallback"
    iptables -t nat -F OUTPUT 2>/dev/null || true
fi

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
# WiFi support — wpasupplicant is required for netplan to connect to WPA2 networks
dpkg -s wpasupplicant    &>/dev/null 2>&1 || PKGS="$PKGS wpasupplicant"
# avahi-daemon — mDNS peer discovery so nodes find each other on the LAN
dpkg -s avahi-daemon     &>/dev/null 2>&1 || PKGS="$PKGS avahi-daemon avahi-utils"
# dnsmasq — DHCP server for p2p ethernet gateway (lets new nodes get internet via us)
dpkg -s dnsmasq          &>/dev/null 2>&1 || PKGS="$PKGS dnsmasq"

if [ -n "$PKGS" ]; then
    apt-get -o Acquire::ForceIPv4=true update -qq
    # shellcheck disable=SC2086
    apt-get -o Acquire::ForceIPv4=true install -y -qq $PKGS
    ok "Installed:$PKGS"
else
    ok "All dependencies present"
fi

# avahi-daemon — advertise this node as a ClusterOS peer over mDNS so other
# nodes on the same Ethernet segment discover it without Tailscale.
# node-agent probes local subnets for Serf port 7946; avahi is an additional
# fallback used by admins (avahi-browse -rtp _clusteros._tcp).
if command -v avahi-daemon &>/dev/null; then
    # Allow avahi on all interfaces including link-local (169.254.x.x, fe80::).
    # This is what makes p2p Ethernet patches work with no DHCP server:
    # avahi multicast runs over link-local so two directly-patched nodes find
    # each other within seconds of link-up.
    AVAHI_CONF=/etc/avahi/avahi-daemon.conf
    if [ -f "$AVAHI_CONF" ]; then
        # Enable IPv4 link-local and disable interface filtering
        sed -i \
            -e 's/^#*use-ipv4=.*/use-ipv4=yes/' \
            -e 's/^#*use-ipv6=.*/use-ipv6=yes/' \
            -e 's/^#*allow-interfaces=.*//' \
            -e 's/^#*deny-interfaces=.*//' \
            -e 's/^#*use-iff-running=.*/use-iff-running=no/' \
            "$AVAHI_CONF" 2>/dev/null || true
    fi

    mkdir -p /etc/avahi/services
    cat > /etc/avahi/services/clusteros.service <<'AVAHIEOF'
<?xml version="1.0" standalone='no'?>
<!DOCTYPE service-group SYSTEM "avahi-service.dtd">
<service-group>
  <name replace-wildcards="yes">ClusterOS node %h</name>
  <service>
    <type>_clusteros._tcp</type>
    <port>7946</port>
  </service>
</service-group>
AVAHIEOF
    systemctl enable --now avahi-daemon 2>/dev/null || true
    systemctl reload-or-restart avahi-daemon 2>/dev/null || true
    ok "avahi-daemon advertising _clusteros._tcp on all interfaces (incl. link-local — p2p patches work)"
fi

# Tailscale — install if not present (chroot may have failed to install it).
# This is the critical step that lets new nodes join the mesh automatically.
if ! command -v tailscale &>/dev/null; then
    ok "Installing Tailscale..."
    if curl -fsSL https://tailscale.com/install.sh | sh - 2>/dev/null; then
        systemctl enable --now tailscaled 2>/dev/null || true
        ok "Tailscale installed and enabled"
    else
        warn "Tailscale install failed — node will not auto-join mesh; connect manually after boot"
    fi
else
    ok "Tailscale already installed ($(tailscale version 2>/dev/null | head -1))"
    # Ensure tailscaled is running
    systemctl start tailscaled 2>/dev/null || true
fi

# k3s — node-agent manages this directly; install binary only (no systemd service).
# INSTALL_K3S_SKIP_ENABLE + INSTALL_K3S_SKIP_START ensure the installer doesn't
# create or start a k3s.service — node-agent owns the lifecycle via exec.
if ! command -v k3s &>/dev/null; then
    ok "Installing k3s (binary only, systemd service will be masked)..."
    if INSTALL_K3S_SKIP_ENABLE=true INSTALL_K3S_SKIP_START=true \
            curl -sfL https://get.k3s.io | sh - 2>/dev/null; then
        ok "k3s installed ($(k3s --version 2>/dev/null | head -1))"
    else
        warn "k3s install failed — ensure internet access or pre-install k3s manually"
    fi
else
    ok "k3s already present ($(k3s --version 2>/dev/null | head -1))"
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

# ── 1d. Extra storage disks ───────────────────────────────────────────────────
# Detect non-boot block devices, format (if unpartitioned), mount at
# /mnt/clusteros/disk-N, persist via fstab, and write the path manifest to
# /etc/clusteros/extra-disks so node-agent can advertise them to Longhorn
# and SLURM (TmpDisk / scratch space).
step "1d/10" "Extra storage disk detection and mounting"

EXTRA_DISK_MANIFEST="/etc/clusteros/extra-disks"
mkdir -p /etc/clusteros

# Identify the root block device (strip partition suffix:
#   /dev/sda2  → sda     /dev/nvme0n1p3 → nvme0n1)
ROOT_DEV=$(findmnt -n -o SOURCE / 2>/dev/null | sed 's|/dev/||')
ROOT_DISK=$(echo "$ROOT_DEV" | sed -E 's/p?[0-9]+$//')

DISK_IDX=0
> "$EXTRA_DISK_MANIFEST"  # start fresh

while IFS= read -r disk; do
    [ -z "$disk" ] && continue
    [ "$disk" = "$ROOT_DISK" ] && continue
    DEV="/dev/$disk"
    [ -b "$DEV" ] || continue

    MOUNT_POINT="/mnt/clusteros/disk-$DISK_IDX"

    # Already mounted at the expected location — just record it.
    if mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
        ok "Extra disk $DEV already mounted at $MOUNT_POINT"
        echo "$MOUNT_POINT" >> "$EXTRA_DISK_MANIFEST"
        DISK_IDX=$((DISK_IDX + 1))
        continue
    fi

    # Mounted somewhere else — skip rather than double-mount.
    if grep -q "^$DEV " /proc/mounts 2>/dev/null; then
        warn "Extra disk $DEV already mounted elsewhere — skipping"
        continue
    fi

    # Skip tiny devices (< 1 GiB) — likely card readers or virtual devices.
    DISK_BYTES=$(lsblk -b -d -n -o SIZE "$DEV" 2>/dev/null || echo 0)
    if [ "${DISK_BYTES:-0}" -lt 1073741824 ]; then
        warn "Extra disk $DEV too small (<1 GiB) — skipping"
        continue
    fi

    # Only format if completely empty (no filesystem signature).
    FS_TYPE=$(blkid -o value -s TYPE "$DEV" 2>/dev/null)
    if [ -z "$FS_TYPE" ]; then
        ok "Formatting $DEV (ext4, unpartitioned disk, label=clusteros-disk-$DISK_IDX)..."
        if ! mkfs.ext4 -L "clusteros-disk-$DISK_IDX" -m 1 -q "$DEV" 2>/dev/null; then
            warn "mkfs.ext4 failed on $DEV — skipping"
            continue
        fi
        FS_TYPE="ext4"
    else
        ok "Extra disk $DEV: existing $FS_TYPE filesystem detected (not reformatted)"
    fi

    mkdir -p "$MOUNT_POINT"
    if ! mount "$DEV" "$MOUNT_POINT" 2>/dev/null; then
        warn "mount $DEV → $MOUNT_POINT failed — skipping"
        continue
    fi

    # Persist via fstab using UUID so renames (sda→sdb) don't break it.
    UUID=$(blkid -o value -s UUID "$DEV" 2>/dev/null)
    if [ -n "$UUID" ] && ! grep -q "$UUID" /etc/fstab 2>/dev/null; then
        echo "UUID=$UUID  $MOUNT_POINT  $FS_TYPE  defaults,nofail,x-systemd.automount  0 2" >> /etc/fstab
    fi

    # Open permissions — Longhorn and SLURM jobs need write access.
    chmod 1777 "$MOUNT_POINT" 2>/dev/null || true

    DISK_SIZE=$(lsblk -d -n -o SIZE "$DEV" 2>/dev/null)
    ok "Mounted extra disk $DEV ($DISK_SIZE) → $MOUNT_POINT"
    echo "$MOUNT_POINT" >> "$EXTRA_DISK_MANIFEST"
    DISK_IDX=$((DISK_IDX + 1))
done < <(lsblk -d -n -o NAME,TYPE 2>/dev/null | awk '$2=="disk"{print $1}')

if [ "$DISK_IDX" -gt 0 ]; then
    # /scratch → first extra disk (SLURM job scratch + general large-file use)
    FIRST_EXTRA=$(head -1 "$EXTRA_DISK_MANIFEST")
    mkdir -p "$FIRST_EXTRA/scratch"
    if [ ! -L /scratch ]; then
        ln -sfn "$FIRST_EXTRA/scratch" /scratch
        ok "/scratch → $FIRST_EXTRA/scratch (SLURM scratch space)"
    fi
    ok "Extra disks: $DISK_IDX found — paths in $EXTRA_DISK_MANIFEST"
else
    ok "No extra disks found — cluster will use root disk only"
    rm -f "$EXTRA_DISK_MANIFEST"
fi

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

# Kill orphaned k3s-embedded containerd.
#
# pkill -9 on 'k3s server'/'k3s agent' SIGKILLs k3s but leaves its embedded
# containerd subprocess orphaned — containerd is a separate child process that
# keeps running at /run/k3s/containerd/containerd.sock with the OLD in-memory
# config (sandbox_image="rancher/mirrored-pause:3.6").  When the new k3s binary
# starts it finds the socket, reuses the running containerd, and inherits the
# stale config — even though the new binary passes
# --pause-image registry.k8s.io/pause:3.6.  Killing it here forces a fresh
# containerd start with the correct config every time.
pkill -TERM -f '/run/k3s/containerd/containerd' 2>/dev/null || true
sleep 2
pkill -KILL -f '/run/k3s/containerd/containerd' 2>/dev/null || true
rm -f /run/k3s/containerd/containerd.sock 2>/dev/null || true
ok "All services stopped (incl. orphaned k3s containerd)"

# Mask SLURM + munge systemd units — node-agent manages them directly via exec.
# Without masking, systemd races node-agent and starts slurmd before slurm.conf exists.
for svc in slurmd slurmctld munge; do
    systemctl disable "$svc".service 2>/dev/null || true
    systemctl mask    "$svc".service 2>/dev/null || true
done
ok "Masked slurmd / slurmctld / munge (node-agent manages these directly)"

# Mask k3s systemd units — node-agent manages k3s directly via exec.
# Without masking, the k3s installer's systemd service races node-agent's direct
# exec, causing double-start and port 6443 conflicts.
for svc in k3s k3s-agent; do
    systemctl stop    "$svc".service 2>/dev/null || true
    systemctl disable "$svc".service 2>/dev/null || true
    systemctl mask    "$svc".service 2>/dev/null || true
done
ok "Masked k3s / k3s-agent (node-agent manages these directly)"

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

# K3s agent — surgical wipe: remove auth/bootstrap/config files so the agent
# re-bootstraps against the correct leader, but PRESERVE the containerd image
# store (/var/lib/rancher/k3s/agent/containerd/).
#
# WHY preserve containerd?
#   Wiping the full agent dir removes all cached images, including pause:3.6.
#   On the next start k3s must re-pull pause:3.6 from the internet.  If the
#   node has no internet at that moment (common when the exit-node setup is
#   still converging), pod sandbox creation fails with
#   "failed to get sandbox image rancher/mirrored-pause:3.6: connection refused".
#   Keeping the containerd blobs means the image is served from disk and no
#   network pull is needed.
if [ -d /var/lib/rancher/k3s/agent ]; then
    for _stale in etc .lock server-ca.crt server-ca.key \
                  client-ca.crt client-kubelet.crt client-kubelet.key \
                  kubelet.kubeconfig token; do
        rm -rf "/var/lib/rancher/k3s/agent/${_stale}" 2>/dev/null || true
    done
    ok "Cleared k3s agent auth/config (containerd image store preserved)"
fi

# ── 5b. Pre-seed cluster CA certificate ────────────────────────────────────────
# Root cause of "failed to get CA certs: connection reset by peer" every 2 s:
#   k3s agent starts an internal load-balancer on port 6444 that proxies to the
#   k3s server on 6443.  During the LB's backend health-probe warm-up (typically
#   5–30 s) the LB RSTs any incoming connection.  Agent goroutines fetching
#   /cacerts through 6444 hit these RSTs and log the error every 2 s.
#
# Fix: place a pre-shared CA cert so k3s agent trusts the server TLS immediately
# without fetching /cacerts at all.  k3s uses two file paths:
#   /var/lib/rancher/k3s/agent/server-ca.crt  — agent reads at startup to trust
#       the server TLS handshake; skips the /cacerts HTTP fetch entirely.
#   /var/lib/rancher/k3s/server/tls/server-ca.crt + server-ca.key  — if present
#       when k3s server starts, k3s uses this CA instead of generating a new one.
#       All nodes sharing the same CA means workers always have the right cert.
#
# This step runs AFTER the surgical wipe above so the cert is not removed again.
if [ -f "$PATCH_DIR/k3s-ca.crt" ] && [ -f "$PATCH_DIR/k3s-ca.key" ]; then
    # Agent path — all nodes (leader and workers).
    mkdir -p /var/lib/rancher/k3s/agent
    cp -f "$PATCH_DIR/k3s-ca.crt" /var/lib/rancher/k3s/agent/server-ca.crt
    chmod 644 /var/lib/rancher/k3s/agent/server-ca.crt
    ok "Installed cluster CA → /var/lib/rancher/k3s/agent/server-ca.crt (agent will skip /cacerts fetch)"

    # Server path — leader only, but harmless on workers (k3s server is not run there).
    mkdir -p /var/lib/rancher/k3s/server/tls
    cp -f "$PATCH_DIR/k3s-ca.crt" /var/lib/rancher/k3s/server/tls/server-ca.crt
    cp -f "$PATCH_DIR/k3s-ca.key" /var/lib/rancher/k3s/server/tls/server-ca.key
    chmod 644 /var/lib/rancher/k3s/server/tls/server-ca.crt
    chmod 600 /var/lib/rancher/k3s/server/tls/server-ca.key
    ok "Installed cluster CA → /var/lib/rancher/k3s/server/tls/ (leader will use shared CA)"
else
    warn "patch/k3s-ca.crt not found — run 'make patch' on dev machine to generate it"
    warn "k3s agent will fall back to fetching /cacerts (expect brief 6444 RST errors at boot)"
fi

# K3s IP-marker file used by the new server.go to detect node IP changes
rm -f /var/lib/rancher/k3s/.cluster-os-ip 2>/dev/null || true

# Stale slurm.conf — will be regenerated with the correct controller IP
rm -f /etc/slurm/slurm.conf 2>/dev/null || true
ok "Removed stale slurm.conf"

# ---------------------------------------------------------------------------
# GPU detection — write /etc/slurm/gres.conf so SLURM can schedule GPU jobs.
# node-agent publishes the gpu= Serf tag on startup using the same detection
# logic; gres.conf must list the same devices or slurmctld rejects the node.
# ---------------------------------------------------------------------------
GPU_GRES_LINES=""

# NVIDIA: each GPU exposes /dev/nvidia0, /dev/nvidia1, …
# Use AutoDetect=nvml when NVML (nvidia-smi / libcuda) is present — it reads
# UUID, type, and topology automatically.  Fall back to manual /dev entries.
NVIDIA_DEVS=( /dev/nvidia[0-9]* )
if [ -e "${NVIDIA_DEVS[0]}" ]; then
    if command -v nvidia-smi &>/dev/null; then
        # AutoDetect=nvml covers all present GPUs; no per-device lines needed.
        GPU_GRES_LINES="${GPU_GRES_LINES}AutoDetect=nvml\n"
        ok "NVIDIA GPU(s) detected — gres.conf: AutoDetect=nvml (${#NVIDIA_DEVS[@]} device(s))"
    else
        # nvidia-smi not installed: list devices manually
        for dev in "${NVIDIA_DEVS[@]}"; do
            GPU_GRES_LINES="${GPU_GRES_LINES}Name=gpu Type=nvidia File=${dev}\n"
        done
        ok "NVIDIA GPU(s) detected — gres.conf: ${#NVIDIA_DEVS[@]} manual device(s)"
    fi
fi

# AMD: renderD128, renderD129, … — vendor ID 0x1002
AMD_COUNT=0
for render in /sys/class/drm/renderD*; do
    [ -f "$render/device/vendor" ] || continue
    vendor=$(cat "$render/device/vendor" 2>/dev/null || true)
    if [ "$vendor" = "0x1002" ]; then
        dev_node="/dev/$(basename "$render")"
        GPU_GRES_LINES="${GPU_GRES_LINES}Name=gpu Type=amd File=${dev_node}\n"
        AMD_COUNT=$((AMD_COUNT + 1))
    fi
done
[ "$AMD_COUNT" -gt 0 ] && ok "AMD GPU(s) detected — gres.conf: ${AMD_COUNT} device(s)"

mkdir -p /etc/slurm
if [ -n "$GPU_GRES_LINES" ]; then
    printf "%b" "$GPU_GRES_LINES" > /etc/slurm/gres.conf
    ok "Wrote /etc/slurm/gres.conf"
else
    # No GPUs — write an empty gres.conf so SLURM doesn't complain if
    # a prior stale file listed devices that no longer exist.
    printf "" > /etc/slurm/gres.conf
fi

# Serf status file — stale phase/leader values would confuse the new daemon
rm -f /run/clusteros/status.json 2>/dev/null || true

# Munge socket left behind by a crashed munged
rm -f /var/run/munge/munge.socket.2 2>/dev/null || true

# Orphaned kubelet pod volumes — kubelet refuses to rmdir() a volume if the
# directory is non-empty (e.g. Longhorn local-volume data left behind when the
# pod was force-deleted or the node crashed).  kubelet logs these as:
#   "orphaned pod found, but failed to rmdir() volume ... directory not empty"
# and retries every 2 s indefinitely — filling the journal with noise.
#
# Root cause of persistence across patches: Longhorn local-volumes use nested
# bind-mounts.  Per-mount 'umount -l' misses nested entries so directories stay
# non-empty.  'umount -R -f' (recursive + force) walks the entire subtree and
# detaches ALL mounts in one call, after which rm -rf succeeds.
# k3s is already stopped above, so this is safe.
KUBELET_PODS_DIR="/var/lib/kubelet/pods"
if [ -d "$KUBELET_PODS_DIR" ]; then
    # Two-pass unmount to reliably remove Longhorn local-volume nested bind-mounts.
    #
    # Pass 1 — enumerate all mount points under the pods dir and lazy-unmount each
    #   individually in reverse depth order (deepest/longest path first).
    #   'umount -l' (lazy) succeeds even on busy mounts by detaching from the
    #   namespace immediately; the kernel cleans up when the last fd is closed.
    #   This handles cases where 'umount -R' fails with "target is busy".
    if command -v findmnt &>/dev/null; then
        findmnt -R -n -o TARGET "$KUBELET_PODS_DIR" 2>/dev/null \
            | awk 'length > 0' \
            | sort -r \
            | while IFS= read -r mp; do
                [ "$mp" = "$KUBELET_PODS_DIR" ] && continue
                umount -l "$mp" 2>/dev/null || true
              done || true   # findmnt exits 1 when no mounts found; || true prevents pipefail exit
    fi
    # Pass 2 — recursive force-unmount for anything pass 1 missed.
    umount -R -f "$KUBELET_PODS_DIR" 2>/dev/null || true
    rm -rf "$KUBELET_PODS_DIR" || true
    mkdir -p "$KUBELET_PODS_DIR"
    ok "Cleared kubelet pod volume trees (findmnt lazy-unmount + recursive remove)"
fi

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

# ── 5b. Pre-seed k3s cluster CA certificate ────────────────────────────────────
# PURPOSE: eliminate the "failed to get CA certs: connection reset by peer" loop.
#
# How the loop happens without this step:
#   1. k3s agent starts and its internal load-balancer (port 6444) begins probing
#      the upstream k3s server (port 6443).
#   2. While the probe is still warming up, k3s's own goroutines try to fetch the
#      cluster CA cert through the same LB: GET https://127.0.0.1:6444/cacerts
#   3. The LB resets the connection (backend health probe not confirmed yet) →
#      k3s logs "failed to get CA certs: connection reset by peer" every 2 s.
#   4. This is NORMAL for the first 30-60 s but fills the journal with noise.
#      If the server is genuinely unreachable it loops indefinitely.
#
# Fix — pre-seed the CA cert before k3s agent starts:
#   • /var/lib/rancher/k3s/agent/server-ca.crt  — agent reads this to trust the
#     server's TLS cert; skips the /cacerts HTTP fetch entirely.
#   • /var/lib/rancher/k3s/server/tls/server-ca.crt + .key — whichever node
#     becomes leader uses these instead of generating a new CA, so ALL nodes
#     share the same CA (consistent TLS across the cluster without extra fetching).
#
# The CA cert+key pair is generated ONCE during 'make patch' and bundled here.
# Stable across re-deployments: the same CA is reused until you delete
# patch/k3s-ca.crt and re-run 'make patch'.

if [ -f "$SCRIPT_DIR/k3s-ca.crt" ]; then
    step "5b/10" "Pre-seeding k3s cluster CA certificate"

    # Server directory — used by whichever node wins leader election.
    # k3s server reads server-ca.crt/.key at startup; if present it uses them
    # rather than generating a new self-signed CA.  All nodes share the same
    # CA so any node can verify any other's cert without a separate fetch step.
    mkdir -p /var/lib/rancher/k3s/server/tls
    cp "$SCRIPT_DIR/k3s-ca.crt" /var/lib/rancher/k3s/server/tls/server-ca.crt
    chmod 0644 /var/lib/rancher/k3s/server/tls/server-ca.crt
    if [ -f "$SCRIPT_DIR/k3s-ca.key" ]; then
        cp "$SCRIPT_DIR/k3s-ca.key" /var/lib/rancher/k3s/server/tls/server-ca.key
        chmod 0600 /var/lib/rancher/k3s/server/tls/server-ca.key
    fi

    # Agent directory — agent checks this file BEFORE making the /cacerts HTTP
    # request.  Finding a trusted CA here means the LB (6444) bootstrap fetch
    # is skipped entirely: no "connection reset by peer" noise during startup.
    mkdir -p /var/lib/rancher/k3s/agent
    cp "$SCRIPT_DIR/k3s-ca.crt" /var/lib/rancher/k3s/agent/server-ca.crt
    chmod 0644 /var/lib/rancher/k3s/agent/server-ca.crt

    ok "k3s cluster CA pre-seeded → server/tls/ and agent/server-ca.crt"
    ok "  Agents will trust the leader TLS cert immediately (no /cacerts fetch loop)"
else
    warn "No k3s-ca.crt in patch directory — k3s will generate a new CA on leader start"
    warn "  Run 'make patch' on the build machine to generate and bundle the CA cert"
fi

# If a pre-seeded token was provided with the patch, install it so agents can
# bootstrap without fetching a token from the leader over the network.
if [ -f "$SCRIPT_DIR/k3s-token" ]; then
    mkdir -p /var/lib/rancher/k3s/agent
    cp -f "$SCRIPT_DIR/k3s-token" /var/lib/rancher/k3s/agent/token
    chmod 0600 /var/lib/rancher/k3s/agent/token || true
    ok "Installed pre-seeded k3s token → /var/lib/rancher/k3s/agent/token"
fi

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

    # If a fallback munge key is bundled in the patch directory, install it now.
    if [ -f "$SCRIPT_DIR/munge.key" ]; then
        step "6a/10" "Installing fallback munge key from patch/munge.key"
        cp "$SCRIPT_DIR/munge.key" /etc/munge/munge.key
        chmod 0400 /etc/munge/munge.key
        if id munge &>/dev/null; then
            chown munge:munge /etc/munge/munge.key
        fi
        ok "Installed fallback munge key to /etc/munge/munge.key (0400)"
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
open_port 80    tcp   # HTTP  — nginx-ingress hostNetwork (no REDIRECT needed)
open_port 443   tcp   # HTTPS — nginx-ingress hostNetwork
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

# Remove any stale NAT REDIRECTs (80→30080, 443→30443) that may redirect
# outbound or inbound traffic to local ports. Older versions created PREROUTING
# or OUTPUT REDIRECT rules which interfere with outbound TLS and local tests.
cleanup_redirect() {
    local from="$1" to="$2"
    # PREROUTING
    while iptables -t nat -C PREROUTING -p tcp --dport "$from" -j REDIRECT --to-ports "$to" 2>/dev/null; do
        iptables -t nat -D PREROUTING -p tcp --dport "$from" -j REDIRECT --to-ports "$to" 2>/dev/null || break
    done
    # OUTPUT (local process redirection)
    while iptables -t nat -C OUTPUT -p tcp --dport "$from" -j REDIRECT --to-ports "$to" 2>/dev/null; do
        iptables -t nat -D OUTPUT -p tcp --dport "$from" -j REDIRECT --to-ports "$to" 2>/dev/null || break
    done
}

cleanup_redirect 80 30080 || true
cleanup_redirect 443 30443 || true

# UFW FORWARD policy (required for pod traffic through kube-proxy DNAT).
# Default is DROP; pods cannot route through the host without ACCEPT.
if [ -f /etc/default/ufw ] && grep -q '^DEFAULT_FORWARD_POLICY="DROP"' /etc/default/ufw; then
    sed -i 's/^DEFAULT_FORWARD_POLICY="DROP"/DEFAULT_FORWARD_POLICY="ACCEPT"/' /etc/default/ufw
    ufw reload &>/dev/null 2>&1 || true
    ok "UFW FORWARD policy set to ACCEPT (was DROP — required for pod routing)"
fi

# IP forwarding — nodes must route packets between LAN, Tailscale, and pod network.
sysctl -w net.ipv4.ip_forward=1 &>/dev/null || true
cat > /etc/sysctl.d/99-clusteros.conf <<'EOF'
net.ipv4.ip_forward = 1
net.ipv6.conf.all.forwarding = 1
EOF
sysctl -p /etc/sysctl.d/99-clusteros.conf &>/dev/null || true
ok "IP forwarding enabled (persistent via /etc/sysctl.d/99-clusteros.conf)"

# Pod CIDR FORWARD rules — Flannel VXLAN packets must traverse the FORWARD chain.
for cidr in "10.42.0.0/16" "10.43.0.0/16"; do
    iptables -C FORWARD -d "$cidr" -j ACCEPT 2>/dev/null || iptables -I FORWARD 1 -d "$cidr" -j ACCEPT 2>/dev/null || true
    iptables -C FORWARD -s "$cidr" -j ACCEPT 2>/dev/null || iptables -I FORWARD 1 -s "$cidr" -j ACCEPT 2>/dev/null || true
done
for iface in flannel.1 cni0; do
    ufw allow in on "$iface" &>/dev/null 2>&1 || true
    iptables -C FORWARD -i "$iface" -j ACCEPT 2>/dev/null || iptables -I FORWARD 1 -i "$iface" -j ACCEPT 2>/dev/null || true
    iptables -C FORWARD -o "$iface" -j ACCEPT 2>/dev/null || iptables -I FORWARD 1 -o "$iface" -j ACCEPT 2>/dev/null || true
done
ok "Pod CIDRs 10.42.0.0/16 + 10.43.0.0/16 allowed in FORWARD"

# ── P2P Ethernet Gateway ──────────────────────────────────────────────────────────
# When a new node is connected via a direct Ethernet cable (p2p) to this node, it
# may have no direct internet path.  It needs internet to authenticate with Tailscale
# before it can join the cluster.  This node acts as a NAT gateway:
#   1. Assign 10.200.0.1/24 to any physical Ethernet interface that has no routable IP
#      (link-local/APIPA only — meaning no DHCP server on that segment).
#   2. Run dnsmasq DHCP on those interfaces so the new node gets 10.200.0.x + gateway.
#   3. MASQUERADE (NAT) traffic from 10.200.0.0/24 → internet so Tailscale OAuth works.
#
# On the new node side: apply-patch.sh already sets dhcp4:true in netplan, so it will
# automatically pick up the DHCP lease and get a default route via this node.
if command -v dnsmasq &>/dev/null; then
    # Always write base config FIRST so dnsmasq never tries to open port 53
    # (which conflicts with systemd-resolved on 127.0.0.53).  This must be
    # unconditional — if we only write it inside the p2p block, nodes without
    # a p2p link leave dnsmasq in its default port-53 mode and it fails.
    mkdir -p /etc/dnsmasq.d
    cat > /etc/dnsmasq.d/clusteros-base.conf << 'BASE_EOF'
# ClusterOS: DHCP-only mode — do not act as a DNS server
# systemd-resolved already owns port 53 on this host.
no-resolv
port=0
except-interface=lo
BASE_EOF

    # Stable per-node gateway IP using Tailscale last octet.
    # Persisted so reboots work even when Tailscale isn't up yet at service start.
    # Using unique /24 subnets avoids ARP conflicts when multiple Tailscale nodes
    # share the same L2 ethernet segment.
    GW_OCTET_FILE=/var/lib/clusteros/p2p-gateway-octet
    if [ -f "$GW_OCTET_FILE" ] && [ -s "$GW_OCTET_FILE" ]; then
        GW_OCTET=$(cat "$GW_OCTET_FILE")
    else
        GW_OCTET=$(tailscale ip -4 2>/dev/null | awk -F. '{print $4}' | head -1 | tr -d ' \n')
        [ -z "$GW_OCTET" ] && GW_OCTET=$(hostname | cksum | awk '{print ($1 % 253) + 1}')
        mkdir -p /var/lib/clusteros
        printf '%s' "$GW_OCTET" > "$GW_OCTET_FILE"
    fi
    GW_IP="10.200.${GW_OCTET}.1"
    GW_SUBNET="10.200.${GW_OCTET}.0/24"
    DHCP_START="10.200.${GW_OCTET}.100"
    DHCP_END="10.200.${GW_OCTET}.200"

    P2P_IFACES=""
    for iface in $(ls /sys/class/net/ 2>/dev/null); do
        # Physical ethernet only — skip loopback, WiFi, tunnels, virtual, overlay
        case "$iface" in lo|tailscale*|wg*|tun*|utun*|docker*|cni*|flannel*|veth*|br-*|dummy*) continue ;; esac
        [ -d "/sys/class/net/$iface/wireless" ] && continue  # skip WiFi
        [ "$(cat /sys/class/net/$iface/operstate 2>/dev/null)" = "up" ] || continue
        # Skip if interface already has a routable (non-APIPA) IPv4 from DHCP/static.
        # Routable = any address that's NOT 169.254.x.x, NOT a pod/overlay address, and
        # NOT our own p2p gateway subnet (10.200.x.x) — otherwise second runs see our
        # own gateway IP as "routable" and skip reconfiguration.
        ROUTABLE=$(ip addr show dev "$iface" 2>/dev/null | \
            grep -E 'inet ' | grep -v '169\.254\.\|10\.42\.\|10\.43\.\|10\.200\.\|127\.' | head -1)
        if [ -z "$ROUTABLE" ]; then
            # No routable IP — configure as p2p gateway interface
            ip addr replace "${GW_IP}/24" dev "$iface" 2>/dev/null || true
            P2P_IFACES="$P2P_IFACES $iface"
        fi
    done

    if [ -n "$P2P_IFACES" ]; then
        # Write dnsmasq config for DHCP on p2p interfaces
        # Regenerate each time to stay in sync with current interface state
        cat > /etc/dnsmasq.d/clusteros-p2p.conf << DNSMASQ_EOF
# ClusterOS p2p gateway — auto-generated by apply-patch.sh
# Serves DHCP on direct Ethernet links so new nodes can reach internet via this node

# Only serve DHCP on p2p interfaces (not on LAN-connected interfaces with a router)
$(for i in $P2P_IFACES; do echo "interface=$i"; done)
bind-interfaces
listen-address=${GW_IP}

# DHCP pool: ${DHCP_START}-${DHCP_END}, lease 12h, subnet /24
dhcp-range=${DHCP_START},${DHCP_END},255.255.255.0,12h

# Default gateway is this node (${GW_IP})
dhcp-option=option:router,${GW_IP}
# DNS: use public resolvers so new nodes can reach login.tailscale.com
dhcp-option=option:dns-server,8.8.8.8,1.1.1.1
DNSMASQ_EOF

        systemctl enable dnsmasq 2>/dev/null || true
        systemctl restart dnsmasq 2>/dev/null && ok "dnsmasq DHCP gateway started on p2p interface(s):$P2P_IFACES (gateway ${GW_IP})" \
            || warn "dnsmasq failed to start — check: journalctl -u dnsmasq"

        # MASQUERADE: NAT traffic from p2p subnet to internet
        iptables -t nat -C POSTROUTING -s "${GW_SUBNET}" -j MASQUERADE 2>/dev/null || \
            iptables -t nat -A POSTROUTING -s "${GW_SUBNET}" -j MASQUERADE 2>/dev/null || true
        iptables -C FORWARD -s "${GW_SUBNET}" -j ACCEPT 2>/dev/null || \
            iptables -I FORWARD 1 -s "${GW_SUBNET}" -j ACCEPT 2>/dev/null || true
        iptables -C FORWARD -d "${GW_SUBNET}" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || \
            iptables -I FORWARD 2 -d "${GW_SUBNET}" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || true
        ok "P2P gateway NAT/MASQUERADE enabled for ${GW_SUBNET} — new nodes can reach internet via this node"
    else
        ok "All physical ethernet interfaces have routable IPs — p2p gateway not needed on this node"
        # Clean up leftover p2p dhcp config (base config stays — keeps dnsmasq quiet)
        rm -f /etc/dnsmasq.d/clusteros-p2p.conf 2>/dev/null || true
        # Restart dnsmasq with base-only config so it doesn't fail on stale p2p conf
        systemctl enable dnsmasq 2>/dev/null || true
        systemctl restart dnsmasq 2>/dev/null || true
    fi
else
    warn "dnsmasq not installed — p2p gateway unavailable (run: apt-get install dnsmasq)"
fi
# ─────────────────────────────────────────────────────────────────────────────────

# Snapshot the clean iptables state after all our rules are in place.
# iptables-persistent is disabled so this file won't be auto-loaded on boot,
# but it acts as a safety net if someone re-enables the service later — they
# get our clean rules rather than old stale REDIRECT/DROP rules.
if command -v iptables-save &>/dev/null; then
    mkdir -p /etc/iptables
    # Write atomically to avoid partial files if the save fails mid-write.
    if iptables-save > /etc/iptables/rules.v4.tmp 2>/dev/null; then
        mv /etc/iptables/rules.v4.tmp /etc/iptables/rules.v4
    else
        rm -f /etc/iptables/rules.v4.tmp 2>/dev/null || true
    fi
    ok "Saved clean iptables snapshot to /etc/iptables/rules.v4 (iptables-persistent is disabled)"
fi

# ── 7b. K3s pause image + registry mirrors ────────────────────────────────────
# Two-layer fix for "failed to get sandbox image rancher/mirrored-pause:3.6":
#
# Layer 1 — config.yaml (persistent, read by k3s server AND agent at every start)
#   Forces sandbox_image = registry.k8s.io/pause:3.6 in containerd's CRI config.
#   More reliable than --pause-image CLI flags because this file is always read
#   regardless of how k3s is invoked.
#
# Layer 2 — registries.yaml rewrite (containerd-level redirect, belt-and-suspenders)
#   Even if containerd still requests "rancher/mirrored-pause:3.6", the rewrite
#   intercepts the pull BEFORE it reaches the network and redirects to
#   registry.k8s.io/pause:3.6 (Google CDN, not Docker Hub).
step "7b/10" "Configuring k3s pause image and registry mirrors"

mkdir -p /etc/rancher/k3s /etc/clusteros

# Reliable upstream DNS file for k3s CoreDNS.
# k3s reads this via 'resolv-conf' below to configure CoreDNS's forwarding upstreams.
# Using public DNS (8.8.8.8/1.1.1.1) means CoreDNS can always resolve external names
# even if the node's ISP DNS is unreachable.  This also prevents the CoreDNS Corefile
# from being configured with "forward . 10.43.0.10" (circular) when the cluster DNS
# entry somehow ends up in /etc/resolv.conf before k3s reads it.
cat > /etc/clusteros/resolv.conf <<'DNSEOF'
nameserver 8.8.8.8
nameserver 1.1.1.1
nameserver 8.8.4.4
DNSEOF
ok "Created /etc/clusteros/resolv.conf with reliable upstream DNS for k3s CoreDNS"

# Configure systemd-resolved FallbackDNS so DNS survives k3s restarts / crashes.
# When k3s is down, /etc/resolv.conf still points to 10.43.0.10 (CoreDNS),
# but with FallbackDNS set, systemd-resolved falls back to 8.8.8.8 automatically.
if [ -f /etc/systemd/resolved.conf ]; then
    if ! grep -q '^FallbackDNS=' /etc/systemd/resolved.conf 2>/dev/null; then
        if grep -q '^\[Resolve\]' /etc/systemd/resolved.conf 2>/dev/null; then
            sed -i '/^\[Resolve\]/a FallbackDNS=8.8.8.8 1.1.1.1' /etc/systemd/resolved.conf
        else
            printf '\n[Resolve]\nFallbackDNS=8.8.8.8 1.1.1.1\n' >> /etc/systemd/resolved.conf
        fi
        systemctl restart systemd-resolved 2>/dev/null || true
        ok "systemd-resolved: FallbackDNS=8.8.8.8 1.1.1.1 configured"
    else
        ok "systemd-resolved: fallback DNS already configured"
    fi
fi

# Layer 1: k3s persistent config — forces the correct pause image for all modes
cat > /etc/rancher/k3s/config.yaml <<'K3SCFG'
# ClusterOS: use the official k8s pause image from Google CDN (not Docker Hub).
# Docker Hub (registry-1.docker.io) is frequently rate-limited or unreachable;
# this setting applies to both k3s server and k3s agent at every startup.
pause-image: "registry.k8s.io/pause:3.6"
# Use our reliable upstream DNS file so CoreDNS always has 8.8.8.8/1.1.1.1 as
# forwarders — even if the node's /etc/resolv.conf is pointing to CoreDNS itself.
resolv-conf: "/etc/clusteros/resolv.conf"
# Force Flannel to use the Tailscale interface for VXLAN overlay.
# Without this, Flannel may select the LAN interface (e.g. 192.168.1.x) as the
# VTEP public-ip. Cross-node pod traffic would then be VXLAN-encapsulated to the
# LAN IP, which may not be reachable from all nodes (especially wired-only nodes
# or nodes on different subnets). Tailscale provides a stable, encrypted overlay
# that reaches all nodes regardless of physical network topology.
flannel-iface: "tailscale0"
K3SCFG
ok "k3s config.yaml written → pause-image + resolv-conf + flannel-iface=tailscale0"

# Layer 2: containerd rewrite rule — redirects pause image pulls at network level.
# If containerd still requests "docker.io/rancher/mirrored-pause:3.6" (e.g. from a
# stale CRI config), the rewrite "^rancher/mirrored-pause:(.*)" → "pause:$1" strips
# the rancher prefix, and the registry.k8s.io endpoint serves the correct image.
# Other docker.io images fall through to registry-1.docker.io unchanged.
cat > /etc/rancher/k3s/registries.yaml <<'REGSEOF'
mirrors:
  "docker.io":
    endpoint:
      - "https://registry.k8s.io"
      - "https://registry-1.docker.io"
    rewrite:
      "^rancher/mirrored-pause:(.*)": "pause:$1"
  "registry.k8s.io":
    endpoint:
      - "https://registry.k8s.io"
  "ghcr.io":
    endpoint:
      - "https://ghcr.io"
REGSEOF
ok "registries.yaml written → docker.io/rancher/mirrored-pause:X redirected to registry.k8s.io/pause:X"

# ── 8. Install files ───────────────────────────────────────────────────────────
step "8/10" "Installing files"

# Show bundle contents so deploy logs clearly show what was uploaded.
echo "  Bundle contents in $SCRIPT_DIR:"
ls -1 "$SCRIPT_DIR/" 2>/dev/null | sed 's/^/    /' || true

mkdir -p /run/clusteros
chmod 755 /run/clusteros

# Backup old binary
mkdir -p /usr/local/bin/.clusteros-backup
for f in node-agent cluster; do
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

# Legacy shim scripts — thin wrappers that delegate to 'cluster <subcommand>'
# Written inline so they work even without the old script files in the bundle.
for _cmd_map in \
    "cluster-dashboard:dash" \
    "cluster-status:status" \
    "cluster-test:test" \
    "cluster-init:help" \
    "cluster-help:help"; do
    _old="${_cmd_map%%:*}"
    _new="${_cmd_map#*:}"
    printf '#!/bin/bash\n# Superseded by: cluster %s\nexec cluster %s "$@"\n' "$_new" "$_new" \
        > "/usr/local/bin/$_old"
    chmod 755 "/usr/local/bin/$_old"
done
# cluster-restart shim
printf '#!/bin/bash\nif [[ $(id -u) -ne 0 ]]; then exec sudo "$0" "$@"; fi\nsystemctl restart node-agent\n' \
    > /usr/local/bin/cluster-restart
chmod 755 /usr/local/bin/cluster-restart
# cluster-os-create-usb shim
printf '#!/bin/bash\n# Superseded by: cluster-make-usb\nexec cluster-make-usb "$@"\n' \
    > /usr/local/bin/cluster-os-create-usb
chmod 755 /usr/local/bin/cluster-os-create-usb
ok "Legacy shim scripts installed (cluster-dashboard, cluster-status, cluster-restart, etc.)"

# cluster-make-usb — re-install in case the early-install above was skipped
# (e.g. manual apply-patch.sh run from a partial bundle without cluster-make-usb.sh
# that was later fixed by re-running from a full bundle reaching step 8).
if [ -f "$SCRIPT_DIR/cluster-make-usb.sh" ]; then
    install -m 755 "$SCRIPT_DIR/cluster-make-usb.sh" /usr/local/bin/cluster-make-usb
    ok "cluster-make-usb confirmed at /usr/local/bin/cluster-make-usb"
fi

# Persist apply-patch.sh to /usr/local/lib/clusteros/ so cluster-make-usb can
# bundle it into USB installers it creates (without needing ~/patch/ to exist).
mkdir -p /usr/local/lib/clusteros
install -m 755 "$SCRIPT_DIR/apply-patch.sh" /usr/local/lib/clusteros/apply-patch.sh
ok "apply-patch.sh persisted → /usr/local/lib/clusteros/apply-patch.sh"
# Persist netplan so cluster-make-usb can bundle it into USB images
if [ -f "$SCRIPT_DIR/99-clusteros.yaml" ]; then
    install -m 600 "$SCRIPT_DIR/99-clusteros.yaml" /usr/local/lib/clusteros/99-clusteros.yaml
fi

# node-agent.service — always update to the version with apply-patch.sh in ExecStartPre.
if [ -f "$SCRIPT_DIR/systemd/node-agent.service" ]; then
    install -m 644 "$SCRIPT_DIR/systemd/node-agent.service" \
        /etc/systemd/system/node-agent.service
    mkdir -p /etc/systemd/system/multi-user.target.wants
    ln -sf /etc/systemd/system/node-agent.service \
        /etc/systemd/system/multi-user.target.wants/node-agent.service 2>/dev/null || true
    systemctl daemon-reload 2>/dev/null || true
    ok "node-agent.service updated (apply-patch.sh in ExecStartPre)"
fi

# clusteros-make-usb systemd service (oneshot, triggered manually).
if [ -f "$SCRIPT_DIR/systemd/clusteros-make-usb.service" ]; then
    install -m 644 "$SCRIPT_DIR/systemd/clusteros-make-usb.service" \
        /etc/systemd/system/clusteros-make-usb.service
    systemctl daemon-reload 2>/dev/null || true
    ok "clusteros-make-usb.service installed (trigger: sudo systemctl start clusteros-make-usb)"
fi

# ── 9. Start and verify ────────────────────────────────────────────────────────

step "9/10" "Starting node-agent and verifying"

# Temporarily disable node-agent firewall modifications while the patch
# completes. The service unit reads /etc/cluster-os/node-agent.env (EnvironmentFile)
# at start; creating this file with CLUSTEROS_SKIP_FIREWALL=1 prevents the daemon
# from re-adding redirect rules that could block apt/containerd during patch.
NODE_ENV_DIR="/etc/cluster-os"
NODE_ENV_FILE="$NODE_ENV_DIR/node-agent.env"
mkdir -p "$NODE_ENV_DIR"
echo 'CLUSTEROS_SKIP_FIREWALL=1' > "$NODE_ENV_FILE"
chmod 0644 "$NODE_ENV_FILE" || true
ok "Temporarily set CLUSTEROS_SKIP_FIREWALL=1 → $NODE_ENV_FILE"

# When apply-patch.sh runs as ExecStartPre inside node-agent.service, systemd
# sets $INVOCATION_ID. Calling 'systemctl start node-agent' from within its own
# ExecStartPre causes systemd to cancel the job — deadlock. Skip the explicit
# start; ExecStart will run automatically once ExecStartPre returns 0.
# Mark bootstrapped before starting node-agent so ExecStartPre does not
# re-invoke apply-patch.sh (which would cause a systemd job cancellation).
mkdir -p /var/lib/clusteros
touch /var/lib/clusteros/.bootstrapped

if [ -n "${INVOCATION_ID:-}" ]; then
    ok "Running as ExecStartPre — node-agent will start after this script"
else
    systemctl start node-agent
    sleep 4
    if ! systemctl is-active --quiet node-agent; then
        err "node-agent failed to start"
        echo ""
        journalctl -u node-agent -n 30 --no-pager
        exit 1
    fi
    ok "node-agent is running"
fi

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

# Remove the temporary env override so the daemon will run its normal
# firewall setup on the next reboot. Leaving this file removed now avoids
# permanently disabling the firewall behavior after the patch completes.
if [ -f "$NODE_ENV_FILE" ]; then
    rm -f "$NODE_ENV_FILE" || true
    ok "Removed temporary node-agent env file: $NODE_ENV_FILE (firewall setup will run on next reboot)"
fi

# Install one-time cleanup script + systemd unit from the patch bundle (if present)
if [ -f "$SCRIPT_DIR/scripts/clear-stale-redirects.sh" ]; then
    install -m 755 "$SCRIPT_DIR/scripts/clear-stale-redirects.sh" /usr/local/bin/clear-stale-redirects.sh
    ok "Installed clear-stale-redirects.sh → /usr/local/bin/clear-stale-redirects.sh"
    if [ -f "$SCRIPT_DIR/systemd/clear-stale-redirects.service" ]; then
        install -m 644 "$SCRIPT_DIR/systemd/clear-stale-redirects.service" /etc/systemd/system/clear-stale-redirects.service
        systemctl daemon-reload
        # Start now and enable for next boot; the script disables & removes the unit after running.
        systemctl enable --now clear-stale-redirects.service 2>/dev/null || systemctl start clear-stale-redirects.service 2>/dev/null || true
        ok "Installed + started clear-stale-redirects.service"
    else
        # If unit not bundled, run script immediately to attempt cleanup now.
        /usr/local/bin/clear-stale-redirects.sh || true
    fi
fi

# ── P2P Gateway Service (persistent across reboots) ──────────────────────────────
# Install a systemd service that re-runs the p2p gateway setup on every boot,
# AFTER network interfaces are up.  This ensures new nodes plugged in after reboot
# still get DHCP + internet via this node without manual intervention.
cat > /usr/local/sbin/clusteros-p2p-gateway.sh << 'P2P_SCRIPT_EOF'
#!/bin/bash
# Re-run at boot: assign per-node 10.200.X.1/24 to any unrouted physical ethernet
# interface and (re)start dnsmasq DHCP so directly-connected new nodes can reach the
# internet via this node's NAT.
set -euo pipefail

# Always ensure dnsmasq won't try to open port 53 (conflicts with systemd-resolved).
# This base conf is written unconditionally so the service never starts in DNS mode.
mkdir -p /etc/dnsmasq.d
cat > /etc/dnsmasq.d/clusteros-base.conf << 'BASE'
# ClusterOS: DHCP-only — do not act as a DNS server
no-resolv
port=0
except-interface=lo
BASE

# Stable per-node gateway octet: persisted so reboots work before Tailscale comes up.
# Using unique /24 subnets prevents ARP conflicts between multiple Tailscale nodes on
# the same L2 ethernet segment.
GW_OCTET_FILE=/var/lib/clusteros/p2p-gateway-octet
if [ -f "$GW_OCTET_FILE" ] && [ -s "$GW_OCTET_FILE" ]; then
    GW_OCTET=$(cat "$GW_OCTET_FILE")
else
    # || true: protect pipeline from set -euo pipefail when tailscale isn't up yet
    GW_OCTET=$(tailscale ip -4 2>/dev/null | awk -F. '{print $4}' | head -1 | tr -d ' \n' || true)
    [ -z "$GW_OCTET" ] && GW_OCTET=$(hostname | cksum | awk '{print ($1 % 253) + 1}')
    mkdir -p /var/lib/clusteros
    printf '%s' "$GW_OCTET" > "$GW_OCTET_FILE"
fi
GW_IP="10.200.${GW_OCTET}.1"
GW_SUBNET="10.200.${GW_OCTET}.0/24"
DHCP_START="10.200.${GW_OCTET}.100"
DHCP_END="10.200.${GW_OCTET}.200"

P2P_IFACES=""
for iface in $(ls /sys/class/net/ 2>/dev/null); do
    case "$iface" in lo|tailscale*|wg*|tun*|utun*|docker*|cni*|flannel*|veth*|br-*|dummy*) continue ;; esac
    [ -d "/sys/class/net/$iface/wireless" ] && continue
    [ "$(cat /sys/class/net/$iface/operstate 2>/dev/null)" = "up" ] || continue
    # Exclude 10.200.x.x from "routable" so second runs don't mistake our own gateway
    # IP for an existing routable address and skip reconfiguration.
    # || true: grep exits 1 when no lines match; protect from set -euo pipefail.
    ROUTABLE=$(ip addr show dev "$iface" 2>/dev/null | \
        grep -E 'inet ' | grep -v '169\.254\.\|10\.42\.\|10\.43\.\|10\.200\.\|127\.' | head -1 || true)
    if [ -z "$ROUTABLE" ]; then
        ip addr replace "${GW_IP}/24" dev "$iface" 2>/dev/null || true
        P2P_IFACES="$P2P_IFACES $iface"
    fi
done

if [ -z "$P2P_IFACES" ]; then
    rm -f /etc/dnsmasq.d/clusteros-p2p.conf
    # Restart with base-only config (port=0) so dnsmasq stays quiet
    systemctl restart dnsmasq 2>/dev/null || true
    exit 0
fi

cat > /etc/dnsmasq.d/clusteros-p2p.conf << DNSMASQ
# ClusterOS p2p gateway — auto-generated
$(for i in $P2P_IFACES; do echo "interface=$i"; done)
bind-interfaces
listen-address=${GW_IP}
dhcp-range=${DHCP_START},${DHCP_END},255.255.255.0,12h
dhcp-option=option:router,${GW_IP}
dhcp-option=option:dns-server,8.8.8.8,1.1.1.1
DNSMASQ

systemctl restart dnsmasq

iptables -t nat -C POSTROUTING -s "${GW_SUBNET}" -j MASQUERADE 2>/dev/null || \
    iptables -t nat -A POSTROUTING -s "${GW_SUBNET}" -j MASQUERADE
iptables -C FORWARD -s "${GW_SUBNET}" -j ACCEPT 2>/dev/null || \
    iptables -I FORWARD 1 -s "${GW_SUBNET}" -j ACCEPT
iptables -C FORWARD -d "${GW_SUBNET}" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || \
    iptables -I FORWARD 2 -d "${GW_SUBNET}" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT

logger "clusteros-p2p-gateway: NAT active on${P2P_IFACES} (${GW_SUBNET} via ${GW_IP})"
P2P_SCRIPT_EOF
chmod 755 /usr/local/sbin/clusteros-p2p-gateway.sh

cat > /etc/systemd/system/clusteros-p2p-gateway.service << 'SVC_EOF'
[Unit]
Description=ClusterOS P2P Ethernet Gateway (DHCP+NAT for new nodes)
After=network-online.target
Wants=network-online.target
# Re-run whenever a network interface state changes
BindsTo=network.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/clusteros-p2p-gateway.sh
# Also triggered by NetworkManager/networkd dispatcher when interfaces change
ExecReload=/usr/local/sbin/clusteros-p2p-gateway.sh

[Install]
WantedBy=multi-user.target
SVC_EOF

systemctl daemon-reload
systemctl enable clusteros-p2p-gateway.service 2>/dev/null || true
systemctl start  clusteros-p2p-gateway.service 2>/dev/null && ok "clusteros-p2p-gateway service started" || true

# Also trigger gateway setup when a new ethernet interface gets link-up via networkd-dispatcher
if [ -d /etc/networkd-dispatcher/routable.d ]; then
    cat > /etc/networkd-dispatcher/routable.d/clusteros-p2p-gateway << 'DISP_EOF'
#!/bin/bash
# Triggered by systemd-networkd when an interface becomes routable
/usr/local/sbin/clusteros-p2p-gateway.sh &
DISP_EOF
    chmod 755 /etc/networkd-dispatcher/routable.d/clusteros-p2p-gateway
    ok "networkd-dispatcher hook installed — gateway reconfigures on interface state changes"
fi
ok "P2P ethernet gateway service installed (DHCP 10.200.0.100-200 + NAT on unrouted eth interfaces)"
# ─────────────────────────────────────────────────────────────────────────────────

# ── Kubeconfig cleanup ─────────────────────────────────────────────────────────
# k3s writes /etc/rancher/k3s/k3s.yaml as mode 0600 (root-only) by default.
# If the node previously ran as a leader, this file may also contain an invalid
# 0.0.0.0:6443 server address (written when Tailscale wasn't up yet at k3s start).
# Fix both so that non-root users (e.g. the 'clusteros' account running
# 'cluster test') can use kubectl immediately without sudo.
KUBECONFIG_FILE="/etc/rancher/k3s/k3s.yaml"
if [ -f "$KUBECONFIG_FILE" ]; then
    # Fix permissions
    chmod 0644 "$KUBECONFIG_FILE" 2>/dev/null && ok "kubeconfig: set mode 0644" || true
    # Fix 0.0.0.0 → 127.0.0.1 (stale address from a previous leader role)
    if grep -q 'server: https://0\.0\.0\.0' "$KUBECONFIG_FILE" 2>/dev/null; then
        sed -i 's|server: https://0\.0\.0\.0:|server: https://127.0.0.1:|g' "$KUBECONFIG_FILE"
        ok "kubeconfig: fixed 0.0.0.0 → 127.0.0.1"
    fi
    # Symlink into standard kubectl location for all users
    mkdir -p /etc/skel/.kube /root/.kube
    ln -sf "$KUBECONFIG_FILE" /root/.kube/config 2>/dev/null || true
    if id clusteros &>/dev/null; then
        CLUSTEROS_HOME=$(getent passwd clusteros | cut -d: -f6)
        mkdir -p "${CLUSTEROS_HOME}/.kube"
        ln -sf "$KUBECONFIG_FILE" "${CLUSTEROS_HOME}/.kube/config" 2>/dev/null || true
    fi
    ok "kubeconfig: symlinked to ~/.kube/config for root and clusteros"
else
    ok "kubeconfig: not present yet (will be written when k3s server starts on leader)"
fi

# Also fix agent kubelet kubeconfig (used by kubelet) if present and points to 0.0.0.0
AGENT_KUBECONFIG="/var/lib/rancher/k3s/agent/kubelet.kubeconfig"
if [ -f "$AGENT_KUBECONFIG" ]; then
    if grep -q 'server: https://0\.0\.0\.0' "$AGENT_KUBECONFIG" 2>/dev/null; then
        sed -i 's|server: https://0\.0\.0\.0:|server: https://127.0.0.1:|g' "$AGENT_KUBECONFIG" || true
        ok "agent kubeconfig: fixed 0.0.0.0 → 127.0.0.1 in $AGENT_KUBECONFIG"
    fi
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

if [[ "$NO_REBOOT" -eq 1 ]]; then
    ok "Skipping reboot (USB live boot or --no-reboot flag)"
    ok "Node is running — use 'cluster status' to verify"
    ok "To install permanently to disk: sudo cluster-os-install"
else
    # A reboot ensures:
    #   • iscsid / multipathd kernel modules are fully loaded (required by Longhorn)
    #   • machine-id change is picked up by DHCP / Tailscale
    #   • any stale K3s / Serf file locks are cleared
    #   • node-agent starts fresh with the new binary under systemd supervision
    echo ""
    echo -e "${CYAN}${BOLD}This node will reboot in 10 seconds.${NC}"
    echo "  Interrupt with Ctrl-C if you need to stay online, then reboot manually."
    echo ""

    REBOOT_DELAY=5
    for i in $(seq "$REBOOT_DELAY" -1 1); do
        printf "\r  Rebooting in %2d s ...  (Ctrl-C to cancel)" "$i"
        sleep 1
    done
    echo ""
    ok "Rebooting now"
    # Background the reboot with a 1s delay so this script (and the SSH session
    # that invoked it) can exit cleanly before the node goes down.  Without this
    # the SSH process in 'make deploy' hangs until the TCP connection drops.
    (sleep 1 && systemctl reboot) &>/dev/null &
    exit 0
fi
