#!/bin/bash
set -e

# Stop QEMU VM Cluster

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VM_DIR="$SCRIPT_DIR/vms"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_info "Stopping QEMU VM cluster..."

if [ ! -d "$VM_DIR" ]; then
    log_warn "VM directory not found: $VM_DIR"
    exit 0
fi

stopped=0
for node_dir in "$VM_DIR"/node*; do
    if [ ! -d "$node_dir" ]; then
        continue
    fi

    node_name=$(basename "$node_dir")
    pid_file="$node_dir/qemu.pid"

    if [ -f "$pid_file" ]; then
        pid=$(cat "$pid_file")
        if kill -0 "$pid" 2>/dev/null; then
            log_info "Stopping $node_name (PID: $pid)..."
            kill "$pid"
            stopped=$((stopped + 1))
        else
            log_warn "$node_name PID file exists but process not running"
            rm -f "$pid_file"
        fi
    fi
done

log_info "Stopped $stopped VM(s)"

# Wait for processes to terminate
sleep 2

log_info "Cluster stopped"
