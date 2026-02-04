package controller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"text/template"
	"time"

	"github.com/cluster-os/node/internal/roles"
	"github.com/cluster-os/node/internal/services/slurm/auth"
	"github.com/cluster-os/node/internal/state"
	"github.com/sirupsen/logrus"
)

// SLURMController implements the SLURM controller role with HA support
type SLURMController struct {
	*roles.BaseRole
	config          *Config
	clusterState    *state.ClusterState
	leaderElector   auth.RaftMungeKeyApplier
	slurmctldCmd    *exec.Cmd
	mungeKey        []byte
	mungeKeyManager *auth.MungeKeyManager
	configPath      string
	statePath       string
	slurmConfPath   string
}

// Config contains configuration for the SLURM controller
type Config struct {
	ConfigPath      string
	StatePath       string
	SlurmConfPath   string
	ClusterName     string
	Port            int
	EnableHA        bool
	UseKubeSlurmdbd bool   // Use Kubernetes-hosted slurmdbd
	SlurmdbdHost    string // Host for slurmdbd (K8s service or node)
	SlurmdbdPort    int    // Port for slurmdbd
}

// NewSLURMController creates a new SLURM controller role with HA support
func NewSLURMController(roleConfig *roles.RoleConfig, logger *logrus.Logger) (roles.Role, error) {
	config := &Config{
		ConfigPath:      "/etc/slurm",
		StatePath:       "/var/lib/slurm",
		SlurmConfPath:   "/etc/slurm/slurm.conf",
		ClusterName:     "cluster-os",
		Port:            6817,
		EnableHA:        true,  // Enable HA by default
		UseKubeSlurmdbd: true,  // Use K8s-hosted slurmdbd by default
		SlurmdbdHost:    "",    // Auto-detect
		SlurmdbdPort:    30819, // NodePort for slurmdbd
	}

	// Override from role config
	if val, ok := roleConfig.Config["config_path"].(string); ok {
		config.ConfigPath = val
	}
	if val, ok := roleConfig.Config["state_path"].(string); ok {
		config.StatePath = val
	}
	if val, ok := roleConfig.Config["cluster_name"].(string); ok {
		config.ClusterName = val
	}
	if val, ok := roleConfig.Config["enable_ha"].(bool); ok {
		config.EnableHA = val
	}
	if val, ok := roleConfig.Config["use_kube_slurmdbd"].(bool); ok {
		config.UseKubeSlurmdbd = val
	}
	if val, ok := roleConfig.Config["slurmdbd_host"].(string); ok {
		config.SlurmdbdHost = val
	}

	return &SLURMController{
		BaseRole:        roles.NewBaseRole("slurm-controller", logger),
		config:          config,
		slurmConfPath:   config.SlurmConfPath,
		configPath:      config.ConfigPath,
		statePath:       config.StatePath,
		leaderElector:   roleConfig.LeaderElector,
		mungeKeyManager: auth.NewMungeKeyManager(logger),
	}, nil
}

// Start starts the SLURM controller role
func (sc *SLURMController) Start(ctx context.Context, clusterState *state.ClusterState) error {
	sc.Logger().Info("Starting SLURM controller role")
	sc.clusterState = clusterState

	// Create necessary directories
	if err := sc.createDirectories(); err != nil {
		return fmt.Errorf("failed to create directories: %w", err)
	}

	// Determine if we're primary or backup controller
	sc.determineControllerRole()

	sc.SetRunning(true)
	sc.Logger().Infof("SLURM controller role started (role: %s)", sc.getRoleString())
	return nil
}

// Stop stops the SLURM controller role
func (sc *SLURMController) Stop(ctx context.Context) error {
	sc.Logger().Info("Stopping SLURM controller role")

	// Stop slurmctld if running
	if err := sc.stopSlurmctld(); err != nil {
		sc.Logger().Warnf("Error stopping slurmctld: %v", err)
	}

	sc.SetRunning(false)
	return nil
}

// determineControllerRole determines if this node is primary or backup controller
func (sc *SLURMController) determineControllerRole() {
	if !sc.config.EnableHA {
		sc.isBackup = false
		return
	}

	// Get all controller nodes
	controllers := sc.clusterState.GetNodesByRole("slurm-controller")

	// Sort by node name for deterministic assignment
	// First controller is primary, second is backup
	if len(controllers) >= 2 {
		// Find our position in the sorted list
		myNode := sc.clusterState.GetLocalNode()
		for i, node := range controllers {
			if node.Name == myNode.Name {
				sc.isBackup = (i > 0) // First is primary, others are backup
				if sc.isBackup && i == 1 {
					sc.backupAddr = controllers[0].Address // Primary address
				}
				break
			}
		}
	} else {
		sc.isBackup = false // Single controller = primary
	}
}

