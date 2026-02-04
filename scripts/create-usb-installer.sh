#!/bin/bash
set -e

# Cluster OS USB Installer Creator
# Creates a bootable USB installer or ISO from the Packer-built image

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Configuration
# Packer outputs to /data/packer-output/cluster-os-node (see packer.pkr.hcl)
IMAGE_DIR="/data/packer-output/cluster-os-node"
BASE_IMAGE="$IMAGE_DIR/cluster-os-node.qcow2"
RAW_IMAGE="$IMAGE_DIR/cluster-os-node.raw"
OUTPUT_DIR="$PROJECT_ROOT/dist"
ISO_OUTPUT="$OUTPUT_DIR/cluster-os-installer.iso"
IMG_OUTPUT="$OUTPUT_DIR/cluster-os-usb.img"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Show usage
usage() {
    cat <<EOF
Cluster OS USB Installer Creator

Usage: $0 [OPTIONS]

Options:
  --iso             Create bootable ISO (default)
  --usb             Create USB image (.img)
  --both            Create both ISO and USB image
  --output PATH     Output directory (default: $OUTPUT_DIR)
  --help            Show this help message

Examples:
  $0 --iso
  $0 --usb --output /tmp/images
  $0 --both

The created image can be written to a USB drive with:
  sudo dd if=$IMG_OUTPUT of=/dev/sdX bs=4M status=progress oflag=sync

EOF
}

# Check prerequisites
check_prereqs() {
    log_info "Checking prerequisites..."

    if [ ! -f "$RAW_IMAGE" ]; then
        log_error "Raw image not found: $RAW_IMAGE"
        log_error "Build the image first with: make image"
        exit 1
    fi

    if ! command -v qemu-img &> /dev/null; then
        log_error "qemu-img not found. Install with: sudo apt-get install qemu-utils"
        exit 1
    fi

    log_info "Prerequisites check passed"
}

# Create bootable USB image
create_usb_image() {
    log_info "Creating USB bootable image..."

    mkdir -p "$OUTPUT_DIR"

    # Copy the raw image as-is (it's already bootable from Packer)
    log_info "Copying raw image..."
    cp "$RAW_IMAGE" "$IMG_OUTPUT"

    local img_size=$(du -h "$IMG_OUTPUT" | cut -f1)
    log_info "USB image created: $IMG_OUTPUT ($img_size)"

    # Embed the installer image into the image itself (self-replicating)
    log_info "Embedding installer image for self-replication..."
    embed_installer_image

    # Final size after embedding
    local final_size=$(du -h "$IMG_OUTPUT" | cut -f1)
    log_info "Final USB image: $IMG_OUTPUT ($final_size)"

    echo ""
    echo "To write to USB drive:"
    echo "  sudo dd if=$IMG_OUTPUT of=/dev/sdX bs=4M status=progress oflag=sync"
    echo ""
    echo "Replace /dev/sdX with your USB device (use lsblk to find it)"
    echo "WARNING: This will erase all data on the USB drive!"
}

# Embed the installer image into the built image for self-replication
embed_installer_image() {
    log_info "Mounting image to embed installer..."
    
    local LOOP_DEV=""
    local MOUNT_DIR="/tmp/clusteros-embed-$$"
    
    # Set up loop device
    LOOP_DEV=$(sudo losetup --find --show --partscan "$IMG_OUTPUT")
    if [ -z "$LOOP_DEV" ]; then
        log_warn "Failed to set up loop device, skipping embed"
        return 0
    fi
    
    # Wait for partitions
    sleep 2
    
    # Find the root partition (usually partition 1 or 2)
    local ROOT_PART=""
    if [ -b "${LOOP_DEV}p2" ]; then
        ROOT_PART="${LOOP_DEV}p2"
    elif [ -b "${LOOP_DEV}p1" ]; then
        ROOT_PART="${LOOP_DEV}p1"
    else
        log_warn "Could not find root partition, skipping embed"
        sudo losetup -d "$LOOP_DEV" 2>/dev/null || true
        return 0
    fi
    
    # Mount
    mkdir -p "$MOUNT_DIR"
    if ! sudo mount "$ROOT_PART" "$MOUNT_DIR" 2>/dev/null; then
        log_warn "Failed to mount root partition, skipping embed"
        sudo losetup -d "$LOOP_DEV" 2>/dev/null || true
        rmdir "$MOUNT_DIR" 2>/dev/null || true
        return 0
    fi
    
    # Create directory and copy compressed image
    sudo mkdir -p "$MOUNT_DIR/usr/share/clusteros"
    
    log_info "Compressing and embedding installer (this may take a minute)..."
    sudo gzip -c "$RAW_IMAGE" > /tmp/installer.img.gz
    sudo mv /tmp/installer.img.gz "$MOUNT_DIR/usr/share/clusteros/installer.img.gz"
    sudo chmod 644 "$MOUNT_DIR/usr/share/clusteros/installer.img.gz"
    
    local embedded_size=$(sudo du -h "$MOUNT_DIR/usr/share/clusteros/installer.img.gz" | cut -f1)
    log_info "Embedded installer: $embedded_size (compressed)"
    
    # Cleanup
    sudo umount "$MOUNT_DIR"
    sudo losetup -d "$LOOP_DEV"
    rmdir "$MOUNT_DIR" 2>/dev/null || true
    
    log_info "Self-replicating installer embedded successfully"
}

