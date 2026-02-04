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

// K3sAgent implements the k3s agent (worker) role
type K3sAgent struct {
	*roles.BaseRole
	config       *AgentConfig
	clusterState *state.ClusterState
	k3sCmd       *exec.Cmd
	dataDir      string
}

// AgentConfig contains configuration for the k3s agent
type AgentConfig struct {
	DataDir    string
	ServerURL  string
	Token      string
	NodeIP     string
}

// NewK3sAgent creates a new k3s agent role
func NewK3sAgent(roleConfig *roles.RoleConfig, logger *logrus.Logger) (roles.Role, error) {
	config := &AgentConfig{
		DataDir:   "/var/lib/rancher/k3s",
		ServerURL: "",
		Token:     "",
		NodeIP:    "",
	}

	// Override from role config
	if val, ok := roleConfig.Config["data_dir"].(string); ok {
		config.DataDir = val
	}
	if val, ok := roleConfig.Config["server_url"].(string); ok {
		config.ServerURL = val
	}
	if val, ok := roleConfig.Config["token"].(string); ok {
		config.Token = val
	}
	if val, ok := roleConfig.Config["node_ip"].(string); ok {
		config.NodeIP = val
	}

	return &K3sAgent{
		BaseRole: roles.NewBaseRole("k3s-agent", logger),
		config:   config,
		dataDir:  config.DataDir,
	}, nil
}

// Start starts the k3s agent role
func (ka *K3sAgent) Start(ctx context.Context, clusterState *state.ClusterState) error {
	ka.Logger().Info("Starting k3s agent role")
	ka.clusterState = clusterState

	// Create data directory
	if err := os.MkdirAll(ka.dataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	// Don't start k3s agent immediately - wait for a server to be available
	// The agent will be started when Reconfigure detects a k3s-server leader
	// or when OnLeadershipChange is called
	serverNode, ok := ka.clusterState.GetLeaderNode("k3s-server")
	if !ok || serverNode.Address == "" {
		ka.Logger().Info("k3s agent role started (waiting for k3s-server to be available)")
		ka.SetRunning(true)
		return nil
	}

	// Server is available, start the agent
	if err := ka.startK3s(); err != nil {
		// Don't fail the role start - just log and wait for reconfigure
		ka.Logger().Warnf("Failed to start k3s agent (will retry): %v", err)
	}

	ka.SetRunning(true)
	ka.Logger().Info("k3s agent role started")
	return nil
}

// Stop stops the k3s agent role
func (ka *K3sAgent) Stop(ctx context.Context) error {
	ka.Logger().Info("Stopping k3s agent role")

	if err := ka.stopK3s(); err != nil {
		ka.Logger().Warnf("Error stopping k3s: %v", err)
	}

	ka.SetRunning(false)
	return nil
}

// Reconfigure updates the configuration
func (ka *K3sAgent) Reconfigure(clusterState *state.ClusterState) error {
	ka.Logger().Info("Reconfiguring k3s agent")
	ka.clusterState = clusterState

	// Check if server URL changed
	serverNode, ok := ka.clusterState.GetLeaderNode("k3s-server")
	if ok && serverNode.Address != "" {
		newServerURL := fmt.Sprintf("https://%s:6443", serverNode.Address)
		if newServerURL != ka.config.ServerURL {
			ka.Logger().Infof("Server URL changed to %s, restarting", newServerURL)
			ka.config.ServerURL = newServerURL

			// Restart agent with new server URL
			if err := ka.stopK3s(); err != nil {
				ka.Logger().Warnf("Error stopping k3s: %v", err)
			}
			return ka.startK3s()
		}
	}

	return nil
}

// HealthCheck checks if k3s agent is running
func (ka *K3sAgent) HealthCheck() error {
	if !ka.IsRunning() {
		return fmt.Errorf("k3s agent role is not running")
	}

	// If we haven't started k3s yet (waiting for server), that's OK
	if ka.k3sCmd == nil || ka.k3sCmd.Process == nil {
		// Check if there's a server available - if not, waiting is expected
		serverNode, ok := ka.clusterState.GetLeaderNode("k3s-server")
		if !ok || serverNode.Address == "" {
			// No server available yet, this is expected - not an error
			return nil
		}
		// Server exists but we're not connected - try to connect
		ka.Logger().Info("k3s server detected, attempting to connect")
		if err := ka.startK3s(); err != nil {
			ka.Logger().Warnf("Failed to connect to k3s server: %v", err)
			// Don't return error - we'll retry on next health check
			return nil
		}
		return nil
	}

	// Check if process is still alive
	if err := ka.k3sCmd.Process.Signal(syscall.Signal(0)); err != nil {
		return fmt.Errorf("k3s agent process health check failed: %w", err)
	}

	return nil
}

// IsLeaderRequired returns false for k3s agent
func (ka *K3sAgent) IsLeaderRequired() bool {
	return false
}

// OnLeadershipChange is not used for agents
func (ka *K3sAgent) OnLeadershipChange(isLeader bool) error {
	return nil
}

// startK3s starts the k3s agent
func (ka *K3sAgent) startK3s() error {
	ka.Logger().Info("Starting k3s agent")

	// Get server URL from cluster state if not configured
	if ka.config.ServerURL == "" {
		serverNode, ok := ka.clusterState.GetLeaderNode("k3s-server")
		if !ok {
			return fmt.Errorf("no k3s server found in cluster")
		}
		ka.config.ServerURL = fmt.Sprintf("https://%s:6443", serverNode.Address)
	}

	// Get token from cluster state or config
	// In production, this should be securely distributed
	if ka.config.Token == "" {
		ka.Logger().Warn("No k3s token configured, using placeholder")
		ka.config.Token = "placeholder-token"
	}

	args := []string{
		"agent",
		"--server", ka.config.ServerURL,
		"--token", ka.config.Token,
		"--data-dir", ka.dataDir,
	}

	// Set node IP to Tailscale IP
	if ka.config.NodeIP != "" {
		args = append(args, "--node-ip", ka.config.NodeIP)
	}

	cmd := exec.Command("k3s", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start k3s agent: %w", err)
	}

	ka.k3sCmd = cmd
	ka.Logger().Infof("k3s agent started successfully, connecting to %s", ka.config.ServerURL)

	return nil
}

// stopK3s stops the k3s agent
func (ka *K3sAgent) stopK3s() error {
	if ka.k3sCmd == nil || ka.k3sCmd.Process == nil {
		return nil
	}

	ka.Logger().Info("Stopping k3s agent")

	// Send terminate signal
	if err := ka.k3sCmd.Process.Signal(os.Interrupt); err != nil {
		ka.Logger().Warnf("Failed to send interrupt signal: %v", err)
		// Try kill
		if err := ka.k3sCmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill k3s: %w", err)
		}
	}

	// Wait for process to exit
	if err := ka.k3sCmd.Wait(); err != nil {
		ka.Logger().Warnf("k3s exit error: %v", err)
	}

	ka.k3sCmd = nil
	ka.Logger().Info("k3s agent stopped")

	return nil
}