func (sc *SLURMController) getRoleString() string {
	if sc.isBackup {
		return "backup"
	}
	return "primary"
}

// Reconfigure regenerates configuration and restarts if leader
func (sc *SLURMController) Reconfigure(clusterState *state.ClusterState) error {
	sc.Logger().Info("Reconfiguring SLURM controller")
	sc.clusterState = clusterState

	// Re-determine our role in case cluster membership changed
	sc.determineControllerRole()

	// Only reconfigure if we're the leader
	if sc.IsLeader() {
		// Regenerate configuration
		if err := sc.generateConfig(); err != nil {
			return fmt.Errorf("failed to generate config: %w", err)
		}

		// Reload slurmctld
		if sc.slurmctldCmd != nil && sc.slurmctldCmd.Process != nil {
			sc.Logger().Info("Reloading slurmctld configuration")
			// Send SIGHUP to reload config
			if err := sc.slurmctldCmd.Process.Signal(os.Signal(os.Interrupt)); err != nil {
				sc.Logger().Warnf("Failed to send reload signal: %v", err)
			}
		}
	}

	return nil
}

// HealthCheck checks controller health and handles failover
func (sc *SLURMController) HealthCheck() error {
	if !sc.IsRunning() {
		return fmt.Errorf("SLURM controller role is not running")
	}

	// If we're the leader, check if slurmctld is running
	if sc.IsLeader() {
		if sc.slurmctldCmd == nil || sc.slurmctldCmd.Process == nil {
			return fmt.Errorf("slurmctld process is not running")
		}

		// Check if process is still alive
		if err := sc.slurmctldCmd.Process.Signal(syscall.Signal(0)); err != nil {
			return fmt.Errorf("slurmctld process health check failed: %w", err)
		}
	}

	// If HA is enabled and we're backup, monitor primary controller
	if sc.config.EnableHA && sc.isBackup && sc.backupAddr != "" {
		if err := sc.checkPrimaryHealth(); err != nil {
			sc.Logger().Warnf("Primary controller health check failed: %v", err)
			// Could trigger failover logic here if needed
		}
	}

	return nil
}

// checkPrimaryHealth checks if the primary controller is responsive
func (sc *SLURMController) checkPrimaryHealth() error {
	// Simple connectivity check to primary controller
	// In a real implementation, this would check SLURM API or database connectivity
	cmd := exec.Command("timeout", "5", "nc", "-z", sc.backupAddr, fmt.Sprintf("%d", sc.config.Port))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cannot connect to primary controller %s:%d", sc.backupAddr, sc.config.Port)
	}
	return nil
}

// IsLeaderRequired returns true since SLURM controller requires leader election
func (sc *SLURMController) IsLeaderRequired() bool {
	return true
}

// OnLeadershipChange handles leadership changes with HA considerations
func (sc *SLURMController) OnLeadershipChange(isLeader bool) error {
	sc.SetLeader(isLeader)

	if isLeader {
		sc.Logger().Infof("Became SLURM controller leader (%s), starting slurmctld", sc.getRoleString())
		return sc.startSlurmctld()
	} else {
		sc.Logger().Infof("Lost SLURM controller leadership (%s), stopping slurmctld", sc.getRoleString())
		return sc.stopSlurmctld()
	}
}

