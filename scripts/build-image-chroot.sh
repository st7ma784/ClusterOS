#!/bin/bash
# ClusterOS Image Builder — No-KVM path
# Builds the OS image using chroot instead of QEMU/KVM.
# Use when /dev/kvm is unavailable (VM host without nested virt passthrough).
#
# Usage: sudo bash scripts/build-image-chroot.sh
#        make image-chroot
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

UBUNTU_URL="https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img"
CACHE_DIR="${CLUSTEROS_CACHE:-/data/packer-cache}"
OUTPUT_DIR="/data/packer-output/cluster-os-node"
VM_NAME="cluster-os-node"
CACHED_IMG="$CACHE_DIR/ubuntu-24.04-cloudimg.img"
WORK_RAW="$CACHE_DIR/clusteros-work.raw"
OUTPUT_QCOW2="$OUTPUT_DIR/$VM_NAME.qcow2"
OUTPUT_RAW="$OUTPUT_DIR/$VM_NAME.raw"
IMAGE_SIZE="8G"

# Mutable — set during prepare_image / mount_image
LOOP_DEV=""
ROOT_PART=""
MOUNT_DIR=""

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
log_info()  { echo -e "${GREEN}[INFO]${NC}  $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }
log_step()  { echo -e "\n${BLUE}==> $1${NC}"; }

# ── Cleanup on exit ─────────────────────────────────────────────────────────
cleanup() {
    for sub in dev/pts dev proc sys run; do
        [ -n "$MOUNT_DIR" ] && mountpoint -q "$MOUNT_DIR/$sub" 2>/dev/null \
            && sudo umount "$MOUNT_DIR/$sub" 2>/dev/null || true
    done
    [ -n "$MOUNT_DIR" ] && mountpoint -q "$MOUNT_DIR" 2>/dev/null \
        && sudo umount "$MOUNT_DIR" 2>/dev/null || true
    [ -n "$LOOP_DEV" ] && sudo losetup -d "$LOOP_DEV" 2>/dev/null || true
    [ -n "$MOUNT_DIR" ] && rm -rf "$MOUNT_DIR" || true
}
trap cleanup EXIT

# ── 1. Prerequisites ────────────────────────────────────────────────────────
check_prereqs() {
    log_step "Checking prerequisites"
    local missing=()
    for tool in qemu-img losetup mount parted e2fsck resize2fs curl; do
        command -v "$tool" &>/dev/null || missing+=("$tool")
    done
    if [ "${#missing[@]}" -gt 0 ]; then
        log_error "Missing: ${missing[*]}\nInstall: sudo apt install qemu-utils e2fsprogs parted curl"
    fi
    command -v sgdisk &>/dev/null || log_warn "sgdisk not found (install gdisk for safer GPT resize)"
    sudo -n true 2>/dev/null || log_error "This script requires passwordless sudo (or run as root)"
    log_info "Prerequisites OK"
}

# ── 2. Download base image ──────────────────────────────────────────────────
download_image() {
    log_step "Ubuntu 24.04 cloud image"
    mkdir -p "$CACHE_DIR"
    if [ -f "$CACHED_IMG" ]; then
        log_info "Cached: $CACHED_IMG ($(du -sh "$CACHED_IMG" | cut -f1))"
        return
    fi
    log_info "Downloading (~650 MB)..."
    curl -L --progress-bar -o "$CACHED_IMG.tmp" "$UBUNTU_URL"
    mv "$CACHED_IMG.tmp" "$CACHED_IMG"
    log_info "Downloaded: $CACHED_IMG"
}

# ── 3. Prepare work image ───────────────────────────────────────────────────
prepare_image() {
    log_step "Preparing raw image (qcow2 -> raw, expand to $IMAGE_SIZE)"
    mkdir -p "$OUTPUT_DIR"

    log_info "Converting qcow2 -> raw..."
    qemu-img convert -f qcow2 -O raw "$CACHED_IMG" "$WORK_RAW"

    log_info "Expanding to $IMAGE_SIZE..."
    qemu-img resize -f raw "$WORK_RAW" "$IMAGE_SIZE"

    # Move GPT backup header to new end of file
    if command -v sgdisk &>/dev/null; then
        sgdisk -e "$WORK_RAW" 2>/dev/null || true
    fi

    log_info "Setting up loop device..."
    LOOP_DEV=$(sudo losetup --find --show --partscan "$WORK_RAW")
    log_info "Loop device: $LOOP_DEV"
    sleep 1

    # Find root partition: prefer label "cloudimg-rootfs", fall back to largest ext4
    ROOT_PART=""
    for p in 1 2 3; do
        part="${LOOP_DEV}p${p}"
        [ -b "$part" ] || continue
        if sudo blkid "$part" 2>/dev/null | grep -qi 'cloudimg-rootfs'; then
            ROOT_PART="$part"; break
        fi
        if sudo blkid "$part" 2>/dev/null | grep -q 'TYPE="ext4"'; then
            ROOT_PART="$part"  # keep looking for a labeled one
        fi
    done
    [ -n "$ROOT_PART" ] || log_error "Could not find root partition in $WORK_RAW"
    ROOT_PART_NUM=$(echo "$ROOT_PART" | grep -o '[0-9]*$')
    log_info "Root partition: $ROOT_PART (p$ROOT_PART_NUM)"

    log_info "Resizing partition to fill disk..."
    sudo parted -s "$LOOP_DEV" resizepart "$ROOT_PART_NUM" 100% 2>/dev/null || {
        log_warn "parted resizepart failed; trying growpart..."
        command -v growpart &>/dev/null && sudo growpart "$LOOP_DEV" "$ROOT_PART_NUM" || true
    }
    sudo partprobe "$LOOP_DEV" 2>/dev/null || true
    sleep 1

    log_info "Resizing ext4 filesystem..."
    sudo e2fsck -f -y "$ROOT_PART" 2>/dev/null || true
    sudo resize2fs "$ROOT_PART"
    log_info "Filesystem resized"
}

# ── 4. Mount image ──────────────────────────────────────────────────────────
mount_image() {
    log_step "Mounting image"
    MOUNT_DIR=$(mktemp -d /tmp/clusteros-chroot-XXXXX)
    sudo mount "$ROOT_PART" "$MOUNT_DIR"
    for sub in proc sys dev dev/pts run; do
        sudo mkdir -p "$MOUNT_DIR/$sub"
        case "$sub" in
            proc)    sudo mount -t proc  proc  "$MOUNT_DIR/proc" ;;
            sys)     sudo mount -t sysfs sysfs "$MOUNT_DIR/sys"  ;;
            run)     sudo mount -t tmpfs tmpfs "$MOUNT_DIR/run"  ;;
            dev)     sudo mount --bind /dev     "$MOUNT_DIR/dev" ;;
            dev/pts) sudo mount --bind /dev/pts "$MOUNT_DIR/dev/pts" ;;
        esac
    done
    # DNS so apt / curl can reach the internet.
    # Ubuntu cloud images symlink /etc/resolv.conf -> ../run/systemd/resolve/stub-resolv.conf
    # which dangles in chroot (systemd-resolved not running). Remove the symlink first.
    sudo rm -f "$MOUNT_DIR/etc/resolv.conf"
    sudo cp /etc/resolv.conf "$MOUNT_DIR/etc/resolv.conf"
    # Force IPv4 for apt (prevents hangs on IPv6-broken networks)
    echo 'Acquire::ForceIPv4 "true";' \
        | sudo tee "$MOUNT_DIR/etc/apt/apt.conf.d/99force-ipv4" > /dev/null
    log_info "Mounted at $MOUNT_DIR"
}

