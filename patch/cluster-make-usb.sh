#!/bin/bash
# cluster-make-usb — Build a USB installer with current live cluster certs baked in.
#
# Runs on any cluster node (leader preferred — it has the CA private key).
# Collects live assets from the running node, assembles a self-contained patch
# bundle, then either:
#
#   a) Writes a bootable USB image (if base installer is available) with the
#      patch bundle pre-staged so new nodes auto-join on first boot.
#
#   b) Writes a portable patch bundle tarball you can apply manually on any
#      new node by running:  sudo tar -xz -C ~/ -f clusteros-patch-DATE.tar.gz
#                            sudo bash ~/patch/apply-patch.sh
#
# Usage:
#   sudo cluster-make-usb [--device /dev/sdX] [--output FILE.tar.gz] [--bundle-dir DIR]
#
#   --device /dev/sdX   Write bootable image to this USB device.
#                       WARNING: all data on the device will be erased.
#   --output FILE       Write patch bundle tarball to FILE.
#                       Default: /tmp/clusteros-patch-YYYY-MM-DD.tar.gz
#   --bundle-dir DIR    Leave assembled bundle dir at DIR (skip tarball/USB write).
#   --help              Show this help.
#
# Prerequisites (auto-installed if missing):
#   pv, tar, gzip — for progress + compression
#   losetup, mount, umount, mkfs.fat, sgdisk — for USB write mode
#   qemu-utils (qemu-img) — for converting Ubuntu cloud image from qcow2 to raw

set -euo pipefail

# ── Colours ──────────────────────────────────────────────────────────────────
GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
ok()   { echo -e "  ${GREEN}✓${NC} $*"; }
warn() { echo -e "  ${YELLOW}!${NC} $*"; }
err()  { echo -e "  ${RED}✗${NC} $*"; }
step() { echo -e "\n${CYAN}${BOLD}[$1]${NC} $2"; }

# ── Arg parse ─────────────────────────────────────────────────────────────────
DEVICE=""
OUTPUT=""
BUNDLE_DIR=""
BUNDLE_DIR_ARG=""  # set if --bundle-dir was passed explicitly

usage() {
    sed -n '3,28p' "$0" | sed 's/^# \?//'
    exit 0
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --device)    DEVICE="$2";     shift 2 ;;
        --output)    OUTPUT="$2";     shift 2 ;;
        --bundle-dir) BUNDLE_DIR="$2"; BUNDLE_DIR_ARG="$2"; shift 2 ;;
        --help|-h)   usage ;;
        *) err "Unknown option: $1"; usage ;;
    esac
done

if [[ $(id -u) -ne 0 ]]; then
    err "Must run as root: sudo cluster-make-usb $*"
    exit 1
fi

DATE=$(date +%Y-%m-%d)
# Do NOT default OUTPUT here — leave it empty so the USB device scanner
# can detect "no explicit output mode" and auto-scan for drives.
if [[ -z "$BUNDLE_DIR" ]]; then
    BUNDLE_DIR="$(mktemp -d /tmp/clusteros-bundle-XXXXXX)"
    CLEANUP_BUNDLE=true   # we created it, we clean it up
else
    CLEANUP_BUNDLE=false  # caller owns the dir, leave it in place
    mkdir -p "$BUNDLE_DIR"
fi

echo -e "${CYAN}${BOLD}"
echo "  ╔══════════════════════════════════════════════════╗"
echo "  ║  ClusterOS — USB Installer Builder              ║"
echo "  ╚══════════════════════════════════════════════════╝"
echo -e "${NC}"

# ── Base paths ────────────────────────────────────────────────────────────────
CLUSTEROS_LIB="/usr/local/lib/clusteros"
BASE_IMAGE="/usr/share/clusteros/installer.img.gz"

# Certs and keys
K3S_SERVER_TLS="/var/lib/rancher/k3s/server/tls"
K3S_AGENT_IMAGES="/var/lib/rancher/k3s/agent/images"
MUNGE_KEY="/etc/munge/munge.key"
NODE_AGENT_BIN="/usr/local/bin/node-agent"
APPLY_PATCH_SH="$CLUSTEROS_LIB/apply-patch.sh"
CLUSTER_CLI="/usr/local/bin/cluster"

# ── Step 1: Collect live assets ───────────────────────────────────────────────
step "1/4" "Collecting live cluster assets"

mkdir -p "$BUNDLE_DIR"

# node-agent binary
if [[ -f "$NODE_AGENT_BIN" ]]; then
    cp "$NODE_AGENT_BIN" "$BUNDLE_DIR/node-agent"
    chmod 755 "$BUNDLE_DIR/node-agent"
    ok "node-agent binary: $(file "$BUNDLE_DIR/node-agent" | grep -oP 'ELF \S+ \S+')"
else
    err "node-agent not found at $NODE_AGENT_BIN — is this a cluster node?"
    exit 1
fi

# apply-patch.sh — prefer the installed persistent copy; fall back to ~/patch/
if [[ -f "$APPLY_PATCH_SH" ]]; then
    cp "$APPLY_PATCH_SH" "$BUNDLE_DIR/apply-patch.sh"
    ok "apply-patch.sh from $APPLY_PATCH_SH"
elif [[ -f ~/patch/apply-patch.sh ]]; then
    cp ~/patch/apply-patch.sh "$BUNDLE_DIR/apply-patch.sh"
    ok "apply-patch.sh from ~/patch/ (fallback)"
else
    err "apply-patch.sh not found — run 'make deploy' from dev machine first"
    exit 1
fi
chmod +x "$BUNDLE_DIR/apply-patch.sh"

# cluster CLI
if [[ -f "$CLUSTER_CLI" ]]; then
    cp "$CLUSTER_CLI" "$BUNDLE_DIR/cluster"
    chmod +x "$BUNDLE_DIR/cluster"
    ok "cluster CLI"
fi

# tailscale-auth (Tailscale OAuth enroller — apply-patch.sh installs it as clusteros-tailscale-init)
if [[ -f /usr/local/bin/clusteros-tailscale-init ]]; then
    cp /usr/local/bin/clusteros-tailscale-init "$BUNDLE_DIR/tailscale-auth"
    chmod 755 "$BUNDLE_DIR/tailscale-auth"
    ok "tailscale-auth bundled (from /usr/local/bin/clusteros-tailscale-init)"
elif [[ -f ~/patch/tailscale-auth ]]; then
    cp ~/patch/tailscale-auth "$BUNDLE_DIR/tailscale-auth"
    chmod 755 "$BUNDLE_DIR/tailscale-auth"
    ok "tailscale-auth bundled (from ~/patch/)"
else
    warn "tailscale-auth not found — new nodes will need manual 'tailscale up' after install"
fi

# k3s CA cert (every node)
if [[ -f "$K3S_SERVER_TLS/server-ca.crt" ]]; then
    cp "$K3S_SERVER_TLS/server-ca.crt" "$BUNDLE_DIR/k3s-ca.crt"
    ok "k3s CA cert (from running cluster)"
elif [[ -f /var/lib/rancher/k3s/agent/server-ca.crt ]]; then
    cp /var/lib/rancher/k3s/agent/server-ca.crt "$BUNDLE_DIR/k3s-ca.crt"
    ok "k3s CA cert (from agent cache)"
else
    warn "k3s CA cert not found — new nodes will need internet to fetch it"
fi

