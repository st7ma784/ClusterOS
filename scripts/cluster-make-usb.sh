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
[[ -z "$OUTPUT" ]] && OUTPUT="/tmp/clusteros-patch-${DATE}.tar.gz"
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

    # Detect boot device to exclude
    BOOT_DEV=""
    ROOT_MOUNT=$(findmnt -n -o SOURCE / 2>/dev/null || true)
    if [[ -n "$ROOT_MOUNT" ]]; then
        BOOT_DEV=$(lsblk -n -o PKNAME "$ROOT_MOUNT" 2>/dev/null | head -1 || true)
        [[ -n "$BOOT_DEV" ]] && BOOT_DEV="/dev/$BOOT_DEV"
    fi

    declare -a _DEVS _DEVINFO
    _IDX=0
    for _dev in /dev/sd? /dev/nvme?n?; do
        [[ -b "$_dev" ]] || continue
        [[ "$_dev" = "$BOOT_DEV" ]] && continue
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
        # Fall through to tarball mode
        OUTPUT="/tmp/clusteros-patch-${DATE}.tar.gz"
        warn "Falling back to tarball: $OUTPUT"
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
        echo -e "    ${RED}[0]${NC} Cancel (write tarball instead)"
        echo ""
        while true; do
            read -rp "  Select USB drive [0-${#_DEVS[@]}]: " _sel
            if [[ "$_sel" =~ ^[0-9]+$ ]]; then
                if [[ "$_sel" -eq 0 ]]; then
                    OUTPUT="/tmp/clusteros-patch-${DATE}.tar.gz"
                    warn "Cancelled — writing tarball: $OUTPUT"
                    break
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

    # Safety check: refuse if device is mounted
    if grep -q "^$DEVICE" /proc/mounts 2>/dev/null; then
        err "$DEVICE or one of its partitions is currently mounted — unmount first"
        exit 1
    fi

    # If no pre-built base image, offer to download Ubuntu server ISO
    if [[ ! -f "$BASE_IMAGE" ]]; then
        warn "No base installer image found at $BASE_IMAGE"
        echo ""
        echo "  Options:"
        echo "    [1] Download Ubuntu 24.04 LTS server ISO now (~1.5 GB, requires internet)"
        echo "    [2] Cancel"
        echo ""
        read -rp "  Choice [1-2]: " _dl_choice
        if [[ "$_dl_choice" = "1" ]]; then
            _ISO_URL="https://releases.ubuntu.com/24.04/ubuntu-24.04-live-server-amd64.iso"
            _ISO_TMP="/tmp/clusteros-ubuntu-base.iso"
            step "3-dl/4" "Downloading Ubuntu 24.04 LTS"
            if command -v wget &>/dev/null; then
                wget -O "$_ISO_TMP" --progress=bar:force "$_ISO_URL" 2>&1 || { err "Download failed"; exit 1; }
            else
                curl -L --progress-bar -o "$_ISO_TMP" "$_ISO_URL" || { err "Download failed"; exit 1; }
            fi
            ok "Downloaded: $_ISO_TMP ($(du -h "$_ISO_TMP" | cut -f1))"

            # Write ISO directly — ISOs are already hybrid bootable (isohybrid/GPT)
            step "3a/4" "Writing Ubuntu ISO to $DEVICE — ALL DATA WILL BE ERASED"
            echo -n "  Confirm (type 'yes'): "
            read -r CONFIRM
            [[ "$CONFIRM" != "yes" ]] && { echo "Cancelled."; exit 0; }
            WRITER="cat"
            command -v pv &>/dev/null && WRITER="pv"
            $WRITER "$_ISO_TMP" | dd of="$DEVICE" bs=4M conv=fsync status=progress 2>&1 || true
            sync
            partprobe "$DEVICE" 2>/dev/null || true
            sleep 2

            ok "Ubuntu ISO written to $DEVICE"

            # Write cluster bundle + autoinstall config to a FAT32 partition after the ISO.
            # Ubuntu's subiquity installer checks for autoinstall.yaml on any mounted partition
            # labelled 'CIDATA' or on the boot medium — putting it on a CIDATA partition is the
            # standard no-cloud/cloud-init mechanism for unattended installs.
            step "3b/4" "Writing autoinstall + cluster bundle (zero-touch first boot)"
            _LAST_SECTOR=$(sgdisk -p "$DEVICE" 2>/dev/null | grep "Last usable sector" | awk '{print $NF}')
            _USED_END=$(sgdisk -p "$DEVICE" 2>/dev/null | grep -E "^[ ]*[0-9]" | tail -1 | awk '{print $3}')
            if [[ -n "$_LAST_SECTOR" && -n "$_USED_END" && $((_LAST_SECTOR - _USED_END)) -gt 4096 ]]; then
                # Create a FAT32 partition labelled CIDATA — subiquity auto-mounts this and
                # reads autoinstall.yaml from its root.
                sgdisk -n 0:"$((_USED_END + 1))":0 -t 0:0700 -c 0:CIDATA "$DEVICE" 2>/dev/null || true
                partprobe "$DEVICE" 2>/dev/null || true; sleep 2
                _DATA_PART=$(lsblk -ln -o NAME,PARTLABEL "$DEVICE" 2>/dev/null | grep CIDATA | awk '{print "/dev/"$1}' | head -1)
                if [[ -b "$_DATA_PART" ]]; then
                    mkfs.fat -F32 -n CIDATA "$_DATA_PART" 2>/dev/null || true
                    _MNT="$(mktemp -d /tmp/clusteros-usb-mnt-XXXXXX)"
                    mount "$_DATA_PART" "$_MNT"

                    # Copy cluster bundle
                    mkdir -p "$_MNT/patch"
                    cp -a "$BUNDLE_DIR/." "$_MNT/patch/"

                    # Write meta-data (required by cloud-init/subiquity, can be empty)
                    echo "instance-id: clusteros-$(hostname)-$(date +%s)" > "$_MNT/meta-data"

                    # Write autoinstall.yaml — fully unattended Ubuntu install
                    cat > "$_MNT/user-data" <<'AUTOINSTALL'
