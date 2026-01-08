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
DOCKER_COMPOSE := docker-compose
DOCKER_COMPOSE_FILE := test/docker/docker-compose.yaml

# Packer configuration
PACKER := packer
PACKER_FILE := images/ubuntu/packer.pkr.hcl

all: deps fmt node test

help:
	@echo "Cluster-OS Build System"
	@echo ""
	@echo "Available targets:"
	@echo "  make node         - Build node-agent binary"
	@echo "  make test         - Run unit tests"
	@echo "  make test-cluster - Start Docker multi-node test cluster"
	@echo "  make image        - Build OS image with Packer"
	@echo "  make release      - Create release artifacts"
	@echo "  make clean        - Clean build artifacts"
	@echo "  make fmt          - Format Go code"
	@echo "  make lint         - Lint Go code"
	@echo "  make deps         - Download Go dependencies"
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
	@if [ ! -f $(DOCKER_COMPOSE_FILE) ]; then \
		echo "Error: Docker compose file not found at $(DOCKER_COMPOSE_FILE)"; \
		exit 1; \
	fi
	@echo "Building cluster containers and starting services..."
	cd test/docker && $(DOCKER_COMPOSE) up --build -d
	@echo ""
	@echo "✓ Test cluster started successfully!"
	@echo ""
	@echo "Useful commands:"
	@echo "  ./test/docker/cluster-ctl.sh status      # Show cluster status"
	@echo "  ./test/docker/cluster-ctl.sh info        # Show detailed info"
	@echo "  ./test/docker/cluster-ctl.sh logs node1  # View logs"
	@echo "  ./test/docker/cluster-ctl.sh test        # Run integration tests"
	@echo "  ./test/docker/cluster-ctl.sh shell node1 # Open shell"
	@echo "  ./test/docker/cluster-ctl.sh stop        # Stop cluster"
	@echo ""

test-cluster-stop:
	@echo "Stopping Docker test cluster..."
	cd test/docker && $(DOCKER_COMPOSE) down -v
	@echo "Test cluster stopped"

test-cluster-logs:
	@echo "Showing test cluster logs..."
	cd test/docker && $(DOCKER_COMPOSE) logs -f

image:
	@echo "Building OS image with Packer..."
	@if [ ! -f $(PACKER_FILE) ]; then \
		echo "Error: Packer file not found at $(PACKER_FILE)"; \
		echo "Packer configuration not yet created"; \
		exit 1; \
	fi
	cd images/ubuntu && $(PACKER) init . && $(PACKER) build $(PACKER_FILE)
	@echo "OS image built"

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

clean:
	@echo "Cleaning build artifacts..."
	rm -rf $(BUILD_DIR)
	rm -rf dist/
	rm -rf node/coverage.out
	rm -rf node/coverage.html
	rm -rf images/**/output-*
	rm -rf images/**/packer_cache
	@echo "Clean complete"

dev-setup:
	@echo "Setting up development environment..."
	@echo "Installing Go tools..."
	$(GO) install golang.org/x/tools/cmd/goimports@latest
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	@echo "Development setup complete"

.DEFAULT_GOAL := help
