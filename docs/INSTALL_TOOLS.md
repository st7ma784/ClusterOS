# Installing Required Tools

This guide covers installing all tools needed to build and test Cluster OS.

## Quick Install (Ubuntu/Debian)

```bash
#!/bin/bash
# One-command install of all dependencies

# Update package lists
sudo apt-get update

# Install QEMU and virtualization tools
sudo apt-get install -y \
    qemu-system-x86 \
    qemu-utils \
    qemu-kvm \
    libvirt-daemon-system \
    cloud-image-utils \
    genisoimage

# Install Packer
wget -O- https://apt.releases.hashicorp.com/gpg | \
    sudo gpg --dearmor -o /usr/share/keyrings/hashicorp-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/hashicorp-archive-keyring.gpg] https://apt.releases.hashicorp.com focal main" | \
    sudo tee /etc/apt/sources.list.d/hashicorp.list
sudo apt-get update
sudo apt-get install -y packer

# Enable KVM access
sudo usermod -aG kvm $USER
sudo usermod -aG libvirt $USER

# Install Go (if not already installed)
if ! command -v go &> /dev/null; then
    wget https://go.dev/dl/go1.22.0.linux-amd64.tar.gz
    sudo tar -C /usr/local -xzf go1.22.0.linux-amd64.tar.gz
    echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
    export PATH=$PATH:/usr/local/go/bin
fi

echo "Installation complete!"
echo "IMPORTANT: Log out and back in for group changes to take effect"
echo ""
echo "Verify installation:"
echo "  packer --version"
echo "  qemu-system-x86_64 --version"
echo "  go version"
```

## Individual Tool Installation

### QEMU

QEMU is the virtualization platform for testing.

```bash
# Install QEMU
sudo apt-get install -y qemu-system-x86 qemu-utils

# Install KVM for acceleration (recommended)
sudo apt-get install -y qemu-kvm libvirt-daemon-system

# Add user to kvm group
sudo usermod -aG kvm $USER
sudo usermod -aG libvirt $USER

# Verify
qemu-system-x86_64 --version
ls -la /dev/kvm
```

**Note**: Log out and back in after adding yourself to groups.

### Packer

Packer builds the OS images.

#### Ubuntu/Debian

```bash
# Add HashiCorp GPG key
wget -O- https://apt.releases.hashicorp.com/gpg | \
    sudo gpg --dearmor -o /usr/share/keyrings/hashicorp-archive-keyring.gpg

# Add repository
echo "deb [signed-by=/usr/share/keyrings/hashicorp-archive-keyring.gpg] https://apt.releases.hashicorp.com focal main" | \
    sudo tee /etc/apt/sources.list.d/hashicorp.list

# Install
sudo apt-get update
sudo apt-get install -y packer

# Verify
packer --version
```

#### Manual Installation

```bash
# Download latest version
PACKER_VERSION="1.10.0"
wget https://releases.hashicorp.com/packer/${PACKER_VERSION}/packer_${PACKER_VERSION}_linux_amd64.zip

# Extract
unzip packer_${PACKER_VERSION}_linux_amd64.zip

# Install
sudo mv packer /usr/local/bin/

# Verify
packer --version
```

### Go

Go is required to build the node-agent.

```bash
# Download Go 1.22
wget https://go.dev/dl/go1.22.0.linux-amd64.tar.gz

# Remove old installation (if exists)
sudo rm -rf /usr/local/go

# Extract
sudo tar -C /usr/local -xzf go1.22.0.linux-amd64.tar.gz

# Add to PATH
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
export PATH=$PATH:/usr/local/go/bin

# Verify
go version
```

### Cloud Image Tools

Tools for creating cloud-init ISOs and handling disk images.

```bash
# Install cloud-image-utils
sudo apt-get install -y cloud-image-utils

# Install genisoimage (for creating ISOs)
sudo apt-get install -y genisoimage

# Verify
cloud-localds --help
genisoimage --help
```

### Optional: VNC Viewer

For graphical VM access during debugging.

```bash
# Install TigerVNC
sudo apt-get install -y tigervnc-viewer

# Or RealVNC
sudo apt-get install -y realvnc-vnc-viewer

# Use
vncviewer localhost:5900
```

## Platform-Specific Instructions

### macOS

```bash
# Install Homebrew (if not already installed)
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"

# Install QEMU
brew install qemu

# Install Packer
brew install packer

# Install Go
brew install go

# Verify
qemu-system-x86_64 --version
packer --version
go version
```

**Note**: macOS doesn't have native KVM support. Consider using:
- UTM (GUI for QEMU on macOS)
- VirtualBox with Vagrant
- Docker Desktop for basic testing

### Windows (WSL2)

Install WSL2 first, then follow Ubuntu instructions:

```powershell
# Install WSL2
wsl --install -d Ubuntu-24.04

# Enter WSL
wsl

# Follow Ubuntu installation steps above
```

### Arch Linux

