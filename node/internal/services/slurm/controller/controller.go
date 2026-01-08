package controller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"text/template"

	"github.com/cluster-os/node/internal/roles"
	"github.com/cluster-os/node/internal/state"
	"github.com/sirupsen/logrus"
)

// SLURMController implements the SLURM controller role
type SLURMController struct {
	*roles.BaseRole
	config        *Config
	clusterState  *state.ClusterState
	slurmctldCmd  *exec.Cmd
	mungeKey      []byte
	configPath    string
	statePath     string
	slurmConfPath string
}

// Config contains configuration for the SLURM controller
type Config struct {
	ConfigPath    string
	StatePath     string
	SlurmConfPath string
	ClusterName   string
	Port          int
}

// NewSLURMController creates a new SLURM controller role
func NewSLURMController(roleConfig *roles.RoleConfig, logger *logrus.Logger) (roles.Role, error) {
	config := &Config{
		ConfigPath:    "/etc/slurm",
		StatePath:     "/var/lib/slurm",
		SlurmConfPath: "/etc/slurm/slurm.conf",
		ClusterName:   "cluster-os",
		Port:          6817,
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

	return &SLURMController{
		BaseRole:      roles.NewBaseRole("slurm-controller", logger),
		config:        config,
		slurmConfPath: config.SlurmConfPath,
		configPath:    config.ConfigPath,
		statePath:     config.StatePath,
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

	// Don't start slurmctld yet - wait for leadership
	sc.SetRunning(true)
	sc.Logger().Info("SLURM controller role started (waiting for leadership)")
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

// Reconfigure regenerates configuration and restarts if leader
func (sc *SLURMController) Reconfigure(clusterState *state.ClusterState) error {
	sc.Logger().Info("Reconfiguring SLURM controller")
	sc.clusterState = clusterState

	// Only reconfigure if we're the leader
	if !sc.IsLeader() {
		return nil
	}

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

	return nil
}

// HealthCheck checks if slurmctld is running
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

	return nil
}

// IsLeaderRequired returns true since SLURM controller requires leader election
func (sc *SLURMController) IsLeaderRequired() bool {
	return true
}

// OnLeadershipChange handles leadership changes
func (sc *SLURMController) OnLeadershipChange(isLeader bool) error {
	sc.SetLeader(isLeader)

	if isLeader {
		sc.Logger().Info("Became SLURM controller leader, starting slurmctld")
		return sc.startSlurmctld()
	} else {
		sc.Logger().Info("Lost SLURM controller leadership, stopping slurmctld")
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
	sc.Logger().Info("Starting slurmctld daemon")

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
	cmd := exec.Command("slurmctld",
		"-D", // Foreground mode
		"-f", sc.slurmConfPath,
	)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start slurmctld: %w", err)
	}

	sc.slurmctldCmd = cmd
	sc.Logger().Info("slurmctld started successfully")

	return nil
}

// stopSlurmctld stops the slurmctld daemon
func (sc *SLURMController) stopSlurmctld() error {
	if sc.slurmctldCmd == nil || sc.slurmctldCmd.Process == nil {
		return nil
	}

	sc.Logger().Info("Stopping slurmctld daemon")

	// Send terminate signal
	if err := sc.slurmctldCmd.Process.Signal(os.Interrupt); err != nil {
		sc.Logger().Warnf("Failed to send interrupt signal: %v", err)
		// Try kill
		if err := sc.slurmctldCmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill slurmctld: %w", err)
		}
	}

	// Wait for process to exit
	if err := sc.slurmctldCmd.Wait(); err != nil {
		sc.Logger().Warnf("slurmctld exit error: %v", err)
	}

	sc.slurmctldCmd = nil
	sc.Logger().Info("slurmctld stopped")

	return nil
}

// generateConfig generates the slurm.conf file
func (sc *SLURMController) generateConfig() error {
	sc.Logger().Info("Generating SLURM configuration")

	// Get all nodes with slurm-worker role
	workers := sc.clusterState.GetNodesByRole("slurm-worker")

	// Prepare template data
	data := struct {
		ClusterName  string
		ControllerNode string
		Port         int
		Nodes        []*state.Node
	}{
		ClusterName:  sc.config.ClusterName,
		ControllerNode: func() string { leader, _ := sc.clusterState.GetLeader("slurm-controller"); return leader }(),
		Port:         sc.config.Port,
		Nodes:        workers,
	}

	// Parse template
	tmpl, err := template.New("slurm.conf").Parse(slurmConfTemplate)
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

	sc.Logger().Infof("Generated SLURM configuration: %s", sc.slurmConfPath)
	return nil
}

// setupMungeKey sets up the munge authentication key
func (sc *SLURMController) setupMungeKey() error {
	// TODO: Implement munge key generation and distribution
	// For now, create a placeholder key
	mungeKeyPath := "/etc/munge/munge.key"

	if _, err := os.Stat(mungeKeyPath); os.IsNotExist(err) {
		sc.Logger().Warn("Munge key not found, creating placeholder")
		// In production, this should be properly generated and distributed
		if err := os.WriteFile(mungeKeyPath, []byte("placeholder-key"), 0400); err != nil {
			return fmt.Errorf("failed to write munge key: %w", err)
		}
	}

	return nil
}

// startMunge starts the munge daemon
func (sc *SLURMController) startMunge() error {
	// Check if munge is already running
	if err := exec.Command("pgrep", "munged").Run(); err == nil {
		sc.Logger().Info("Munge daemon already running")
		return nil
	}

	sc.Logger().Info("Starting munge daemon")

	cmd := exec.Command("munged", "--foreground")
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start munged: %w", err)
	}

	sc.Logger().Info("Munge daemon started")
	return nil
}

const slurmConfTemplate = `# SLURM Configuration (Auto-generated by Cluster-OS)
# Cluster: {{.ClusterName}}

ClusterName={{.ClusterName}}
SlurmctldHost={{.ControllerNode}}
SlurmctldPort={{.Port}}

# Authentication
AuthType=auth/munge
CryptoType=crypto/munge

# Scheduling
SchedulerType=sched/backfill
SelectType=select/cons_tres
SelectTypeParameters=CR_Core_Memory

# Logging
SlurmctldLogFile=/var/log/slurm/slurmctld.log
SlurmdLogFile=/var/log/slurm/slurmd.log
SlurmctldDebug=info
SlurmdDebug=info

# State preservation
StateSaveLocation=/var/lib/slurm/slurmctld
SlurmdSpoolDir=/var/lib/slurm/slurmd

# Process tracking
ProctrackType=proctrack/linuxproc
TaskPlugin=task/none

# MPI
MpiDefault=none

# Node definitions
{{range .Nodes}}
NodeName={{.Name}} NodeAddr={{.Address}} CPUs={{.Capabilities.CPU}} RealMemory=4096 State=UNKNOWN
{{end}}

# Partition definitions
PartitionName=all Nodes=ALL Default=YES MaxTime=INFINITE State=UP
`
