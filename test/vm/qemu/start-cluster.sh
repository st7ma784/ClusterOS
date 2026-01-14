#!/bin/bash
set -e

# QEMU VM Cluster Launcher for Cluster OS
# Launches multiple QEMU VMs with full systemd support

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

# Configuration
NUM_NODES="${NUM_NODES:-3}"
# Packer outputs to /data/packer-output/cluster-os-node (see images/ubuntu/packer.pkr.hcl)
BASE_IMAGE="${BASE_IMAGE:-/data/packer-output/cluster-os-node/cluster-os-node.qcow2}"
VM_PREFIX="cluster-os-vm"
BASE_PORT=2222
BASE_VNC_PORT=5900
MEMORY="${MEMORY:-2048}"
CPUS="${CPUS:-2}"
VM_DIR="$SCRIPT_DIR/vms"
SHARED_KEY="$PROJECT_ROOT/cluster.key"

# Detect KVM availability
if [ -e /dev/kvm ] && grep -q kvm /proc/cpuinfo 2>/dev/null; then
    QEMU_ACCEL="kvm"
    QEMU_CPU="host"
else
    QEMU_ACCEL="tcg"
    QEMU_CPU="qemu64"  # Use generic CPU model for TCG
fi

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Functions
log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check prerequisites
check_prereqs() {
    log_info "Checking prerequisites..."

    if ! command -v qemu-system-x86_64 &> /dev/null; then
        log_error "qemu-system-x86_64 not found. Install with: sudo apt-get install qemu-system-x86"
        exit 1
    fi

    if ! command -v qemu-img &> /dev/null; then
        log_error "qemu-img not found. Install with: sudo apt-get install qemu-utils"
        exit 1
    fi

    if [ ! -f "$BASE_IMAGE" ]; then
        log_error "Base image not found: $BASE_IMAGE"
        log_error "Build it first with: make image"
        exit 1
    fi

    log_info "Prerequisites check passed"
}

# Create VM directory structure
setup_vm_dirs() {
    log_info "Setting up VM directories..."
    mkdir -p "$VM_DIR"

    for i in $(seq 1 "$NUM_NODES"); do
        mkdir -p "$VM_DIR/node$i"
    done
}

# Create VM disk images
create_vm_disks() {
    log_info "Creating VM disk images..."

    for i in $(seq 1 "$NUM_NODES"); do
        VM_DISK="$VM_DIR/node$i/disk.qcow2"

        if [ -f "$VM_DISK" ]; then
            log_warn "VM disk already exists: $VM_DISK (skipping)"
        else
            log_info "Creating backing disk for node$i..."
            qemu-img create -f qcow2 -b "$BASE_IMAGE" -F qcow2 "$VM_DISK" 20G
        fi
    done
}

# Generate cloud-init config for each VM
create_cloud_init() {
    local node_num=$1
    local vm_dir="$VM_DIR/node$node_num"

    log_info "Creating cloud-init config for node$node_num..."

    # User data
    cat > "$vm_dir/user-data" <<EOF
#cloud-config
hostname: cluster-node-$node_num
fqdn: cluster-node-$node_num.cluster.local

# Preserve hostname
preserve_hostname: false

# Write cluster key
write_files:
  - path: /etc/cluster-os/cluster.key
    content: $(cat "$SHARED_KEY" 2>/dev/null || echo "auto-generated-key-$(date +%s)")
    permissions: '0600'
    owner: root:root

# Run on first boot
runcmd:
  - systemctl daemon-reload
  - systemctl restart node-agent.service
  - netplan apply

# Set timezone
timezone: UTC

# Disable cloud-init on subsequent boots
final_message: "Cluster OS node$node_num is ready!"
EOF

    # Meta data
    cat > "$vm_dir/meta-data" <<EOF
instance-id: cluster-os-node-$node_num
local-hostname: cluster-node-$node_num
EOF

    # Network config (optional)
    cat > "$vm_dir/network-config" <<EOF
version: 2
ethernets:
  ens3:
    dhcp4: true
EOF

    # Create cloud-init ISO
    if command -v cloud-localds &> /dev/null; then
        # cloud-localds syntax: cloud-localds output user-data [meta-data] [--network-config=file]
        cloud-localds -N "$vm_dir/network-config" "$vm_dir/cloud-init.iso" "$vm_dir/user-data" "$vm_dir/meta-data"
    elif command -v genisoimage &> /dev/null; then
        # Fallback to genisoimage if cloud-localds not available
        mkdir -p "$vm_dir/cidata"
        cp "$vm_dir/user-data" "$vm_dir/cidata/"
        cp "$vm_dir/meta-data" "$vm_dir/cidata/"
        cp "$vm_dir/network-config" "$vm_dir/cidata/"
        genisoimage -output "$vm_dir/cloud-init.iso" \
            -volid cidata -joliet -rock \
            -V cidata "$vm_dir/cidata"
        rm -rf "$vm_dir/cidata"
    else
        log_error "Neither cloud-localds nor genisoimage found. Install cloud-image-utils or genisoimage"
        exit 1
    fi
}

