#!/bin/bash

# Debug SSH connection issues
set -e

QEMU_PID=$(cat vms/node1/qemu.pid 2>/dev/null || echo "")
PORT=2223

echo "=== QEMU Debug ==="
echo "PID: $QEMU_PID"
if [ -n "$QEMU_PID" ]; then
    if kill -0 "$QEMU_PID" 2>/dev/null; then
        echo "Status: RUNNING"
    else
        echo "Status: DEAD"
    fi
fi

echo ""
echo "=== Port Status ==="
ss -tuln | grep 2223 || echo "Port 2223 not listening"

echo ""
echo "=== Network Test ==="
nc -zv -w2 localhost 2223 && echo "Port 2223 is open" || echo "Port 2223 connection failed"

echo ""
echo "=== SSH Version Check ==="
timeout 3 ssh -v localhost -p 2223 2>&1 | head -5 || echo "SSH connection failed during banner exchange"

echo ""
echo "=== Serial Log (last 50 lines) ==="
tail -50 vms/node1/serial.log 2>/dev/null || echo "Serial log is empty or doesn't exist"

echo ""
echo "=== Cloud-init Log (via QEMU monitor) ==="
echo "Note: Cannot access VM logs directly without additional tools"
echo "To debug further, consider:"
echo "  1. Using VNC to connect to vnc://localhost:5900"
echo "  2. Adding more verbose logging to cloud-init"
echo "  3. Checking if node-agent is interfering with SSH"