# Create bootable ISO
create_iso() {
    log_info "Creating bootable ISO..."

    if ! command -v genisoimage &> /dev/null && ! command -v mkisofs &> /dev/null; then
        log_error "genisoimage or mkisofs not found. Install with: sudo apt-get install genisoimage"
        exit 1
    fi

    mkdir -p "$OUTPUT_DIR/iso_build"
    local iso_build="$OUTPUT_DIR/iso_build"

    # Extract kernel and initrd from the image
    log_info "Extracting boot files from image..."

    # Mount the image temporarily
    local mount_point="$OUTPUT_DIR/mnt"
    mkdir -p "$mount_point"

    # Find first partition offset
    local offset=$(parted "$RAW_IMAGE" unit B print | grep '^ *1' | awk '{print $2}' | tr -d 'B')

    if [ -z "$offset" ]; then
        log_warn "Could not determine partition offset, trying default..."
        offset=$((2048 * 512))  # Common default
    fi

    sudo mount -o loop,offset="$offset",ro "$RAW_IMAGE" "$mount_point" 2>/dev/null || {
        log_error "Failed to mount image. Trying alternative method..."

        # Alternative: use qemu-nbd
        if command -v qemu-nbd &> /dev/null; then
            log_info "Using qemu-nbd to mount image..."
            sudo modprobe nbd max_part=8
            sudo qemu-nbd -c /dev/nbd0 "$RAW_IMAGE"
            sleep 1
            sudo mount -o ro /dev/nbd0p1 "$mount_point"
        else
            log_error "Could not mount image. Install qemu-utils or check image format."
            exit 1
        fi
    }

    # Copy boot files
    mkdir -p "$iso_build/boot"
    sudo cp "$mount_point/boot/vmlinuz"* "$iso_build/boot/vmlinuz" 2>/dev/null || true
    sudo cp "$mount_point/boot/initrd"* "$iso_build/boot/initrd.img" 2>/dev/null || true

    # Copy the entire image as payload
    cp "$RAW_IMAGE" "$iso_build/cluster-os.img"

    # Create isolinux boot config
    mkdir -p "$iso_build/isolinux"

    cat > "$iso_build/isolinux/isolinux.cfg" <<'EOF'
DEFAULT install
LABEL install
  MENU LABEL Install Cluster OS
  KERNEL /boot/vmlinuz
  APPEND initrd=/boot/initrd.img boot=live quiet splash
PROMPT 0
TIMEOUT 50
EOF

    # Unmount
    sudo umount "$mount_point" 2>/dev/null || true
    [ -b /dev/nbd0 ] && sudo qemu-nbd -d /dev/nbd0 2>/dev/null || true

    # Create ISO
    log_info "Building ISO image..."

    if command -v genisoimage &> /dev/null; then
        genisoimage -o "$ISO_OUTPUT" \
            -b isolinux/isolinux.bin \
            -c isolinux/boot.cat \
            -no-emul-boot \
            -boot-load-size 4 \
            -boot-info-table \
            -J -R -V "ClusterOS" \
            "$iso_build" 2>/dev/null || {
                log_warn "Could not create bootable ISO, creating data ISO instead..."
                genisoimage -o "$ISO_OUTPUT" -J -R -V "ClusterOS" "$iso_build"
            }
    fi

    # Make ISO bootable (if possible)
    if command -v isohybrid &> /dev/null; then
        log_info "Making ISO hybrid (bootable on USB)..."
        isohybrid "$ISO_OUTPUT" 2>/dev/null || true
    fi

    # Cleanup
    rm -rf "$iso_build" "$mount_point"

    local iso_size=$(du -h "$ISO_OUTPUT" | cut -f1)
    log_info "ISO created: $ISO_OUTPUT ($iso_size)"

    echo ""
    echo "To write to USB drive:"
    echo "  sudo dd if=$ISO_OUTPUT of=/dev/sdX bs=4M status=progress oflag=sync"
    echo ""
    echo "Or burn to DVD using your preferred burning software"
}