# ── 5. Install wrappers ─────────────────────────────────────────────────────
install_wrappers() {
    log_step "Installing chroot wrappers"
    local wrap_dir="$MOUNT_DIR/usr/local/.chroot-wrappers"
    sudo mkdir -p "$wrap_dir"

    # systemctl: enable/disable/mask use --root=/ (no D-Bus needed in chroot).
    # start/stop/daemon-reload are no-ops (no init process running).
    sudo tee "$wrap_dir/systemctl" > /dev/null << 'WRAPPER'
#!/bin/bash
cmd="${1:-}"; shift || true
case "$cmd" in
    enable|disable|mask|unmask|preset|preset-all)
        /bin/systemctl "$cmd" --root=/ "$@" 2>/dev/null || true ;;
    daemon-reload|start|stop|restart|try-restart|reload|condrestart|\
is-active|is-enabled|is-failed|status)
        exit 0 ;;
    *)
        /bin/systemctl "$cmd" "$@" 2>/dev/null || true ;;
esac
WRAPPER
    sudo chmod +x "$wrap_dir/systemctl"

    # sysctl: no-op — writing to host /proc/sys is both unnecessary and risky
    printf '#!/bin/bash\nexit 0\n' | sudo tee "$wrap_dir/sysctl" > /dev/null
    sudo chmod +x "$wrap_dir/sysctl"

    # update-initramfs / mkinitramfs: replace the actual binaries in /usr/sbin/.
    # dpkg triggers call these via FULL PATH (/usr/sbin/update-initramfs), which
    # bypasses our PATH-based wrapper directory entirely.  We must replace the
    # real file.  Save the original as .real so finalize() can restore it —
    # the shipped image needs a working update-initramfs for future kernel updates.
    for _cmd in update-initramfs mkinitramfs; do
        _real="$MOUNT_DIR/usr/sbin/$_cmd"
        [ -f "$_real" ] && sudo cp "$_real" "${_real}.real"
        printf '#!/bin/sh\necho "[chroot] %s suppressed"\nexit 0\n' "$_cmd" \
            | sudo tee "$_real" > /dev/null
        sudo chmod +x "$_real"
    done

    # invoke-rc.d: use dpkg-divert so the stub survives apt installing/upgrading
    # sysvinit-utils (which owns invoke-rc.d). Plain file replacement gets
    # overwritten the moment dpkg touches that package during apt-get install.
    # dpkg-divert tells dpkg to send sysvinit-utils's copy to .real instead,
    # leaving our no-op stub permanently in place until we remove the diversion
    # in finalize().
    sudo chroot "$MOUNT_DIR" dpkg-divert --local --rename \
        --divert /usr/sbin/invoke-rc.d.real \
        /usr/sbin/invoke-rc.d 2>/dev/null || true
    printf '#!/bin/sh\nexit 0\n' \
        | sudo tee "$MOUNT_DIR/usr/sbin/invoke-rc.d" > /dev/null
    sudo chmod +x "$MOUNT_DIR/usr/sbin/invoke-rc.d"

    log_info "Wrappers installed"
}