# Launch a single VM
launch_vm() {
    local node_num=$1
    local vm_dir="$VM_DIR/node$node_num"
    local ssh_port=$((BASE_PORT + node_num))
    local vnc_port=$((BASE_VNC_PORT + node_num - 1))
    local pid_file="$vm_dir/qemu.pid"

    # Check if already running
    if [ -f "$pid_file" ] && kill -0 $(cat "$pid_file") 2>/dev/null; then
        log_warn "VM node$node_num already running (PID: $(cat "$pid_file"))"
        return
    fi

    log_info "Launching VM node$node_num (SSH: $ssh_port, VNC: $vnc_port)..."

    # Create TAP interface for networking (optional, using user networking for simplicity)
    # This uses QEMU user networking with port forwarding

    qemu-system-x86_64 \
        -name "cluster-os-node$node_num" \
        -machine type=pc,accel=$QEMU_ACCEL \
        -cpu $QEMU_CPU \
        -smp cpus=$CPUS \
        -m $MEMORY \
        -drive file="$vm_dir/disk.qcow2",format=qcow2,if=virtio \
        -drive file="$vm_dir/cloud-init.iso",format=raw,if=virtio,readonly=on \
        -netdev user,id=net0,hostfwd=tcp::${ssh_port}-:22 \
        -device virtio-net-pci,netdev=net0 \
        -vnc ":$((vnc_port - 5900))" \
        -daemonize \
        -pidfile "$pid_file" \
        -serial file:"$vm_dir/serial.log" \
        -display none

    log_info "VM node$node_num launched (PID: $(cat "$pid_file"))"
    log_info "  SSH: ssh -p $ssh_port clusteros@localhost"
    log_info "  VNC: vnc://localhost:$vnc_port"
}

# Main execution
main() {
    log_info "========================================="
    log_info "Cluster OS QEMU VM Cluster Launcher"
    log_info "========================================="
    log_info "Configuration:"
    log_info "  Nodes: $NUM_NODES"
    log_info "  Base Image: $BASE_IMAGE"
    log_info "  Memory per VM: ${MEMORY}M"
    log_info "  CPUs per VM: $CPUS"
    log_info "  QEMU Accelerator: $QEMU_ACCEL"
    if [ "$QEMU_ACCEL" = "tcg" ]; then
        log_info "  (TCG is slow - VMs will take 2-5 minutes to boot)"
    fi
    log_info "========================================="
    echo ""

    check_prereqs
    setup_vm_dirs
    create_vm_disks

    # Create cloud-init configs
    for i in $(seq 1 "$NUM_NODES"); do
        create_cloud_init "$i"
    done

    echo ""
    log_info "Launching VMs..."

    # Launch VMs
    for i in $(seq 1 "$NUM_NODES"); do
        launch_vm "$i"
        sleep 2  # Stagger launches
    done

    echo ""
    log_info "========================================="
    log_info "Cluster launched successfully!"
    log_info "========================================="
    echo ""
    echo "VM Access:"
    for i in $(seq 1 "$NUM_NODES"); do
        ssh_port=$((BASE_PORT + i))
        vnc_port=$((BASE_VNC_PORT + i - 1))
        echo "  Node $i: ssh -p $ssh_port clusteros@localhost (VNC: $vnc_port)"
    done
    echo ""
    echo "Useful commands:"
    echo "  $SCRIPT_DIR/cluster-ctl.sh status    # Check cluster status"
    echo "  $SCRIPT_DIR/cluster-ctl.sh stop      # Stop all VMs"
    echo "  $SCRIPT_DIR/cluster-ctl.sh shell N   # SSH to node N"
    echo "  $SCRIPT_DIR/stop-cluster.sh          # Stop cluster"
    echo ""
}

main "$@"
