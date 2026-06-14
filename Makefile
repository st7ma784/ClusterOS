.PHONY: all node test test-cluster image image-chroot usb usb-chroot release clean fmt lint deps help patch cluster-key

# Version info (from git tags or default)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S')

# Go configuration — prefer ~/go/bin/go (local dev), fall back to PATH (CI)
GO    := $(shell [ -x $(HOME)/go/bin/go ] && echo $(HOME)/go/bin/go || echo go)
GOFMT := $(shell [ -x $(HOME)/go/bin/gofmt ] && echo $(HOME)/go/bin/gofmt || echo gofmt)
GOPATH := $(shell $(GO) env GOPATH)
GOBIN := $(GOPATH)/bin

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
	@echo "                      NODES='IP1 IP2 ...'         (auto-detected via tailscale if omitted)"
	@echo "                      SSH_USER=clusteros           (default)"
	@echo "                      SSH_PASS_FALLBACK=clusteros  (password tried when key auth fails; set empty to disable)"
	@echo "  make deploy-status- Quick phase/leader/member check across all nodes"
	@echo ""
	@echo "Build Targets:"
	@echo "  make node         - Build node-agent binary (local arch)"
	@echo "  make image        - Build OS image with Packer (requires Packer & KVM)"
	@echo "  make image-chroot - Build OS image without KVM (use when /dev/kvm unavailable)"
	@echo "  make usb          - Create USB installer image (requires Packer build)"
	@echo "  make usb-chroot   - Create USB installer image (uses image-chroot build)"
	@echo "  make usb-local    - Create USB installer image on the local machine"
	@echo "  make release      - Create release artifacts (amd64 + arm64)"
	@echo ""
	@echo "On-node USB builder (after 'make deploy'):"
	@echo "  sudo cluster-make-usb --output /tmp/patch.tar.gz  # any node: patch bundle"
	@echo "  sudo cluster-make-usb --device /dev/sdb           # leader: bootable USB"
	@echo "  sudo systemctl start clusteros-make-usb           # via systemd"
	@echo ""
	@echo "Test Targets (Docker):"
	@echo "  make test           - Run unit tests"
	@echo "  make test-cluster   - Start Docker multi-node test cluster (bridge + Tailscale overlay)"
	@echo "  make test-slurm     - Test SLURM integration only"
	@echo "  make test-k3s       - Test K3s integration only"
	@echo "  make test-full      - Full integration suite (SLURM + K3s)"
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

image: node patch cluster-key
	@echo "Staging current node-agent binary for Packer build..."
	@cp $(BUILD_DIR)/$(BINARY_NAME) images/ubuntu/node-agent
	@echo "========================================="
	@echo "Building OS image with Packer"
	@echo "========================================="
	@if [ ! -f images/ubuntu/.env ]; then \
		echo "Error: images/ubuntu/.env missing — copy images/ubuntu/.env.example and add Tailscale credentials"; \
		exit 1; \
	fi
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

image-chroot: node patch cluster-key
	@echo "Staging current node-agent binary..."
	@cp $(BUILD_DIR)/$(BINARY_NAME) images/ubuntu/node-agent
	@echo "========================================="
	@echo "Building OS image (no-KVM / chroot path)"
	@echo "========================================="
	@if [ ! -f images/ubuntu/.env ]; then \
		echo "Error: images/ubuntu/.env missing — copy images/ubuntu/.env.example and add Tailscale credentials"; \
		exit 1; \
	fi
	@mkdir -p /data/packer-cache /data/packer-output/cluster-os-node
	@sudo bash scripts/build-image-chroot.sh
	@echo ""
	@echo "========================================="
	@echo "OS image built successfully!"
	@echo "========================================="
	@ls -lh /data/packer-output/cluster-os-node/

RAW_IMAGE := /data/packer-output/cluster-os-node/cluster-os-node.raw