# ── 6. Copy provision files ─────────────────────────────────────────────────
copy_files() {
    log_step "Staging provision files"
    local img_dir="$PROJECT_ROOT/images/ubuntu"

    sudo cp "$img_dir/node-agent"   "$MOUNT_DIR/tmp/node-agent"
    sudo cp "$img_dir/provision.sh" "$MOUNT_DIR/tmp/provision.sh"

    # remote installer (optional — handled gracefully by provision.sh)
    for candidate in \
        "$PROJECT_ROOT/patch/remote-node-installer.sh" \
        "$PROJECT_ROOT/remote-install/remote-node-installer.sh"; do
        if [ -f "$candidate" ]; then
            sudo cp "$candidate" "$MOUNT_DIR/tmp/cluster-os-install"
            break
        fi
    done

    [ -f "$PROJECT_ROOT/cluster.key" ] \
        && sudo cp "$PROJECT_ROOT/cluster.key" "$MOUNT_DIR/tmp/cluster.key"

    sudo mkdir -p "$MOUNT_DIR/tmp/clusteros-files"
    for d in config systemd netplan motd bin tailscale; do
        [ -d "$img_dir/files/$d" ] \
            && sudo cp -r "$img_dir/files/$d" "$MOUNT_DIR/tmp/clusteros-files/"
    done
    [ -f "$img_dir/.env" ] \
        && sudo cp "$img_dir/.env" "$MOUNT_DIR/tmp/clusteros-files/.env"

    # Patch bundle -> /root/patch (apply-patch.sh runs on first boot via node-agent.service)
    sudo mkdir -p "$MOUNT_DIR/root/patch"
    sudo cp -r "$PROJECT_ROOT/patch/." "$MOUNT_DIR/root/patch/"
    sudo chmod +x \
        "$MOUNT_DIR/root/patch/apply-patch.sh" \
        "$MOUNT_DIR/root/patch/cluster" 2>/dev/null || true

    log_info "Files staged"
}

# ── 7. Create clusteros user ────────────────────────────────────────────────
create_user() {
    log_step "Creating clusteros user"
    sudo chroot "$MOUNT_DIR" id clusteros &>/dev/null \
        || sudo chroot "$MOUNT_DIR" useradd -m -G sudo,adm -s /bin/bash clusteros
    echo "clusteros ALL=(ALL) NOPASSWD:ALL" \
        | sudo tee "$MOUNT_DIR/etc/sudoers.d/clusteros-nopasswd" > /dev/null
    sudo chmod 440 "$MOUNT_DIR/etc/sudoers.d/clusteros-nopasswd"
    # Password and SSH key are set in finalize() AFTER provision.sh so no
    # apt postinst script can override them.
    log_info "clusteros user created (password + SSH key will be set in finalize)"
}

