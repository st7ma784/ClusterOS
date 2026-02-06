#!/bin/bash
# ClusterOS USB Writer
# Writes the ClusterOS image to a USB drive and fixes the GPT partition table
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
IMG_FILE=""
DEVICE=""

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Normalize device path (add /dev/ if missing)
normalize_device() {
    local dev="$1"
    if [[ "$dev" == /dev/* ]]; then
        echo "$dev"
    else
        echo "/dev/$dev"
    fi
}

usage() {
    cat <<EOF
ClusterOS USB Writer

Usage: $0 [image_file] <device>

Arguments:
  image_file    Path to ClusterOS image (default: dist/cluster-os-usb.img)
  device        Target USB device (e.g., /dev/sdb, /dev/sdc)

Examples:
  $0 /dev/sdb                           # Use default image
  $0 dist/cluster-os-usb.img /dev/sdb   # Specify image file

This script:
  1. Writes the image to the USB drive using dd
  2. Fixes the GPT partition table (moves backup header to end of disk)
  3. Syncs to ensure all data is written

EOF
    exit 1
}

# Parse arguments
if [ $# -eq 0 ]; then
    usage
elif [ $# -eq 1 ]; then
    # Single argument = device only, use default image
    DEVICE=$(normalize_device "$1")
    IMG_FILE="$PROJECT_ROOT/dist/cluster-os-usb.img"
elif [ $# -eq 2 ]; then
    # Two arguments = image file and device
    IMG_FILE="$1"
    DEVICE=$(normalize_device "$2")
else
    usage
fi

# Validate
if [ ! -f "$IMG_FILE" ]; then
    log_error "Image file not found: $IMG_FILE"
    log_error "Build it first with: make usb"
    exit 1
fi

if [ ! -b "$DEVICE" ]; then
    log_error "Device not found or not a block device: $DEVICE"
    echo ""
    echo "Available block devices:"
    lsblk -d -o NAME,SIZE,TYPE,MODEL | grep -v "loop"
    exit 1
fi

# Check for sgdisk
if ! command -v sgdisk &>/dev/null; then
    log_error "sgdisk not found. Install with: sudo apt-get install gdisk"
    exit 1
fi

# Safety check - don't write to mounted devices
if mount | grep -q "^$DEVICE"; then
    log_error "Device $DEVICE appears to be mounted!"
    log_error "Unmount it first with: sudo umount ${DEVICE}*"
    exit 1
fi

# Confirm
echo ""
log_warn "========================================="
log_warn "WARNING: This will ERASE ALL DATA on $DEVICE"
log_warn "========================================="
echo ""
echo "Device info:"
lsblk "$DEVICE" -o NAME,SIZE,TYPE,MODEL 2>/dev/null || true
echo ""
read -p "Are you sure you want to continue? (yes/no): " confirm
if [ "$confirm" != "yes" ]; then
    echo "Aborted."
    exit 1
fi

echo ""
log_info "Writing image to $DEVICE..."
log_info "Image: $IMG_FILE ($(du -h "$IMG_FILE" | cut -f1))"
echo ""

# Write image
sudo dd if="$IMG_FILE" of="$DEVICE" bs=4M status=progress oflag=sync conv=fsync

echo ""
log_info "Fixing GPT partition table..."
# Fix backup GPT header location - this is critical for real hardware
sudo sgdisk -e "$DEVICE"

log_info "Syncing..."
sync

echo ""
log_info "========================================="
log_info "USB drive written successfully!"
log_info "========================================="
echo ""
echo "You can now boot from $DEVICE"
echo ""
echo "Default login:"
echo "  Username: clusteros"
echo "  Password: clusteros"
echo ""
