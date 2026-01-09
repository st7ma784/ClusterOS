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

// LeaderElectorInterface defines the interface for leader election
// Both Raft-based and Serf-based electors implement this
type LeaderElectorInterface interface {
	IsLeader() bool
	IsLeaderForRole(role string) bool
	GetLeader() (string, error)
	WaitForLeader(timeout time.Duration) error
	RegisterRoleLeadershipObserver(role string) <-chan bool
	ApplySetMungeKey(mungeKey []byte, mungeKeyHash string) error
	GetClusterState() *state.ClusterState
	AddVoter(nodeID string, address string) error
	RemoveServer(nodeID string) error
	Shutdown() error
}

// Daemon represents the node agent daemon
type Daemon struct {
	config            *config.Config
	identity          *identity.Identity
	clusterState      *state.ClusterState
	discovery         *discovery.SerfDiscovery
	leaderElector     *state.LeaderElector     // Raft-based (persistent)
	serfLeaderElector *state.SerfLeaderElector // Serf-based (stateless)
	wireguard         *networking.WireGuardManager
	roleManager       *roles.Manager
	logger            *logrus.Logger
	ctx               context.Context
	cancel            context.CancelFunc
	electionMode      string // "serf" or "raft"
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

	// Determine election mode
	d.electionMode = d.config.Cluster.ElectionMode
	if d.electionMode == "" {
		d.electionMode = "serf" // Default to stateless
	}
	d.logger.Infof("Using election mode: %s", d.electionMode)

	// Initialize cluster state
	d.clusterState = state.NewClusterState()

	// Get WireGuard public key for this node
	wgPubKey, err := d.identity.WireGuardPublicKey()
	if err != nil {
		return fmt.Errorf("failed to get WireGuard public key: %w", err)
	}
	d.logger.Infof("Local WireGuard public key: %s", wgPubKey)

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
		Status:          state.StatusAlive,
		WireGuardPubKey: wgPubKey,
	}

	// Add ourselves to cluster state
	d.clusterState.AddNode(localNode)

	// For Raft mode, initialize Raft first (before Serf)
	if d.electionMode == "raft" {
		if err := d.initRaftLeaderElection(); err != nil {
			return fmt.Errorf("failed to initialize Raft leader election: %w", err)
		}
	}

	// Initialize discovery layer (Serf) - always needed
	if err := d.initDiscovery(localNode); err != nil {
		return fmt.Errorf("failed to initialize discovery: %w", err)
	}

	// For Serf mode, initialize Serf-based leader election (after Serf discovery)
	if d.electionMode == "serf" {
		if err := d.initSerfLeaderElection(); err != nil {
			return fmt.Errorf("failed to initialize Serf leader election: %w", err)
		}
	}

	// Initialize networking (WireGuard) - this is critical
	if err := d.initNetworking(); err != nil {
		d.logger.Errorf("Failed to initialize networking (WireGuard): %v", err)
		return fmt.Errorf("failed to initialize networking: %w", err)
	}

	// Initialize role manager and start roles
	if err := d.initRoles(); err != nil {
		return fmt.Errorf("failed to initialize roles: %w", err)
	}

	d.logger.Info("Daemon started successfully")
	return nil
}

// initRaftLeaderElection initializes the Raft-based leader election (persistent mode)
func (d *Daemon) initRaftLeaderElection() error {
	d.logger.Info("Initializing leader election (Raft - persistent mode)")

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

	d.logger.Info("Raft leader election initialized")
	return nil
}

// initSerfLeaderElection initializes the Serf-based leader election (stateless mode)
func (d *Daemon) initSerfLeaderElection() error {
	d.logger.Info("Initializing leader election (Serf - stateless mode)")

	if d.discovery == nil {
		return fmt.Errorf("serf discovery must be initialized before serf leader election")
	}

	electionCfg := &state.SerfElectionConfig{
		NodeID:       d.identity.NodeID,
		NodeName:     d.config.Discovery.NodeName,
		Serf:         d.discovery.GetSerf(),
		ClusterState: d.clusterState,
		Logger:       d.logger,
	}

	elector, err := state.NewSerfLeaderElector(electionCfg)
	if err != nil {
		return fmt.Errorf("failed to create serf leader elector: %w", err)
	}

	d.serfLeaderElector = elector

	// Register user event handler to receive state synchronization events
	d.discovery.RegisterUserEventHandler(func(name string, payload []byte) error {
		if name == "cluster-state" {
			d.logger.Debug("Received cluster-state event, processing...")
			return elector.HandleStateEvent(payload)
		}
		return nil
	})

	// Wait for leader election (quick for Serf)
	if err := elector.WaitForLeader(10 * time.Second); err != nil {
		d.logger.Warnf("Leader election timeout: %v (continuing anyway)", err)
	}

	// If we're not the leader, request state from the leader
	if !elector.IsLeader() {
		d.logger.Info("Not leader, requesting state from leader")
		if err := elector.RequestState(); err != nil {
			d.logger.Warnf("Failed to request state from leader: %v", err)
		}
	}

	d.logger.Info("Serf leader election initialized")
	return nil
}