# k3s CA key (leader only)
if [[ -f "$K3S_SERVER_TLS/server-ca.key" ]]; then
    cp "$K3S_SERVER_TLS/server-ca.key" "$BUNDLE_DIR/k3s-ca.key"
    chmod 600 "$BUNDLE_DIR/k3s-ca.key"
    ok "k3s CA key (this node is leader — all nodes will share the same CA)"
else
    warn "k3s CA key not found (this is a worker node)"
    warn "For a fully offline USB installer, run cluster-make-usb on the leader node"
    warn "New nodes will still join correctly — they'll receive the CA via k3s bootstrap"
fi

# Munge key
if [[ -f "$MUNGE_KEY" ]]; then
    cp "$MUNGE_KEY" "$BUNDLE_DIR/munge.key"
    chmod 600 "$BUNDLE_DIR/munge.key"
    ok "munge key"
else
    warn "munge.key not found — generating a fresh one (SLURM jobs won't share auth with existing nodes)"
    head -c 32 /dev/urandom > "$BUNDLE_DIR/munge.key"
    chmod 600 "$BUNDLE_DIR/munge.key"
fi

# pause image airgap tarball
if [[ -f "$K3S_AGENT_IMAGES/pause-3.6.tar" ]]; then
    cp "$K3S_AGENT_IMAGES/pause-3.6.tar" "$BUNDLE_DIR/pause-3.6.tar"
    ok "pause image airgap tarball ($(du -h "$BUNDLE_DIR/pause-3.6.tar" | cut -f1))"
else
    warn "pause-3.6.tar not found — new nodes will pull it from the internet"
fi

# cluster-make-usb script itself — so nodes built from this USB can make their own
cp "$0" "$BUNDLE_DIR/cluster-make-usb.sh"
chmod +x "$BUNDLE_DIR/cluster-make-usb.sh"
ok "cluster-make-usb bundled (nodes can create further USB installers)"

# Netplan — WiFi + wired DHCP config for physical hardware
if [[ -f /etc/netplan/99-clusteros.yaml ]]; then
    cp /etc/netplan/99-clusteros.yaml "$BUNDLE_DIR/99-clusteros.yaml"
    chmod 600 "$BUNDLE_DIR/99-clusteros.yaml"
    ok "Netplan bundled (WiFi + wired DHCP)"
elif [[ -f /usr/local/lib/clusteros/99-clusteros.yaml ]]; then
    cp /usr/local/lib/clusteros/99-clusteros.yaml "$BUNDLE_DIR/99-clusteros.yaml"
    chmod 600 "$BUNDLE_DIR/99-clusteros.yaml"
    ok "Netplan bundled (from lib)"
else
    warn "No 99-clusteros.yaml found — new nodes may not get WiFi automatically"
fi

# Tailscale credentials — new nodes need these to auto-join the overlay network.
# Without a valid tailscale.env, a new node boots isolated: it has no Tailscale IP
# so 'make deploy' can never find it and it can never receive further updates.
if [[ -f /etc/clusteros/tailscale.env ]]; then
    cp /etc/clusteros/tailscale.env "$BUNDLE_DIR/tailscale.env"
    chmod 600 "$BUNDLE_DIR/tailscale.env"
    ok "Tailscale credentials bundled (/etc/clusteros/tailscale.env)"
else
    warn "No /etc/clusteros/tailscale.env found on this node"
    warn "New nodes built from this USB will need manual 'tailscale up --authkey=...' to join"
fi

# VERSION stamp
NODE_VER=$("$NODE_AGENT_BIN" --version 2>/dev/null | head -1 || echo "unknown")
printf 'version=%s\ncommit=live\nbuilt=%s\nsource-node=%s\n' \
    "$NODE_VER" "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$(hostname)" \
    > "$BUNDLE_DIR/VERSION"
ok "VERSION stamp: $NODE_VER (source: $(hostname))"

# ── Step 2: Validate bundle ───────────────────────────────────────────────────
step "2/4" "Validating bundle"

REQUIRED=("node-agent" "apply-patch.sh")
MISSING=false
for f in "${REQUIRED[@]}"; do
    if [[ ! -f "$BUNDLE_DIR/$f" ]]; then
        err "Required file missing from bundle: $f"
        MISSING=true
    fi
done
$MISSING && exit 1

echo "  Bundle contents:"
ls -lh "$BUNDLE_DIR/" | tail -n +2 | sed 's/^/    /'

# ── Step 3: Write output ──────────────────────────────────────────────────────
step "3/4" "Writing output"

