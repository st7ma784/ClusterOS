#!/bin/bash
# Generate a unique cluster authentication key derived from this git repo
#
# The key is deterministic: HMAC-SHA256(repo_remote_url, HEAD_commit_hash)
# This means:
#   - Every fork gets its own unique cluster key (different remote URL)
#   - Rebuilding the same commit produces the same key (reproducible)
#   - Nodes from different forks can never accidentally join each other
#   - You can force a new key by passing --random
#
# Usage:
#   scripts/generate-cluster-key.sh          # derive from git repo
#   scripts/generate-cluster-key.sh --random # fully random key

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
KEY_FILE="$REPO_ROOT/cluster.key"

echo "=== ClusterOS Cluster Key Generator ==="
echo ""

# Check if key already exists
if [ -f "$KEY_FILE" ]; then
    echo "WARNING: cluster.key already exists!"
    echo "Regenerating this key will prevent existing nodes from joining"
    echo "unless they are also updated."
    echo ""
    read -p "Overwrite the existing key? (yes/no): " confirm
    if [ "$confirm" != "yes" ]; then
        echo "Aborted."
        exit 1
    fi
    echo ""
fi

if [ "$1" = "--random" ]; then
    echo "Generating fully random cluster key..."
    KEY=$(openssl rand -base64 32)
    echo "Mode: random"
else
    echo "Deriving cluster key from git repo identity..."

    # Get the git remote URL (origin) — unique per fork
    REMOTE_URL=$(git -C "$REPO_ROOT" remote get-url origin 2>/dev/null || echo "local-repo-no-remote")

    # Get the HEAD commit hash — ties key to a specific version
    COMMIT_HASH=$(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || echo "no-git")

    echo "  Remote:  $REMOTE_URL"
    echo "  Commit:  $COMMIT_HASH"

    # HMAC-SHA256(remote_url, commit_hash) → base64
    # The remote URL is the key, commit hash is the message
    # Different forks → different remote URLs → different keys
    KEY=$(echo -n "$COMMIT_HASH" | openssl dgst -sha256 -hmac "$REMOTE_URL" -binary | base64)

    echo "  Mode:    git-derived (deterministic)"
fi

# Write key to file
echo "$KEY" > "$KEY_FILE"
chmod 600 "$KEY_FILE"

echo ""
echo "✓ Cluster key saved to: $KEY_FILE"
echo ""
echo "Key (base64): $KEY"
echo ""
echo "SECURITY NOTES:"
echo "  - cluster.key is gitignored — it won't be committed"
echo "  - Anyone who forks this repo will derive a DIFFERENT key"
echo "  - All nodes in YOUR cluster must share the same key"
echo "  - The key is injected into images automatically at build time"
echo ""
