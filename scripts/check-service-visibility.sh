#!/bin/bash
# check-service-visibility.sh
# Verifies that all cluster services are reachable on the LAN/Tailscale IP of
# every registered K3s node.
#
# Usage: bash scripts/check-service-visibility.sh [--tailscale]
#   --tailscale  use Tailscale IPs (100.x.x.x) instead of InternalIP

set -euo pipefail

USE_TAILSCALE=0
[[ "${1:-}" == "--tailscale" ]] && USE_TAILSCALE=1

PASS=0
FAIL=0

check() {
  local name="$1" url="$2"
  local code
  code=$(curl -sfk -o /dev/null -w "%{http_code}" --connect-timeout 5 "$url" 2>/dev/null || true)
  if echo "$code" | grep -qE '^(200|301|302|401)'; then
    printf "  %-35s \033[32mOK\033[0m (%s)\n" "$name" "$code"
    (( PASS++ )) || true
  else
    printf "  %-35s \033[31mFAIL\033[0m (got: %s)\n" "$name" "${code:-timeout}"
    (( FAIL++ )) || true
  fi
}

if ! command -v k3s &>/dev/null; then
  echo "ERROR: k3s not found — run this on a K3s server node"
  exit 1
fi

if [[ $USE_TAILSCALE -eq 1 ]]; then
  # Get Tailscale IPs from Serf tags or tailscale status
  NODES=$(sudo k3s kubectl get nodes \
    -o jsonpath='{range .items[*]}{.metadata.annotations.tailscale\.com/ips}{"\n"}{end}' 2>/dev/null \
    || tailscale status --json 2>/dev/null | python3 -c "
import json,sys
data=json.load(sys.stdin)
for peer in data.get('Peer',{}).values():
  if peer.get('Online'):
    print(peer['TailscaleIPs'][0])
" 2>/dev/null || true)
  IP_TYPE="Tailscale"
else
  NODES=$(sudo k3s kubectl get nodes \
    -o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}')
  IP_TYPE="LAN/InternalIP"
fi

if [[ -z "$NODES" ]]; then
  echo "ERROR: No nodes found. Is k3s running and kubeconfig accessible?"
  exit 1
fi

echo "=== ClusterOS Service Visibility Check (${IP_TYPE}) ==="
echo "Nodes: $(echo "$NODES" | tr '\n' ' ')"
echo ""

for NODE_IP in $NODES; do
  echo "--- Node: $NODE_IP ---"
  check "Landing page"       "http://${NODE_IP}/"
  check "Longhorn UI"        "http://${NODE_IP}/longhorn/"
  check "SLURM REST API"     "http://${NODE_IP}/slurm"
  check "Rancher redirect"   "http://${NODE_IP}/rancher"
  check "Rancher HTTPS"      "https://${NODE_IP}:30444/"
  check "K3s API health"     "https://${NODE_IP}:6443/healthz"
  echo ""
done

echo "=== Result: ${PASS} passed, ${FAIL} failed ==="
if [[ $FAIL -gt 0 ]]; then
  echo ""
  echo "Troubleshooting:"
  echo "  - Is nginx-ingress DaemonSet running on the failing node?"
  echo "    sudo k3s kubectl -n ingress-nginx get pods -o wide"
  echo "  - Is port 80 bound on the node?"
  echo "    ssh clusteros@NODE_IP 'ss -tlnp | grep :80'"
  echo "  - Are INPUT firewall rules present?"
  echo "    ssh clusteros@NODE_IP 'iptables -L INPUT -n | grep -E dpt:80'"
  echo "  - Restart node-agent to rebuild firewall rules:"
  echo "    ssh clusteros@NODE_IP 'sudo systemctl restart node-agent'"
  exit 1
fi
