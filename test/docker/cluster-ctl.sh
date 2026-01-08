#!/bin/bash
# Cluster control script for managing the Docker test cluster

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_FILE="$SCRIPT_DIR/docker-compose.yaml"

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

# Start the cluster
start_cluster() {
    log_info "Starting Cluster-OS test cluster..."
    cd "$SCRIPT_DIR"
    ./start-cluster-direct.sh
    log_info "Cluster is ready!"
    show_status
}

# Stop the cluster
stop_cluster() {
    log_info "Stopping Cluster-OS test cluster..."
    cd "$SCRIPT_DIR"
    ./stop-cluster.sh
}

# Restart the cluster
restart_cluster() {
    log_info "Restarting Cluster-OS test cluster..."
    stop_cluster
    sleep 2
    start_cluster
}

# Clean up everything
clean_cluster() {
    log_warn "Cleaning up cluster (this will delete all data)..."
    cd "$SCRIPT_DIR"
    ./clean-cluster.sh
}

# Show cluster status
show_status() {
    log_info "Cluster Status:"
    echo ""
    docker ps --filter "name=cluster-os-" --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"
    echo ""
}

# Show logs for a node
show_logs() {
    node=${1:-node1}
    log_info "Showing logs for $node..."
    docker logs -f "cluster-os-$node"
}

# Show node-agent logs from journald
show_agent_logs() {
    node=${1:-node1}
    log_info "Showing node-agent service logs for $node..."
    docker exec "cluster-os-$node" journalctl -u node-agent.service -f
}

# Execute command on a node
exec_node() {
    node=$1
    shift
    log_info "Executing on $node: $*"
    docker exec -it "cluster-os-$node" "$@"
}

# Show node identity
show_identity() {
    node=${1:-node1}
    log_info "Identity for $node:"
    docker exec "cluster-os-$node" cat /var/lib/cluster-os/identity.json 2>/dev/null | python3 -m json.tool || echo "Identity file not found"
}

# Show node configuration
show_config() {
    node=${1:-node1}
    log_info "Configuration for $node:"
    docker exec "cluster-os-$node" cat /etc/cluster-os/node.yaml
}

# Run integration tests
run_tests() {
    log_info "Running integration tests..."
    bash "$SCRIPT_DIR/../integration/test_cluster.sh"
}

# Show cluster info
show_info() {
    echo "=========================================="
    echo "Cluster-OS Test Cluster Information"
    echo "=========================================="
    echo ""

    for node in node1 node2 node3 node4 node5; do
        echo -e "${BLUE}$node:${NC}"

        # Check if container is running
        if docker ps | grep -q "cluster-os-$node"; then
            echo "  Status: Running"

            # Get IP address
            ip=$(docker inspect -f '{{range.NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "cluster-os-$node" 2>/dev/null || echo "N/A")
            echo "  IP: $ip"

            # Get node ID if available
            node_id=$(docker exec "cluster-os-$node" cat /var/lib/cluster-os/identity.json 2>/dev/null | grep -o '"node_id":"[^"]*"' | cut -d'"' -f4 || echo "N/A")
            echo "  Node ID: ${node_id:0:32}..."
        else
            echo "  Status: Stopped"
        fi
        echo ""
    done
}

# Shell into a node
shell() {
    node=${1:-node1}
    log_info "Opening shell on $node..."
    docker exec -it "cluster-os-$node" /bin/bash
}

# Show help
show_help() {
    cat <<EOF
Cluster-OS Test Cluster Control Script

Usage: $0 <command> [arguments]

Commands:
  start               Start the test cluster
  stop                Stop the test cluster
  restart             Restart the test cluster
  clean               Stop and remove all data (volumes)
  status              Show cluster status
  logs <node>         Show container logs for a node (default: node1)
  agent-logs <node>   Show node-agent service logs (default: node1)
  exec <node> <cmd>   Execute command on a node
  identity <node>     Show node identity (default: node1)
  config <node>       Show node configuration (default: node1)
  info                Show detailed cluster information
  test                Run integration tests
  shell <node>        Open interactive shell on node (default: node1)
  help                Show this help message

Examples:
  $0 start            # Start the cluster
  $0 logs node2       # Show logs for node2
  $0 shell node3      # Open shell on node3
  $0 test             # Run integration tests
  $0 clean            # Clean up everything

EOF
}

# Main
case "${1:-help}" in
    start)
        start_cluster
        ;;
    stop)
        stop_cluster
        ;;
    restart)
        restart_cluster
        ;;
    clean)
        clean_cluster
        ;;
    status)
        show_status
        ;;
    logs)
        show_logs "${2:-node1}"
        ;;
    agent-logs)
        show_agent_logs "${2:-node1}"
        ;;
    exec)
        if [ -z "$2" ]; then
            log_error "Usage: $0 exec <node> <command>"
            exit 1
        fi
        exec_node "$2" "${@:3}"
        ;;
    identity)
        show_identity "${2:-node1}"
        ;;
    config)
        show_config "${2:-node1}"
        ;;
    info)
        show_info
        ;;
    test)
        run_tests
        ;;
    shell)
        shell "${2:-node1}"
        ;;
    help|*)
        show_help
        ;;
esac
