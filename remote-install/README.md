# ClusterOS Remote Node Installer

This directory contains the remote node installer script for ClusterOS. Use this script to install all ClusterOS services on a remote Ubuntu node that you cannot access directly.

## Prerequisites

- Ubuntu 24.04 LTS (or compatible)
- Internet connection
- SSH access to the target node
- Tailscale OAuth credentials (recommended) or auth key

## Quick Start

1. Copy the installer script to the target node:
   ```bash
   scp remote-node-installer.sh user@remote-node:~
   ```

2. Run the installer with your credentials:
   ```bash
   ssh user@remote-node
   chmod +x remote-node-installer.sh
   ./remote-node-installer.sh \
     --tailscale-oauth-id "your-oauth-client-id" \
     --tailscale-oauth-secret "your-oauth-client-secret" \
     --cluster-key "8kTPYsVncC5JaHEUsfIhqFtg5fSHbWTae+salJEBtuU="
   ```

## Configuration Options

The installer accepts the following options:

- `--tailscale-oauth-id` - Tailscale OAuth Client ID (recommended)
- `--tailscale-oauth-secret` - Tailscale OAuth Client Secret
- `--tailscale-authkey` - Tailscale Auth Key (fallback if OAuth not available)
- `--cluster-key` - **REQUIRED**: Cluster encryption key (get from `cluster.key` in repo root)
- `--wifi-ssid` - WiFi network SSID (optional)
- `--wifi-key` - WiFi network password (optional)

You can also set these as environment variables instead of command line arguments.

## What Gets Installed

The installer sets up:

- **Tailscale** - Mesh networking with OAuth authentication
- **K3s** - Lightweight Kubernetes (disabled by default)
- **SLURM** - HPC workload manager with Munge authentication (disabled by default)
- **Node Agent** - ClusterOS management service
- **Network Configuration** - Netplan with WiFi support
- **Cluster Configuration** - Node identity and discovery settings (with Tailscale API discovery)

## Post-Installation

After installation:

1. Connect to Tailscale:
   ```bash
   sudo tailscale up
   ```

2. Check node status:
   ```bash
   sudo node-agent status
   ```

3. Monitor logs:
   ```bash
   journalctl -u node-agent -f
   ```

## Security Notes

- The script requires sudo access for installation
- Credentials are stored securely in `/etc/cluster-os/`
- Services are configured but not automatically started
- Review the generated `/etc/cluster-os/node.yaml` configuration

## Troubleshooting

- If Tailscale OAuth fails, use `--tailscale-authkey` instead
- Check logs with `journalctl -u node-agent` for issues
- Ensure the node-agent binary is available (currently downloads from GitHub releases)
- For WiFi issues, verify SSID and password

## Building the Node Agent

If the node-agent binary download fails, you'll need to build it manually:

```bash
# On a build machine with Go
cd /path/to/cluster-os/node
go build -o node-agent ./cmd/node-agent
# Then copy to /usr/local/bin/node-agent on the target node
```