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
	@echo "Rollout Targets:"
	@echo "  make patch        - Build binary + stage patch/ folder (run before deploy)"
	@echo "  make patch-cross  - Cross-compile for amd64 + arm64"
	@echo "  make deploy       - SCP patch/ to nodes and run apply-patch.sh"
	@echo "                      NODES='IP1 IP2 ...'  (auto-detected via tailscale if omitted)"
	@echo "                      SSH_USER=clusteros   (default)"
	@echo "  make deploy-status- Quick phase/leader/member check across all nodes"
	@echo ""
	@echo "Build Targets:"
	@echo "  make node         - Build node-agent binary (local arch)"
	@echo "  make image        - Build OS image with Packer (requires Packer & QEMU)"
	@echo "  make usb          - Create USB installer image (requires Packer build)"
	@echo "  make usb-local    - Create USB installer image on the local machine"
	@echo "  make release      - Create release artifacts (amd64 + arm64)"
	@echo ""
	@echo "On-node USB builder (after 'make deploy'):"
	@echo "  sudo cluster-make-usb --output /tmp/patch.tar.gz  # any node: patch bundle"
	@echo "  sudo cluster-make-usb --device /dev/sdb           # leader: bootable USB"
	@echo "  sudo systemctl start clusteros-make-usb           # via systemd"
	@echo ""
	@echo "Test Targets (Docker):"
	@echo "  make test         - Run unit tests"
	@echo "  make test-cluster - Start Docker multi-node test cluster"
	@echo "  make test-slurm   - Test SLURM integration only"
	@echo "  make test-k3s     - Test K3s integration only"
	@echo "  make test-full    - Full integration suite (SLURM + K3s)"
	@echo ""
	@echo "Test Targets (QEMU VMs):"
	@echo "  make test-vm      - Start QEMU VM cluster (3 nodes)"
	@echo "  make test-vm-5    - Start QEMU VM cluster (5 nodes)"
	@echo "  make vm-status    - Show VM cluster status"
	@echo "  make vm-stop      - Stop VM cluster"
	@echo "  make vm-clean     - Stop and remove all VM data"
	@echo ""
	@echo "Development:"
	@echo "  make fmt          - Format Go code"
	@echo "  make lint         - Lint Go code"
	@echo "  make deps         - Download Go dependencies"
	@echo "  make clean        - Clean build artifacts"
	@echo ""
	@echo "Version: $(VERSION)  Commit: $(COMMIT)  Built: $(BUILD_TIME)"

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

cluster-key:
	@if [ ! -f cluster.key ]; then \
		echo "Generating cluster key from git repo identity..."; \
		bash scripts/generate-cluster-key.sh; \
	else \
		echo "Cluster key already exists: cluster.key"; \
	fi

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

image: node cluster-key
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
	@echo ""
	@echo "========================================="
	@echo "USB installer created!"
	@echo "========================================="
	@ls -lh dist/cluster-os-usb.img
	@echo ""
	@echo "Write to USB with:"
	@echo "  sudo dd if=dist/cluster-os-usb.img of=/dev/sdX bs=4M status=progress"
	@echo ""

