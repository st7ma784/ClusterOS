package daemon

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cluster-os/node/internal/config"
	"github.com/cluster-os/node/internal/discovery"
	"github.com/cluster-os/node/internal/identity"
	"github.com/cluster-os/node/internal/networking"
	"github.com/cluster-os/node/internal/roles"
	"github.com/cluster-os/node/internal/services/kubernetes/k3s"
	"github.com/cluster-os/node/internal/services/slurm/controller"
	"github.com/cluster-os/node/internal/services/slurm/worker"
	"github.com/cluster-os/node/internal/state"
	"github.com/sirupsen/logrus"
)

// Daemon represents the node agent daemon
type Daemon struct {
	config        *config.Config
	identity      *identity.Identity
	clusterState  *state.ClusterState
	discovery     *discovery.SerfDiscovery
	leaderElector *state.LeaderElector
	wireguard     *networking.WireGuardManager
	roleManager   *roles.Manager
	logger        *logrus.Logger
	ctx           context.Context
	cancel        context.CancelFunc
}

// Config contains configuration for the daemon
type Config struct {
	Config   *config.Config
	Identity *identity.Identity
	Logger   *logrus.Logger
}

// New creates a new daemon instance
func New(cfg *Config) (*Daemon, error) {
	if cfg.Logger == nil {
		cfg.Logger = logrus.New()
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Daemon{
		config:   cfg.Config,
		identity: cfg.Identity,
		logger:   cfg.Logger,
		ctx:      ctx,
		cancel:   cancel,
	}, nil
}

// Start starts the daemon and all its components
func (d *Daemon) Start() error {
	d.logger.Info("Starting Cluster-OS daemon")

	// Initialize cluster state
	d.clusterState = state.NewClusterState()

	// Create local node representation
	localNode := &state.Node{
		ID:   d.identity.NodeID,
		Name: d.config.Discovery.NodeName,
		Roles: d.config.Roles.Enabled,
		Capabilities: state.Capabilities{
			CPU:  d.config.Roles.Capabilities.CPU,
			RAM:  d.config.Roles.Capabilities.RAM,
			GPU:  d.config.Roles.Capabilities.GPU,
			Arch: d.config.Roles.Capabilities.Arch,
		},
		Status: state.StatusAlive,
	}

	// Add ourselves to cluster state
	d.clusterState.AddNode(localNode)

	// Initialize leader election (Raft)
	if err := d.initLeaderElection(); err != nil {
		return fmt.Errorf("failed to initialize leader election: %w", err)
	}

	// Initialize discovery layer (Serf)
	if err := d.initDiscovery(localNode); err != nil {
		return fmt.Errorf("failed to initialize discovery: %w", err)
	}

	// Initialize networking (WireGuard)
	if err := d.initNetworking(); err != nil {
		d.logger.Warnf("Failed to initialize networking: %v (continuing anyway)", err)
	}

	// Initialize role manager and start roles
	if err := d.initRoles(); err != nil {
		return fmt.Errorf("failed to initialize roles: %w", err)
	}

	d.logger.Info("Daemon started successfully")
	return nil
}

// initLeaderElection initializes the Raft-based leader election
func (d *Daemon) initLeaderElection() error {
	d.logger.Info("Initializing leader election (Raft)")

	// Use node name (hostname) as the advertise address
	// In Docker, this will be the container hostname which is resolvable
	advertiseAddr := d.config.Discovery.NodeName
	if advertiseAddr == "" {
		advertiseAddr = "localhost"
	}

	electionCfg := &state.ElectionConfig{
		NodeID:        d.identity.NodeID,
		NodeAddr:      advertiseAddr,
		DataDir:       "/var/lib/cluster-os/raft",
		BindAddr:      "0.0.0.0",
		BindPort:      7373,
		BootstrapNode: d.config.IsBootstrap(),
		Logger:        d.logger,
	}

	elector, err := state.NewLeaderElector(electionCfg, d.clusterState)
	if err != nil {
		return fmt.Errorf("failed to create leader elector: %w", err)
	}

	d.leaderElector = elector

	// Wait for leader election
	if err := elector.WaitForLeader(30 * time.Second); err != nil {
		d.logger.Warnf("Leader election timeout: %v (continuing anyway)", err)
	}

	d.logger.Info("Leader election initialized")
	return nil
}

// initDiscovery initializes the Serf discovery layer
func (d *Daemon) initDiscovery(localNode *state.Node) error {
	d.logger.Info("Initializing discovery layer (Serf)")

	discoveryCfg := &discovery.Config{
		NodeName:       d.config.Discovery.NodeName,
		NodeID:         d.identity.NodeID,
		BindAddr:       d.config.Discovery.BindAddr,
		BindPort:       d.config.Discovery.BindPort,
		BootstrapPeers: d.config.Discovery.BootstrapPeers,
		Logger:         d.logger,
	}

	disc, err := discovery.New(discoveryCfg, d.clusterState, localNode)
	if err != nil {
		return fmt.Errorf("failed to create discovery: %w", err)
	}

	d.discovery = disc
	d.logger.Infof("Discovery initialized, cluster size: %d", disc.GetClusterSize())
	return nil
}

// initNetworking initializes the WireGuard networking
func (d *Daemon) initNetworking() error {
	d.logger.Info("Initializing WireGuard networking")

	ipam, err := networking.NewIPAM(d.config.Networking.Subnet)
	if err != nil {
		return fmt.Errorf("failed to create IPAM: %w", err)
	}

	wgCfg := &networking.WireGuardConfig{
		Identity:      d.identity,
		IPAM:          ipam,
		ClusterState:  d.clusterState,
		InterfaceName: d.config.Networking.Interface,
		ListenPort:    d.config.Networking.ListenPort,
		Logger:        d.logger,
	}

	wg, err := networking.NewWireGuardManager(wgCfg)
	if err != nil {
		return fmt.Errorf("failed to create WireGuard manager: %w", err)
	}

	d.wireguard = wg

	// Apply initial configuration
	if err := wg.ApplyConfig(); err != nil {
		d.logger.Warnf("Failed to apply WireGuard config: %v (may need kernel module)", err)
	}

	// Start maintenance loop to reconfigure on cluster changes
	go wg.StartMaintenance(60 * time.Second)

	d.logger.Infof("WireGuard initialized, local IP: %s", wg.GetLocalIP())
	return nil
}

// initRoles initializes the role manager and starts configured roles
func (d *Daemon) initRoles() error {
	d.logger.Info("Initializing role manager")

	// Create role registry
	registry := roles.NewRegistry(d.logger)

	// Register available roles
	registry.Register("slurm-controller", controller.NewSLURMController)
	registry.Register("slurm-worker", worker.NewSLURMWorker)
	registry.Register("k3s-server", k3s.NewK3sServer)
	registry.Register("k3s-agent", k3s.NewK3sAgent)

	// Create role manager
	managerCfg := &roles.ManagerConfig{
		Registry:      registry,
		ClusterState:  d.clusterState,
		LeaderElector: d.leaderElector,
		Logger:        d.logger,
	}

	manager, err := roles.NewManager(managerCfg)
	if err != nil {
		return fmt.Errorf("failed to create role manager: %w", err)
	}

	d.roleManager = manager

	// Start configured roles
	for _, roleName := range d.config.Roles.Enabled {
		d.logger.Infof("Starting role: %s", roleName)

		roleConfig := &roles.RoleConfig{
			Name:    roleName,
			Enabled: true,
			Config:  make(map[string]interface{}),
		}

		// Add WireGuard IP for k3s roles
		if d.wireguard != nil && (roleName == "k3s-server" || roleName == "k3s-agent") {
			roleConfig.Config["node_ip"] = d.wireguard.GetLocalIP().String()
		}

		if err := manager.StartRole(roleName, roleConfig); err != nil {
			d.logger.Errorf("Failed to start role %s: %v", roleName, err)
			// Continue with other roles
		}
	}

	// Start health check loop
	manager.StartHealthCheckLoop(30 * time.Second)

	// Start reconfigure loop
	manager.StartReconfigureLoop(60 * time.Second)

	d.logger.Info("Role manager initialized")
	return nil
}

// Run runs the daemon until interrupted
func (d *Daemon) Run() error {
	d.logger.Info("Daemon running, waiting for signals")

	// Set up signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Wait for signal
	sig := <-sigCh
	d.logger.Infof("Received signal: %s", sig)

	return d.Shutdown()
}

// Shutdown gracefully shuts down the daemon
func (d *Daemon) Shutdown() error {
	d.logger.Info("Shutting down daemon")

	// Cancel context
	d.cancel()

	// Shutdown role manager
	if d.roleManager != nil {
		if err := d.roleManager.Shutdown(); err != nil {
			d.logger.Errorf("Failed to shutdown role manager: %v", err)
		}
	}

	// Shutdown WireGuard
	if d.wireguard != nil {
		if err := d.wireguard.Shutdown(); err != nil {
			d.logger.Errorf("Failed to shutdown WireGuard: %v", err)
		}
	}

	// Shutdown discovery
	if d.discovery != nil {
		if err := d.discovery.Shutdown(); err != nil {
			d.logger.Errorf("Failed to shutdown discovery: %v", err)
		}
	}

	// Shutdown leader elector
	if d.leaderElector != nil {
		if err := d.leaderElector.Shutdown(); err != nil {
			d.logger.Errorf("Failed to shutdown leader elector: %v", err)
		}
	}

	d.logger.Info("Daemon shut down successfully")
	return nil
}