// createDirectories creates necessary directories
func (sc *SLURMController) createDirectories() error {
	dirs := []string{
		sc.configPath,
		sc.statePath,
		filepath.Join(sc.statePath, "slurmctld"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	return nil
}

// startSlurmctld starts the slurmctld daemon
func (sc *SLURMController) startSlurmctld() error {
	sc.Logger().Infof("Starting slurmctld daemon (%s)", sc.getRoleString())

	// Generate configuration
	if err := sc.generateConfig(); err != nil {
		return fmt.Errorf("failed to generate config: %w", err)
	}

	// Generate or load munge key
	if err := sc.setupMungeKey(); err != nil {
		return fmt.Errorf("failed to setup munge key: %w", err)
	}

	// Start munge daemon first
	if err := sc.startMunge(); err != nil {
		return fmt.Errorf("failed to start munge: %w", err)
	}

	// Start slurmctld
	args := []string{
		"-D", // Foreground mode
		"-f", sc.slurmConfPath,
	}

	// If backup controller, add backup mode flag
	if sc.isBackup {
		args = append(args, "--backup_controller")
		sc.Logger().Info("Starting as backup controller")
	}

	cmd := exec.Command("slurmctld", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start slurmctld: %w", err)
	}

	sc.slurmctldCmd = cmd
	sc.Logger().Infof("slurmctld started successfully (%s)", sc.getRoleString())

	return nil
}

// stopSlurmctld stops the slurmctld daemon
func (sc *SLURMController) stopSlurmctld() error {
	if sc.slurmctldCmd == nil || sc.slurmctldCmd.Process == nil {
		return nil
	}

	sc.Logger().Infof("Stopping slurmctld daemon (%s)", sc.getRoleString())

	// Send terminate signal
	if err := sc.slurmctldCmd.Process.Signal(os.Interrupt); err != nil {
		sc.Logger().Warnf("Failed to send interrupt signal: %v", err)
		// Try kill
		if err := sc.slurmctldCmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill slurmctld: %w", err)
		}
	}

	// Wait for process to exit with timeout
	done := make(chan error, 1)
	go func() {
		done <- sc.slurmctldCmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			sc.Logger().Warnf("slurmctld exit error: %v", err)
		}
	case <-time.After(10 * time.Second):
		sc.Logger().Warn("slurmctld did not exit gracefully, force killing")
		sc.slurmctldCmd.Process.Kill()
		<-done // Wait for kill to complete
	}

	sc.slurmctldCmd = nil
	sc.Logger().Info("slurmctld stopped")

	return nil
}

// generateConfig generates the slurm.conf file with HA configuration
func (sc *SLURMController) generateConfig() error {
	sc.Logger().Info("Generating SLURM configuration with HA support")

	// Get all nodes with slurm-worker role
	workers := sc.clusterState.GetNodesByRole("slurm-worker")

	// Get controller node info
	controllerName := ""
	if leaderNode, ok := sc.clusterState.GetLeaderNode("slurm-controller"); ok {
		// Use node name for SlurmctldHost (resolves via DNS in Docker)
		controllerName = leaderNode.Name
		sc.Logger().Infof("SLURM controller node: %s (address: %s)", leaderNode.Name, leaderNode.Address)
	} else {
		sc.Logger().Warn("No SLURM controller leader found, using placeholder")
		controllerName = "localhost"
	}

	// Prepare template data
	data := struct {
		ClusterName    string
		ControllerNode string
		Port           int
		Nodes          []*state.Node
	}{
		ClusterName:    sc.config.ClusterName,
		ControllerNode: controllerName,
		Port:           sc.config.Port,
		Nodes:          workers,
	}

	// Parse template
	tmpl, err := template.New("slurm.conf").Parse(slurmConfHATemplate)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	// Create temp file
	tempPath := sc.slurmConfPath + ".tmp"
	f, err := os.Create(tempPath)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer f.Close()

	// Execute template
	if err := tmpl.Execute(f, data); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("failed to execute template: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tempPath, sc.slurmConfPath); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("failed to rename config: %w", err)
	}

	sc.Logger().Infof("Generated HA SLURM configuration: %s", sc.slurmConfPath)
	return nil
}

// setupMungeKey sets up the munge authentication key using Raft consensus
func (sc *SLURMController) setupMungeKey() error {
	sc.Logger().Info("Setting up munge authentication key via Raft")

	// First check if cluster already has a munge key in Raft state
	if sc.clusterState.HasMungeKey() {
		sc.Logger().Info("Munge key already exists in cluster state, fetching from Raft")
		key, hash, err := sc.mungeKeyManager.FetchFromRaft(sc.clusterState)
		if err != nil {
			return fmt.Errorf("failed to fetch munge key from Raft: %w", err)
		}

		sc.Logger().Infof("Fetched existing munge key from Raft (hash: %s)", hash[:16]+"...")
		sc.mungeKey = key

		// Write to local disk
		if err := sc.mungeKeyManager.WriteMungeKey(key); err != nil {
			return fmt.Errorf("failed to write munge key to disk: %w", err)
		}

		return nil
	}

	// No munge key in cluster - we need to generate one
	// Only the leader should generate and store
	if !sc.IsLeader() {
		sc.Logger().Warn("Not leader and no munge key in cluster, waiting for leader to generate")
		return fmt.Errorf("waiting for leader to generate munge key")
	}

	sc.Logger().Info("Generating new munge key (first controller)")
	key, err := sc.mungeKeyManager.GenerateMungeKey()
	if err != nil {
		return fmt.Errorf("failed to generate munge key: %w", err)
	}

	// Store in Raft (this replicates to all nodes)
	if err := sc.mungeKeyManager.StoreInRaft(sc.leaderElector, key); err != nil {
		return fmt.Errorf("failed to store munge key in Raft: %w", err)
	}

	sc.mungeKey = key

	// Write to local disk
	if err := sc.mungeKeyManager.WriteMungeKey(key); err != nil {
		return fmt.Errorf("failed to write munge key to disk: %w", err)
	}

	sc.Logger().Info("Munge key generated and stored in Raft successfully")
	return nil
}