# If no device specified, scan for USB drives and let user pick interactively
if [[ -z "$DEVICE" && -z "$OUTPUT" && -z "$BUNDLE_DIR_ARG" ]]; then
    echo ""
    echo -e "  ${YELLOW}No --device specified. Scanning for USB drives...${NC}"
    echo ""

    # Detect boot device to exclude.
    # lsblk PKNAME fails for LVM/dm-crypt/overlay roots, so use two methods:
    # 1. PKNAME lookup (works for simple partitions)
    # 2. Check if any partitions/children of the device are currently mounted
    BOOT_DEV=""
    ROOT_MOUNT=$(findmnt -n -o SOURCE / 2>/dev/null || true)
    if [[ -n "$ROOT_MOUNT" ]]; then
        _pkname=$(lsblk -n -o PKNAME "$ROOT_MOUNT" 2>/dev/null | head -1 || true)
        [[ -n "$_pkname" ]] && BOOT_DEV="/dev/$_pkname"
    fi

    # Helper: returns 0 if the given disk has any mounted partitions/children
    _disk_is_mounted() {
        local d="$1"
        # Direct partition match (e.g. /dev/sda1, /dev/nvme0n1p1)
        if grep -qE "^${d}p?[0-9]" /proc/mounts 2>/dev/null; then return 0; fi
        # lsblk children (catches LVM PVs, dm-crypt, etc.)
        while IFS= read -r child; do
            grep -q "^/dev/$child " /proc/mounts 2>/dev/null && return 0
        done < <(lsblk -ln -o NAME "$d" 2>/dev/null | tail -n +2)
        return 1
    }

    declare -a _DEVS _DEVINFO
    _IDX=0
    for _dev in /dev/sd? /dev/nvme?n?; do
        [[ -b "$_dev" ]] || continue
        [[ "$_dev" = "$BOOT_DEV" ]] && continue
        # Also skip any disk that has mounted partitions (catches LVM/dm roots
        # where PKNAME lookup above may have returned empty)
        if _disk_is_mounted "$_dev"; then
            [[ -z "$BOOT_DEV" ]] && BOOT_DEV="$_dev"
            continue
        fi
        _removable=$(cat "/sys/block/$(basename "$_dev")/removable" 2>/dev/null || echo 0)
        _tran=$(lsblk -d -n -o TRAN "$_dev" 2>/dev/null || echo "")
        _size_b=$(lsblk -b -d -n -o SIZE "$_dev" 2>/dev/null || echo 0)
        # Include removable, USB transport, or <256 GB
        if [[ "$_removable" = "1" || "$_tran" = "usb" || "$_size_b" -lt 274877906944 ]]; then
            _size=$(lsblk -d -n -o SIZE "$_dev" 2>/dev/null || echo "??")
            _model=$(lsblk -d -n -o MODEL "$_dev" 2>/dev/null | xargs || echo "Unknown")
            _DEVS[$_IDX]="$_dev"
            _DEVINFO[$_IDX]="$_size  $_model  [$_tran]"
            _IDX=$((_IDX + 1))
        fi
    done 2>/dev/null || true

    if [[ ${#_DEVS[@]} -eq 0 ]]; then
        err "No USB drives found. Insert a USB drive and retry, or use --device /dev/sdX"
        echo ""
        echo "  Current block devices:"
        lsblk -d -o NAME,SIZE,MODEL,TRAN,RM 2>/dev/null | grep -v "loop\|sr\|rom" | sed 's/^/    /'
        exit 1
    elif [[ ${#_DEVS[@]} -eq 1 ]]; then
        DEVICE="${_DEVS[0]}"
        if [[ -n "$BOOT_DEV" ]]; then
            echo -e "  ${CYAN}System disk (excluded):${NC} $BOOT_DEV"
        fi
        ok "Auto-selected only USB drive: $DEVICE  —  ${_DEVINFO[0]}"
    else
        if [[ -n "$BOOT_DEV" ]]; then
            _boot_size=$(lsblk -d -n -o SIZE "$BOOT_DEV" 2>/dev/null || echo "??")
            _boot_model=$(lsblk -d -n -o MODEL "$BOOT_DEV" 2>/dev/null | xargs || echo "")
            echo -e "  ${CYAN}System disk (excluded):${NC} $BOOT_DEV  $_boot_size  $_boot_model"
            echo ""
        fi
        echo -e "  ${YELLOW}Available USB drives:${NC}"
        echo ""
        for _i in "${!_DEVS[@]}"; do
            _num=$((_i + 1))
            echo -e "    ${GREEN}[$_num]${NC} ${_DEVS[$_i]}  —  ${_DEVINFO[$_i]}"
        done
        echo ""
        echo -e "    ${RED}[0]${NC} Cancel"
        echo ""
        while true; do
            read -rp "  Select USB drive [0-${#_DEVS[@]}]: " _sel
            if [[ "$_sel" =~ ^[0-9]+$ ]]; then
                if [[ "$_sel" -eq 0 ]]; then
                    echo "Cancelled."; exit 0
                elif [[ "$_sel" -ge 1 && "$_sel" -le ${#_DEVS[@]} ]]; then
                    DEVICE="${_DEVS[$((_sel - 1))]}"
                    break
                fi
            fi
            err "Invalid selection — enter a number between 0 and ${#_DEVS[@]}"
        done
    fi
fi

if [[ -n "$DEVICE" ]]; then
    # ── USB device mode ───────────────────────────────────────────────────────
    if [[ ! -b "$DEVICE" ]]; then
        err "Not a block device: $DEVICE"
        exit 1
    fi

    # Unmount any mounted partitions of the target device before writing.
    # Partitions can be auto-mounted by udisks2 or left from a previous partial run.
    _mounted=$(grep -E "^${DEVICE}p?[0-9]" /proc/mounts 2>/dev/null | awk '{print $2}' | sort -r || true)
    if [[ -n "$_mounted" ]]; then
        warn "$DEVICE has mounted partitions — unmounting automatically..."
        while IFS= read -r _mnt; do
            umount "$_mnt" 2>/dev/null && ok "Unmounted $_mnt" \
                || { err "Cannot unmount $_mnt — please run: sudo umount $_mnt"; exit 1; }
        done <<< "$_mounted"
    fi
    # Sanity check: if the device itself appears as a root/boot mount, refuse
    if grep -qE "^${DEVICE} " /proc/mounts 2>/dev/null; then
        err "$DEVICE is directly mounted — this looks like a system disk, not a USB target"
        exit 1
    fi

    # If no pre-built base image, download Ubuntu 24.04 cloud image, provision it
    # (chroot: install all deps, Tailscale, k3s, SLURM, node-agent, cluster-os-install),
    # then dd the provisioned image to the USB.  Same approach as the Packer build —
    # the USB boots live as a fully functional cluster node, no internet needed at runtime.
    if [[ ! -f "$BASE_IMAGE" ]]; then
        _IMG_URL="https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img"
        _IMG_TMP="/tmp/clusteros-ubuntu-base.img"
        _IMG_WORK="/tmp/clusteros-work.img"

        step "3-dl/4" "Downloading Ubuntu 24.04 cloud image (~600 MB)"
        if [[ ! -f "$_IMG_TMP" ]]; then
            if command -v wget &>/dev/null; then
                wget -O "$_IMG_TMP" --progress=bar:force "$_IMG_URL" 2>&1 || { err "Download failed"; exit 1; }
            else
                curl -L --progress-bar -o "$_IMG_TMP" "$_IMG_URL" || { err "Download failed"; exit 1; }
            fi
        else
            ok "Reusing cached download: $_IMG_TMP ($(du -h "$_IMG_TMP" | cut -f1))"
        fi
        ok "Base image ready: $(du -h "$_IMG_TMP" | cut -f1)"

        step "3-prep/4" "Preparing working image (expand to 8 GB for packages)"
        # Ubuntu cloud images use qcow2 format despite the .img extension.
        # losetup treats the file as raw, so sfdisk finds no partition table →
        # "failed to dump sfdisk info for /dev/loopN" → no /dev/loopNpX devices.
        # Fix: convert to raw format first.
        if ! command -v qemu-img &>/dev/null; then
            apt-get -o Acquire::ForceIPv4=true install -y -qq qemu-utils || { err "qemu-utils install failed — cannot convert cloud image"; exit 1; }
        fi
        ok "Converting qcow2 → raw (this takes ~30s, image is ~2 GB expanded)"
        qemu-img convert -f qcow2 -O raw "$_IMG_TMP" "$_IMG_WORK"
        # Expand to 8 GB so there's room for all packages
        truncate -s 8G "$_IMG_WORK"
        # Set up loop device for the working image
        _LOOP=$(losetup --find --show --partscan "$_IMG_WORK")
        ok "Loop device: $_LOOP"
        # Wait for udev to create partition block devices (more reliable than sleep)
        udevadm settle --timeout=10 2>/dev/null || sleep 3
        # Grow the root partition to fill the image (cloud images use partition 1)
        _ROOT_PART="${_LOOP}p1"
        if [[ ! -b "$_ROOT_PART" ]]; then _ROOT_PART="${_LOOP}p2"; fi
        _PART_NUM=$(echo "$_ROOT_PART" | grep -oE '[0-9]+$')
        growpart "$_LOOP" "$_PART_NUM" 2>/dev/null || true
        e2fsck -f -y "$_ROOT_PART" 2>/dev/null || true
        resize2fs "$_ROOT_PART" 2>/dev/null || true
        ok "Root partition expanded"

        step "3-prov/4" "Provisioning image (chroot — installs packages, Tailscale, k3s, node-agent)"
        _MNT="$(mktemp -d /tmp/clusteros-chroot-XXXXXX)"
        mount "$_ROOT_PART" "$_MNT"
        # Bind-mount kernel interfaces needed for apt and systemctl
        mount --bind /proc "$_MNT/proc"
        mount --bind /sys  "$_MNT/sys"
        mount --bind /dev  "$_MNT/dev"
        mount --bind /dev/pts "$_MNT/dev/pts"

        # Copy bundle into chroot
        mkdir -p "$_MNT/root/patch"
        cp -a "$BUNDLE_DIR/." "$_MNT/root/patch/"
        # Copy node-agent binary to final location inside chroot
        cp "$BUNDLE_DIR/node-agent" "$_MNT/usr/local/bin/node-agent"
        chmod 755 "$_MNT/usr/local/bin/node-agent"
        [[ -f "$BUNDLE_DIR/cluster" ]] && { cp "$BUNDLE_DIR/cluster" "$_MNT/usr/local/bin/cluster"; chmod 755 "$_MNT/usr/local/bin/cluster"; }
        cp "$0" "$_MNT/usr/local/bin/cluster-make-usb"; chmod 755 "$_MNT/usr/local/bin/cluster-make-usb"

        # Disable cloud-init from running (we manage first boot ourselves)
        touch "$_MNT/etc/cloud/cloud-init.disabled"

        # ── Network config for physical hardware ──────────────────────────────
        # Ubuntu cloud images ship with a netplan config for virtual interfaces
        # (ens3, ens4). Physical machines use enp*/eth*/wl* names — without this
        # file DHCP never runs on first boot so Tailscale can't auth and apt fails.
        mkdir -p "$_MNT/etc/netplan"
        if [[ -f "$BUNDLE_DIR/99-clusteros.yaml" ]]; then
            install -m 600 "$BUNDLE_DIR/99-clusteros.yaml" "$_MNT/etc/netplan/99-clusteros.yaml"
            ok "Netplan installed from bundle (wired DHCP + WiFi)"
        else
            warn "99-clusteros.yaml not in bundle — WiFi may not work on new nodes"
        fi

        # ── Verify host internet connectivity before entering chroot ─────────────
        # The chroot uses the HOST's network stack. If the host can't reach the
        # internet (e.g. k3s overwrote /etc/resolv.conf with 10.43.0.10 and
        # CoreDNS is down), apt-get update will silently fail and then package
        # installs fail with "Unable to locate package".
        _HOST_DNS_FIXED=false
        if ! getent hosts archive.ubuntu.com &>/dev/null 2>&1; then
            warn "Host DNS broken (likely k3s CoreDNS at $(grep nameserver /etc/resolv.conf 2>/dev/null | head -1 | awk '{print $2}') is unreachable)"
            warn "Temporarily injecting 8.8.8.8 into host /etc/resolv.conf for provisioning..."
            _orig_resolv=$(cat /etc/resolv.conf 2>/dev/null || true)
            printf 'nameserver 8.8.8.8\nnameserver 1.1.1.1\n' > /etc/resolv.conf
            _HOST_DNS_FIXED=true
            # Verify it worked
            if ! getent hosts archive.ubuntu.com &>/dev/null 2>&1; then
                # Restore and fail — no point entering chroot without internet
                echo "$_orig_resolv" > /etc/resolv.conf 2>/dev/null || true
                err "Cannot reach archive.ubuntu.com even with 8.8.8.8 — check this node's internet connection"
                err "Try: ping 8.8.8.8    (basic connectivity)"
                err "Try: curl -v http://archive.ubuntu.com/  (HTTP reachability)"
                exit 1
            fi
            ok "Host DNS restored via 8.8.8.8 — internet reachable"
        fi

        # ── Fix DNS in chroot for apt/curl ────────────────────────────────────
        # Ubuntu 24.04 /etc/resolv.conf is a symlink to an absolute path
        # (/run/systemd/resolve/stub-resolv.conf). Writing through the symlink
        # from outside the chroot follows the absolute path on the HOST, not the
        # guest. Remove the symlink first so we create a real file in the image.
        rm -f "$_MNT/etc/resolv.conf"
        printf 'nameserver 8.8.8.8\nnameserver 1.1.1.1\n' > "$_MNT/etc/resolv.conf"
        ok "Chroot DNS set to 8.8.8.8 (symlink replaced with real file)"

        # ── apt-get update with retry ─────────────────────────────────────────
        # Run update separately so a transient DNS failure doesn't leave us
        # trying to install from a broken/empty package index.
        _APT_UPDATED=false
        for _attempt in 1 2 3; do
            if chroot "$_MNT" apt-get -o Acquire::ForceIPv4=true update -qq 2>&1; then
                _APT_UPDATED=true
                break
            fi
            warn "apt-get update attempt $_attempt/3 failed — retrying in 5s..."
            sleep 5
        done
        if [[ "$_APT_UPDATED" != "true" ]]; then
            # Restore host DNS if we changed it, then bail
            if [[ "$_HOST_DNS_FIXED" == "true" ]]; then
                echo "$_orig_resolv" > /etc/resolv.conf 2>/dev/null || true
            fi
            err "apt-get update failed after 3 attempts — cannot install packages"
            err "The USB image will be incomplete. Fix internet access on this node and retry."
            exit 1
        fi
        ok "apt-get update succeeded"

        # Install all dependencies via chroot apt
        chroot "$_MNT" /bin/bash -c "
            export DEBIAN_FRONTEND=noninteractive
            apt-get -o Acquire::ForceIPv4=true install -y -qq \
                openssh-server jq curl wget munge slurm-wlm slurm-client \
                libpmix-dev openmpi-bin libopenmpi-dev python3-mpi4py \
                build-essential open-iscsi nfs-common multipath-tools \
                gdisk parted dosfstools pv wpasupplicant
            systemctl enable ssh
        " && ok "Packages installed" || warn "Some packages failed — node will retry on first boot"

        # ── Create clusteros user ─────────────────────────────────────────────
        # cloud-init would normally do this (see images/ubuntu/cloud-init/user-data).
        # Since cloud-init is disabled, create the user manually so SSH + make deploy work.
        chroot "$_MNT" /bin/bash -c "
            id clusteros &>/dev/null || useradd -m -s /bin/bash -G sudo,adm clusteros
            echo 'clusteros:clusteros' | chpasswd
            echo 'clusteros ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/clusteros
            chmod 440 /etc/sudoers.d/clusteros
            sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config
            sed -i 's/^#\?KbdInteractiveAuthentication.*/KbdInteractiveAuthentication yes/' /etc/ssh/sshd_config
        " && ok "clusteros user created (password: clusteros, sudo NOPASSWD)" || warn "User setup failed"

        # Install Tailscale inside chroot
        # NOTE: keep host DNS fix in place until after ALL curl operations below
        chroot "$_MNT" /bin/bash -c "
            curl -fsSL https://tailscale.com/install.sh | sh
            systemctl enable tailscaled
        " && ok "Tailscale installed" || warn "Tailscale install failed — node will retry on first boot"

        # Install k3s binary inside chroot (no systemd service)
        chroot "$_MNT" /bin/bash -c "
            curl -sfL https://get.k3s.io | INSTALL_K3S_SKIP_ENABLE=true INSTALL_K3S_SKIP_START=true sh -
        " && ok "k3s installed" || warn "k3s install failed — node will retry on first boot"

        # Restore host /etc/resolv.conf now that all network operations are done
        if [[ "$_HOST_DNS_FIXED" == "true" ]]; then
            echo "$_orig_resolv" > /etc/resolv.conf 2>/dev/null || true
            ok "Host /etc/resolv.conf restored"
        fi

        # ── Fix DNS on the booted image ───────────────────────────────────────
        # On first boot, systemd-resolved starts before DHCP completes and may
        # use a stub resolver that doesn't work yet. Set FallbackDNS so the
        # booted node can always resolve names even before DHCP DNS arrives.
        mkdir -p "$_MNT/etc/systemd"
        cat >> "$_MNT/etc/systemd/resolved.conf" <<'RESOLVEDCONF'

# ClusterOS: ensure public DNS fallback so apt/curl work on first boot
# even before DHCP-provided DNS is available (or if k3s CoreDNS is down).
[Resolve]
FallbackDNS=8.8.8.8 1.1.1.1
DNSStubListener=yes
RESOLVEDCONF
        ok "systemd-resolved FallbackDNS=8.8.8.8 configured in image"

        # Leave 8.8.8.8 as the resolv.conf for runtime too — systemd-resolved
        # will replace the nameserver entry once it starts, but this ensures
        # DNS works in early boot before systemd-resolved is up.
        ok "resolv.conf left as 8.8.8.8 for first-boot network operations"

        # Install cluster-os-install: dd USB→internal disk, resize to fill, fix UEFI boot
        cat > "$_MNT/usr/local/bin/cluster-os-install" <<'INSTALL_SCRIPT'
#!/bin/bash
set -e
GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
ok()   { echo -e "  ${GREEN}✓${NC} $*"; }
warn() { echo -e "  ${YELLOW}!${NC} $*"; }
err()  { echo -e "  ${RED}✗${NC} $*"; }
step() { echo -e "\n${CYAN}${BOLD}[$1]${NC} $2"; }

if [[ $(id -u) -ne 0 ]]; then echo -e "${RED}Run as root: sudo cluster-os-install${NC}"; exit 1; fi

# Find the device we're currently running from
USB_DEV=$(findmnt -n -o SOURCE / | sed 's/p\?[0-9]*$//' | head -1)

echo -e "${CYAN}${BOLD}"
echo "  ╔══════════════════════════════════════════╗"
echo "  ║  ClusterOS — Install to Internal Disk   ║"
echo "  ╚══════════════════════════════════════════╝"
echo -e "${NC}"
echo -e "  Running from: ${CYAN}$USB_DEV${NC}  ($(lsblk -d -n -o SIZE "$USB_DEV" 2>/dev/null))"
echo ""

# Build disk list excluding the USB we're running from
declare -a _DISKS _DINFO
_I=0
while IFS= read -r _line; do
    _dev="/dev/$(echo "$_line" | awk '{print $1}')"
    [[ "$_dev" = "$USB_DEV" ]] && continue
    _size=$(echo "$_line" | awk '{print $2}')
    _model=$(echo "$_line" | awk '{$1=$2=""; print $0}' | xargs)
    _tran=$(lsblk -d -n -o TRAN "$_dev" 2>/dev/null | xargs)
    _DISKS[$_I]="$_dev"; _DINFO[$_I]="$_size  $_model  [$_tran]"; _I=$((_I+1))
done < <(lsblk -d -n -o NAME,SIZE,MODEL 2>/dev/null | grep -v loop)

if [[ ${#_DISKS[@]} -eq 0 ]]; then
    err "No internal disks found."; exit 1
fi

echo -e "  ${YELLOW}Available disks:${NC}"; echo ""
for _i in "${!_DISKS[@]}"; do
    echo -e "    ${GREEN}[$((_i+1))]${NC} ${_DISKS[$_i]}  —  ${_DINFO[$_i]}"
done
echo ""; echo -e "    ${RED}[0]${NC} Cancel"; echo ""
while true; do
    read -rp "  Select disk to install to [0-${#_DISKS[@]}]: " _sel
    [[ "$_sel" =~ ^[0-9]+$ ]] || continue
    [[ "$_sel" -eq 0 ]] && { echo "Cancelled."; exit 0; }
    [[ "$_sel" -ge 1 && "$_sel" -le ${#_DISKS[@]} ]] && { TARGET="${_DISKS[$((_sel-1))]}"; break; }
done

TARGET_SIZE=$(lsblk -d -n -o SIZE "$TARGET" 2>/dev/null)
echo ""
echo -e "  ${RED}${BOLD}⚠  ALL DATA ON $TARGET ($TARGET_SIZE) WILL BE ERASED!${NC}"
echo ""
read -rp "  Type YES to confirm: " _confirm
[[ "$_confirm" != "YES" ]] && { echo "Cancelled."; exit 0; }

# ── 1. Write image ────────────────────────────────────────────────────────────
step "1/4" "Writing ClusterOS image to $TARGET"

# Only copy the used sectors of the USB — NOT the full USB device size.
# The ClusterOS image is ~8 GB regardless of how large the USB stick is.
# sgdisk -E prints the last used sector; dd only reads up to that point.
USB_LAST_SECTOR=$(sgdisk -E "$USB_DEV" 2>/dev/null | tail -1 | tr -d '[:space:]')
if [[ "$USB_LAST_SECTOR" =~ ^[0-9]+$ && "$USB_LAST_SECTOR" -gt 0 ]]; then
    # Convert sectors (512B) to 4M blocks, rounding up
    IMAGE_BYTES=$(( (USB_LAST_SECTOR + 1) * 512 ))
    IMAGE_4M_BLOCKS=$(( (IMAGE_BYTES + 4194303) / 4194304 ))
    IMAGE_GB=$(( IMAGE_BYTES / 1073741824 ))
    ok "Image size: ~${IMAGE_GB} GB (${IMAGE_4M_BLOCKS} × 4M blocks, last sector ${USB_LAST_SECTOR})"
else
    # Fallback: count blocks from the image size stored in /etc/clusteros/image-size
    IMAGE_4M_BLOCKS=""
    IMAGE_GB="unknown"
    warn "Could not determine image size from sgdisk — will copy full USB (may fail if target is smaller)"
fi

# Sanity check: make sure target is large enough for the image
TARGET_BYTES=$(lsblk -b -d -n -o SIZE "$TARGET" 2>/dev/null | head -1)
if [[ -n "$IMAGE_BYTES" && -n "$TARGET_BYTES" && "$TARGET_BYTES" -lt "$IMAGE_BYTES" ]]; then
    err "Target disk ($TARGET) is too small:"
    err "  Image:  ${IMAGE_GB} GB  ($(( IMAGE_BYTES / 1073741824 * 1073741824 / 1000000000 )) GB)"
    err "  Target: $(( TARGET_BYTES / 1073741824 )) GB"
    err "Use a larger disk or rebuild the USB image."
    exit 1
fi

WRITER="cat"
command -v pv &>/dev/null && WRITER="pv"
if [[ -n "$IMAGE_4M_BLOCKS" ]]; then
    dd if="$USB_DEV" of="$TARGET" bs=4M count="$IMAGE_4M_BLOCKS" conv=fsync status=progress
else
    $WRITER "$USB_DEV" | dd of="$TARGET" bs=4M conv=fsync status=progress
fi
sync
ok "Image written"

# ── 2. Fix GPT + resize partition ─────────────────────────────────────────────
step "2/4" "Resizing partition and filesystem to fill $TARGET"

# Move GPT backup header to end of the (now larger) disk
sgdisk -e "$TARGET" 2>/dev/null && ok "GPT backup header relocated" || warn "sgdisk -e failed (non-fatal)"
partprobe "$TARGET" 2>/dev/null || true
sleep 2

# Find the root (ext4) partition and its number
ROOT_PART=""
ROOT_PARTNUM=""
for _candidate in "${TARGET}p2" "${TARGET}2" "${TARGET}p1" "${TARGET}1"; do
    if [[ -b "$_candidate" ]] && blkid "$_candidate" 2>/dev/null | grep -qi ext4; then
        ROOT_PART="$_candidate"
        ROOT_PARTNUM=$(echo "$_candidate" | grep -oE '[0-9]+$')
        break
    fi
done

if [[ -z "$ROOT_PART" ]]; then
    warn "Could not find ext4 root partition — disk written but not resized"
else
    # Expand partition table entry to fill disk
    if command -v growpart &>/dev/null; then
        growpart "$TARGET" "$ROOT_PARTNUM" && ok "Partition $ROOT_PART expanded" || warn "growpart failed"
    else
        # growpart not available — use sgdisk to delete and recreate the partition at full size
        _PART_START=$(sgdisk -i "$ROOT_PARTNUM" "$TARGET" 2>/dev/null | grep 'First sector:' | awk '{print $3}')
        if [[ -n "$_PART_START" ]]; then
            sgdisk -d "$ROOT_PARTNUM" "$TARGET" 2>/dev/null || true
            sgdisk -n "${ROOT_PARTNUM}:${_PART_START}:0" -t "${ROOT_PARTNUM}:8300" "$TARGET" 2>/dev/null \
                && ok "Partition recreated at full size" || warn "Partition resize failed"
        fi
    fi
    partprobe "$TARGET" 2>/dev/null || true
    sleep 1

    # Check and resize filesystem
    e2fsck -f -y "$ROOT_PART" 2>/dev/null || true
    resize2fs "$ROOT_PART" && ok "Filesystem expanded to fill partition" || warn "resize2fs failed"
fi

# ── 3. Fix UEFI boot ─────────────────────────────────────────────────────────
step "3/4" "Fixing UEFI boot (ensuring BOOTX64.EFI fallback)"

EFI_PART=""
for _candidate in "${TARGET}p15" "${TARGET}15" "${TARGET}p1" "${TARGET}1"; do
    if [[ -b "$_candidate" ]] && blkid "$_candidate" 2>/dev/null | grep -qi "vfat\|fat"; then
        EFI_PART="$_candidate"; break
    fi
done

if [[ -n "$EFI_PART" ]]; then
    EFI_MNT="$(mktemp -d /tmp/clusteros-efi-XXXXXX)"
    mount "$EFI_PART" "$EFI_MNT"
    mkdir -p "$EFI_MNT/EFI/BOOT"
    # Prefer shim (Secure Boot chain) → grub → systemd-boot
    if [[ -f "$EFI_MNT/EFI/ubuntu/shimx64.efi" ]]; then
        cp "$EFI_MNT/EFI/ubuntu/shimx64.efi" "$EFI_MNT/EFI/BOOT/BOOTX64.EFI"
        ok "Secure Boot shim installed as BOOTX64.EFI"
    elif [[ -f "$EFI_MNT/EFI/ubuntu/grubx64.efi" ]]; then
        cp "$EFI_MNT/EFI/ubuntu/grubx64.efi" "$EFI_MNT/EFI/BOOT/BOOTX64.EFI"
        ok "GRUB installed as BOOTX64.EFI"
    else
        warn "No shim or grub found in EFI partition — may need manual UEFI boot entry"
    fi
    # Write a fallback grub.cfg that searches by label (survives partition UUID changes)
    if [[ ! -f "$EFI_MNT/EFI/BOOT/grub.cfg" ]]; then
        cat > "$EFI_MNT/EFI/BOOT/grub.cfg" <<'GRUBCFG'
set timeout=3
set default=0
search --no-floppy --label --set=root cloudimg-rootfs
if [ -z "$root" ]; then search --no-floppy --label --set=root UEFI; fi
set prefix=($root)/boot/grub
configfile $prefix/grub.cfg
menuentry "ClusterOS (fallback)" {
    search --no-floppy --label --set=root cloudimg-rootfs
    linux /boot/vmlinuz root=LABEL=cloudimg-rootfs ro quiet
    initrd /boot/initrd.img
}
GRUBCFG
        ok "Fallback grub.cfg written"
    fi
    umount "$EFI_MNT"; rmdir "$EFI_MNT"
else
    warn "No EFI partition found — BIOS/legacy boot only"
fi

# ── 4. Prepare installed disk for first boot ──────────────────────────────────
step "4/4" "Preparing installed disk for first-boot cluster join"

# Mount the root partition to clear USB-specific state.
# Critical: the dd'd image has .bootstrapped set (from USB first boot), which
# would prevent apply-patch.sh from running on disk first boot — meaning
# Tailscale never re-authenticates with a fresh identity and cluster commands
# stay at the USB image version rather than pulling fresh from the bundle.
_INSTALL_MNT="$(mktemp -d /tmp/clusteros-install-mnt-XXXXXX)"
if mount "$ROOT_PART" "$_INSTALL_MNT" 2>/dev/null; then
    # Force apply-patch.sh to run on first disk boot
    rm -f "$_INSTALL_MNT/var/lib/clusteros/.bootstrapped"
    ok "Cleared .bootstrapped marker — apply-patch.sh will run on first disk boot"

    # Clear Tailscale identity so the new node gets a fresh registration
    rm -rf "$_INSTALL_MNT/var/lib/tailscale/tailscaled.state" 2>/dev/null || true
    ok "Cleared Tailscale state — node will get fresh Tailscale identity"

    # Clear cluster node identity (Ed25519 keypair — must be unique per node)
    rm -f "$_INSTALL_MNT/var/lib/clusteros/identity.json" 2>/dev/null || true
    ok "Cleared node identity — new keypair generated on first boot"

    # Clear K3s state (etcd member entries are wrong if cloned)
    rm -rf "$_INSTALL_MNT/var/lib/rancher/k3s/server/db" \
           "$_INSTALL_MNT/var/lib/rancher/k3s/agent" 2>/dev/null || true
    ok "Cleared K3s state"

    # Reset machine-id so DHCP, Tailscale, and systemd see a new machine
    truncate -s 0 "$_INSTALL_MNT/etc/machine-id" 2>/dev/null || true
    ok "Reset machine-id — regenerated on first boot"

    umount "$_INSTALL_MNT"
else
    warn "Could not mount $ROOT_PART — state not cleared; node may conflict with USB identity"
fi
rmdir "$_INSTALL_MNT" 2>/dev/null || true

echo ""
ok "ClusterOS installed to $TARGET"
echo ""
echo "  Next steps:"
echo "    1. Remove the USB drive"
echo "    2. Reboot — the node boots from $TARGET"
echo "    3. On first boot: apply-patch.sh runs, Tailscale joins, node-agent starts"
echo "    4. Node appears in 'cluster status' within ~5 minutes"
INSTALL_SCRIPT
        chmod 755 "$_MNT/usr/local/bin/cluster-os-install"
        ok "cluster-os-install written"

        # Write node-agent systemd service — runs on every boot
        # NOTE: no --no-reboot flag here; apply-patch.sh auto-detects USB vs disk via
        # lsblk TRAN and sets NO_REBOOT accordingly:
        #   USB boot  → _ROOT_TRAN=usb  → NO_REBOOT=1 (don't reboot, stay live)
        #   Disk boot → _ROOT_TRAN=sata/nvme → NO_REBOOT=0 (reboot to pick up config)
        mkdir -p "$_MNT/etc/systemd/system"
        cat > "$_MNT/etc/systemd/system/node-agent.service" <<'SVCEOF'
[Unit]
Description=ClusterOS node-agent
After=network-online.target tailscaled.service
Wants=network-online.target

[Service]
Type=simple
ExecStartPre=/bin/bash -c 'if [ -f /root/patch/apply-patch.sh ] && [ ! -f /var/lib/clusteros/.bootstrapped ]; then bash /root/patch/apply-patch.sh >> /var/log/clusteros-boot.log 2>&1 && mkdir -p /var/lib/clusteros && touch /var/lib/clusteros/.bootstrapped; fi'
ExecStart=/usr/local/bin/node-agent
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
SVCEOF
        # Mask services node-agent manages directly
        for _svc in munge slurmd slurmctld k3s k3s-agent; do
            ln -sf /dev/null "$_MNT/etc/systemd/system/${_svc}.service" 2>/dev/null || true
        done
        # Enable node-agent
        mkdir -p "$_MNT/etc/systemd/system/multi-user.target.wants"
        ln -sf /etc/systemd/system/node-agent.service \
            "$_MNT/etc/systemd/system/multi-user.target.wants/node-agent.service"
        ok "node-agent.service enabled"

        # Install tailscale.env if present in bundle
        if [[ -f "$BUNDLE_DIR/tailscale.env" ]]; then
            mkdir -p "$_MNT/etc/clusteros"
            install -m 600 "$BUNDLE_DIR/tailscale.env" "$_MNT/etc/clusteros/tailscale.env"
            ok "tailscale.env installed"
        fi

        # Clean state that must be unique per node
        truncate -s 0 "$_MNT/etc/machine-id" 2>/dev/null || true
        rm -f "$_MNT/var/lib/clusteros/identity.json" 2>/dev/null || true
        rm -rf "$_MNT/var/lib/rancher/k3s/server/db" "$_MNT/var/lib/rancher/k3s/agent" 2>/dev/null || true
        rm -f "$_MNT/etc/slurm/slurm.conf" 2>/dev/null || true

        # Unmount chroot
        umount "$_MNT/dev/pts" "$_MNT/dev" "$_MNT/sys" "$_MNT/proc" 2>/dev/null || true
        umount "$_MNT"
        rmdir "$_MNT"
        losetup -d "$_LOOP"
        ok "Provisioning complete"

        step "3a/4" "Writing provisioned image to $DEVICE — ALL DATA WILL BE ERASED"
        echo -n "  Confirm (type 'yes'): "
        read -r CONFIRM
        [[ "$CONFIRM" != "yes" ]] && { echo "Cancelled."; exit 0; }
        WRITER="cat"
        command -v pv &>/dev/null && WRITER="pv"
        $WRITER "$_IMG_WORK" | dd of="$DEVICE" bs=4M conv=fsync status=progress 2>&1 || true
        sync
        sgdisk -e "$DEVICE" 2>/dev/null || true
        partprobe "$DEVICE" 2>/dev/null || true
        sleep 2
        rm -f "$_IMG_WORK"

        echo ""
        ok "USB ready. On first boot from USB:"
        echo "    1. Ubuntu boots directly as a live cluster node"
        echo "    2. apply-patch.sh runs once (Tailscale joins, node-agent starts)"
        echo "    3. Node appears in 'cluster status' — fully functional from USB"
        echo ""
        echo "  To install permanently to internal disk:"
        echo "    sudo cluster-os-install"
        _ISO_WRITE_DONE=true
    fi

    if [[ -f "$BASE_IMAGE" ]]; then
        warn "Writing base installer image to $DEVICE — ALL DATA WILL BE ERASED"
        echo -n "  Confirm (type 'yes'): "
        read -r CONFIRM
        [[ "$CONFIRM" != "yes" ]] && { echo "Cancelled."; exit 0; }

        step "3a/4" "Writing base installer image to $DEVICE"
        WRITER="cat"
        command -v pv &>/dev/null && WRITER="pv"
        gzip -dc "$BASE_IMAGE" | $WRITER | dd of="$DEVICE" bs=4M conv=fsync status=progress 2>&1 || true
        sync
        # Fix GPT backup header (required after writing to a different-sized device)
        sgdisk -e "$DEVICE" 2>/dev/null || true
        partprobe "$DEVICE" 2>/dev/null || true
        sleep 2

        step "3b/4" "Injecting cluster bundle into image"
        # Find root partition (Ubuntu cloud images: p2 or p1 with ext4)
        ROOT_PART=""
        for candidate in "${DEVICE}p2" "${DEVICE}p1" "${DEVICE}2" "${DEVICE}1"; do
            if [[ -b "$candidate" ]] && blkid "$candidate" 2>/dev/null | grep -qi ext4; then
                ROOT_PART="$candidate"
                break
            fi
        done

        if [[ -z "$ROOT_PART" ]]; then
            warn "Could not identify root partition — patch bundle will be written as a tarball instead"
            warn "Apply manually:  tar -xzf $OUTPUT -C ~/ && sudo bash ~/patch/apply-patch.sh"
        else
            MOUNT_DIR="$(mktemp -d /tmp/clusteros-usb-mnt-XXXXXX)"
            mount "$ROOT_PART" "$MOUNT_DIR"

            # Install patch bundle into /root/patch/ on the image
            mkdir -p "$MOUNT_DIR/root/patch"
            cp -a "$BUNDLE_DIR/." "$MOUNT_DIR/root/patch/"
            chmod +x "$MOUNT_DIR/root/patch/apply-patch.sh"

            # Overwrite the pre-built image's node-agent binary + cluster CLI
            # with the current versions from the bundle.
            cp "$BUNDLE_DIR/node-agent" "$MOUNT_DIR/usr/local/bin/node-agent"
            chmod 755 "$MOUNT_DIR/usr/local/bin/node-agent"
            ok "node-agent binary updated in image"
            if [[ -f "$BUNDLE_DIR/cluster" ]]; then
                cp "$BUNDLE_DIR/cluster" "$MOUNT_DIR/usr/local/bin/cluster"
                chmod 755 "$MOUNT_DIR/usr/local/bin/cluster"
                ok "cluster CLI updated in image"
            fi

            # Install Tailscale credentials so new nodes auto-join on first boot
            if [[ -f "$BUNDLE_DIR/tailscale.env" ]]; then
                mkdir -p "$MOUNT_DIR/etc/clusteros"
                install -m 600 "$BUNDLE_DIR/tailscale.env" "$MOUNT_DIR/etc/clusteros/tailscale.env"
                ok "tailscale.env installed to /etc/clusteros/"
            fi

            # Netplan: physical hardware uses en*/eth*/wl* — the Packer image was
            # built for virtual ens3. Without this, DHCP never runs → no network
            # → Tailscale can't auth, apt fails, etc.
            mkdir -p "$MOUNT_DIR/etc/netplan"
            if [[ -f "$BUNDLE_DIR/99-clusteros.yaml" ]]; then
                install -m 600 "$BUNDLE_DIR/99-clusteros.yaml" "$MOUNT_DIR/etc/netplan/99-clusteros.yaml"
                ok "Netplan installed from bundle (wired DHCP + WiFi)"
            else
                warn "99-clusteros.yaml not in bundle — WiFi may not work on new nodes"
            fi

            # CRITICAL: Overwrite node-agent.service with the version that calls
            # apply-patch.sh in ExecStartPre.  The pre-built Packer image has an
            # old service unit without this, so the bundle in /root/patch/ would
            # never be executed and the node would never join the cluster.
            mkdir -p "$MOUNT_DIR/etc/systemd/system"
            cat > "$MOUNT_DIR/etc/systemd/system/node-agent.service" <<'SVCEOF'
[Unit]
Description=ClusterOS node-agent
After=network-online.target tailscaled.service
Wants=network-online.target

[Service]
Type=simple
ExecStartPre=/bin/bash -c 'if [ -f /root/patch/apply-patch.sh ] && [ ! -f /var/lib/clusteros/.bootstrapped ]; then bash /root/patch/apply-patch.sh >> /var/log/clusteros-boot.log 2>&1 && mkdir -p /var/lib/clusteros && touch /var/lib/clusteros/.bootstrapped; fi'
ExecStart=/usr/local/bin/node-agent --log-level info start --foreground
TimeoutStartSec=0
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal
EnvironmentFile=-/etc/cluster-os/node-agent.env

[Install]
WantedBy=multi-user.target
SVCEOF
            # Mask services node-agent manages directly so they don't conflict
            for _svc in munge slurmd slurmctld k3s k3s-agent; do
                ln -sf /dev/null "$MOUNT_DIR/etc/systemd/system/${_svc}.service" 2>/dev/null || true
            done
            # Ensure node-agent.service is enabled (symlink in wants)
            mkdir -p "$MOUNT_DIR/etc/systemd/system/multi-user.target.wants"
            ln -sf /etc/systemd/system/node-agent.service \
                "$MOUNT_DIR/etc/systemd/system/multi-user.target.wants/node-agent.service" 2>/dev/null || true
            ok "node-agent.service updated (apply-patch.sh in ExecStartPre)"

            # Install wpasupplicant so WiFi works immediately on first boot.
            # Without it, netplan writes the WiFi config but nothing connects.
            # We chroot into the mounted image to install it with apt.
            if ! chroot "$MOUNT_DIR" dpkg -s wpasupplicant &>/dev/null 2>&1; then
                ok "Installing wpasupplicant into image (needed for WiFi on first boot)..."
                mount --bind /proc "$MOUNT_DIR/proc" 2>/dev/null || true
                mount --bind /sys  "$MOUNT_DIR/sys"  2>/dev/null || true
                mount --bind /dev  "$MOUNT_DIR/dev"  2>/dev/null || true
                rm -f "$MOUNT_DIR/etc/resolv.conf"
                printf 'nameserver 8.8.8.8\nnameserver 1.1.1.1\n' > "$MOUNT_DIR/etc/resolv.conf"
                chroot "$MOUNT_DIR" /bin/bash -c "
                    export DEBIAN_FRONTEND=noninteractive
                    apt-get -o Acquire::ForceIPv4=true install -y -qq wpasupplicant 2>/dev/null
                " && ok "wpasupplicant installed" || warn "wpasupplicant install failed — WiFi needs wired first boot"
                umount "$MOUNT_DIR/dev" "$MOUNT_DIR/sys" "$MOUNT_DIR/proc" 2>/dev/null || true
            else
                ok "wpasupplicant already present in image"
            fi

            # Clear .bootstrapped so apply-patch.sh runs on first disk boot
            rm -f "$MOUNT_DIR/var/lib/clusteros/.bootstrapped" 2>/dev/null || true
            # Clear Tailscale + cluster identity so the new node gets fresh IDs
            rm -rf "$MOUNT_DIR/var/lib/tailscale/tailscaled.state" 2>/dev/null || true
            rm -f "$MOUNT_DIR/var/lib/clusteros/identity.json" 2>/dev/null || true
            truncate -s 0 "$MOUNT_DIR/etc/machine-id" 2>/dev/null || true

            umount "$MOUNT_DIR"
            rmdir "$MOUNT_DIR"
            ok "Cluster bundle injected into $ROOT_PART"
        fi

        ok "USB installer ready: $DEVICE"
        echo ""
        echo "  Write instructions:"
        echo "    1. Boot new machine from $DEVICE"
        echo "    2. Node auto-joins cluster on first boot (cloud-init runcmd)"
        echo "    3. Or manually: sudo bash /root/patch/apply-patch.sh"

    elif [[ "${_ISO_WRITE_DONE:-false}" != "true" ]]; then
        # No base image and no ISO download — format a FAT32 data partition with the bundle
        warn "No base installer image at $BASE_IMAGE"
        warn "Formatting $DEVICE as FAT32 and writing patch bundle only"
        warn "(new nodes will need a separate Ubuntu install + run apply-patch.sh from this drive)"
        echo -n "  Confirm format of $DEVICE (type 'yes'): "
        read -r CONFIRM
        [[ "$CONFIRM" != "yes" ]] && { echo "Cancelled."; exit 0; }

        # Write a clean MBR partition table with a single FAT32 partition
        parted -s "$DEVICE" mklabel msdos mkpart primary fat32 1MiB 100%
        partprobe "$DEVICE" 2>/dev/null || true
        sleep 1
        DATA_PART="${DEVICE}1"
        [[ -b "${DEVICE}p1" ]] && DATA_PART="${DEVICE}p1"
        mkfs.fat -F32 -n CLUSTEROS "$DATA_PART"
        MOUNT_DIR="$(mktemp -d /tmp/clusteros-usb-mnt-XXXXXX)"
        mount "$DATA_PART" "$MOUNT_DIR"
        mkdir -p "$MOUNT_DIR/patch"
        cp -a "$BUNDLE_DIR/." "$MOUNT_DIR/patch/"
        # Write a README
        cat > "$MOUNT_DIR/README.txt" <<README
ClusterOS Patch Bundle — $(date +%Y-%m-%d)
Source node: $(hostname)

To join this node to the cluster:
  1. Install Ubuntu Server (LTS) on this machine
  2. Boot and log in
  3. Mount this USB:   sudo mount /dev/sdX1 /mnt
  4. Apply the patch:  sudo bash /mnt/patch/apply-patch.sh
  5. Node will auto-join the cluster and reboot

No internet connection is required if all bundle files are present.
README
        sync
        umount "$MOUNT_DIR"
        rmdir "$MOUNT_DIR"
        ok "Patch bundle written to $DATA_PART (FAT32, label: CLUSTEROS)"
    fi

else
    # ── Tarball mode ──────────────────────────────────────────────────────────
    [[ -z "$OUTPUT" ]] && OUTPUT="/tmp/clusteros-patch-${DATE}.tar.gz"
    mkdir -p "$(dirname "$OUTPUT")"
    tar -czf "$OUTPUT" -C "$(dirname "$BUNDLE_DIR")" "$(basename "$BUNDLE_DIR")"
    # Rename the tar root entry to 'patch' for clean extraction
    # Re-pack with consistent top-level dir name
    REPACK_DIR="$(mktemp -d /tmp/clusteros-repack-XXXXXX)"
    cp -a "$BUNDLE_DIR/." "$REPACK_DIR/patch/"
    tar -czf "$OUTPUT" -C "$REPACK_DIR" patch
    rm -rf "$REPACK_DIR"

    local_size=$(du -h "$OUTPUT" | cut -f1)
    ok "Patch bundle tarball: $OUTPUT ($local_size)"
    echo ""
    echo "  To deploy to a new node:"
    echo "    scp $OUTPUT user@new-node:~/"
    echo "    ssh user@new-node 'cd ~ && tar -xzf $(basename "$OUTPUT") && sudo bash ~/patch/apply-patch.sh'"
fi

# ── Step 4: Cleanup ───────────────────────────────────────────────────────────
step "4/4" "Done"

if [[ "$CLEANUP_BUNDLE" == true ]] && [[ -d "$BUNDLE_DIR" ]]; then
    rm -rf "$BUNDLE_DIR"
fi

echo ""
echo -e "${GREEN}${BOLD}USB installer build complete.${NC}"
echo ""