// initDiscovery initializes the Serf discovery layer
func (d *Daemon) initDiscovery(localNode *state.Node) error {
	d.logger.Info("Initializing discovery layer (Serf)")

	// Parse encryption key if provided
	var encryptKey []byte
	if d.config.Discovery.EncryptKey != "" {
		var err error
		encryptKey, err = discovery.ParseEncryptKey(d.config.Discovery.EncryptKey)
		if err != nil {
			return fmt.Errorf("failed to parse encryption key: %w", err)
		}
	}

	// Build tags including WireGuard public key
	tags := map[string]string{
		"wg_pubkey": localNode.WireGuardPubKey,
	}

	discoveryCfg := &discovery.Config{
		NodeName:       d.config.Discovery.NodeName,
		NodeID:         d.identity.NodeID,
		BindAddr:       d.config.Discovery.BindAddr,
		BindPort:       d.config.Discovery.BindPort,
		BootstrapPeers: d.config.Discovery.BootstrapPeers,
		EncryptKey:     encryptKey,
		ClusterAuthKey: d.config.Cluster.AuthKey,
		Tags:           tags,
		Logger:         d.logger,
		RaftPort:       7373, // Raft consensus port
	}

	// Pass Raft leader elector only in raft mode
	// In serf mode, we'll set up Serf-based leader election after
	var leaderElector discovery.LeaderElector
	if d.electionMode == "raft" && d.leaderElector != nil {
		leaderElector = d.leaderElector
	}

	disc, err := discovery.New(discoveryCfg, d.clusterState, localNode, leaderElector)
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

	// Apply initial configuration with retries
	maxRetries := 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if err := wg.ApplyConfig(); err != nil {
			d.logger.Warnf("Failed to apply WireGuard config (attempt %d/%d): %v", attempt, maxRetries, err)
			if attempt < maxRetries {
				time.Sleep(2 * time.Second)
				continue
			}
			d.logger.Errorf("Failed to apply WireGuard config after %d attempts", maxRetries)
			return fmt.Errorf("failed to apply WireGuard configuration: %w", err)
		}
		d.logger.Info("WireGuard configuration applied successfully")
		break
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

	// Determine which leader elector to use for manager
	var managerLeaderElector roles.LeaderElectorManager
	var roleLeaderElector roles.LeaderElector
	if d.electionMode == "serf" && d.serfLeaderElector != nil {
		managerLeaderElector = d.serfLeaderElector
		roleLeaderElector = d.serfLeaderElector
		d.logger.Info("Using Serf-based leader election for roles")
	} else if d.leaderElector != nil {
		managerLeaderElector = d.leaderElector
		roleLeaderElector = d.leaderElector
		d.logger.Info("Using Raft-based leader election for roles")
	}

	// Create role manager
	managerCfg := &roles.ManagerConfig{
		Registry:      registry,
		ClusterState:  d.clusterState,
		LeaderElector: managerLeaderElector,
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
			Name:          roleName,
			Enabled:       true,
			Config:        make(map[string]interface{}),
			LeaderElector: roleLeaderElector, // Pass the correct LeaderElector
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

	// Shutdown leader elector (based on mode)
	if d.electionMode == "serf" && d.serfLeaderElector != nil {
		if err := d.serfLeaderElector.Shutdown(); err != nil {
			d.logger.Errorf("Failed to shutdown Serf leader elector: %v", err)
		}
	} else if d.leaderElector != nil {
		if err := d.leaderElector.Shutdown(); err != nil {
			d.logger.Errorf("Failed to shutdown Raft leader elector: %v", err)
		}
	}

	d.logger.Info("Daemon shut down successfully")
	return nil
}
