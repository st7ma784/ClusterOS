#!/bin/bash

# Simple SSH test VM launcher
set -e

VM_DIR="/tmp/ssh-test-vm"
BASE_IMAGE="/data/packer-output/cluster-os-node/cluster-os-node.qcow2"
SSH_PORT=3333

echo "Creating test VM..."
mkdir -p "$VM_DIR"

# Create disk
qemu-img create -f qcow2 -b "$BASE_IMAGE" -F qcow2 "$VM_DIR/disk.qcow2" 10G

# Create minimal cloud-init
mkdir -p "$VM_DIR/cidata"
cat > "$VM_DIR/cidata/user-data" <<'EOF'
#cloud-config
hostname: test-node
runcmd:
  - echo "Cloud-init running"
  - touch /tmp/cloud-init-ran
EOF

cat > "$VM_DIR/cidata/meta-data" <<'EOF'
instance-id: test-node
local-hostname: test-node
EOF

# Create cloud-init ISO
genisoimage -output "$VM_DIR/cloud-init.iso" \
    -volid cidata -joliet -rock \
    -V cidata "$VM_DIR/cidata"

echo "Launching VM on port $SSH_PORT..."
qemu-system-x86_64 \
    -name test-vm \
    -machine type=pc,accel=kvm \
    -cpu host \
    -enable-kvm \
    -smp cpus=2 \
    -m 2048 \
    -drive file="$VM_DIR/disk.qcow2",format=qcow2,if=virtio \
    -drive file="$VM_DIR/cloud-init.iso",format=raw,if=virtio,readonly=on \
    -netdev user,id=net0,hostfwd=tcp::${SSH_PORT}-:22 \
    -device virtio-net-pci,netdev=net0 \
    -display none \
    -daemonize \
    -pidfile "$VM_DIR/qemu.pid" \
    -serial mon:stdio

echo "VM launching...waiting for SSH"
sleep 60

echo "Attempting SSH connection..."
for i in {1..30}; do
    if timeout 2 ssh -p $SSH_PORT -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null clusteros@localhost "echo SSH works!" 2>/dev/null; then
        echo "SUCCESS: SSH is working!"
        break
    fi
    echo "Attempt $i: SSH not ready yet..."
    sleep 2
done

echo "Test complete. VM still running at port $SSH_PORT"
echo "To stop: kill \$(cat $VM_DIR/qemu.pid)"