# Create simple installer ISO (alternative method)
create_simple_iso() {
    log_info "Checking image size for ISO compatibility..."

    local raw_size=$(stat -c%s "$RAW_IMAGE" 2>/dev/null)
    local max_iso_size=$((4294967296 - 1))  # 4GB - 1

    if [ "$raw_size" -gt "$max_iso_size" ]; then
        log_warn "Image size ($((raw_size / 1024 / 1024 / 1024))GB) exceeds ISO 9660 4GB limit"
        log_warn "Skipping ISO creation - the USB image (cluster-os-usb.img.gz) is the recommended method"
        return 0
    fi

    log_info "Creating simple installer ISO..."

    mkdir -p "$OUTPUT_DIR"

    if ! command -v genisoimage &> /dev/null; then
        log_error "genisoimage not found. Install with: sudo apt-get install genisoimage"
        exit 1
    fi

    # Create temporary directory structure
    local iso_dir="$OUTPUT_DIR/iso_temp"
    mkdir -p "$iso_dir"

    # Copy the raw image
    cp "$RAW_IMAGE" "$iso_dir/cluster-os.img"

    # Create README
    cat > "$iso_dir/README.txt" <<EOF
Cluster OS Installer

This ISO contains a raw disk image of Cluster OS.

To install on hardware:
1. Boot from a Linux live USB
2. Copy cluster-os.img to the target disk:
   sudo dd if=/path/to/cluster-os.img of=/dev/sdX bs=4M status=progress

Replace /dev/sdX with your target drive (use lsblk to identify it)

For more information, visit: https://github.com/cluster-os
EOF

    # Create install script
    cat > "$iso_dir/install.sh" <<'EOF'
#!/bin/bash
echo "Cluster OS Installer"
echo "===================="
echo ""
lsblk
echo ""
read -p "Enter target device (e.g., /dev/sda): " device
echo ""
echo "WARNING: This will erase all data on $device!"
read -p "Are you sure? (yes/no): " confirm

if [ "$confirm" = "yes" ]; then
    echo "Installing Cluster OS to $device..."
    sudo dd if=cluster-os.img of="$device" bs=4M status=progress oflag=sync
    echo "Installation complete!"
else
    echo "Installation cancelled"
fi
EOF
    chmod +x "$iso_dir/install.sh"

    # Create ISO
    genisoimage -o "$ISO_OUTPUT" \
        -J -R -V "ClusterOS Installer" \
        -A "Cluster OS Installer" \
        "$iso_dir"

    # Cleanup
    rm -rf "$iso_dir"

    local iso_size=$(du -h "$ISO_OUTPUT" | cut -f1)
    log_info "Simple ISO created: $ISO_OUTPUT ($iso_size)"

    echo ""
    echo "This ISO can be mounted to access the installer image"
}

# Main execution
main() {
    local create_iso=false
    local create_usb=false

    # Parse arguments
    while [ $# -gt 0 ]; do
        case "$1" in
            --iso)
                create_iso=true
                ;;
            --usb)
                create_usb=true
                ;;
            --both)
                create_iso=true
                create_usb=true
                ;;
            --output)
                OUTPUT_DIR="$2"
                shift
                ;;
            --help|-h)
                usage
                exit 0
                ;;
            *)
                log_error "Unknown option: $1"
                usage
                exit 1
                ;;
        esac
        shift
    done

    # Default to ISO if nothing specified
    if [ "$create_iso" = false ] && [ "$create_usb" = false ]; then
        create_iso=true
    fi

    log_info "========================================="
    log_info "Cluster OS USB Installer Creator"
    log_info "========================================="

    check_prereqs

    if [ "$create_usb" = true ]; then
        create_usb_image
    fi

    if [ "$create_iso" = true ]; then
        create_simple_iso
    fi

    log_info "========================================="
    log_info "Done!"
    log_info "========================================="

    echo ""
    echo "Created artifacts:"
    ls -lh "$OUTPUT_DIR"/*.{iso,img.gz} 2>/dev/null || true
}

main "$@"
