package k3s

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/cluster-os/node/internal/roles"
	"github.com/sirupsen/logrus"
)

// K3sAgent manages the k3s agent process on worker nodes.
// All connection details (serverURL, token) are passed explicitly by the
// phase machine — no cluster-state lookups, no retry loops.
type K3sAgent struct {
	*roles.BaseRole
	serverURL string
	token     string
	nodeIP    string
	dataDir   string
	k3sCmd    *exec.Cmd
}

// NewK3sAgentRole creates a K3sAgent with explicit connection parameters.
func NewK3sAgentRole(serverURL, token, nodeIP string, logger *logrus.Logger) *K3sAgent {
	return &K3sAgent{
		BaseRole:  roles.NewBaseRole("k3s-agent", logger),
		serverURL: serverURL,
		token:     token,
		nodeIP:    nodeIP,
		dataDir:   "/var/lib/rancher/k3s",
	}
}

// Start starts the k3s agent connecting to the given server.
// If a local k3s server is already running, this is a no-op.
func (ka *K3sAgent) Start() error {
	ka.Logger().Infof("Starting k3s agent → %s", ka.serverURL)

	if err := os.MkdirAll(ka.dataDir, 0755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	// Don't start agent if a local k3s server is already running
	if ka.isLocalK3sServerRunning() {
		ka.Logger().Info("Local k3s server running — skipping agent start")
		ka.SetRunning(true)
		return nil
	}

	if ka.serverURL == "" {
		return fmt.Errorf("k3s server URL is empty")
	}
	if ka.token == "" {
		return fmt.Errorf("k3s token is empty")
	}

	args := []string{
		"agent",
		"--server", ka.serverURL,
		"--token", ka.token,
		"--data-dir", ka.dataDir,
	}
	if ka.nodeIP != "" {
		args = append(args, "--node-ip", ka.nodeIP)
	}

	cmd := exec.Command("k3s", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("k3s agent start: %w", err)
	}

	ka.k3sCmd = cmd
	ka.SetRunning(true)
	ka.Logger().Infof("k3s agent started (PID %d)", cmd.Process.Pid)
	return nil
}

// HealthCheck verifies the k3s agent process is alive.
func (ka *K3sAgent) HealthCheck() error {
	if ka.k3sCmd == nil || ka.k3sCmd.Process == nil {
		return fmt.Errorf("k3s agent not started")
	}
	if err := ka.k3sCmd.Process.Signal(syscall.Signal(0)); err != nil {
		ka.SetRunning(false)
		return fmt.Errorf("k3s agent process dead: %w", err)
	}
	return nil
}

// Stop terminates the k3s agent process.
func (ka *K3sAgent) Stop(ctx context.Context) error {
	ka.Logger().Info("Stopping k3s agent")
	if ka.k3sCmd == nil || ka.k3sCmd.Process == nil {
		return nil
	}
	if err := ka.k3sCmd.Process.Signal(os.Interrupt); err != nil {
		ka.k3sCmd.Process.Kill()
	}
	ka.k3sCmd.Wait()
	ka.k3sCmd = nil
	ka.SetRunning(false)
	return nil
}

// isLocalK3sServerRunning returns true if a k3s server is listening on port 6443
func (ka *K3sAgent) isLocalK3sServerRunning() bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:6443", 1*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