```bash
# Install QEMU
sudo pacman -S qemu-full libvirt

# Install Packer
sudo pacman -S packer

# Install Go
sudo pacman -S go

# Enable KVM
sudo usermod -aG kvm $USER
sudo systemctl enable --now libvirtd

# Verify
packer --version
qemu-system-x86_64 --version
go version
```

### Fedora/RHEL/CentOS

```bash
# Install QEMU
sudo dnf install -y qemu-kvm qemu-img libvirt

# Add HashiCorp repository
sudo dnf install -y dnf-plugins-core
sudo dnf config-manager --add-repo https://rpm.releases.hashicorp.com/fedora/hashicorp.repo

# Install Packer
sudo dnf install -y packer

# Install Go
sudo dnf install -y golang

# Enable KVM
sudo usermod -aG kvm $USER
sudo systemctl enable --now libvirtd

# Verify
packer --version
qemu-system-x86_64 --version
go version
```

## Verifying Installation

Run this script to verify all tools are installed:

```bash
#!/bin/bash

echo "Verifying Cluster OS build tools..."
echo ""

# Check Packer
if command -v packer &> /dev/null; then
    echo "✓ Packer: $(packer --version)"
else
    echo "✗ Packer: NOT FOUND"
fi

# Check QEMU
if command -v qemu-system-x86_64 &> /dev/null; then
    echo "✓ QEMU: $(qemu-system-x86_64 --version | head -n1)"
else
    echo "✗ QEMU: NOT FOUND"
fi

# Check Go
if command -v go &> /dev/null; then
    echo "✓ Go: $(go version)"
else
    echo "✗ Go: NOT FOUND"
fi

# Check KVM
if [ -r /dev/kvm ]; then
    echo "✓ KVM: AVAILABLE"
else
    echo "⚠ KVM: NOT AVAILABLE (VMs will be slower)"
fi

# Check cloud-localds
if command -v cloud-localds &> /dev/null; then
    echo "✓ cloud-image-utils: INSTALLED"
else
    echo "✗ cloud-image-utils: NOT FOUND"
fi

# Check genisoimage
if command -v genisoimage &> /dev/null; then
    echo "✓ genisoimage: INSTALLED"
else
    echo "✗ genisoimage: NOT FOUND"
fi

echo ""
echo "Group memberships:"
groups | grep -q kvm && echo "✓ kvm group" || echo "✗ kvm group (run: sudo usermod -aG kvm \$USER)"
groups | grep -q libvirt && echo "✓ libvirt group" || echo "✗ libvirt group (optional)"

echo ""
echo "Ready to build Cluster OS!"
```

Save as `verify-tools.sh`, make executable, and run:

```bash
chmod +x verify-tools.sh
./verify-tools.sh
```

## Disk Space Requirements

Ensure adequate disk space:

| Component | Size |
|-----------|------|
| Packer cache | ~2 GB |
| Base image | ~5 GB |
| QEMU VMs (3 nodes) | ~15 GB |
| Build artifacts | ~3 GB |
| **Total recommended** | **30 GB** |

Check available space:

```bash
df -h .
```

## Network Requirements

Packer will download:
- Ubuntu 24.04 ISO (~2.5 GB)
- Package updates (~500 MB)
- Total download: ~3 GB

Ensure:
- Stable internet connection
- No restrictive firewall blocking downloads
- Sufficient bandwidth

## Troubleshooting

### "Permission denied" on /dev/kvm

```bash
# Add yourself to kvm group
sudo usermod -aG kvm $USER

# Log out and back in

# Verify
ls -la /dev/kvm
groups | grep kvm
```

### Packer not found after installation

```bash
# Reload PATH
source ~/.bashrc

# Or add manually
export PATH=$PATH:/usr/local/bin

# Verify
which packer
```

### QEMU acceleration not working

```bash
# Check CPU virtualization support
egrep -c '(vmx|svm)' /proc/cpuinfo  # Should be > 0

# Check KVM module
lsmod | grep kvm

# Load KVM module
sudo modprobe kvm_intel  # For Intel CPUs
# OR
sudo modprobe kvm_amd    # For AMD CPUs
```

### Go not in PATH

```bash
# Add to .bashrc
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
echo 'export PATH=$PATH:$HOME/go/bin' >> ~/.bashrc

# Reload
source ~/.bashrc

# Verify
go version
```

## Next Steps

After installing tools:

1. Build the OS image: `make image`
2. Test with QEMU VMs: `make test-vm`
3. Read the [Quick Start Guide](../PACKER_QEMU_QUICKSTART.md)

## Support

If you encounter issues:

1. Check [Troubleshooting](#troubleshooting) section above
2. Review [PACKER_QEMU_QUICKSTART.md](../PACKER_QEMU_QUICKSTART.md)
3. Open an issue on GitHub with:
   - Output of `verify-tools.sh`
   - Your OS version: `lsb_release -a`
   - Error messages
