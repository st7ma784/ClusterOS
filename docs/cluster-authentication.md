# Cluster Authentication

## Overview

Cluster-OS uses a **cluster authentication key** to ensure that only authorized nodes can join your cluster network. This prevents accidental or unauthorized nodes from joining your cluster when you fork or clone the repository.

## How It Works

### Authentication Mechanism

1. **Cluster Key**: Each cluster has a unique 32-byte authentication key (base64-encoded)
2. **Join Token**: When a node starts, it generates a join token by:
   - Creating a challenge with its node ID and timestamp
   - Signing the challenge using HMAC-SHA256 with the cluster key
   - Encoding the challenge and signature as a base64 token
3. **Validation**: When a node attempts to join:
   - The node presents its join token via Serf tags
   - Existing nodes verify the token signature using their cluster key
   - Only nodes with matching keys can successfully join
4. **Time-based Expiry**: Tokens are valid for 5 minutes to prevent replay attacks

### Security Properties

- **Shared Secret**: All nodes in a cluster must have the same cluster key
- **HMAC-SHA256**: Provides cryptographic proof of key possession
- **No Key Transmission**: The key itself is never transmitted over the network
- **Fork Isolation**: Each fork with a different key forms a separate cluster

## Usage

### Initial Setup

The repository includes a default cluster key in:
- `cluster.key` - The key file (base64-encoded)
- `node/config/node.yaml` - Default configuration with the key

**Default key**: `7RB0TPs+d/VuD3rL/7ZD2JEcpA14aNCBvLOPHwEBy9s=`

### Creating Your Own Cluster (Fork Scenario)

When you fork or clone this repository to create your own cluster, you **MUST** regenerate the cluster key:

```bash
# Generate a new cluster key
./scripts/generate-cluster-key.sh

# The script will:
# 1. Generate a new 32-byte random key
# 2. Save it to cluster.key
# 3. Display the key for you to copy
```

Then update your configuration:

**Option 1: Update node/config/node.yaml**
```yaml
cluster:
  auth_key: "YOUR-NEW-KEY-HERE"
```

**Option 2: Environment Variable**
```bash
export CLUSTEROS_CLUSTER_AUTH_KEY="YOUR-NEW-KEY-HERE"
```

### Distributing the Key

You have two options for distributing the cluster key to your nodes:

#### Option 1: Commit the Key (Recommended for private repos)
```bash
# The cluster.key file can be committed to your repo
git add cluster.key node/config/node.yaml
git commit -m "Add cluster authentication key"
```

**Pros:**
- Automatic key distribution to all nodes
- Simplifies deployment

**Cons:**
- Anyone with repo access can join the cluster

#### Option 2: Keep Key Secret (Recommended for public repos)
```bash
# Add cluster.key to .gitignore
echo "cluster.key" >> .gitignore
git add .gitignore
git commit -m "Ignore cluster key file"

# Distribute key via secure channel (e.g., secrets management)
```

**Pros:**
- More secure for public repositories
- Controlled access to cluster

**Cons:**
- Requires manual key distribution
- Must configure each node separately

## Configuration

### YAML Configuration

Edit `node/config/node.yaml`:

```yaml
cluster:
  name: my-cluster
  auth_key: "YOUR-CLUSTER-KEY-HERE"
```

### Environment Variable

Override the YAML configuration:

```bash
export CLUSTEROS_CLUSTER_AUTH_KEY="YOUR-CLUSTER-KEY-HERE"
```

### Docker Deployment

Pass the key as an environment variable:

```bash
docker run -e CLUSTEROS_CLUSTER_AUTH_KEY="YOUR-KEY" cluster-os/node
```

Or mount a config file:

```bash
docker run -v /path/to/node.yaml:/etc/cluster-os/node.yaml cluster-os/node
```

## Validation

The cluster key is validated on node startup:

- **Format**: Must be valid base64 encoding
- **Length**: Decoded key must be at least 32 bytes
- **Required**: Node will fail to start without a valid key

## Troubleshooting

### Node fails to start with "cluster.auth_key must be set"

**Solution**: Set the cluster auth key in your configuration or environment variable.

```bash
# Check your configuration
cat node/config/node.yaml | grep auth_key

# Or set via environment
export CLUSTEROS_CLUSTER_AUTH_KEY="$(cat cluster.key)"
```

### Node is rejected with "failed authentication"

**Cause**: The node has a different cluster key than the existing nodes.

**Solution**: Ensure all nodes use the same cluster key.

```bash
# On the rejected node, verify the key
echo $CLUSTEROS_CLUSTER_AUTH_KEY

# On an existing node, compare keys
# They must match exactly
```

### Node is rejected with "challenge expired"

**Cause**: System clocks are out of sync (>5 minutes difference).

**Solution**: Synchronize system clocks using NTP.

```bash
# Install NTP
sudo apt-get install ntp

# Sync time
sudo ntpdate pool.ntp.org
```

## Security Best Practices

1. **Regenerate on Fork**: Always generate a new key when forking the repository
2. **Rotate Regularly**: Consider rotating the cluster key periodically
3. **Secure Storage**: Store keys in secrets management systems (e.g., Vault, AWS Secrets Manager)
4. **Access Control**: Limit who can access the cluster key
5. **Audit**: Monitor authentication failures in logs
6. **Transport Security**: Use Serf encryption key in addition to cluster auth key

## Key Rotation

To rotate the cluster key:

1. **Plan Downtime**: Key rotation requires restarting all nodes
2. **Generate New Key**: Run `./scripts/generate-cluster-key.sh`
3. **Update Configuration**: Update all node configurations with the new key
4. **Rolling Restart**: Restart all nodes (they won't join each other during rotation)
5. **Coordination**: All nodes must be updated to the new key simultaneously

**Note**: There is currently no graceful key rotation. All nodes must be updated and restarted.

## Technical Details

### Authentication Flow

```
Node A (joining)                    Node B (existing)
     |                                     |
     | 1. Generate challenge               |
     |    - NodeID: A                      |
     |    - Timestamp: now                 |
     |    - Nonce: random                  |
     |                                     |
     | 2. Sign challenge                   |
     |    - HMAC-SHA256(challenge, key)    |
     |                                     |
     | 3. Create join token                |
     |    - Base64(challenge + signature)  |
     |                                     |
     | 4. Join via Serf                    |
     |    - Tag: auth_token=<token>        |
     |------------------------------------>|
     |                                     |
     |                         5. Verify token
     |                            - Decode token
     |                            - Check timestamp
     |                            - Verify HMAC
     |                                     |
     |                         6. Accept/Reject
     |<------------------------------------|
```

### Token Format

```json
{
  "challenge": {
    "nonce": "base64-encoded-random-32-bytes",
    "timestamp": "2026-01-09T12:34:56Z",
    "node_id": "base58-encoded-ed25519-pubkey"
  },
  "signature": "base64-encoded-hmac-sha256"
}
```

## Related Security Features

- **Serf Encryption**: Encrypts gossip protocol traffic (configure via `discovery.encrypt_key`)
- **WireGuard**: Encrypts all node-to-node data traffic
- **Node Identity**: Ed25519 keypairs provide cryptographic node identity

## References

- [HMAC-SHA256 Specification (RFC 2104)](https://tools.ietf.org/html/rfc2104)
- [Serf Security](https://www.serf.io/docs/internals/security.html)
- [WireGuard Protocol](https://www.wireguard.com/protocol/)