// startMunge starts the munge daemon
func (sc *SLURMController) startMunge() error {
	return sc.mungeKeyManager.StartMungeDaemon()
}

// getSlurmdbdHost returns the host address for the Kubernetes-hosted slurmdbd
// It finds a K3s server node to access the NodePort service
func (sc *SLURMController) getSlurmdbdHost() string {
	// If explicitly configured, use that
	if sc.config.SlurmdbdHost != "" {
		return sc.config.SlurmdbdHost
	}

	if sc.clusterState == nil {
		return ""
	}

	// Find a K3s server node to access the NodePort
	k3sServers := sc.clusterState.GetNodesByRole("k3s-server")
	for _, node := range k3sServers {
		if node.Status == state.StatusAlive && node.TailscaleIP != nil {
			return node.TailscaleIP.String()
		}
		// Fallback to regular address if no Tailscale IP
		if node.Status == state.StatusAlive && node.Address != "" {
			return node.Address
		}
	}

	// No K3s server found - slurmdbd not available yet
	sc.Logger().Debug("No K3s server found for slurmdbd access")
	return ""
}

const slurmConfHATemplate = `# SLURM Configuration (Auto-generated by Cluster-OS with HA)
# Cluster: {{.ClusterName}}

ClusterName={{.ClusterName}}
SlurmctldHost={{.ControllerNode}}
SlurmctldPort={{.Port}}

{{if .EnableHA}}
# High Availability Configuration
{{if .BackupController}}BackupController={{.BackupController}}{{end}}
{{if .BackupAddr}}BackupAddr={{.BackupAddr}}{{end}}
SlurmctldTimeout=300
SlurmctldParameters=enable_configless
{{end}}

# Authentication
AuthType=auth/munge
CryptoType=crypto/munge

{{if .AccountingStorageHost}}
# Accounting Storage (Kubernetes-hosted slurmdbd)
AccountingStorageType=accounting_storage/slurmdbd
AccountingStorageHost={{.AccountingStorageHost}}
AccountingStoragePort={{.AccountingStoragePort}}
AccountingStorageEnforce=associations,limits,qos
JobAcctGatherType=jobacct_gather/linux
JobAcctGatherFrequency=30
{{else}}
# No accounting storage configured
AccountingStorageType=accounting_storage/none
{{end}}

# Scheduling
SchedulerType=sched/backfill
SelectType=select/cons_tres
SelectTypeParameters=CR_Core_Memory

# Logging
SlurmctldLogFile=/var/log/slurm/slurmctld.log
SlurmdLogFile=/var/log/slurm/slurmd.log
SlurmctldDebug=info
SlurmdDebug=info

# State preservation (shared storage required for HA)
StateSaveLocation=/var/lib/slurm/slurmctld
SlurmdSpoolDir=/var/lib/slurm/slurmd

# Process tracking
ProctrackType=proctrack/linuxproc
TaskPlugin=task/none

# MPI Configuration
MpiDefault=pmix
MpiParams=ports=12000-12999
PrologFlags=Alloc

# Node definitions
{{range .Nodes}}
NodeName={{.Name}} NodeAddr={{.Address}} CPUs={{if le .Capabilities.CPU 1}}1{{else}}{{.Capabilities.CPU}}{{end}} RealMemory=4096 State=UNKNOWN
{{end}}

# Partition definitions
PartitionName=all Nodes=ALL Default=YES MaxTime=INFINITE State=UP
`
