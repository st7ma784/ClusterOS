.PHONY: all node test test-cluster image release clean fmt lint deps help

# Version info (from git tags or default)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S')

# Go configuration
GOPATH := $(shell $(HOME)/go/bin/go env GOPATH)
GOBIN := $(GOPATH)/bin
GO := $(HOME)/go/bin/go
GOFMT := $(HOME)/go/bin/gofmt

# Build configuration
BINARY_NAME := node-agent
BUILD_DIR := ./bin
NODE_DIR := ./node
LDFLAGS := -ldflags "-X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildTime=$(BUILD_TIME)"

# Docker configuration
DOCKER_COMPOSE := docker compose
DOCKER_COMPOSE_FILE := test/docker/docker-compose.yaml

# Packer configuration
PACKER := packer
PACKER_FILE := images/ubuntu/packer.pkr.hcl

all: deps fmt node test

help:
	@echo "Cluster-OS Build System"
	@echo ""
	@echo "Build Targets:"
	@echo "  make node         - Build node-agent binary"
	@echo "  make image        - Build OS image with Packer (requires Packer & QEMU)"
	@echo "  make usb          - Create USB installer image"
	@echo "  make release      - Create release artifacts"
	@echo ""
	@echo "Test Targets (Docker - Limited):"
	@echo "  make test         - Run unit tests"
	@echo "  make test-cluster - Start Docker multi-node test cluster"
	@echo "  make test-slurm   - Test SLURM integration only"
	@echo "  make test-k3s     - Test K3s integration only"
	@echo "  make test-full    - Run full integration test suite (SLURM + K3s)"
	@echo ""
	@echo "Test Targets (QEMU VMs - Full systemd support):"
	@echo "  make test-vm      - Start QEMU VM cluster (3 nodes)"
	@echo "  make test-vm-5    - Start QEMU VM cluster (5 nodes)"
	@echo "  make vm-status    - Show VM cluster status"
	@echo "  make vm-stop      - Stop VM cluster"
	@echo "  make vm-clean     - Stop and remove all VM data"
	@echo ""
	@echo "Development Targets:"
	@echo "  make fmt          - Format Go code"
	@echo "  make lint         - Lint Go code"
	@echo "  make deps         - Download Go dependencies"
	@echo "  make clean        - Clean build artifacts"
	@echo "  make help         - Show this help message"
	@echo ""
	@echo "Version: $(VERSION)"
	@echo "Commit: $(COMMIT)"
	@echo "Build Time: $(BUILD_TIME)"

deps:
	@echo "Downloading dependencies..."
	cd $(NODE_DIR) && $(GO) mod download
	cd $(NODE_DIR) && $(GO) mod verify

node: deps
	@echo "Building node-agent..."
	@mkdir -p $(BUILD_DIR)
	cd $(NODE_DIR) && $(GO) build $(LDFLAGS) -o ../$(BUILD_DIR)/$(BINARY_NAME) ./cmd/node-agent
	@echo "Binary built: $(BUILD_DIR)/$(BINARY_NAME)"
	@$(BUILD_DIR)/$(BINARY_NAME) --version 2>/dev/null || echo "Binary built successfully"

fmt:
	@echo "Formatting Go code..."
	cd $(NODE_DIR) && $(GOFMT) -s -w .
	@echo "Code formatted"

lint:
	@echo "Linting Go code..."
	cd $(NODE_DIR) && $(GO) vet ./...
	@echo "Lint complete"

test: deps
	@echo "Running unit tests..."
	cd $(NODE_DIR) && $(GO) test -v -race -coverprofile=coverage.out ./...
	@echo "Tests complete"

