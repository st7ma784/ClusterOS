#!/bin/bash

# Quick SSH test with standard Ubuntu cloud image (skips Packer)
set -e

TEST_DIR="/tmp/ssh-quick-test"
SSH_PORT=3334
CLOUD_IMAGE_URL="https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img"

mkdir -p "$TEST_DIR"
cd "$TEST_DIR"

echo "Downloading Ubuntu Cloud Image..."
if [ ! -f jammy-server-cloudimg-amd64.img ]; then
    curl -# -o jammy-server-cloudimg-amd64.img "$CLOUD_IMAGE_URL"
fi

echo "Creating test disk..."
qemu-img create -f qcow2 -b jammy-server-cloudimg-amd64.img -F qcow2 test-disk.qcow2 10G

echo "Creating cloud-init ISO..."
mkdir -p cidata
cat > cidata/user-data <<'CLOUD_INIT'
#cloud-config
hostname: test-ssh-node
runcmd:
  - echo "SSH is ready!"
  - touch /tmp/cloud-init-ran
CLOUD_INIT

cat > cidata/meta-data <<'META'
instance-id: test-ssh-node
local-hostname: test-ssh-node
META

genisoimage -output cloud-init.iso -volid cidata -joliet -rock -V cidata cidata

echo "Launching test VM on SSH port $SSH_PORT..."
qemu-system-x86_64 \
    -m 1024 \
    -drive file=test-disk.qcow2,if=virtio \
    -drive file=cloud-init.iso,if=virtio,media=cdrom \
    -netdev user,id=net0,hostfwd=tcp::${SSH_PORT}-:22 \
    -device virtio-net,netdev=net0 \
    -display none \
    -daemonize \
    -pidfile qemu.pid

echo "VM starting on SSH port $SSH_PORT..."
echo "Waiting 120 seconds for boot and SSH to start..."

for i in {1..60}; do
    if timeout 2 ssh -p $SSH_PORT -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=2 ubuntu@localhost "echo SSH is working" 2>/dev/null; then
        echo "✓ SUCCESS: SSH is working on port $SSH_PORT!"
        exit 0
    fi
    sleep 2
    echo -n "."
done

echo ""
echo "✗ SSH failed to start after 120 seconds"
kill $(cat qemu.pid) 2>/dev/null || true
exit 1
