#!/bin/bash
# Generate a unique cluster authentication key
# This key ensures that only nodes with the correct secret can join your cluster
#
# IMPORTANT: Run this script when forking the repo to create your own unique cluster!

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
KEY_FILE="$REPO_ROOT/cluster.key"

echo "=== Cluster-OS Cluster Key Generator ==="
echo ""
echo "This script generates a unique 32-byte cluster authentication key."
echo "Only nodes with this key can join your cluster network."
echo ""

# Check if key already exists
if [ -f "$KEY_FILE" ]; then
    echo "WARNING: cluster.key already exists!"
    echo "Regenerating this key will prevent existing nodes from joining."
    echo ""
    read -p "Do you want to overwrite the existing key? (yes/no): " confirm
    if [ "$confirm" != "yes" ]; then
        echo "Aborted."
        exit 1
    fi
    echo ""
fi

# Generate a cryptographically secure 32-byte key
echo "Generating new cluster key..."
KEY=$(openssl rand -base64 32)

# Write key to file
echo "$KEY" > "$KEY_FILE"
chmod 600 "$KEY_FILE"

echo "✓ Cluster key generated and saved to: $KEY_FILE"
echo ""
echo "Key (base64):"
echo "$KEY"
echo ""
echo "IMPORTANT SECURITY NOTES:"
echo "  1. Keep this key SECRET - anyone with it can join your cluster"
echo "  2. Add cluster.key to .gitignore if you don't want to commit it"
echo "  3. Or commit it if you want all repo users to join the same cluster"
echo "  4. When forking this repo, regenerate this key to create a separate cluster"
echo ""
echo "Next steps:"
echo "  1. Copy this key to your node configuration files (node/config/node.yaml)"
echo "  2. Or set the CLUSTEROS_CLUSTER_AUTH_KEY environment variable"
echo "  3. All nodes in your cluster must use the same key"
echo ""
