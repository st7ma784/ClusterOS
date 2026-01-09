# Security

## Cluster Authentication

**IMPORTANT**: When you fork or clone this repository, you MUST regenerate the cluster authentication key to create your own isolated cluster.

### Quick Start

```bash
# Generate a new cluster key for your fork
./scripts/generate-cluster-key.sh

# Update the configuration
# Edit node/config/node.yaml and set cluster.auth_key to the new key
# Or set via environment variable:
export CLUSTEROS_CLUSTER_AUTH_KEY="<your-new-key>"
```

### Why This Matters

The cluster authentication key ensures that only nodes with the correct secret can join your cluster. Without regenerating this key:

- **Your nodes might join someone else's cluster** if they're using the same default key
- **Unauthorized nodes might join your cluster** if they have access to your repository

### Default Key (DO NOT USE IN PRODUCTION)

This repository includes a default cluster key for testing:

```
7RB0TPs+d/VuD3rL/7ZD2JEcpA14aNCBvLOPHwEBy9s=
```

**This key is PUBLIC and should ONLY be used for:**
- Local development
- Testing
- Learning the system

**Never use the default key for:**
- Production deployments
- Any system exposed to the internet
- Any cluster containing sensitive data

### Security Layers

Cluster-OS implements multiple security layers:

1. **Cluster Authentication** (this document)
   - Prevents unauthorized nodes from joining
   - Uses HMAC-SHA256 challenge-response
   - Required for all deployments

2. **Serf Encryption** (optional but recommended)
   - Encrypts gossip protocol traffic
   - Configure via `discovery.encrypt_key`
   - Protects membership information

3. **WireGuard VPN** (automatic)
   - Encrypts all data plane traffic
   - Automatic mesh network
   - Each node has unique keypair

4. **Node Identity** (automatic)
   - Ed25519 cryptographic identity
   - Unique per node
   - Used for authentication and encryption

### Documentation

For detailed information, see:
- [Cluster Authentication Guide](docs/cluster-authentication.md)
- [Architecture Documentation](docs/architecture.md)

### Reporting Security Issues

If you discover a security vulnerability, please email: security@cluster-os.dev

Do NOT open a public GitHub issue for security vulnerabilities.