# ── 8. Run provisioning ─────────────────────────────────────────────────────
run_provision() {
    log_step "Running provision.sh in chroot"

    local wrap_path="/usr/local/.chroot-wrappers"
    local std_path="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
    local chroot_env="PATH=$wrap_path:$std_path DEBIAN_FRONTEND=noninteractive HOME=/root"

    # Cloud-init never ran, so the apt cache is empty. Update it first.
    log_info "Updating apt cache..."
    sudo chroot "$MOUNT_DIR" env $chroot_env apt-get update -qq

    # needrestart runs as a dpkg trigger after every apt-get install. It scans
    # /proc (host processes bleed through the bind mount), then calls invoke-rc.d
    # to restart detected services — which fails even with our diversion in place
    # because needrestart also checks policy. Remove it entirely before any
    # service packages are installed.
    log_info "Removing needrestart..."
    sudo chroot "$MOUNT_DIR" env $chroot_env \
        apt-get purge -y needrestart 2>/dev/null || true

    # Pre-configure any half-installed packages before provision.sh runs.
    # If a previous (interrupted) run left dpkg in a broken state, this
    # recovers it so apt calls inside provision.sh don't immediately fail.
    sudo chroot "$MOUNT_DIR" env $chroot_env dpkg --configure -a 2>/dev/null || true

    log_info "Installing k3s, SLURM, Tailscale, WiFi firmware... (~10-15 min)"
    sudo chroot "$MOUNT_DIR" env $chroot_env bash /tmp/provision.sh
}

