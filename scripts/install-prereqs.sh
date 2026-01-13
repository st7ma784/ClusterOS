#!/bin/bash
set -e

# Cluster OS Prerequisites Installation Script
# Installs all required tools for building and testing

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

echo "========================================="
echo "Cluster OS Prerequisites Installer"
echo "========================================="
echo ""

# Check if running as root
if [ "$EUID" -eq 0 ]; then
    log_error "Do not run this script as root!"
    log_error "It will use sudo when needed."
    exit 1
fi

# Update package lists
log_info "Updating package lists..."
sudo apt-get update

# Install QEMU and virtualization tools
log_info "Installing QEMU and virtualization tools..."
sudo apt-get install -y \
    qemu-system-x86 \
    qemu-utils \
    qemu-kvm \
    libvirt-daemon-system \
    cloud-image-utils \
    genisoimage \
    wget \
    curl \
    gnupg

log_info "QEMU and tools installed"

# Install Packer
log_info "Installing Packer..."

# Check if already installed
if command -v packer &> /dev/null; then
    log_info "Packer already installed: $(packer --version)"
else
    # Add HashiCorp GPG key
    wget -O- https://apt.releases.hashicorp.com/gpg | \
        sudo gpg --dearmor -o /usr/share/keyrings/hashicorp-archive-keyring.gpg

    # Add repository
    echo "deb [signed-by=/usr/share/keyrings/hashicorp-archive-keyring.gpg] https://apt.releases.hashicorp.com focal main" | \
        sudo tee /etc/apt/sources.list.d/hashicorp.list

    # Install
    sudo apt-get update
    sudo apt-get install -y packer

    log_info "Packer installed: $(packer --version)"
fi

# Configure user groups
log_info "Configuring user groups..."
sudo usermod -aG kvm $USER
sudo usermod -aG libvirt $USER

log_info "User $USER added to kvm and libvirt groups"

# Check Go installation
log_info "Checking Go installation..."
if command -v go &> /dev/null; then
    log_info "Go already installed: $(go version)"
else
    log_warn "Go not found. Installing Go 1.22.0..."

    # Download Go
    GO_VERSION="1.22.0"
    wget "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"

    # Remove old installation
    sudo rm -rf /usr/local/go

    # Extract
    sudo tar -C /usr/local -xzf "go${GO_VERSION}.linux-amd64.tar.gz"
    rm "go${GO_VERSION}.linux-amd64.tar.gz"

    # Add to PATH if not already there
    if ! grep -q '/usr/local/go/bin' ~/.bashrc; then
        echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
        echo 'export PATH=$PATH:$HOME/go/bin' >> ~/.bashrc
    fi

    export PATH=$PATH:/usr/local/go/bin

    log_info "Go installed: $(go version)"
fi

echo ""
echo "========================================="
echo "Installation Complete!"
echo "========================================="
echo ""
echo "IMPORTANT: You must log out and back in for group changes to take effect!"
echo ""
echo "After logging back in, verify installation:"
echo "  ./scripts/verify-prereqs.sh"
echo ""
echo "Then proceed with:"
echo "  make node     # Build node-agent"
echo "  make image    # Build OS image"
echo "  make test-vm  # Test with QEMU VMs"
echo ""
