package k3s

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/cluster-os/node/internal/roles"
	"github.com/cluster-os/node/internal/state"
	"github.com/sirupsen/logrus"
)

//go:embed manifests/slurm/slurmdbd.yaml
var slurmdbdManifest []byte

const (
	// MinNodesForHA is the minimum number of nodes before enabling multi-server HA
	MinNodesForHA = 3
	// MaxServers is the maximum number of K3s servers (odd number for etcd quorum)
	MaxServers = 3
)

// K3sServer implements the k3s server role
type K3sServer struct {
	*roles.BaseRole
	config             *ServerConfig
	clusterState       *state.ClusterState
	k3sCmd             *exec.Cmd
	dataDir            string
	tokenPath          string
	slurmdbdDeployed   bool
	manifestsDir       string
}

// ServerConfig contains configuration for the k3s server
type ServerConfig struct {
	DataDir        string
	TokenPath      string
	ClusterInit    bool
	DisableFlannel bool
	NodeIP         string
	EnableHA       bool   // Enable multi-server HA mode
	JoinServer     string // URL of existing server to join (for HA)
}

// NewK3sServer creates a new k3s server role
func NewK3sServer(roleConfig *roles.RoleConfig, logger *logrus.Logger) (roles.Role, error) {
	config := &ServerConfig{
		DataDir:        "/var/lib/rancher/k3s",
		TokenPath:      "/var/lib/rancher/k3s/server/token",
		ClusterInit:    false,
		DisableFlannel: false, // Enable Flannel for pod networking (Tailscale handles node connectivity)
		NodeIP:         "",
		EnableHA:       true, // Enable HA by default
		JoinServer:     "",
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
	if val, ok := roleConfig.Config["enable_ha"].(bool); ok {
		config.EnableHA = val
	}

	return &K3sServer{
		BaseRole:     roles.NewBaseRole("k3s-server", logger),
		config:       config,
		dataDir:      config.DataDir,
		tokenPath:    config.TokenPath,
		manifestsDir: "/var/lib/cluster-os/k8s-manifests",
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

	// Check if we should be running as a server
	shouldRun := ks.shouldBeServer()

	if !shouldRun {
		// We shouldn't be a server - if we're running, that's still OK
		// (could be transitioning, or cluster size decreased)
		return nil
	}

	// We should be running as a server
	if ks.k3sCmd == nil || ks.k3sCmd.Process == nil {
		// Should be server but not started - try to start
		ks.Logger().Info("k3s server should be running, attempting to start")
		if err := ks.startK3s(); err != nil {
			ks.Logger().Warnf("Failed to start k3s server: %v", err)
			// Don't return error - we'll retry on next health check
			return nil
		}
		return nil
	}

	// Check if process is still alive
	if err := ks.k3sCmd.Process.Signal(syscall.Signal(0)); err != nil {
		return fmt.Errorf("k3s server process health check failed: %w", err)
	}

	// Check if API server is responsive (with grace period for startup)
	if err := ks.checkAPIServer(); err != nil {
		// Log but don't fail - API server may still be starting
		ks.Logger().Warnf("k3s API server check: %v", err)
	} else if ks.IsLeader() && !ks.slurmdbdDeployed {
		// API is responsive and we're the leader - deploy slurmdbd
		go func() {
			time.Sleep(10 * time.Second) // Wait for cluster to stabilize
			if err := ks.deploySlurmdbd(); err != nil {
				ks.Logger().Warnf("Failed to deploy slurmdbd: %v", err)
			}
		}()
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

	// In HA mode, use dynamic server selection
	if ks.config.EnableHA {
		shouldRun := ks.shouldBeServer()

		if isLeader {
			ks.Logger().Info("Became k3s server leader (HA mode)")
			// Leader always starts
			if ks.k3sCmd == nil {
				if err := ks.startK3s(); err != nil {
					return err
				}
			}
			// Share token with cluster for other servers to join
			go ks.shareClusterToken()
		} else if shouldRun {
			ks.Logger().Info("Promoted to k3s server (HA mode - multi-server)")
			// We're a server candidate but not leader - start/join
			if ks.k3sCmd == nil {
				return ks.startK3s()
			}
		} else {
			ks.Logger().Info("Not selected as k3s server (cluster too small or not in top nodes)")
			// Not enough nodes for multi-server, and not leader - just wait
		}
		return nil
	}

	// Non-HA mode: single server, stop when not leader
	if isLeader {
		ks.Logger().Info("Became k3s server leader, starting k3s")
		return ks.startK3s()
	} else {
		ks.Logger().Info("Lost k3s server leadership, stopping k3s")
		return ks.stopK3s()
	}
}

// shareClusterToken reads and shares the K3s token with the cluster
func (ks *K3sServer) shareClusterToken() {
	// Wait a bit for K3s to generate the token
	time.Sleep(5 * time.Second)

	token, err := ks.GetToken()
	if err != nil {
		ks.Logger().Warnf("Failed to read K3s token: %v", err)
		return
	}

	if ks.clusterState != nil {
		ks.clusterState.SetK3sToken(token)
		ks.Logger().Info("K3s cluster token shared with cluster state")
	}
}

// startK3s starts the k3s server
func (ks *K3sServer) startK3s() error {
	ks.Logger().Info("Starting k3s server")

	args := []string{
		"server",
		"--data-dir", ks.dataDir,
	}

	// HA Mode: Determine if we should init a new cluster or join existing
	if ks.config.EnableHA {
		existingServer := ks.findExistingK3sServer()
		if existingServer != "" {
			// Join existing cluster
			ks.Logger().Infof("HA Mode: Joining existing K3s cluster at %s", existingServer)
			args = append(args, "--server", existingServer)

			// Get token from existing server or shared location
			token := ks.getClusterToken()
			if token != "" {
				args = append(args, "--token", token)
			}
		} else {
			// First server - initialize new cluster with embedded etcd
			ks.Logger().Info("HA Mode: Initializing new K3s cluster with embedded etcd")
			args = append(args, "--cluster-init")
		}
	} else if ks.config.ClusterInit {
		// Non-HA mode with explicit cluster-init
		args = append(args, "--cluster-init")
	}

	// Disable Flannel (we use Tailscale)
	if ks.config.DisableFlannel {
		args = append(args, "--flannel-backend=none")
	}

	// Set node IP to Tailscale IP
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

	// Register service endpoint
	if ks.clusterState != nil {
		localNode := ks.clusterState.GetLocalNode()
		if localNode != nil {
			apiIP := ks.config.NodeIP
			if apiIP == "" {
				apiIP = "127.0.0.1" // fallback
			}
			apiEndpoint := fmt.Sprintf("%s:6443", apiIP)
			ks.clusterState.UpdateServiceEndpoint("kubernetes-api", apiEndpoint, 6443, localNode.ID, "running")
			ks.Logger().Infof("Registered Kubernetes API endpoint: %s", apiEndpoint)
		}
	}

	return nil
}

// findExistingK3sServer looks for an existing K3s server in the cluster to join
func (ks *K3sServer) findExistingK3sServer() string {
	if ks.clusterState == nil {
		return ""
	}

	// Look for nodes with k3s-server role that are alive
	nodes := ks.clusterState.GetNodesByRole("k3s-server")
	localNode := ks.clusterState.GetLocalNode()

	for _, node := range nodes {
		// Skip ourselves
		if localNode != nil && node.ID == localNode.ID {
			continue
		}

		// Check if this node has a Tailscale IP (means it's reachable via mesh)
		if node.TailscaleIP != nil {
			serverURL := fmt.Sprintf("https://%s:6443", node.TailscaleIP.String())
			ks.Logger().Debugf("Found potential K3s server: %s at %s", node.Name, serverURL)
			return serverURL
		}
	}

	return ""
}

// getClusterToken retrieves the K3s cluster token for joining
func (ks *K3sServer) getClusterToken() string {
	// Try to read from shared location (distributed via Serf state)
	if ks.clusterState != nil {
		if token := ks.clusterState.GetK3sToken(); token != "" {
			return token
		}
	}

	// Try to read from local file (if previously joined)
	if data, err := os.ReadFile(ks.tokenPath); err == nil {
		return string(data)
	}

	return ""
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

// shouldBeServer determines if this node should run as a K3s server
// Returns true if:
// 1. We are the leader (always runs as server)
// 2. Cluster has >= MinNodesForHA nodes AND we are in the top MaxServers by node ID
func (ks *K3sServer) shouldBeServer() bool {
	// Leader always runs as server
	if ks.IsLeader() {
		return true
	}

	if ks.clusterState == nil {
		return false
	}

	// Check cluster size
	aliveNodes := ks.clusterState.GetAliveNodes()
	if len(aliveNodes) < MinNodesForHA {
		// Not enough nodes for multi-server HA
		return false
	}

	// Get our node ID
	localNode := ks.clusterState.GetLocalNode()
	if localNode == nil {
		return false
	}

	// Get top N nodes by lexicographic node ID (deterministic selection)
	serverNodes := ks.getServerCandidates(aliveNodes)

	// Check if we're in the server list
	for _, node := range serverNodes {
		if node.ID == localNode.ID {
			return true
		}
	}

	return false
}

// getServerCandidates returns the top MaxServers nodes sorted by node ID
func (ks *K3sServer) getServerCandidates(nodes []*state.Node) []*state.Node {
	if len(nodes) == 0 {
		return nil
	}

	// Sort by node ID (lexicographic - deterministic)
	sorted := make([]*state.Node, len(nodes))
	copy(sorted, nodes)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ID < sorted[j].ID
	})

	// Take top MaxServers
	count := MaxServers
	if len(sorted) < count {
		count = len(sorted)
	}

	return sorted[:count]
}

// deploySlurmdbd deploys the slurmdbd manifests to Kubernetes
func (ks *K3sServer) deploySlurmdbd() error {
	if ks.slurmdbdDeployed {
		return nil
	}

	ks.Logger().Info("Deploying slurmdbd to Kubernetes")

	// Create manifests directory
	if err := os.MkdirAll(ks.manifestsDir, 0755); err != nil {
		return fmt.Errorf("failed to create manifests directory: %w", err)
	}

	// Write slurmdbd manifest
	manifestPath := filepath.Join(ks.manifestsDir, "slurmdbd.yaml")
	if err := os.WriteFile(manifestPath, slurmdbdManifest, 0644); err != nil {
		return fmt.Errorf("failed to write slurmdbd manifest: %w", err)
	}

	// First, create the munge key secret if it doesn't exist
	if err := ks.createMungeKeySecret(); err != nil {
		ks.Logger().Warnf("Failed to create munge key secret: %v", err)
		// Continue anyway - it might already exist
	}

	// Apply the manifest
	cmd := exec.Command("k3s", "kubectl", "apply", "-f", manifestPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to apply slurmdbd manifest: %w: %s", err, string(output))
	}

	ks.slurmdbdDeployed = true
	ks.Logger().Info("slurmdbd deployed successfully to Kubernetes")
	return nil
}

// createMungeKeySecret creates the munge key secret in Kubernetes
func (ks *K3sServer) createMungeKeySecret() error {
	if ks.clusterState == nil {
		return fmt.Errorf("cluster state not available")
	}

	mungeKey, _, err := ks.clusterState.GetMungeKey()
	if err != nil {
		return fmt.Errorf("failed to get munge key: %w", err)
	}

	// Create namespace first
	nsCmd := exec.Command("k3s", "kubectl", "create", "namespace", "slurm", "--dry-run=client", "-o", "yaml")
	nsYaml, _ := nsCmd.Output()
	applyNs := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	applyNs.Stdin = strings.NewReader(string(nsYaml))
	applyNs.Run()

	// Write munge key to temp file
	tmpFile, err := os.CreateTemp("", "munge-key-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Write(mungeKey)
	tmpFile.Close()

	// Create secret from file
	cmd := exec.Command("k3s", "kubectl", "create", "secret", "generic", "munge-key",
		"--namespace", "slurm",
		"--from-file=munge.key="+tmpFile.Name(),
		"--dry-run=client", "-o", "yaml")
	secretYaml, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to generate munge key secret: %w", err)
	}

	applyCmd := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	applyCmd.Stdin = strings.NewReader(string(secretYaml))
	if output, err := applyCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to apply munge key secret: %w: %s", err, string(output))
	}

	return nil
}

// GetServerCount returns the current number of K3s servers in the cluster
func (ks *K3sServer) GetServerCount() int {
	if ks.clusterState == nil {
		return 0
	}

	// Count nodes with k3s-server role that are alive
	count := 0
	nodes := ks.clusterState.GetNodesByRole("k3s-server")
	for _, node := range nodes {
		if node.Status == state.StatusAlive {
			count++
		}
	}
	return count
}
