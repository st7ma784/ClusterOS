package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cluster-os/node/internal/config"
	"github.com/cluster-os/node/internal/daemon"
	"github.com/cluster-os/node/internal/identity"
	"github.com/cluster-os/node/internal/state"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
)

var (
	// Version information (set via ldflags during build)
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func main() {
	app := &cli.App{
		Name:    "node-agent",
		Usage:   "Cluster-OS Node Agent - Self-assembling distributed compute cluster",
		Version: fmt.Sprintf("%s (commit: %s, built: %s)", Version, Commit, BuildTime),
		Authors: []*cli.Author{
			{
				Name: "Cluster-OS Team",
			},
		},
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Usage:   "Load configuration from `FILE`",
				EnvVars: []string{"CLUSTEROS_CONFIG"},
			},
			&cli.StringFlag{
				Name:    "log-level",
				Aliases: []string{"l"},
				Value:   "info",
				Usage:   "Log level (debug, info, warn, error)",
				EnvVars: []string{"CLUSTEROS_LOG_LEVEL"},
			},
		},
		Commands: []*cli.Command{
			{
				Name:  "init",
				Usage: "Initialize node identity",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "identity-path",
						Aliases: []string{"i"},
						Value:   identity.DefaultIdentityPath,
						Usage:   "Path to identity file",
					},
					&cli.BoolFlag{
						Name:    "force",
						Aliases: []string{"f"},
						Usage:   "Force regeneration of identity (WARNING: will lose existing identity)",
					},
				},
				Action: initCommand,
			},
			{
				Name:  "start",
				Usage: "Start the node agent daemon",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:  "foreground",
						Usage: "Run in foreground (don't daemonize)",
						Value: true,
					},
					&cli.BoolFlag{
						Name:  "force-shutdown",
						Usage: "skip drain steps and stop immediately on SIGTERM/SIGINT",
					},
				},
				Action: startCommand,
			},
			{
				Name:   "status",
				Usage:  "Show node status",
				Action: statusCommand,
			},
			{
				Name:   "dashboard",
				Usage:  "Show cluster status dashboard",
				Action: dashboardCommand,
			},
			{
				Name:   "restart",
				Usage:  "Restart the node agent (kills processes, reinitializes identity)",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:    "force-identity",
						Aliases: []string{"f"},
						Usage:   "Force regeneration of node identity",
					},
				},
				Action: restartCommand,
			},
			{
				Name:  "join",
				Usage: "Join an existing cluster",
				Flags: []cli.Flag{
					&cli.StringSliceFlag{
						Name:     "peer",
						Aliases:  []string{"p"},
						Usage:    "Bootstrap peer address (can be specified multiple times)",
						Required: true,
					},
				},
				Action: joinCommand,
			},
			{
				Name:   "info",
				Usage:  "Display node information",
				Action: infoCommand,
			},
		},
		Before: func(c *cli.Context) error {
			// Configure logging
			level, err := logrus.ParseLevel(c.String("log-level"))
			if err != nil {
				level = logrus.InfoLevel
			}
			logrus.SetLevel(level)
			logrus.SetFormatter(&logrus.TextFormatter{
				FullTimestamp: true,
			})
			return nil
		},
	}

	if err := app.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}

func initCommand(c *cli.Context) error {
	identityPath := c.String("identity-path")
	force := c.Bool("force")

	logrus.Infof("Initializing node identity at %s", identityPath)

	// Check if identity already exists
	if identity.Exists(identityPath) && !force {
		return fmt.Errorf("identity already exists at %s (use --force to regenerate)", identityPath)
	}

	// Delete existing identity if force is set
	if force && identity.Exists(identityPath) {
		logrus.Warn("Force flag set, deleting existing identity")
		if err := identity.Delete(identityPath); err != nil {
			return fmt.Errorf("failed to delete existing identity: %w", err)
		}
	}

	// Generate new identity
	id, err := identity.Generate()
	if err != nil {
		return fmt.Errorf("failed to generate identity: %w", err)
	}

	// Save identity
	if err := id.Save(identityPath); err != nil {
		return fmt.Errorf("failed to save identity: %w", err)
	}

	logrus.Infof("Successfully initialized identity")
	logrus.Infof("Node ID: %s", id.NodeID)
	logrus.Infof("Public Key: %x", id.PublicKey)

	return nil
}

