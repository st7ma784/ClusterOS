package k3s

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/cluster-os/node/internal/roles"
	"github.com/cluster-os/node/internal/state"
	"github.com/sirupsen/logrus"
)

// K3sServer implements the k3s server role
type K3sServer struct {
	*roles.BaseRole
	config       *ServerConfig
	clusterState *state.ClusterState
	k3sCmd       *exec.Cmd
	dataDir      string
	tokenPath    string
}

// ServerConfig contains configuration for the k3s server
type ServerConfig struct {
	DataDir       string
	TokenPath     string
	ClusterInit   bool
	DisableFlannel bool
	NodeIP        string
}

// NewK3sServer creates a new k3s server role
func NewK3sServer(roleConfig *roles.RoleConfig, logger *logrus.Logger) (roles.Role, error) {
	config := &ServerConfig{
		DataDir:       "/var/lib/rancher/k3s",
		TokenPath:     "/var/lib/rancher/k3s/server/token",
		ClusterInit:   false,
		DisableFlannel: true, // We use WireGuard
		NodeIP:        "",
	}

	// Override from role config
	if val, ok := roleConfig.Config["data_dir"].(string); ok {
		config.DataDir = val
	}
	if val, ok := roleConfig.Config["cluster_init"].(bool); ok {
		config.ClusterInit = val
	}
	if val, ok := roleConfig.Config["node_ip"].(string); ok {
		config.NodeIP = val
	}

	return &K3sServer{
		BaseRole: roles.NewBaseRole("k3s-server", logger),
		config:   config,
		dataDir:  config.DataDir,
		tokenPath: config.TokenPath,
	}, nil
}

// Start starts the k3s server role
func (ks *K3sServer) Start(ctx context.Context, clusterState *state.ClusterState) error {
	ks.Logger().Info("Starting k3s server role")
	ks.clusterState = clusterState

	// Create data directory
	if err := os.MkdirAll(ks.dataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	// Don't start k3s yet - wait for leadership
	ks.SetRunning(true)
	ks.Logger().Info("k3s server role started (waiting for leadership)")
	return nil
}

// Stop stops the k3s server role
func (ks *K3sServer) Stop(ctx context.Context) error {
	ks.Logger().Info("Stopping k3s server role")

	if err := ks.stopK3s(); err != nil {
		ks.Logger().Warnf("Error stopping k3s: %v", err)
	}

	ks.SetRunning(false)
	return nil
}

// Reconfigure updates the configuration
func (ks *K3sServer) Reconfigure(clusterState *state.ClusterState) error {
	ks.Logger().Info("Reconfiguring k3s server")
	ks.clusterState = clusterState

	// k3s handles most reconfiguration internally
	// Just update our cluster state reference
	return nil
}

// HealthCheck checks if k3s server is running
func (ks *K3sServer) HealthCheck() error {
	if !ks.IsRunning() {
		return fmt.Errorf("k3s server role is not running")
	}

	// If we're the leader, check if k3s is running
	if ks.IsLeader() {
		if ks.k3sCmd == nil || ks.k3sCmd.Process == nil {
			return fmt.Errorf("k3s server process is not running")
		}

		// Check if process is still alive
		if err := ks.k3sCmd.Process.Signal(syscall.Signal(0)); err != nil {
			return fmt.Errorf("k3s server process health check failed: %w", err)
		}

		// Check if API server is responsive
		if err := ks.checkAPIServer(); err != nil {
			return fmt.Errorf("k3s API server health check failed: %w", err)
		}
	}

	return nil
}

// IsLeaderRequired returns true for k3s server (multi-control-plane)
func (ks *K3sServer) IsLeaderRequired() bool {
	return true
}

// OnLeadershipChange handles leadership changes
func (ks *K3sServer) OnLeadershipChange(isLeader bool) error {
	ks.SetLeader(isLeader)

	if isLeader {
		ks.Logger().Info("Became k3s server leader, starting k3s")
		return ks.startK3s()
	} else {
		ks.Logger().Info("Lost k3s server leadership, stopping k3s")
		return ks.stopK3s()
	}
}

// startK3s starts the k3s server
func (ks *K3sServer) startK3s() error {
	ks.Logger().Info("Starting k3s server")

	args := []string{
		"server",
		"--data-dir", ks.dataDir,
	}

	// Cluster init for first server
	if ks.config.ClusterInit {
		args = append(args, "--cluster-init")
	}

	// Disable Flannel (we use WireGuard)
	if ks.config.DisableFlannel {
		args = append(args, "--flannel-backend=none")
	}

	// Set node IP to WireGuard IP
	if ks.config.NodeIP != "" {
		args = append(args, "--node-ip", ks.config.NodeIP)
		args = append(args, "--advertise-address", ks.config.NodeIP)
	}

	// Disable built-in load balancer (we may run multiple servers)
	args = append(args, "--disable", "servicelb")

	// Disable Traefik (we'll use our own ingress)
	args = append(args, "--disable", "traefik")

	// Use native snapshotter for Docker/container environments where overlayfs may not work
	args = append(args, "--snapshotter", "native")

	cmd := exec.Command("k3s", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start k3s: %w", err)
	}

	ks.k3sCmd = cmd
	ks.Logger().Info("k3s server started successfully")

	return nil
}

// stopK3s stops the k3s server
func (ks *K3sServer) stopK3s() error {
	if ks.k3sCmd == nil || ks.k3sCmd.Process == nil {
		return nil
	}

	ks.Logger().Info("Stopping k3s server")

	// Send terminate signal
	if err := ks.k3sCmd.Process.Signal(os.Interrupt); err != nil {
		ks.Logger().Warnf("Failed to send interrupt signal: %v", err)
		// Try kill
		if err := ks.k3sCmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill k3s: %w", err)
		}
	}

	// Wait for process to exit
	if err := ks.k3sCmd.Wait(); err != nil {
		ks.Logger().Warnf("k3s exit error: %v", err)
	}

	ks.k3sCmd = nil
	ks.Logger().Info("k3s server stopped")

	return nil
}

// checkAPIServer checks if the k3s API server is responsive
func (ks *K3sServer) checkAPIServer() error {
	// Simple check: kubectl get nodes
	cmd := exec.Command("k3s", "kubectl", "get", "nodes")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("API server not responsive: %w", err)
	}
	return nil
}

// GetToken returns the cluster token for agents to join
func (ks *K3sServer) GetToken() (string, error) {
	if _, err := os.Stat(ks.tokenPath); os.IsNotExist(err) {
		return "", fmt.Errorf("token file not found")
	}

	token, err := os.ReadFile(ks.tokenPath)
	if err != nil {
		return "", fmt.Errorf("failed to read token: %w", err)
	}

	return string(token), nil
}