# ── 9. Finalize ─────────────────────────────────────────────────────────────
finalize() {
    log_step "Finalizing image"

    local wrap_path="/usr/local/.chroot-wrappers"
    local std_path="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

    # Ensure node-agent starts on first boot
    sudo chroot "$MOUNT_DIR" \
        env PATH="$wrap_path:$std_path" \
        bash -c "systemctl enable node-agent.service 2>/dev/null || true"

    # ── Password — set AFTER provision.sh so no postinst can override it ────────
    # Multiple methods in descending reliability order.
    _hash=$(openssl passwd -6 -salt 'clusteros' 'clusteros' 2>/dev/null || true)
    if [ -z "$_hash" ]; then
        _hash=$(python3 -c "import crypt; print(crypt.crypt('clusteros', crypt.mksalt(crypt.METHOD_SHA512)))" 2>/dev/null || true)
    fi
    if [ -n "$_hash" ] && sudo grep -q "^clusteros:" "$MOUNT_DIR/etc/shadow" 2>/dev/null; then
        sudo sed -i "s|^clusteros:[^:]*:|clusteros:${_hash}:|" "$MOUNT_DIR/etc/shadow"
    elif [ -n "$_hash" ]; then
        sudo chroot "$MOUNT_DIR" usermod -p "$_hash" clusteros 2>/dev/null || true
    else
        log_warn "Hash generation failed — falling back to chpasswd"
        echo "clusteros:clusteros" | sudo chroot "$MOUNT_DIR" chpasswd 2>/dev/null || true
    fi
    # Verify — first 10 chars of hash appear in build log so we know it was set.
    _stored=$(sudo grep "^clusteros:" "$MOUNT_DIR/etc/shadow" 2>/dev/null | cut -d: -f2)
    if [ -n "$_stored" ] && [ "$_stored" != "!" ] && [ "$_stored" != "*" ] && [ "$_stored" != "!!" ]; then
        log_info "PASSWORD VERIFIED: clusteros shadow hash=${_stored:0:10}..."
    else
        log_warn "PASSWORD NOT SET: clusteros shadow field='$_stored' -- login WILL fail"
    fi

    # ── SSH: remove cloud-init override that blocks password auth ─────────────
    # Ubuntu cloud images ship /etc/ssh/sshd_config.d/50-cloud-init.conf with
    # PasswordAuthentication no.  OpenSSH processes includes FIRST (first-match
    # wins) so our main sshd_config PasswordAuthentication yes is silently ignored.
    sudo rm -f "$MOUNT_DIR/etc/ssh/sshd_config.d/50-cloud-init.conf" 2>/dev/null || true
    sudo sed -i \
        -e 's/^#\?PasswordAuthentication.*/PasswordAuthentication yes/' \
        -e 's/^#\?KbdInteractiveAuthentication.*/KbdInteractiveAuthentication yes/' \
        "$MOUNT_DIR/etc/ssh/sshd_config" 2>/dev/null || true

    # ── Console autologin on tty1 ─────────────────────────────────────────────
    # Belt-and-suspenders for the installer image: even if PAM or the password
    # hash has any issue, the physical console logs in automatically as clusteros.
    sudo mkdir -p "$MOUNT_DIR/etc/systemd/system/getty@tty1.service.d"
    printf '[Service]\nExecStart=\nExecStart=-/sbin/agetty --autologin clusteros --noclear %%I $TERM\n' \
        | sudo tee "$MOUNT_DIR/etc/systemd/system/getty@tty1.service.d/autologin.conf" > /dev/null
    log_info "Console: tty1 autologin configured for clusteros"

    # ── Pre-install SSH authorised key ──────────────────────────────────────────
    # apply-patch.sh writes to /home/clusteros/.ssh at runtime; pre-seeding it here
    # means the key works even before first-boot bootstrapping completes.
    if [ -f "$PROJECT_ROOT/patch/authorized_keys" ]; then
        sudo mkdir -p "$MOUNT_DIR/home/clusteros/.ssh"
        sudo cp "$PROJECT_ROOT/patch/authorized_keys" \
            "$MOUNT_DIR/home/clusteros/.ssh/authorized_keys"
        sudo chmod 700 "$MOUNT_DIR/home/clusteros/.ssh"
        sudo chmod 600 "$MOUNT_DIR/home/clusteros/.ssh/authorized_keys"
        sudo chroot "$MOUNT_DIR" chown -R clusteros:clusteros /home/clusteros/.ssh
        log_info "SSH authorized key pre-installed in image"
    fi

    # Disable cloud-init entirely for chroot-built images.
    # cloud-init clean --seed makes it re-run on first boot, but Ubuntu 24.04's
    # base /etc/cloud/cloud.cfg has ssh_pwauth: false which overrides our
    # create_user() SSH config, breaking clusteros:clusteros password login.
    # node-agent.service + apply-patch.sh own first-boot setup for this path.
    sudo touch "$MOUNT_DIR/etc/cloud/cloud-init.disabled"

    # Add console=tty0 so VGA keyboard+monitor shows a login prompt.
    # Patch /etc/default/grub for persistence, then patch grub.cfg directly
    # so it takes effect without running update-grub (which bleeds host /dev
    # into the chroot and can embed the wrong root UUID).
    sudo sed -i \
        's/^GRUB_CMDLINE_LINUX="\(.*\)"/GRUB_CMDLINE_LINUX="console=tty0 console=ttyS0,115200n8 \1"/' \
        "$MOUNT_DIR/etc/default/grub" 2>/dev/null || true
    if [ -f "$MOUNT_DIR/boot/grub/grub.cfg" ]; then
        sudo sed -i \
            's/\(linux.*vmlinuz[^ ]*\)\(.*\)$/\1 console=tty0\2/' \
            "$MOUNT_DIR/boot/grub/grub.cfg" 2>/dev/null || true
        log_info "GRUB: console=tty0 patched directly into grub.cfg"
    fi

    # Identity and state must not be shared across clones
    sudo rm -f  "$MOUNT_DIR/var/lib/cluster-os/identity.json" 2>/dev/null || true
    sudo rm -rf "$MOUNT_DIR/var/lib/rancher/k3s/server/db"    2>/dev/null || true
    sudo rm -rf "$MOUNT_DIR/var/lib/rancher/k3s/agent"        2>/dev/null || true
    sudo rm -f  "$MOUNT_DIR/etc/slurm/slurm.conf"             2>/dev/null || true
    sudo truncate -s 0 "$MOUNT_DIR/etc/machine-id"            2>/dev/null || true

    # Restore invoke-rc.d: remove stub, then remove the dpkg-divert so the
    # real binary (.real) moves back to its original path.
    sudo rm -f "$MOUNT_DIR/usr/sbin/invoke-rc.d"
    sudo chroot "$MOUNT_DIR" dpkg-divert --local --rename \
        --divert /usr/sbin/invoke-rc.d.real \
        --remove /usr/sbin/invoke-rc.d 2>/dev/null || true

    # Restore update-initramfs and mkinitramfs from the .real backups made in
    # install_wrappers().  The shipped image needs working initramfs tools for
    # future kernel updates on hardware.
    for _cmd in update-initramfs mkinitramfs; do
        _real="$MOUNT_DIR/usr/sbin/$_cmd"
        if [ -f "${_real}.real" ]; then
            sudo mv "${_real}.real" "$_real"
        fi
    done

    # Tidy up — remove build-time-only artefacts
    sudo rm -rf \
        "$MOUNT_DIR/tmp/node-agent" \
        "$MOUNT_DIR/tmp/provision.sh" \
        "$MOUNT_DIR/tmp/clusteros-files" \
        "$MOUNT_DIR/tmp/cluster.key" \
        "$MOUNT_DIR/tmp/cluster-os-install" \
        "$MOUNT_DIR/usr/local/.chroot-wrappers" \
        "$MOUNT_DIR/etc/apt/apt.conf.d/99force-ipv4" \
        2>/dev/null || true

    sudo chroot "$MOUNT_DIR" apt-get clean 2>/dev/null || true
    sudo rm -rf "$MOUNT_DIR/var/tmp/"* 2>/dev/null || true

    log_info "Image finalized"
}