# usb — always rebuilds so changes to build scripts, patch bundle, and service
# files are always reflected. Use usb-cached to reuse a previous raw image.
usb:
	@if [ -e /dev/kvm ]; then \
		$(MAKE) image; \
	else \
		$(MAKE) image-chroot; \
	fi
	@echo "Creating USB installer..."
	@sudo ./scripts/create-usb-installer.sh --usb
	@echo ""
	@echo "========================================="
	@echo "USB installer created!"
	@echo "========================================="
	@ls -lh dist/cluster-os-usb.img dist/cluster-os-usb.img.gz 2>/dev/null || true
	@echo ""
	@echo "Flash to USB with Balena Etcher: dist/cluster-os-usb.img.gz"
	@echo "Or with dd:"
	@echo "  sudo dd if=dist/cluster-os-usb.img of=/dev/sdX bs=4M status=progress oflag=sync"
	@echo ""

usb-chroot: image-chroot
	@echo "Creating USB installer from chroot-built image..."
	@sudo ./scripts/create-usb-installer.sh --usb
	@echo ""
	@echo "========================================="
	@echo "USB installer created!"
	@echo "========================================="
	@ls -lh dist/cluster-os-usb.img dist/cluster-os-usb.img.gz 2>/dev/null || true
	@echo ""
	@echo "Flash to USB with Balena Etcher: dist/cluster-os-usb.img.gz"
	@echo "Or with dd:"
	@echo "  sudo dd if=dist/cluster-os-usb.img of=/dev/sdX bs=4M status=progress oflag=sync"
	@echo ""