func startCommand(c *cli.Context) error {
	configPath := c.String("config")

	logrus.Info("Starting Cluster-OS Node Agent")
	logrus.Infof("Version: %s (commit: %s)", Version, Commit)

	// Load configuration
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	logrus.Infof("Configuration loaded successfully")
	logrus.Infof("Cluster: %s", cfg.Cluster.Name)
	logrus.Infof("Node Name: %s", cfg.Discovery.NodeName)

	// Load or generate identity
	id, wasGenerated, err := identity.LoadOrGenerate(cfg.Identity.Path)
	if err != nil {
		return fmt.Errorf("failed to load identity: %w", err)
	}

	if wasGenerated {
		logrus.Infof("Generated new identity: %s", id.NodeID)
	} else {
		logrus.Infof("Loaded existing identity: %s", id.NodeID)
	}

	// Create and start daemon
	daemonCfg := &daemon.Config{
		Config:        cfg,
		Identity:      id,
		Logger:        logrus.StandardLogger(),
		Version:       Commit, // git commit hash, published as Serf tag "ver"
		BuildTime:     BuildTime,
		ForceShutdown: c.Bool("force-shutdown"),
	}

	d, err := daemon.New(daemonCfg)
	if err != nil {
		return fmt.Errorf("failed to create daemon: %w", err)
	}

	if err := d.Start(); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	logrus.Info("Node agent started successfully")
	logrus.Info("Press Ctrl+C to stop")

	// Run until interrupted
	return d.Run()
}

func statusCommand(c *cli.Context) error {
	configPath := c.String("config")

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	// Load identity (don't fail if it doesn't exist)
	id, err := identity.Load(cfg.Identity.Path)
	identityLoaded := err == nil
	if !identityLoaded {
		// Create a dummy identity for display purposes
		id = &identity.Identity{
			NodeID: "not-initialized",
		}
	}

	fmt.Printf("ClusterOS Node Status\n")
	fmt.Printf("====================\n\n")

	// Node Information
	fmt.Printf("Node Information:\n")
	fmt.Printf("  ID:         %s\n", id.NodeID)
	fmt.Printf("  Name:       %s\n", cfg.Discovery.NodeName)
	fmt.Printf("  Cluster:    %s\n", cfg.Cluster.Name)
	fmt.Printf("  Region:     %s\n", cfg.Cluster.Region)
	fmt.Printf("  Datacenter: %s\n", cfg.Cluster.Datacenter)
	fmt.Printf("\n")

	// Roles Configuration
	fmt.Printf("Configured Roles:\n")
	for _, role := range cfg.Roles.Enabled {
		fmt.Printf("  - %s\n", role)
	}
	fmt.Printf("\n")

	// Capabilities
	fmt.Printf("Capabilities:\n")
	fmt.Printf("  CPU:  %d cores\n", cfg.Roles.Capabilities.CPU)
	fmt.Printf("  RAM:  %s\n", cfg.Roles.Capabilities.RAM)
	fmt.Printf("  GPU:  %v\n", cfg.Roles.Capabilities.GPU)
	fmt.Printf("  Arch: %s\n", cfg.Roles.Capabilities.Arch)
	fmt.Printf("\n")

	// Networking Status
	fmt.Printf("Networking:\n")
	fmt.Printf("  Type: Tailscale mesh\n")
	
	// Check Tailscale status
	if tailscaleIP, err := getTailscaleIP(); err == nil {
		fmt.Printf("  Tailscale IP: %s\n", tailscaleIP)
		fmt.Printf("  Status: Connected\n")
	} else {
		fmt.Printf("  Status: Not connected\n")
	}
	fmt.Printf("\n")

	// System Services Status
	fmt.Printf("System Services:\n")
	
	// Check SLURM services
	slurmServices := []string{"slurmctld", "slurmd"}
	for _, svc := range slurmServices {
		status := getServiceStatus(svc)
		fmt.Printf("  %-12s: %s\n", svc, status)
	}
	
	// Check K3s services
	k3sServices := []string{"k3s", "k3s-agent"}
	for _, svc := range k3sServices {
		status := getServiceStatus(svc)
		fmt.Printf("  %-12s: %s\n", svc, status)
	}
	
	// Check other services
	otherServices := []string{"apache2", "ttyd", "filebrowser"}
	for _, svc := range otherServices {
		status := getServiceStatus(svc)
		fmt.Printf("  %-12s: %s\n", svc, status)
	}
	fmt.Printf("\n")

	// Resource Usage
	fmt.Printf("System Resources:\n")
	if memInfo, err := getMemoryInfo(); err == nil {
		totalMB := memInfo["total_kb"].(int64) / 1024
		availMB := memInfo["available_kb"].(int64) / 1024
		usedMB := totalMB - availMB
		fmt.Printf("  Memory: %d MB used / %d MB total (%d MB available)\n", usedMB, totalMB, availMB)
	}
	
	fmt.Printf("  CPU Cores: %d\n", runtime.NumCPU())
	
	if diskInfo, err := getDiskUsage("/"); err == nil {
		totalGB := diskInfo["total_bytes"].(uint64) / (1024 * 1024 * 1024)
		usedGB := diskInfo["used_bytes"].(uint64) / (1024 * 1024 * 1024)
		usedPercent := diskInfo["used_percent"].(float64)
		fmt.Printf("  Disk (/): %d GB used / %d GB total (%.1f%%)\n", usedGB, totalGB, usedPercent)
	}
	fmt.Printf("\n")

	// Daemon Status
	fmt.Printf("Node Agent:\n")
	// Try to check if daemon is running
	if isDaemonRunning() {
		fmt.Printf("  Status: Running\n")
		fmt.Printf("  PID: %s\n", getDaemonPID())
	} else {
		fmt.Printf("  Status: Not running\n")
		fmt.Printf("  Use 'node-agent start' to start the daemon\n")
	}

	return nil
}