# patch — build binary for the local arch and stage the complete patch folder.
# Run this before 'make deploy'.
patch: node
	@echo "Staging patch folder..."
	@mkdir -p patch
	@cp bin/$(BINARY_NAME) patch/$(BINARY_NAME)
	@printf '%s\n' "version=$(VERSION)" "commit=$(COMMIT)" "built=$(BUILD_TIME)" > patch/VERSION
	@echo "Binary version: $(VERSION) ($(COMMIT))"
	@echo "Generating fresh munge key (32 bytes) for patch/munge.key..."
	@head -c 32 /dev/urandom > patch/munge.key || openssl rand -out patch/munge.key 32
	@chmod 600 patch/munge.key
	@if [ ! -f patch/k3s-ca.crt ] || [ ! -f patch/k3s-ca.key ]; then \
		echo "Generating k3s cluster CA certificate (shared across all nodes)..."; \
		openssl genrsa -out patch/k3s-ca.key 2048 2>/dev/null && \
		openssl req -new -x509 -days 3650 \
			-key patch/k3s-ca.key \
			-out patch/k3s-ca.crt \
			-subj "/O=cluster-os/CN=k3s-server-ca" 2>/dev/null && \
		chmod 600 patch/k3s-ca.key && \
		echo "  k3s CA generated: patch/k3s-ca.crt (reused on subsequent make patch runs)"; \
	else \
		echo "k3s CA cert already present (patch/k3s-ca.crt) — reusing existing CA"; \
	fi
	@if [ ! -f patch/pause-3.6.tar ]; then \
		echo "Pre-bundling pause image for airgap deploy (both registry.k8s.io and rancher aliases)..."; \
		if command -v docker &>/dev/null && docker info &>/dev/null 2>&1; then \
			docker pull registry.k8s.io/pause:3.6 2>/dev/null && \
			docker tag  registry.k8s.io/pause:3.6 rancher/mirrored-pause:3.6 2>/dev/null && \
			docker save registry.k8s.io/pause:3.6 rancher/mirrored-pause:3.6 \
				-o patch/pause-3.6.tar 2>/dev/null && \
			echo "  pause image bundled (both tags): patch/pause-3.6.tar" || \
			echo "  WARNING: docker pull failed — nodes will fall back to runtime pull"; \
		elif command -v skopeo &>/dev/null; then \
			skopeo copy docker://registry.k8s.io/pause:3.6 \
				docker-archive:patch/pause-3.6.tar:registry.k8s.io/pause:3.6 2>/dev/null && \
			echo "  pause image bundled via skopeo: patch/pause-3.6.tar" || \
			echo "  WARNING: skopeo copy failed — nodes will fall back to runtime pull"; \
		else \
			echo "  WARNING: no docker/skopeo on dev machine — nodes will pull pause image at boot"; \
			echo "           Install docker or skopeo to enable fully-airgap pause image"; \
		fi; \
	else \
		echo "Pause image already bundled (patch/pause-3.6.tar) — reusing"; \
	fi
	@chmod +x patch/cluster patch/apply-patch.sh
	@# One-time iptables cleanup script + service
	@cp -f scripts/clear-stale-redirects.sh patch/ 2>/dev/null || true
	@# cluster-make-usb: on-node USB installer builder (runs on any cluster node)
	@cp -f scripts/cluster-make-usb.sh patch/ 2>/dev/null || true
	@chmod +x patch/cluster-make-usb.sh 2>/dev/null || true
	@# Legacy dev-machine USB creator (still useful for building from Packer images)
	@cp -f scripts/create-usb-installer.sh patch/ 2>/dev/null || true
	@mkdir -p patch/systemd
	@cp -f systemd/clear-stale-redirects.service patch/systemd/ 2>/dev/null || true
	@cp -f systemd/clusteros-make-usb.service patch/systemd/ 2>/dev/null || true
	@chmod +x patch/create-usb-installer.sh 2>/dev/null || true
	@echo ""
	@echo "Patch staged:"
	@ls -lh patch/
	@echo ""
	@echo "Deploy with:  make deploy NODES='100.x.x.1 100.x.x.2'"
	@echo "Or manually:  scp -r patch/ clusteros@<ip>:~/patch/ && ssh clusteros@<ip> 'sudo bash ~/patch/apply-patch.sh'"

# patch-cross — cross-compile for amd64 and arm64 then pick the right one.
patch-cross:
	@echo "Cross-compiling node-agent for amd64 and arm64..."
	@mkdir -p bin
	cd $(NODE_DIR) && GOOS=linux GOARCH=amd64 $(GO) build $(LDFLAGS) -o ../bin/$(BINARY_NAME)-amd64 ./cmd/node-agent
	cd $(NODE_DIR) && GOOS=linux GOARCH=arm64 $(GO) build $(LDFLAGS) -o ../bin/$(BINARY_NAME)-arm64 ./cmd/node-agent
	@echo "Binaries: bin/$(BINARY_NAME)-amd64  bin/$(BINARY_NAME)-arm64"
	@echo "Copy the right one to patch/node-agent before running make deploy."

# deploy — scp the patch folder to each node and run apply-patch.sh.
# Usage:  make deploy NODES="100.x.x.1 100.x.x.2 100.x.x.3"
#   or:   make deploy  (reads Tailscale peer IPs automatically)
#
# Authentication (in order of preference):
#   SSH_KEY=~/.ssh/id_rsa     — key-based auth (default; tries ~/.ssh/id_rsa and cluster_key)
#   SSH_PASS=clusteros        — password auth via sshpass (default password from cloud-init)
#   Set SSH_KEY="" to disable key auth and fall back to SSH_PASS only.
NODES ?= $(shell tailscale status --json 2>/dev/null | \
	python3 -c "import sys,json; peers=json.load(sys.stdin).get('Peer',{}); \
	[print(p['TailscaleIPs'][0]) for p in peers.values() if p.get('Online') and p.get('TailscaleIPs')]" 2>/dev/null)
SSH_USER ?= clusteros
SSH_PASS ?= clusteros
SSH_KEY  ?= $(HOME)/.ssh/cluster_key