# ── 10. Unmount and produce output ──────────────────────────────────────────
produce_output() {
    log_step "Producing output images"

    # Unmount everything — all || true so a stale/already-released mount
    # doesn't abort the step and leave the output dir empty.
    for sub in dev/pts dev run proc sys; do
        sudo umount "$MOUNT_DIR/$sub" 2>/dev/null || true
    done
    sudo umount "$MOUNT_DIR" 2>/dev/null || true
    [ -n "$LOOP_DEV" ] && { sudo losetup -d "$LOOP_DEV" 2>/dev/null || true; LOOP_DEV=""; }
    [ -n "$MOUNT_DIR" ] && { rm -rf "$MOUNT_DIR" 2>/dev/null || true; MOUNT_DIR=""; }
    sync

    # Sanity check — a fully provisioned image is at least 4 GB on disk.
    # If work.raw is smaller, provisioning almost certainly failed mid-way.
    local work_size
    work_size=$(stat -c%s "$WORK_RAW" 2>/dev/null || echo 0)
    if [ "$work_size" -lt 4294967296 ]; then
        log_error "work.raw is only $(( work_size / 1024 / 1024 )) MB — provisioning likely failed. Re-run make image-chroot."
    fi

    log_info "Checking provisioned filesystem usage..."
    local loop_tmp; loop_tmp=$(sudo losetup --find --show --partscan "$WORK_RAW" 2>/dev/null || true)
    if [ -n "$loop_tmp" ]; then
        sleep 1
        local root_tmp=""
        for p in "${loop_tmp}p1" "${loop_tmp}p2" "${loop_tmp}p3"; do
            [ -b "$p" ] && sudo blkid "$p" 2>/dev/null | grep -q 'ext4' && root_tmp="$p" && break
        done
        if [ -n "$root_tmp" ]; then
            local used_pct; used_pct=$(sudo tune2fs -l "$root_tmp" 2>/dev/null \
                | awk '/Block count/{bc=$3} /Free blocks/{fb=$3} END{printf "%d%%", int((bc-fb)*100/bc)}')
            log_info "Root filesystem: $used_pct used"
            [ "${used_pct%%%}" -lt 20 ] \
                && log_warn "Filesystem is less than 20% full — packages may be missing from provisioning"
        fi
        sudo losetup -d "$loop_tmp" 2>/dev/null || true
    fi

    log_info "Copying raw image to output dir..."
    cp --sparse=always "$WORK_RAW" "$OUTPUT_RAW"

    log_info "Compressing raw image for Etcher (.img.gz)..."
    gzip -kf "$OUTPUT_RAW"

    # qcow2 is optional (useful for QEMU testing) — non-fatal if it fails
    log_info "Converting to qcow2 (for QEMU testing)..."
    qemu-img convert -f raw -O qcow2 -c "$WORK_RAW" "$OUTPUT_QCOW2" 2>/dev/null \
        || log_warn "qcow2 conversion failed — raw and .gz images are still usable"

    echo ""
    log_info "Output files:"
    ls -lh "$OUTPUT_DIR/"
}

# ── Main ────────────────────────────────────────────────────────────────────
main() {
    echo ""
    log_info "ClusterOS Image Builder (no-KVM / chroot path)"
    log_info "Project: $PROJECT_ROOT"
    echo ""

    check_prereqs
    download_image
    prepare_image
    mount_image
    install_wrappers
    copy_files
    create_user
    run_provision
    finalize
    produce_output

    echo ""
    log_info "========================================="
    log_info "Build complete!"
    log_info "Output: $OUTPUT_DIR"
    log_info "Next:   make usb   (create USB installer)"
    log_info "========================================="
}

main "$@"