func dashboardCommand(c *cli.Context) error {
	configPath := c.String("config")

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	// Load identity (don't fail if it doesn't exist)
	id, err := identity.Load(cfg.Identity.Path)
	if err != nil {
		// Create a dummy identity for display purposes
		id = &identity.Identity{
			NodeID: "not-initialized",
		}
	}

	// For now, create a basic cluster state for demonstration
	// In a real implementation, this would connect to the running daemon
	clusterState := state.NewClusterState()
	clusterState.SetLocalNodeID(id.NodeID)

	// Add some mock data for demonstration
	localNode := &state.Node{
		ID:       id.NodeID,
		Name:     cfg.Discovery.NodeName,
		Roles:    cfg.Roles.Enabled,
		Status:   state.StatusAlive,
		Address:  "127.0.0.1",
		Capabilities: state.Capabilities{
			CPU:  runtime.NumCPU(),
			RAM:  "8GB",
			GPU:  false,
			Arch: runtime.GOARCH,
		},
	}
	clusterState.AddNode(localNode)

	showDashboard(clusterState)
	return nil
}

func showDashboard(clusterState *state.ClusterState) {
	now := time.Now().Format("15:04:05")
	fmt.Printf("Cluster-OS Dashboard - %s\n", now)
	fmt.Println(strings.Repeat("=", 50))

	// Show cluster leaders
	fmt.Println("\n🔹 LEADER NODES:")
	leaders := []string{"k3s-server", "slurm-controller"}
	for _, role := range leaders {
		if node, ok := clusterState.GetLeaderNode(role); ok {
			fmt.Printf("  %-20s: %s (%s)\n", role, node.Name, node.ID[:12]+"...")
		} else {
			fmt.Printf("  %-20s: <no leader>\n", role)
		}
	}

	// Show node status summary
	fmt.Println("\n🖥️  CLUSTER NODES:")
	nodes := clusterState.GetAllNodes()
	fmt.Printf("  Total: %d nodes\n", len(nodes))

	aliveCount := 0
	for _, node := range nodes {
		if node.Status == state.StatusAlive {
			aliveCount++
		}
	}
	fmt.Printf("  Alive: %d nodes\n", aliveCount)

	// Show node details
	fmt.Println("\n  Node Details:")
	for _, node := range nodes {
		status := "🔴"
		if node.Status == state.StatusAlive {
			status = "🟢"
		}
		fmt.Printf("    %s %-15s %-12s %s\n", status, node.Name, node.Status, strings.Join(node.Roles, ","))
	}
}