# patch — build binary for the local arch and stage the complete patch folder.
# Run this before 'make deploy'.
patch: node
	@echo "Staging patch folder..."
	@mkdir -p patch
	@cp bin/$(BINARY_NAME) patch/$(BINARY_NAME)
	@printf '%s\n' "version=$(VERSION)" "commit=$(COMMIT)" "built=$(BUILD_TIME)" > patch/VERSION
	@echo "Binary version: $(VERSION) ($(COMMIT))"
	@if [ -f cluster.key ]; then \
		printf 'CLUSTER_AUTH_KEY=%s\n' "$$(cat cluster.key | tr -d '[:space:]')" > test/docker/.env; \
		echo "  test/docker/.env written from cluster.key (docker-compose will use it)"; \
	fi
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
	@test -f patch/cloudflare.env || cp patch/cloudflare.env.example patch/cloudflare.env
	@# Tailscale credentials — generate patch/tailscale.env from images/ubuntu/.env so
	@# apply-patch.sh can call clusteros-tailscale-init on first boot.  This runs AFTER
	@# iptables cleanup and DNS rescue, making it far more reliable than the standalone
	@# tailscale-auth.service which fires at 15s (before the network is clean).
	@if [ -f images/ubuntu/.env ]; then \
		_TS_ID=$$(grep -v '^#' images/ubuntu/.env | grep '^TAILSCALE_OAUTH_CLIENT_ID=' | head -1 | cut -d= -f2-); \
		_TS_SEC=$$(grep -v '^#' images/ubuntu/.env | grep '^TAILSCALE_OAUTH_CLIENT_SECRET=' | head -1 | cut -d= -f2-); \
		_TS_KEY=$$(grep -v '^#' images/ubuntu/.env | grep '^TAILSCALE_AUTHKEY=' | head -1 | cut -d= -f2-); \
		printf '# Tailscale credentials — generated from images/ubuntu/.env by make patch\n' > patch/tailscale.env; \
		printf '# DO NOT COMMIT — this file is in .gitignore\n' >> patch/tailscale.env; \
		printf 'TAILSCALE_OAUTH_CLIENT_ID=%s\n' "$$_TS_ID" >> patch/tailscale.env; \
		printf 'TAILSCALE_OAUTH_CLIENT_SECRET=%s\n' "$$_TS_SEC" >> patch/tailscale.env; \
		printf 'TAILSCALE_TAGS=clusteros\n' >> patch/tailscale.env; \
		if [ -n "$$_TS_KEY" ]; then printf 'TAILSCALE_AUTHKEY=%s\n' "$$_TS_KEY" >> patch/tailscale.env; fi; \
		chmod 600 patch/tailscale.env; \
		echo "  Tailscale credentials staged -> patch/tailscale.env"; \
	else \
		echo "  WARNING: images/ubuntu/.env not found -- Tailscale auto-join will not be configured"; \
		echo "           Copy images/ubuntu/.env.example to images/ubuntu/.env and fill in credentials."; \
	fi
	@# WiFi credentials — generate patch/wifi.env from images/ubuntu/.env so
	@# apply-patch.sh can install /etc/clusteros/wifi.env on deployed nodes and
	@# cluster-make-usb can bundle it into fresh USB images. No hardcoded fallback —
	@# nodes without these credentials are wired/Tailscale-only.
	@if [ -f images/ubuntu/.env ]; then \
		_WIFI_SSID=$$(grep -v '^#' images/ubuntu/.env | grep '^WIFI_SSID=' | head -1 | cut -d= -f2-); \
		_WIFI_KEY=$$(grep -v '^#' images/ubuntu/.env | grep '^WIFI_KEY=' | head -1 | cut -d= -f2-); \
		if [ -n "$$_WIFI_SSID" ] && [ -n "$$_WIFI_KEY" ]; then \
			printf '# WiFi credentials — generated from images/ubuntu/.env by make patch\n' > patch/wifi.env; \
			printf '# DO NOT COMMIT — this file is in .gitignore\n' >> patch/wifi.env; \
			printf 'WIFI_SSID=%s\n' "$$_WIFI_SSID" >> patch/wifi.env; \
			printf 'WIFI_KEY=%s\n' "$$_WIFI_KEY" >> patch/wifi.env; \
			chmod 600 patch/wifi.env; \
			echo "  WiFi credentials staged -> patch/wifi.env"; \
		else \
			echo "  WIFI_SSID/WIFI_KEY not set in images/ubuntu/.env -- patch/wifi.env not generated (wired/Tailscale-only)"; \
		fi; \
	fi
	@# Netplan wired-DHCP config — sync from images/ubuntu/files/netplan. This file
	@# contains no credentials (WiFi is handled separately via wifi.env); always
	@# overwrite so patch/99-clusteros.yaml never drifts from the tracked source.
	@cp -f images/ubuntu/files/netplan/99-clusteros.yaml patch/99-clusteros.yaml
	@chmod 600 patch/99-clusteros.yaml
	@echo "  Netplan config synced -> patch/99-clusteros.yaml"
	@echo "Bundling cluster SSH public key..."
	@if [ ! -f "$(HOME)/.ssh/cluster_key" ]; then \
		echo "  Generating new cluster SSH keypair (~/.ssh/cluster_key)..."; \
		ssh-keygen -t ed25519 -f "$(HOME)/.ssh/cluster_key" -N "" -C "clusteros-deploy" -q; \
		echo "  Generated. Nodes will get this key via patch bundle."; \
	else \
		echo "  Cluster SSH key already present — reusing"; \
	fi
	@cp "$(HOME)/.ssh/cluster_key.pub" patch/authorized_keys
	@echo "  Bundled: patch/authorized_keys"
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
	@cp -f systemd/cloudflared.service patch/systemd/ 2>/dev/null || true
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
# Authentication:
#   SSH_KEY=~/.ssh/cluster_key   — ed25519 keypair generated by `make patch` (default)
#   SSH_PASS=<password>          — explicit password override (disables key auth, bootstrap mode)
#   SSH_PASS_FALLBACK=clusteros  — tried when key auth fails (default: "clusteros", cloud-init default)
#                                  disable with: make deploy SSH_PASS_FALLBACK=
NODES ?= $(shell tailscale status --json 2>/dev/null | \
	python3 -c "import sys,json; peers=json.load(sys.stdin).get('Peer',{}); \
	[print(p['TailscaleIPs'][0]) for p in peers.values() if p.get('Online') and p.get('TailscaleIPs')]" 2>/dev/null)