test-coverage: test
	@echo "Generating coverage report..."
	cd $(NODE_DIR) && $(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: node/coverage.html"

test-cluster: node
	@echo "Starting Docker test cluster..."
	@echo "Note: This will stop and remove any existing cluster containers"
	@echo "Building cluster containers and starting services..."
	@./test/docker/start-cluster-direct.sh
	@echo ""
	@echo "Useful commands:"
	@echo "  ./test/docker/cluster-ctl.sh status      # Show cluster status"
	@echo "  ./test/docker/cluster-ctl.sh info        # Show detailed info"
	@echo "  ./test/docker/cluster-ctl.sh logs node1  # View logs"
	@echo "  ./test/docker/cluster-ctl.sh shell node1 # Open shell"
	@echo "  ./test/docker/stop-cluster.sh            # Stop cluster"
	@echo "  ./test/docker/clean-cluster.sh           # Clean all data"
	@echo ""

test-cluster-stop:
	@echo "Stopping Docker test cluster..."
	./test/docker/stop-cluster.sh

test-cluster-clean:
	@echo "Cleaning Docker test cluster..."
	./test/docker/clean-cluster.sh

test-cluster-logs:
	@echo "Showing test cluster logs..."
	./test/docker/cluster-ctl.sh logs node1

test-slurm: node
	@echo "=========================================="
	@echo "Testing SLURM Integration"
	@echo "=========================================="
	@echo "Building and starting cluster..."
	@./test/docker/start-cluster-direct.sh
	@echo ""
	@echo "Waiting for cluster to stabilize..."
	@sleep 20
	@echo ""
	@echo "Running SLURM tests..."
	@RUN_K3S_TESTS=false ./test/integration/test_cluster.sh
	@echo ""

test-k3s: node
	@echo "=========================================="
	@echo "Testing K3s Integration"
	@echo "=========================================="
	@echo "Building and starting cluster..."
	@./test/docker/start-cluster-direct.sh
	@echo ""
	@echo "Waiting for cluster to stabilize..."
	@sleep 20
	@echo ""
	@echo "Running K3s tests..."
	@RUN_SLURM_TESTS=false ./test/integration/test_cluster.sh
	@echo ""

test-full: node
	@echo "=========================================="
	@echo "Full Integration Test Suite"
	@echo "Testing: WireGuard + SLURM + K3s"
	@echo "=========================================="
	@echo "Building and starting cluster..."
	@./test/docker/start-cluster-direct.sh
	@echo ""
	@echo "Waiting for cluster to stabilize..."
	@sleep 20
	@echo ""
	@echo "Running all tests..."
	@./test/integration/test_cluster.sh
	@echo ""
	@echo "=========================================="
	@echo "Full test suite complete!"
	@echo "=========================================="

image: node
	@echo "========================================="
	@echo "Building OS image with Packer"
	@echo "========================================="
	@if [ ! -f $(PACKER_FILE) ]; then \
		echo "Error: Packer file not found at $(PACKER_FILE)"; \
		exit 1; \
	fi
	@if ! command -v packer >/dev/null 2>&1; then \
		echo "Error: Packer not installed"; \
		echo "Install with: wget -O- https://apt.releases.hashicorp.com/gpg | sudo gpg --dearmor -o /usr/share/keyrings/hashicorp-archive-keyring.gpg"; \
		echo "  echo 'deb [signed-by=/usr/share/keyrings/hashicorp-archive-keyring.gpg] https://apt.releases.hashicorp.com focal main' | sudo tee /etc/apt/sources.list.d/hashicorp.list"; \
		echo "  sudo apt update && sudo apt install packer"; \
		exit 1; \
	fi
	@if ! command -v qemu-system-x86_64 >/dev/null 2>&1; then \
		echo "Error: QEMU not installed"; \
		echo "Install with: sudo apt-get install qemu-system-x86 qemu-utils"; \
		exit 1; \
	fi
	@echo "Initializing Packer plugins..."
	cd images/ubuntu && $(PACKER) init .
	@echo "Cleaning previous build output..."
	rm -rf /data/packer-output/cluster-os-node
	@echo "Building image (this may take 10-20 minutes)..."
	cd images/ubuntu && $(PACKER) build packer.pkr.hcl
	@echo ""
	@echo "========================================="
	@echo "OS image built successfully!"
	@echo "========================================="
	@ls -lh /data/packer-output/cluster-os-node/

usb: image
	@echo "Creating USB installer..."
	@./scripts/create-usb-installer.sh --usb
	@echo "USB installer created successfully"

release: clean node test
	@echo "Creating release artifacts..."
	@mkdir -p dist/$(VERSION)

	@echo "Building for linux/amd64..."
	cd $(NODE_DIR) && GOOS=linux GOARCH=amd64 $(GO) build $(LDFLAGS) -o ../dist/$(VERSION)/$(BINARY_NAME)-linux-amd64 ./cmd/node-agent

	@echo "Building for linux/arm64..."
	cd $(NODE_DIR) && GOOS=linux GOARCH=arm64 $(GO) build $(LDFLAGS) -o ../dist/$(VERSION)/$(BINARY_NAME)-linux-arm64 ./cmd/node-agent

	@echo "Creating checksums..."
	cd dist/$(VERSION) && sha256sum * > SHA256SUMS

	@echo "Creating tarball..."
	tar -czf dist/cluster-os-$(VERSION).tar.gz -C dist/$(VERSION) .

	@echo "Release artifacts created in dist/"
	@echo "Version: $(VERSION)"
	@ls -lh dist/cluster-os-$(VERSION).tar.gz

test-vm: image
	@echo "========================================="
	@echo "Starting QEMU VM Cluster (3 nodes)"
	@echo "========================================="
	@NUM_NODES=3 ./test/vm/qemu/start-cluster.sh
	@echo ""
	@echo "VM cluster started!"
	@echo "Use 'make vm-status' to check status"

test-vm-5: image
	@echo "========================================="
	@echo "Starting QEMU VM Cluster (5 nodes)"
	@echo "========================================="
	@NUM_NODES=5 ./test/vm/qemu/start-cluster.sh
	@echo ""
	@echo "VM cluster started!"
	@echo "Use 'make vm-status' to check status"

vm-status:
	@./test/vm/qemu/cluster-ctl.sh status

vm-info:
	@./test/vm/qemu/cluster-ctl.sh info

vm-stop:
	@./test/vm/qemu/cluster-ctl.sh stop

vm-clean:
	@./test/vm/qemu/cluster-ctl.sh clean

test-vm-integration: test-vm
	@echo "========================================="
	@echo "Running QEMU VM Integration Tests"
	@echo "========================================="
	@./test/vm/integration-test.sh

clean:
	@echo "Cleaning build artifacts..."
	rm -rf $(BUILD_DIR)
	rm -rf dist/
	rm -rf node/coverage.out
	rm -rf node/coverage.html
	rm -rf images/**/output-*
	rm -rf images/**/packer_cache
	rm -rf test/vm/qemu/vms
	@echo "Clean complete"

dev-setup:
	@echo "Setting up development environment..."
	@echo "Installing Go tools..."
	$(GO) install golang.org/x/tools/cmd/goimports@latest
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	@echo "Development setup complete"

.DEFAULT_GOAL := help