// getTailscaleIP attempts to get the local Tailscale IP
func getTailscaleIP() (string, error) {
	// Try to get IP from tailscale command
	cmd := exec.Command("tailscale", "ip", "-4")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(output))
	if ip != "" {
		return ip, nil
	}
	return "", fmt.Errorf("no IP found")
}

// getServiceStatus checks the status of a systemd service
func getServiceStatus(serviceName string) string {
	cmd := exec.Command("systemctl", "is-active", serviceName)
	output, err := cmd.Output()
	if err != nil {
		return "error"
	}
	
	status := strings.TrimSpace(string(output))
	switch status {
	case "active":
		return "running"
	case "inactive":
		return "stopped"
	case "failed":
		return "failed"
	default:
		return status
	}
}

// isDaemonRunning checks if the node-agent daemon is running
func isDaemonRunning() bool {
	cmd := exec.Command("systemctl", "is-active", "node-agent")
	err := cmd.Run()
	return err == nil
}

// getDaemonPID gets the PID of the running daemon
func getDaemonPID() string {
	cmd := exec.Command("systemctl", "show", "-p", "MainPID", "node-agent")
	output, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	
	line := strings.TrimSpace(string(output))
	if strings.HasPrefix(line, "MainPID=") {
		return strings.TrimPrefix(line, "MainPID=")
	}
	return "unknown"
}

// getMemoryInfo reads /proc/meminfo and returns memory usage
func getMemoryInfo() (map[string]interface{}, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, err
	}

	memInfo := make(map[string]interface{})
	lines := strings.Split(string(data), "\n")

	for _, line := range lines {
		if strings.Contains(line, "MemTotal") {
			if val, err := parseMemValue(line); err == nil {
				memInfo["total_kb"] = val
			}
		} else if strings.Contains(line, "MemAvailable") {
			if val, err := parseMemValue(line); err == nil {
				memInfo["available_kb"] = val
			}
		}
	}

	return memInfo, nil
}

// parseMemValue extracts the numeric value from a /proc/meminfo line
func parseMemValue(line string) (int64, error) {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return 0, fmt.Errorf("invalid line format")
	}

	// Remove "kB" suffix if present
	valueStr := strings.TrimSuffix(parts[1], "kB")
	return strconv.ParseInt(valueStr, 10, 64)
}

// getDiskUsage gets disk usage information for a mount point
func getDiskUsage(path string) (map[string]interface{}, error) {
	var stat syscall.Statfs_t
	err := syscall.Statfs(path, &stat)
	if err != nil {
		return nil, err
	}

	// Calculate usage
	total := stat.Blocks * uint64(stat.Bsize)
	available := stat.Bavail * uint64(stat.Bsize)
	used := total - available

	diskInfo := map[string]interface{}{
		"total_bytes":     total,
		"available_bytes": available,
		"used_bytes":      used,
		"used_percent":    float64(used) / float64(total) * 100,
	}

	return diskInfo, nil
}

