#!/bin/bash

# Cluster Control Script for QEMU VMs

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VM_DIR="$SCRIPT_DIR/vms"
BASE_PORT=2222

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Show usage
usage() {
    cat <<EOF
Cluster OS QEMU VM Control Script

Usage: $0 <command> [arguments]

Commands:
  status              Show status of all VMs
  info [node]         Show detailed info for all nodes or specific node
  shell <node>        SSH into a specific node (e.g., shell 1)
  logs <node>         Show serial console logs for a node
  exec <node> <cmd>   Execute command on a node via SSH
  stop                Stop all VMs
  clean               Stop VMs and remove all VM data
  help                Show this help message

Examples:
  $0 status
  $0 shell 1
  $0 logs 2
  $0 exec 1 "sudo systemctl status node-agent"
  $0 info
  $0 stop

EOF
}

# Check if VMs exist
check_vms() {
    if [ ! -d "$VM_DIR" ]; then
        log_error "No VMs found. Start cluster first with: ./start-cluster.sh"
        exit 1
    fi
}

# Show VM status
show_status() {
    check_vms

    echo "========================================="
    echo "Cluster OS VM Status"
    echo "========================================="

    for node_dir in "$VM_DIR"/node*; do
        if [ ! -d "$node_dir" ]; then
            continue
        fi

        node_name=$(basename "$node_dir")
        node_num=${node_name#node}
        pid_file="$node_dir/qemu.pid"
        ssh_port=$((BASE_PORT + node_num))

        if [ -f "$pid_file" ] && kill -0 $(cat "$pid_file") 2>/dev/null; then
            pid=$(cat "$pid_file")
            echo -e "${GREEN}✓${NC} $node_name - RUNNING (PID: $pid, SSH port: $ssh_port)"
        else
            echo -e "${RED}✗${NC} $node_name - STOPPED"
        fi
    done

    echo ""
}

# Show detailed info
show_info() {
    local target_node=$1
    check_vms

    echo "========================================="
    echo "Cluster OS VM Information"
    echo "========================================="
    echo ""

    for node_dir in "$VM_DIR"/node*; do
        if [ ! -d "$node_dir" ]; then
            continue
        fi

        node_name=$(basename "$node_dir")
        node_num=${node_name#node}

        # Skip if target node specified and this isn't it
        if [ -n "$target_node" ] && [ "$node_num" != "$target_node" ]; then
            continue
        fi

        pid_file="$node_dir/qemu.pid"
        ssh_port=$((BASE_PORT + node_num))
        vnc_port=$((5900 + node_num - 1))

        echo "Node $node_num ($node_name)"
        echo "  Status: $([ -f "$pid_file" ] && kill -0 $(cat "$pid_file") 2>/dev/null && echo "RUNNING" || echo "STOPPED")"
        [ -f "$pid_file" ] && echo "  PID: $(cat "$pid_file")"
        echo "  SSH: ssh -p $ssh_port clusteros@localhost"
        echo "  VNC: vnc://localhost:$vnc_port"
        echo "  Disk: $node_dir/disk.qcow2"
        echo "  Logs: $node_dir/serial.log"

        # Try to get node-agent status if VM is running
        if [ -f "$pid_file" ] && kill -0 $(cat "$pid_file") 2>/dev/null; then
            echo -n "  Node-Agent: "
            if timeout 2 ssh -p "$ssh_port" -o StrictHostKeyChecking=no -o ConnectTimeout=2 \
                clusteros@localhost "sudo systemctl is-active node-agent" 2>/dev/null | grep -q "active"; then
                echo "ACTIVE"
            else
                echo "UNKNOWN (SSH timeout or not ready)"
            fi
        fi

        echo ""
    done
}

# SSH to a node
ssh_to_node() {
    local node_num=$1

    if [ -z "$node_num" ]; then
        log_error "Node number required. Usage: $0 shell <node>"
        exit 1
    fi

    check_vms

    local ssh_port=$((BASE_PORT + node_num))
    local node_dir="$VM_DIR/node$node_num"

    if [ ! -d "$node_dir" ]; then
        log_error "Node $node_num not found"
        exit 1
    fi

    local pid_file="$node_dir/qemu.pid"
    if [ ! -f "$pid_file" ] || ! kill -0 $(cat "$pid_file") 2>/dev/null; then
        log_error "Node $node_num is not running"
        exit 1
    fi

    log_info "Connecting to node $node_num..."
    ssh -p "$ssh_port" -o StrictHostKeyChecking=no clusteros@localhost
}

# Show logs for a node
show_logs() {
    local node_num=$1

    if [ -z "$node_num" ]; then
        log_error "Node number required. Usage: $0 logs <node>"
        exit 1
    fi

    check_vms

    local node_dir="$VM_DIR/node$node_num"
    local log_file="$node_dir/serial.log"

    if [ ! -f "$log_file" ]; then
        log_error "Log file not found: $log_file"
        exit 1
    fi

    tail -f "$log_file"
}

# Execute command on node
exec_on_node() {
    local node_num=$1
    shift
    local cmd="$*"

    if [ -z "$node_num" ] || [ -z "$cmd" ]; then
        log_error "Usage: $0 exec <node> <command>"
        exit 1
    fi

    check_vms

    local ssh_port=$((BASE_PORT + node_num))
    local node_dir="$VM_DIR/node$node_num"

    if [ ! -d "$node_dir" ]; then
        log_error "Node $node_num not found"
        exit 1
    fi

    ssh -p "$ssh_port" -o StrictHostKeyChecking=no clusteros@localhost "$cmd"
}

# Stop cluster
stop_cluster() {
    "$SCRIPT_DIR/stop-cluster.sh"
}

# Clean cluster
clean_cluster() {
    check_vms

    log_warn "This will stop all VMs and delete all VM data!"
    read -p "Are you sure? (yes/no): " confirm

    if [ "$confirm" != "yes" ]; then
        log_info "Cancelled"
        exit 0
    fi

    # Stop VMs first
    stop_cluster

    # Remove VM directory
    log_info "Removing VM data..."
    rm -rf "$VM_DIR"

    log_info "Cluster cleaned"
}

# Main
case "${1:-}" in
    status)
        show_status
        ;;
    info)
        show_info "${2:-}"
        ;;
    shell)
        ssh_to_node "$2"
        ;;
    logs)
        show_logs "$2"
        ;;
    exec)
        shift
        exec_on_node "$@"
        ;;
    stop)
        stop_cluster
        ;;
    clean)
        clean_cluster
        ;;
    help|--help|-h)
        usage
        ;;
    *)
        log_error "Unknown command: ${1:-}"
        echo ""
        usage
        exit 1
        ;;
esac