#cloud-config
autoinstall:
  version: 1
  # Use the first available disk, wipe it, install Ubuntu
  storage:
    layout:
      name: direct
  identity:
    hostname: clusteros-node
    username: clusteros
    # password: clusteros  (mkpasswd -m sha-512 clusteros)
    password: "$6$rounds=4096$clusteros$yH0QrAnTE3.pxGDhOBqkqa7G8JCu3B7y9PBXqWDvHr7BYh0pqhWKKrfNWSlbXqWRnm1HO0kX5CKHl0jCB1Pq1"
  ssh:
    install-server: true
    allow-pw: true
  packages:
    - curl
    - wget
  # After install, copy the cluster bundle and schedule apply-patch.sh on first boot
  late-commands:
    - mkdir -p /target/root/patch
    - "find /cdrom /run/mount/cd -name 'apply-patch.sh' -path '*/patch/*' 2>/dev/null | head -1 | xargs -I{} cp -a $(dirname {}) /target/root/ || true"
    - "cp -a /run/initramfs/live/patch/. /target/root/patch/ 2>/dev/null || true"
    - |
      cat > /target/etc/systemd/system/clusteros-join.service << 'EOF'
      [Unit]
      Description=ClusterOS first-boot join
      After=network-online.target tailscaled.service
      Wants=network-online.target
      ConditionPathExists=/root/patch/apply-patch.sh

      [Service]
      Type=oneshot
      ExecStart=/bin/bash /root/patch/apply-patch.sh
      ExecStartPost=/bin/systemctl disable clusteros-join.service
      RemainAfterExit=yes
      StandardOutput=journal+console
      StandardError=journal+console

      [Install]
      WantedBy=multi-user.target
      EOF
    - curtin in-target --target=/target systemctl enable clusteros-join.service
AUTOINSTALL

                    sync; umount "$_MNT"; rmdir "$_MNT"
                    ok "CIDATA partition written to $_DATA_PART"
                    ok "  autoinstall.yaml → unattended Ubuntu install + cluster join"
                    ok "  Cluster bundle → copied to /root/patch/ during install"
                    ok "  clusteros-join.service → runs apply-patch.sh on first post-install boot"
                else
                    warn "Could not create CIDATA partition — USB will require manual install"
                fi
            else
                warn "Not enough free space on device for CIDATA partition"
            fi

            echo ""
            ok "USB is ready. Boot sequence:"
            echo "    1. Insert USB into new machine and boot from it"
            echo "    2. Ubuntu installs automatically (no prompts)"
            echo "    3. On first post-install boot, node joins the cluster automatically"
            echo "    4. Node appears in 'cluster status' within ~5 minutes"
            rm -f "$_ISO_TMP"
            _ISO_WRITE_DONE=true
        else
            echo "Cancelled."
            exit 0
        fi
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

            # Write cloud-init runcmd that auto-applies the patch on first boot
            mkdir -p "$MOUNT_DIR/etc/cloud/cloud.cfg.d"
            cat > "$MOUNT_DIR/etc/cloud/cloud.cfg.d/99-clusteros-join.cfg" <<'CLOUDINIT'
#cloud-config
# Auto-generated by cluster-make-usb — runs apply-patch.sh on first boot
# to join this node to the cluster without any manual intervention.
runcmd:
  - [ bash, -c, "if [ -f /root/patch/apply-patch.sh ]; then bash /root/patch/apply-patch.sh >> /var/log/clusteros-join.log 2>&1; fi" ]
CLOUDINIT
            ok "cloud-init runcmd written → node will auto-join cluster on first boot"

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