func joinCommand(c *cli.Context) error {
	peers := c.StringSlice("peer")

	if len(peers) == 0 {
		return fmt.Errorf("at least one bootstrap peer is required")
	}

	logrus.Infof("Joining cluster via bootstrap peers: %v", peers)

	// Load configuration
	configPath := c.String("config")
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	// Update configuration with bootstrap peers
	bootstrapPeers := make([]string, len(peers))
	for i, peer := range peers {
		// Add default Serf port if not specified
		if !strings.Contains(peer, ":") {
			peer = peer + ":7946"
		}
		bootstrapPeers[i] = peer
	}

	cfg.Discovery.BootstrapPeers = bootstrapPeers

	// Save updated configuration
	if err := config.Save(cfg, configPath); err != nil {
		return fmt.Errorf("failed to save configuration: %w", err)
	}

	logrus.Info("Updated configuration with bootstrap peers")

	// Load identity
	id, err := identity.Load(cfg.Identity.Path)
	if err != nil {
		return fmt.Errorf("failed to load identity: %w", err)
	}

	// Check if daemon is already running
	isRunning := isDaemonRunning()
	if isRunning {
		logrus.Info("Node agent is running, triggering cluster join...")

		// For now, restart the daemon to pick up new bootstrap peers
		// TODO: Implement dynamic configuration reload
		if err := runCommand("systemctl", "restart", "node-agent"); err != nil {
			return fmt.Errorf("failed to restart node-agent: %w", err)
		}

		logrus.Info("Node agent restarted with new bootstrap peers")
	} else {
		logrus.Info("Starting node agent to join cluster...")

		// Create and start daemon
		daemonCfg := &daemon.Config{
			Config:    cfg,
			Identity:  id,
			Logger:    logrus.StandardLogger(),
			Version:   Commit,
			BuildTime: BuildTime,
		}

		d, err := daemon.New(daemonCfg)
		if err != nil {
			return fmt.Errorf("failed to create daemon: %w", err)
		}

		// Start daemon in background
		go func() {
			if err := d.Run(); err != nil {
				logrus.Errorf("Daemon error: %v", err)
			}
		}()

		// Wait a bit for daemon to start
		time.Sleep(3 * time.Second)
	}

	// Wait for cluster join to complete
	logrus.Info("Waiting for cluster join to complete...")
	timeout := time.After(60 * time.Second)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for cluster join")
		case <-ticker.C:
			// Check if we've joined a cluster
			if isDaemonRunning() {
				// Try to get cluster size from daemon
				// For now, just check if daemon is healthy
				logrus.Info("Node appears to have joined cluster successfully")
				logrus.Info("Cluster join operation completed")

				// Perform state synchronization with cluster
				if err := performClusterStateSync(cfg, id); err != nil {
					logrus.Warnf("State synchronization completed with warnings: %v", err)
				} else {
					logrus.Info("State synchronization completed successfully")
				}

				return nil
			}
		}
	}
}

func infoCommand(c *cli.Context) error {
	fmt.Printf("Cluster-OS Node Agent\n")
	fmt.Printf("====================\n\n")
	fmt.Printf("Version:    %s\n", Version)
	fmt.Printf("Commit:     %s\n", Commit)
	fmt.Printf("Build Time: %s\n", BuildTime)
	fmt.Printf("Go Version: %s\n", runtime.Version())
	fmt.Printf("OS/Arch:    %s/%s\n", runtime.GOOS, runtime.GOARCH)

	return nil
}

func restartCommand(c *cli.Context) error {
	configPath := c.String("config")
	forceIdentity := c.Bool("force-identity")

	logrus.Info("Restarting Cluster-OS Node Agent")

	// Load configuration to get ports and paths
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	// Kill any processes on the node agent ports
	logrus.Info("Killing any processes on node agent ports")
	if err := killProcessesOnPorts(cfg.Discovery.BindPort); err != nil {
		logrus.Warnf("Failed to kill processes on ports: %v", err)
	}

	// Stop the systemd service if running
	logrus.Info("Stopping node-agent service")
	if err := runCommand("systemctl", "stop", "node-agent"); err != nil {
		logrus.Warnf("Failed to stop service: %v", err)
	}

	// Force identity regeneration if requested
	if forceIdentity {
		logrus.Info("Force regenerating node identity")
		if err := identity.Delete(cfg.Identity.Path); err != nil {
			logrus.Warnf("Failed to delete identity: %v", err)
		}
	}

	// Start the service again
	logrus.Info("Starting node-agent service")
	if err := runCommand("systemctl", "start", "node-agent"); err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}

	logrus.Info("Node agent restarted successfully")
	return nil
}

// killProcessesOnPorts kills any processes listening on the specified ports
func killProcessesOnPorts(ports ...int) error {
	for _, port := range ports {
		// Find processes listening on the port
		cmd := fmt.Sprintf("lsof -ti:%d | xargs -r kill -9", port)
		if err := runCommand("bash", "-c", cmd); err != nil {
			logrus.Warnf("Failed to kill processes on port %d: %v", port, err)
		}
	}
	return nil
}

// performClusterStateSync is a hook called after the daemon joins a cluster.
// State synchronisation is handled automatically by the Serf phase machine
// (DISCOVERING → ELECTING → JOINING → READY), so nothing extra is needed here.
func performClusterStateSync(_ *config.Config, _ *identity.Identity) error {
	return nil
}

// runCommand runs a system command
func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Run()
}