# Internal: build ssh/scp command prefix (sshpass + key or just key).
_SSH_AUTH := $(if $(SSH_PASS),sshpass -p '$(SSH_PASS)') ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 $(if $(SSH_KEY),-i $(SSH_KEY))
_SCP_AUTH := $(if $(SSH_PASS),sshpass -p '$(SSH_PASS)') scp -o StrictHostKeyChecking=no $(if $(SSH_KEY),-i $(SSH_KEY))

deploy: patch
	@if [ -z "$(NODES)" ]; then \
		echo "Error: No nodes specified. Use: make deploy NODES='IP1 IP2 IP3'"; \
		echo "Or ensure tailscale is running so peers are auto-detected."; \
		exit 1; \
	fi
	@echo "Deploying $(VERSION) ($(COMMIT)) to: $(NODES)"
	@echo "SSH user: $(SSH_USER)  key: $(SSH_KEY)  pass: $(if $(SSH_PASS),set,unset)"
	@for node in $(NODES); do \
		echo ""; \
		echo "==> $$node"; \
		echo "    [1/4] Stopping old node-agent (prevents stale REDIRECT rule re-injection during upload)"; \
		$(_SSH_AUTH) $(SSH_USER)@$$node \
			'sudo systemctl stop node-agent 2>/dev/null; \
			 sudo pkill -KILL -f /usr/local/bin/node-agent 2>/dev/null; \
			 sudo pkill -KILL -f /tmp/node-agent 2>/dev/null; \
			 true' 2>/dev/null || true; \
		echo "    [2/4] Uploading patch bundle"; \
		$(_SCP_AUTH) -r patch/ $(SSH_USER)@$$node:~/patch/ \
			|| { echo "WARNING: SCP to $$node failed — skipping"; continue; }; \
		echo "    [3/4] Running apply-patch.sh"; \
		$(_SSH_AUTH) $(SSH_USER)@$$node 'sudo bash ~/patch/apply-patch.sh' \
			|| echo "WARNING: apply-patch.sh on $$node exited non-zero (check output above)"; \
		echo "    [4/4] Installing cluster-make-usb (direct, apply-patch.sh-independent)"; \
		$(_SSH_AUTH) $(SSH_USER)@$$node \
			'if [ -f ~/patch/cluster-make-usb.sh ]; then \
			   sudo install -m 755 ~/patch/cluster-make-usb.sh /usr/local/bin/cluster-make-usb && \
			   echo "      cluster-make-usb installed ok"; \
			 else \
			   echo "      WARNING: ~/patch/cluster-make-usb.sh missing on node — SCP may have failed"; \
			 fi' \
			|| echo "WARNING: cluster-make-usb install step failed on $$node"; \
	done
	@echo ""
	@echo "Deploy complete. Check: make deploy-status"

# setup-ssh-keys — copy this machine's public key to all nodes (one-time setup).
# After this, deploy works without passwords.
# Usage:  make setup-ssh-keys              (uses SSH_PASS=clusteros)
#         make setup-ssh-keys SSH_PASS=mypass
setup-ssh-keys:
	@if [ -z "$(NODES)" ]; then echo "No nodes (set NODES=...)"; exit 1; fi
	@PUB_KEY=""; \
	for candidate in $(SSH_KEY).pub $(HOME)/.ssh/id_rsa.pub $(HOME)/.ssh/id_ed25519.pub; do \
		if [ -f "$$candidate" ]; then PUB_KEY="$$candidate"; break; fi; \
	done; \
	if [ -z "$$PUB_KEY" ]; then echo "No public key found — run: ssh-keygen -t ed25519"; exit 1; fi; \
	echo "Copying $$PUB_KEY to nodes..."; \
	for node in $(NODES); do \
		printf "  %-20s " "$$node:"; \
		sshpass -p '$(SSH_PASS)' ssh-copy-id -i "$$PUB_KEY" \
			-o StrictHostKeyChecking=no $(SSH_USER)@$$node 2>/dev/null && \
			echo "OK" || echo "FAILED"; \
	done; \
	echo ""; \
	echo "Done. Future deploys will use key auth (no password needed)."

# deploy-status — quick status check across all nodes after deploy.
check-services:
	@echo "Checking service visibility across all cluster nodes..."
	@bash scripts/check-service-visibility.sh

deploy-status:
	@if [ -z "$(NODES)" ]; then echo "No nodes (set NODES=...)"; exit 1; fi
	@for node in $(NODES); do \
		printf "%-20s " "$$node:"; \
		$(_SSH_AUTH) -o ConnectTimeout=5 $(SSH_USER)@$$node \
			"cluster status 2>/dev/null | grep -E 'Phase|Members|Leader' | tr '\n' ' '" 2>/dev/null || \
		echo "(unreachable)"; \
	done

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

usb-local:
	@echo "Creating USB installer locally (runs scripts/create-usb-installer.sh)"
	@sudo ./scripts/create-usb-installer.sh --usb

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