SSH_USER         ?= clusteros
SSH_KEY          ?= $(HOME)/.ssh/cluster_key
SSH_PASS_FALLBACK ?= clusteros
# SSH_PASS is intentionally unset by default.
_COMMA := ,

# Internal auth helpers — three modes:
#   SSH_PASS set        → explicit password (sshpass, PasswordAuthentication=yes; ignores key)
#   SSH_PASS unset,
#     SSH_PASS_FALLBACK set → key preferred, password fallback via sshpass (works on both old and new nodes)
#   both unset          → key-only (PasswordAuthentication=no)
_SSH_PASS_CMD  := $(if $(SSH_PASS),sshpass -p '$(SSH_PASS)',$(if $(SSH_PASS_FALLBACK),sshpass -p '$(SSH_PASS_FALLBACK)'))
_SSH_AUTH_OPTS := $(if $(SSH_PASS),-o PasswordAuthentication=yes,$(if $(SSH_PASS_FALLBACK),-o PreferredAuthentications=publickey$(_COMMA)password -o PasswordAuthentication=yes,-o PasswordAuthentication=no))
_SSH_AUTH := $(_SSH_PASS_CMD) ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 $(if $(SSH_KEY),-i $(SSH_KEY)) $(_SSH_AUTH_OPTS)
_SCP_AUTH := $(_SSH_PASS_CMD) scp -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 $(if $(SSH_KEY),-i $(SSH_KEY)) $(if $(SSH_PASS)$(SSH_PASS_FALLBACK),-o PasswordAuthentication=yes,-o PasswordAuthentication=no)

deploy: patch
	@if [ -z "$(NODES)" ]; then \
		echo "Error: No nodes specified. Use: make deploy NODES='IP1 IP2 IP3'"; \
		echo "Or ensure tailscale is running so peers are auto-detected."; \
		exit 1; \
	fi
	@echo "Deploying $(VERSION) ($(COMMIT)) to: $(NODES)"
	@echo "SSH user: $(SSH_USER)  key: $(SSH_KEY)  $(if $(SSH_PASS),auth: password-only (bootstrap),$(if $(SSH_PASS_FALLBACK),auth: key→fallback '$(SSH_PASS_FALLBACK)',auth: key-only))"
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
		$(_SSH_AUTH) $(SSH_USER)@$$node 'rm -rf ~/patch && mkdir -p ~/patch' 2>/dev/null || true; \
		$(_SCP_AUTH) -r patch/ $(SSH_USER)@$$node:~/ \
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
		echo "    [5/5] Ensuring node-agent is running (recovers if apply-patch.sh aborted before restart/reboot)"; \
		$(_SSH_AUTH) $(SSH_USER)@$$node \
			'sudo systemctl enable node-agent 2>/dev/null; \
			 if ! systemctl is-active --quiet node-agent; then \
			   echo "      node-agent not active — starting"; \
			   sudo systemctl start node-agent; \
			 fi; \
			 sleep 3; \
			 if systemctl is-active --quiet node-agent; then \
			   echo "      node-agent active"; \
			 else \
			   echo "      ERROR: node-agent still not active after start attempt"; \
			   sudo journalctl -u node-agent -n 15 --no-pager; \
			 fi' \
			|| echo "WARNING: node-agent ensure-running step failed on $$node"; \
	done
	@echo ""
	@echo "Deploy complete. Check: make deploy-status"

