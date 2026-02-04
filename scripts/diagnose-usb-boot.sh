#!/bin/bash
set -e

# Diagnostic script for USB boot issues
# Usage: sudo ./diagnose-usb-boot.sh /dev/sdX

if [ -z "$1" ]; then
    echo "Usage: sudo $0 /dev/sdX"
    echo ""
    echo "Diagnose boot issues on a USB device or image"
    echo "Examples:"
    echo "  sudo $0 /dev/sda      (check physical USB drive)"
    echo "  sudo $0 /dev/nbd0     (check NBD-mounted image)"
    echo "  $0 cluster-os-usb.img (check image file)"
    exit 1
fi

DEVICE="$1"
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() { echo -e "${BLUE}[INFO]${NC} $1"; }
log_ok() { echo -e "${GREEN}[OK]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Check if device exists
if [ ! -e "$DEVICE" ]; then
    log_error "Device/file not found: $DEVICE"
    exit 1
fi

# Check read permissions
if [ ! -r "$DEVICE" ]; then
    log_error "Cannot read $DEVICE - may need sudo"
    exit 1
fi

echo ""
log_info "Diagnosing boot configuration for: $DEVICE"
echo ""

# 1. Check MBR boot signature
log_info "Checking MBR boot signature..."
mbr_bytes=$(dd if="$DEVICE" bs=2 count=256 skip=255 2>/dev/null | od -An -tx1 | head -1)
if echo "$mbr_bytes" | grep -q "55.*aa"; then
    log_ok "MBR boot signature (55 aa) found"
else
    log_warn "MBR boot signature NOT found"
fi

# 2. Check for bootloader
log_info "Checking for bootloader signatures..."

# Check for GRUB
if strings "$DEVICE" 2>/dev/null | grep -q "GRUB"; then
    log_ok "GRUB bootloader detected"
else
    log_warn "GRUB bootloader NOT detected"
fi

# Check for LILO
if strings "$DEVICE" 2>/dev/null | grep -q "LILO"; then
    log_ok "LILO bootloader detected"
else
    :
fi

# 3. Check partition table
log_info "Checking partition table..."
echo ""

# Try parted first
if command -v parted &>/dev/null; then
    parted -m "$DEVICE" print 2>/dev/null || true
    echo ""
fi

# Also try fdisk
if command -v fdisk &>/dev/null; then
    echo "fdisk analysis:"
    sudo fdisk -l "$DEVICE" 2>/dev/null | head -20
    echo ""
fi

# 4. Check for boot flag
log_info "Checking for boot flag..."
if command -v parted &>/dev/null; then
    boot_partitions=$(parted -m "$DEVICE" print 2>/dev/null | grep "boot" || true)
    if [ -n "$boot_partitions" ]; then
        log_ok "Boot flag found on partition"
        echo "$boot_partitions"
    else
        log_warn "No boot flag found on partitions"
    fi
fi
echo ""

# 5. Check kernel/initrd in /boot
log_info "Checking for kernel and initrd..."

# Try to mount first partition
MOUNT_DIR=$(mktemp -d)
trap "umount -f $MOUNT_DIR 2>/dev/null || true; rmdir $MOUNT_DIR 2>/dev/null || true" EXIT

# Determine offset for first partition
if [ -b "$DEVICE" ]; then
    # It's a block device (USB drive)
    PART="${DEVICE}1"
    if [ ! -b "$PART" ]; then
        log_warn "Could not find first partition at ${DEVICE}1"
    else
        if mount "$PART" "$MOUNT_DIR" 2>/dev/null; then
            if [ -f "$MOUNT_DIR/boot/vmlinuz"* ]; then
                log_ok "Kernel found in /boot"
                ls -lh "$MOUNT_DIR/boot/vmlinuz"*
            else
                log_warn "No kernel found in /boot"
            fi
            
            if [ -f "$MOUNT_DIR/boot/initrd"* ]; then
                log_ok "Initrd found in /boot"
                ls -lh "$MOUNT_DIR/boot/initrd"*
            else
                log_warn "No initrd found in /boot"
            fi
            
            umount "$MOUNT_DIR"
        else
            log_warn "Could not mount $PART"
        fi
    fi
else
    # It's an image file - use loop device or qemu-nbd
    if command -v qemu-nbd &>/dev/null; then
        log_info "Using qemu-nbd to mount image..."
        sudo qemu-nbd -c /dev/nbd0 "$DEVICE" 2>/dev/null || true
        
        if [ -b /dev/nbd0p1 ]; then
            if sudo mount /dev/nbd0p1 "$MOUNT_DIR" 2>/dev/null; then
                if [ -f "$MOUNT_DIR/boot/vmlinuz"* ]; then
                    log_ok "Kernel found in /boot"
                    ls -lh "$MOUNT_DIR/boot/vmlinuz"*
                else
                    log_warn "No kernel found in /boot"
                fi
                
                sudo umount "$MOUNT_DIR"
            fi
            
            sudo qemu-nbd -d /dev/nbd0 2>/dev/null || true
        fi
    else
        log_warn "Cannot mount image file without qemu-nbd"
    fi
fi

echo ""
log_info "Diagnosis complete"
echo ""
echo "Boot troubleshooting guide:"
echo "  - If MBR signature is missing: image may be corrupted"
echo "  - If GRUB not detected: bootloader not installed"
echo "  - If boot flag missing: BIOS may not boot"
echo "  - If kernel missing: filesystem is corrupted"
echo ""
echo "Common fixes:"
echo "  1. Reinstall image: gunzip -c cluster-os-usb.img.gz | sudo dd of=/dev/sdX bs=4M"
echo "  2. Rebuild image:   make image && make usb"
echo "  3. Check hardware:  Try booting from different USB port or device"
echo "  4. Enable BIOS:     Check BIOS settings for USB/Legacy boot"
echo ""
