#!/bin/bash

# Cluster OS Prerequisites Verification Script
# Checks if all required tools are installed and configured

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

ERRORS=0
WARNINGS=0

log_ok() { echo -e "${GREEN}✓${NC} $1"; }
log_error() { echo -e "${RED}✗${NC} $1"; ERRORS=$((ERRORS + 1)); }
log_warn() { echo -e "${YELLOW}⚠${NC} $1"; WARNINGS=$((WARNINGS + 1)); }
log_info() { echo -e "${BLUE}ℹ${NC} $1"; }

echo "========================================="
echo "Cluster OS Prerequisites Verification"
echo "========================================="
echo ""

# Check Packer
echo "Checking Packer..."
if command -v packer &> /dev/null; then
    VERSION=$(packer --version)
    log_ok "Packer installed: $VERSION"
else
    log_error "Packer NOT installed"
    echo "  Install with:"
    echo "    wget -O- https://apt.releases.hashicorp.com/gpg | sudo gpg --dearmor -o /usr/share/keyrings/hashicorp-archive-keyring.gpg"
    echo "    echo 'deb [signed-by=/usr/share/keyrings/hashicorp-archive-keyring.gpg] https://apt.releases.hashicorp.com focal main' | sudo tee /etc/apt/sources.list.d/hashicorp.list"
    echo "    sudo apt update && sudo apt install packer"
fi
echo ""

# Check QEMU
echo "Checking QEMU..."
if command -v qemu-system-x86_64 &> /dev/null; then
    VERSION=$(qemu-system-x86_64 --version | head -n1)
    log_ok "QEMU installed: $VERSION"
else
    log_error "QEMU NOT installed"
    echo "  Install with: sudo apt-get install qemu-system-x86 qemu-utils"
fi
echo ""

# Check qemu-img
echo "Checking qemu-img..."
if command -v qemu-img &> /dev/null; then
    log_ok "qemu-img available"
else
    log_error "qemu-img NOT installed"
    echo "  Install with: sudo apt-get install qemu-utils"
fi
echo ""

# Check Go
echo "Checking Go..."
if command -v go &> /dev/null; then
    VERSION=$(go version)
    log_ok "Go installed: $VERSION"
else
    log_warn "Go NOT installed (needed to build node-agent)"
    echo "  Install with:"
    echo "    wget https://go.dev/dl/go1.22.0.linux-amd64.tar.gz"
    echo "    sudo tar -C /usr/local -xzf go1.22.0.linux-amd64.tar.gz"
    echo "    echo 'export PATH=\$PATH:/usr/local/go/bin' >> ~/.bashrc"
fi
echo ""

# Check KVM
echo "Checking KVM acceleration..."
if [ -e /dev/kvm ]; then
    if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
        log_ok "KVM available and accessible"
    else
        log_warn "KVM exists but not accessible"
        echo "  Fix with:"
        echo "    sudo usermod -aG kvm \$USER"
        echo "    (then log out and back in)"
    fi
else
    log_warn "KVM NOT available (VMs will be slower)"
    echo "  Check CPU virtualization:"
    echo "    egrep -c '(vmx|svm)' /proc/cpuinfo"
    echo "  Install KVM:"
    echo "    sudo apt-get install qemu-kvm"
fi
echo ""

# Check cloud-image-utils
echo "Checking cloud-image-utils..."
if command -v cloud-localds &> /dev/null; then
    log_ok "cloud-image-utils installed"
else
    log_error "cloud-image-utils NOT installed"
    echo "  Install with: sudo apt-get install cloud-image-utils"
fi
echo ""

# Check genisoimage
echo "Checking genisoimage..."
if command -v genisoimage &> /dev/null; then
    log_ok "genisoimage installed"
else
    log_warn "genisoimage NOT installed (needed for ISO creation)"
    echo "  Install with: sudo apt-get install genisoimage"
fi
echo ""

# Check disk space
echo "Checking disk space..."
AVAILABLE=$(df . | tail -1 | awk '{print $4}')
AVAILABLE_GB=$((AVAILABLE / 1024 / 1024))
if [ "$AVAILABLE_GB" -ge 30 ]; then
    log_ok "Disk space: ${AVAILABLE_GB}GB available (30GB+ recommended)"
elif [ "$AVAILABLE_GB" -ge 20 ]; then
    log_warn "Disk space: ${AVAILABLE_GB}GB available (30GB recommended)"
else
    log_error "Disk space: ${AVAILABLE_GB}GB available (30GB required)"
fi
echo ""

# Check CPU virtualization
echo "Checking CPU virtualization..."
if [ -f /proc/cpuinfo ]; then
    VMX_COUNT=$(egrep -c '(vmx|svm)' /proc/cpuinfo 2>/dev/null || echo 0)
    if [ "$VMX_COUNT" -gt 0 ]; then
        log_ok "CPU virtualization supported (${VMX_COUNT} cores)"
    else
        log_warn "CPU virtualization NOT detected (enable in BIOS)"
    fi
fi
echo ""

# Check group memberships
echo "Checking group memberships..."
if groups | grep -q kvm; then
    log_ok "User in 'kvm' group"
else
    log_warn "User NOT in 'kvm' group (KVM may not work)"
    echo "  Fix with:"
    echo "    sudo usermod -aG kvm \$USER"
    echo "    (then log out and back in)"
fi
echo ""

# Check if node-agent binary exists
echo "Checking node-agent binary..."
if [ -f "bin/node-agent" ]; then
    log_ok "node-agent binary exists"
else
    log_info "node-agent binary not built yet"
    echo "  Build with: make node"
fi
echo ""

# Summary
echo "========================================="
echo "Summary"
echo "========================================="

if [ $ERRORS -eq 0 ] && [ $WARNINGS -eq 0 ]; then
    log_ok "All prerequisites met! Ready to build."
    echo ""
    echo "Next steps:"
    echo "  1. Build node-agent: make node"
    echo "  2. Build OS image: make image"
    echo "  3. Test cluster: make test-vm"
elif [ $ERRORS -eq 0 ]; then
    log_warn "$WARNINGS warning(s) found"
    echo ""
    echo "You can proceed, but some features may not work optimally."
    echo "Review warnings above and install optional components."
else
    log_error "$ERRORS error(s) and $WARNINGS warning(s) found"
    echo ""
    echo "Please install missing required components before proceeding."
    echo "See installation commands above."
    exit 1
fi

echo ""