# setup-ssh-keys — one-time bootstrap: copy this machine's public key to nodes using a password.
# Only needed for nodes that don't yet have the key (e.g. manually-provisioned nodes).
# USB/cloud-init-provisioned nodes get the key from the patch bundle automatically.
# Usage:  make setup-ssh-keys SSH_PASS=<node-password>
setup-ssh-keys:
	@if [ -z "$(NODES)" ]; then echo "No nodes (set NODES=...)"; exit 1; fi
	@if [ -z "$(SSH_PASS)" ]; then \
		echo "Error: SSH_PASS is required for key bootstrap (nodes use key-only auth by default)."; \
		echo "Usage: make setup-ssh-keys SSH_PASS=<node-password>"; \
		exit 1; \
	fi
	@PUB_KEY=""; \
	for candidate in $(SSH_KEY).pub $(HOME)/.ssh/id_rsa.pub $(HOME)/.ssh/id_ed25519.pub; do \
		if [ -f "$$candidate" ]; then PUB_KEY="$$candidate"; break; fi; \
	done; \
	if [ -z "$$PUB_KEY" ]; then echo "No public key found — run: make patch (generates ~/.ssh/cluster_key)"; exit 1; fi; \
	echo "Copying $$PUB_KEY to nodes (one-time bootstrap)..."; \
	for node in $(NODES); do \
		printf "  %-20s " "$$node:"; \
		sshpass -p '$(SSH_PASS)' ssh-copy-id -i "$$PUB_KEY" \
			-o StrictHostKeyChecking=accept-new $(SSH_USER)@$$node 2>/dev/null && \
			echo "OK" || echo "FAILED"; \
	done; \
	echo ""; \
	echo "Done. Future deploys use key auth only."

DOCKER_IMAGE ?= st7ma784/clusteros

# docker-push — build and push the Docker image to Docker Hub from your dev machine.
# Requires: docker login (or DOCKERHUB_USERNAME/TOKEN env vars).
# Run 'make patch' first so all credential files are present.
# Usage:  make docker-push
#         make docker-push DOCKER_IMAGE=myuser/clusteros TAG=v1.2.3
TAG ?= latest
docker-push: patch
	@echo "Building $(DOCKER_IMAGE):$(TAG)..."
	docker build -t $(DOCKER_IMAGE):$(TAG) -f node/Dockerfile .
	@echo "Pushing $(DOCKER_IMAGE):$(TAG)..."
	docker push $(DOCKER_IMAGE):$(TAG)
	@if [ "$(TAG)" != "latest" ]; then \
		docker tag $(DOCKER_IMAGE):$(TAG) $(DOCKER_IMAGE):latest && \
		docker push $(DOCKER_IMAGE):latest; \
	fi
	@echo "Pushed: $(DOCKER_IMAGE):$(TAG)"

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

cloudflare-status:
	@if [ -z "$(NODES)" ]; then echo "No nodes (set NODES=...)"; exit 1; fi
	@for node in $(NODES); do \
		echo ""; \
		echo "==> $$node"; \
		$(_SSH_AUTH) -o ConnectTimeout=5 $(SSH_USER)@$$node \
		"echo '--- cloudflare.env (requires sudo) ---'; \
		 sudo cat /etc/clusteros/cloudflare.env 2>/dev/null | sed 's/TOKEN=.*/TOKEN=[redacted]/' || echo 'MISSING'; \
		 echo '--- cluster phase ---'; \
		 sudo cat /var/lib/cluster-os/status.json 2>/dev/null | python3 -c 'import sys,json; d=json.load(sys.stdin); print(\"phase:\",d.get(\"phase\",\"?\"), \"leader:\",d.get(\"leader_name\",\"?\"))' 2>/dev/null || echo 'no status'; \
		 echo '--- cloudflared namespace ---'; \
		 sudo k3s kubectl get all -n cloudflared 2>/dev/null || echo 'namespace not found (not leader or k3s not ready)'; \
		 echo '--- cloudflared pod logs (last 30 lines) ---'; \
		 sudo k3s kubectl logs -n cloudflared -l app=cloudflared --tail=30 2>/dev/null || echo 'no pods'; \
		 echo '--- node-agent cloudflare log entries ---'; \
		 sudo journalctl -u node-agent --no-pager -n 300 2>/dev/null | grep -i cloudflare | tail -20 || echo 'none'; \
		 echo '--- ingress-nginx service ---'; \
		 sudo k3s kubectl get svc -n ingress-nginx 2>/dev/null || echo 'not found'; \
		 echo '--- flannel subnet.env ---'; \
		 ls -la /run/flannel/subnet.env 2>/dev/null || echo 'MISSING (flannel not ready)'" 2>/dev/null || echo "(unreachable)"; \
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
