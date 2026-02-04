package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cluster-os/node/internal/config"
	"github.com/cluster-os/node/internal/discovery"
	"github.com/cluster-os/node/internal/identity"
	"github.com/cluster-os/node/internal/networking"
	"github.com/cluster-os/node/internal/roles"
	"github.com/cluster-os/node/internal/services/kubernetes/k3s"
	"github.com/cluster-os/node/internal/services/ondemand"
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
	tailscale         *networking.TailscaleManager
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

// waitForNetwork waits for at least one network interface to have a routable IP address
func (d *Daemon) waitForNetwork() error {
	maxWait := 90 * time.Second
	interval := 2 * time.Second
	start := time.Now()

	d.logger.Info("Waiting for network to be available...")

	for {
		// Check if we have any usable IP addresses
		addrs, err := d.getUsableAddresses()
		if err == nil && len(addrs) > 0 {
			d.logger.Infof("Network available: found %d usable address(es)", len(addrs))
			for _, addr := range addrs {
				d.logger.Infof("  - %s", addr)
			}
			return nil
		}

		elapsed := time.Since(start)
		if elapsed >= maxWait {
			// List all interfaces for debugging
			d.logNetworkState()
			return fmt.Errorf("timeout waiting for network after %v", elapsed)
		}

		remaining := maxWait - elapsed
		d.logger.Infof("No usable network address yet, waiting... (%v remaining)", remaining.Round(time.Second))
		time.Sleep(interval)
	}
}

// getUsableAddresses returns a list of non-loopback, non-link-local IP addresses
func (d *Daemon) getUsableAddresses() ([]string, error) {
	var usable []string

	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	for _, iface := range ifaces {
		// Skip loopback and down interfaces
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip == nil {
				continue
			}

			// Skip loopback, link-local, and IPv6 link-local
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}

			// We have a usable address
			usable = append(usable, fmt.Sprintf("%s (%s)", ip.String(), iface.Name))
		}
	}

	return usable, nil
}

// GetClusterState returns the current cluster state
func (d *Daemon) GetClusterState() *state.ClusterState {
	return d.clusterState
}

// logNetworkState logs the current state of all network interfaces for debugging
func (d *Daemon) logNetworkState() {
	d.logger.Warn("Network state dump for debugging:")

	ifaces, err := net.Interfaces()
	if err != nil {
		d.logger.Errorf("Failed to list interfaces: %v", err)
		return
	}

	for _, iface := range ifaces {
		flags := ""
		if iface.Flags&net.FlagUp != 0 {
			flags += "UP "
		}
		if iface.Flags&net.FlagLoopback != 0 {
			flags += "LOOPBACK "
		}
		if iface.Flags&net.FlagRunning != 0 {
			flags += "RUNNING "
		}

		d.logger.Infof("  Interface %s: flags=[%s] mac=%s", iface.Name, flags, iface.HardwareAddr)

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			d.logger.Infof("    Address: %s", addr.String())
		}
	}
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
	d.clusterState.SetLocalNodeID(d.identity.NodeID)

	// Use hostname for node name to avoid conflicts from shared NodeID
	// This provides unique names per physical machine
	nodeName := d.config.Discovery.NodeName
	if nodeName == "" || nodeName == "cluster-node" || nodeName == "localhost" {
		hostname, err := os.Hostname()
		if err != nil {
			hostname = fmt.Sprintf("node-%s", d.identity.NodeID[:8])
		}
		nodeName = hostname
		d.config.Discovery.NodeName = nodeName
		d.logger.Infof("Using hostname-based node name: %s", nodeName)
	}

	// Get WireGuard public key for this node
	wgPubKey, err := d.identity.WireGuardPublicKey()
	if err != nil {
		return fmt.Errorf("failed to get WireGuard public key: %w", err)
	}
	d.logger.Infof("Local WireGuard public key: %s", wgPubKey)

	// Create local node representation
	localNode := &state.Node{
		ID:   d.identity.NodeID,
		Name: nodeName,
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

// initNetworking initializes networking using Tailscale
// We use Tailscale's existing mesh instead of running our own WireGuard
func (d *Daemon) initNetworking() error {
	d.logger.Info("Initializing networking (using Tailscale)")

	// Check if Tailscale is available
	if !networking.IsTailscaleAvailable() {
		d.logger.Warn("Tailscale not detected - cluster networking may be limited")
		d.logger.Info("Ensure Tailscale is installed and connected: tailscale up")
		// Don't fail - node can still work on local network
		return nil
	}

	tsCfg := &networking.TailscaleConfig{
		Logger: d.logger,
	}

	ts, err := networking.NewTailscaleManager(tsCfg)
	if err != nil {
		d.logger.Warnf("Failed to initialize Tailscale manager: %v", err)
		d.logger.Info("Continuing without Tailscale - using local network only")
		return nil
	}

	d.tailscale = ts

	// Update cluster state with our Tailscale IP (stored in TailscaleIP field)
	d.clusterState.UpdateNodeTailscaleIP(d.identity.NodeID, ts.GetLocalIP().String())

	// Update node name to use Tailscale IP for better identification
	tailscaleIP := ts.GetLocalIP().String()
	if tailscaleIP != "" && tailscaleIP != "<nil>" {
		// Update the node name to use Tailscale IP
		oldName := d.config.Discovery.NodeName
		d.config.Discovery.NodeName = tailscaleIP
		d.logger.Infof("Updated node name from %s to Tailscale IP: %s", oldName, tailscaleIP)
		
		// Update local node in cluster state
		if localNode, found := d.clusterState.GetNode(d.identity.NodeID); found {
			localNode.Name = tailscaleIP
			d.clusterState.AddNode(localNode) // This will update the existing node
		}
	}

	// Update Serf tags with our Tailscale IP
	if d.discovery != nil {
		if err := d.updateSerfTailscaleIP(ts.GetLocalIP()); err != nil {
			d.logger.Warnf("Failed to update Serf tags with Tailscale IP: %v", err)
		}
	}

	d.logger.Infof("Tailscale networking initialized, local IP: %s", ts.GetLocalIP())

	// Set unique hostname based on Tailscale IP to avoid SLURM conflicts
	if err := d.setUniqueHostname(ts.GetLocalIP()); err != nil {
		d.logger.Warnf("Failed to set unique hostname: %v (continuing with default)", err)
	}

	return nil
}

// updateSerfTailscaleIP updates the Serf tags with the current Tailscale IP
func (d *Daemon) updateSerfTailscaleIP(ip net.IP) error {
	if d.discovery == nil {
		return nil
	}

	// Get existing tags and add/update the Tailscale IP (using wgip tag for compatibility)
	tags := map[string]string{
		"wgip": ip.String(),
	}

	return d.discovery.UpdateTags(tags)
}

// setUniqueHostname sets a unique hostname based on the Tailscale IP
// This prevents duplicate hostname issues in SLURM and other cluster services
func (d *Daemon) setUniqueHostname(tsIP net.IP) error {
	if tsIP == nil {
		return fmt.Errorf("no Tailscale IP available")
	}

	// Convert IP to hostname format: node-X-Y (last two octets)
	// e.g., 100.64.5.123 -> node-5-123
	ip4 := tsIP.To4()
	if ip4 == nil {
		return fmt.Errorf("not an IPv4 address: %s", tsIP)
	}

	newHostname := fmt.Sprintf("node-%d-%d", ip4[2], ip4[3])

	// Get current hostname
	currentHostname, err := os.Hostname()
	if err != nil {
		currentHostname = "unknown"
	}

	// Skip if hostname is already unique (not the default)
	if currentHostname == newHostname {
		d.logger.Infof("Hostname already set to %s", newHostname)
		return nil
	}

	d.logger.Infof("Setting unique hostname: %s -> %s (based on Tailscale IP %s)", currentHostname, newHostname, tsIP)

	// Update /etc/hostname
	if err := os.WriteFile("/etc/hostname", []byte(newHostname+"\n"), 0644); err != nil {
		d.logger.Warnf("Failed to write /etc/hostname: %v", err)
	}

	// Update hostname using hostnamectl (if available)
	cmd := exec.Command("hostnamectl", "set-hostname", newHostname)
	if err := cmd.Run(); err != nil {
		// Fallback: use hostname command
		cmd = exec.Command("hostname", newHostname)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to set hostname: %w", err)
		}
	}

	// Update /etc/hosts to include the new hostname
	d.updateHostsFile(newHostname, tsIP)

	// Update the discovery node name so SLURM and Serf use the new hostname
	d.config.Discovery.NodeName = newHostname

	d.logger.Infof("Hostname set to %s", newHostname)
	return nil
}

// updateHostsFile adds the new hostname to /etc/hosts
func (d *Daemon) updateHostsFile(hostname string, ip net.IP) {
	hostsEntry := fmt.Sprintf("%s\t%s\n", ip.String(), hostname)

	// Read existing hosts file
	content, err := os.ReadFile("/etc/hosts")
	if err != nil {
		d.logger.Warnf("Failed to read /etc/hosts: %v", err)
		return
	}

	// Check if hostname already exists in hosts file
	if strings.Contains(string(content), hostname) {
		d.logger.Debugf("Hostname %s already in /etc/hosts", hostname)
		return
	}

	// Append new entry
	f, err := os.OpenFile("/etc/hosts", os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		d.logger.Warnf("Failed to open /etc/hosts for writing: %v", err)
		return
	}
	defer f.Close()

	if _, err := f.WriteString(hostsEntry); err != nil {
		d.logger.Warnf("Failed to update /etc/hosts: %v", err)
	} else {
		d.logger.Infof("Added %s to /etc/hosts", hostname)
	}
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
	registry.Register("ondemand", ondemand.NewOpenOnDemand)

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

		// Add Tailscale IP for k3s roles
		if d.tailscale != nil && (roleName == "k3s-server" || roleName == "k3s-agent") {
			roleConfig.Config["node_ip"] = d.tailscale.GetLocalIP().String()
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

// setupFirewallRules configures firewall rules to allow essential services
// This ensures SSH and other critical services work properly
func (d *Daemon) setupFirewallRules() error {
	d.logger.Info("Setting up firewall rules for essential services")

	rules := []struct {
		port    string
		proto   string
		comment string
	}{
		{"22", "tcp", "SSH"},
		{"7946", "tcp", "Serf TCP"},
		{"7946", "udp", "Serf UDP"},
		{"6443", "tcp", "K3s API"},
		{"6817", "tcp", "SLURM"},
	}

	for _, rule := range rules {
		// Try using ufw if available
		cmd := exec.Command("ufw", "allow", fmt.Sprintf("%s/%s", rule.port, rule.proto))
		if out, err := cmd.CombinedOutput(); err != nil {
			d.logger.Warnf("ufw allow %s/%s failed: %v (%s), trying iptables", rule.port, rule.proto, err, string(out))

			// Fallback to iptables
			ipt := exec.Command("iptables", "-A", "INPUT", "-p", rule.proto, "--dport", rule.port, "-j", "ACCEPT")
			if out, err := ipt.CombinedOutput(); err != nil {
				d.logger.Warnf("iptables rule for %s failed: %v (%s)", rule.comment, err, string(out))
				// Don't return error - continue with other rules
			}
		}
	}

	d.logger.Info("Firewall rules configured")
	return nil
}

// ServiceStatus represents the status of a service
type ServiceStatus struct {
	Name    string
	Status  string // "running", "stopped", "error", "unknown"
	Message string
}

// GetComprehensiveStatus returns comprehensive status of all services and components
func (d *Daemon) GetComprehensiveStatus() map[string]interface{} {
	status := make(map[string]interface{})

	// Node information
	status["node"] = map[string]interface{}{
		"id":         d.identity.NodeID,
		"name":       d.config.Discovery.NodeName,
		"cluster":    d.config.Cluster.Name,
		"region":     d.config.Cluster.Region,
		"datacenter": d.config.Cluster.Datacenter,
	}

	// Networking status
	networkStatus := map[string]interface{}{
		"type": "tailscale",
	}

	// Check Tailscale status
	if d.tailscale != nil {
		tsIP := d.tailscale.GetLocalIP()
		if tsIP != nil {
			networkStatus["tailscale_ip"] = tsIP.String()
			networkStatus["connected"] = true
		} else {
			networkStatus["connected"] = false
			networkStatus["message"] = "Tailscale IP not available"
		}
	} else {
		networkStatus["connected"] = false
		networkStatus["message"] = "Tailscale manager not initialized"
	}

	status["networking"] = networkStatus

	// Discovery status
	discoveryStatus := map[string]interface{}{
		"serf": map[string]interface{}{
			"running": d.discovery != nil,
		},
	}

	// Cluster membership - simplified for now
	discoveryStatus["members"] = 0
	discoveryStatus["member_list"] = []string{}

	status["discovery"] = discoveryStatus

	// Leader election status
	leaderStatus := map[string]interface{}{
		"mode": d.electionMode,
	}

	if d.electionMode == "raft" && d.leaderElector != nil {
		leaderStatus["is_leader"] = d.leaderElector.IsLeader()
		if leader, err := d.leaderElector.GetLeader(); err == nil {
			leaderStatus["leader"] = leader
		}
		leaderStatus["cluster_state"] = d.leaderElector.GetClusterState()
	} else if d.electionMode == "serf" && d.serfLeaderElector != nil {
		leaderStatus["is_leader"] = d.serfLeaderElector.IsLeader()
		if leader, err := d.serfLeaderElector.GetLeader(); err == nil {
			leaderStatus["leader"] = leader
		}
	}

	status["leadership"] = leaderStatus

	// Role statuses
	roleStatuses := []map[string]interface{}{}
	if d.roleManager != nil {
		allStatuses := d.roleManager.GetAllStatuses()
		for _, rs := range allStatuses {
			roleStatuses = append(roleStatuses, map[string]interface{}{
				"name":     rs.Name,
				"running":  rs.Running,
				"healthy":  rs.Healthy,
				"is_leader": rs.IsLeader,
				"error":    rs.Error,
			})
		}
	}
	status["roles"] = roleStatuses

	// System services status (check via systemctl)
	systemServices := []ServiceStatus{}

	// Check SLURM services
	slurmServices := []string{"slurmctld", "slurmd"}
	for _, svc := range slurmServices {
		status := d.checkSystemdService(svc)
		systemServices = append(systemServices, status)
	}

	// Check K3s services
	k3sServices := []string{"k3s", "k3s-agent"}
	for _, svc := range k3sServices {
		status := d.checkSystemdService(svc)
		systemServices = append(systemServices, status)
	}

	// Check other services
	otherServices := []string{"apache2", "ttyd", "filebrowser"}
	for _, svc := range otherServices {
		status := d.checkSystemdService(svc)
		systemServices = append(systemServices, status)
	}

	status["system_services"] = systemServices

	// Resource usage
	status["resources"] = d.getResourceUsage()

	return status
}

// checkSystemdService checks the status of a systemd service
func (d *Daemon) checkSystemdService(serviceName string) ServiceStatus {
	cmd := exec.Command("systemctl", "is-active", serviceName)
	output, err := cmd.Output()

	status := ServiceStatus{Name: serviceName}

	if err != nil {
		status.Status = "error"
		status.Message = fmt.Sprintf("Failed to check: %v", err)
		return status
	}

	activeStatus := strings.TrimSpace(string(output))
	switch activeStatus {
	case "active":
		status.Status = "running"
		status.Message = "Service is active"
	case "inactive":
		status.Status = "stopped"
		status.Message = "Service is inactive"
	case "failed":
		status.Status = "error"
		status.Message = "Service has failed"
	default:
		status.Status = "unknown"
		status.Message = fmt.Sprintf("Unknown status: %s", activeStatus)
	}

	return status
}

// getResourceUsage returns basic resource usage information
func (d *Daemon) getResourceUsage() map[string]interface{} {
	resources := map[string]interface{}{
		"cpu_cores": runtime.NumCPU(),
	}

	// Get memory info
	if memInfo, err := d.getMemoryInfo(); err == nil {
		resources["memory"] = memInfo
	}

	// Get disk usage for root filesystem
	if diskInfo, err := d.getDiskUsage("/"); err == nil {
		resources["disk"] = diskInfo
	}

	return resources
}

// getMemoryInfo reads /proc/meminfo and returns memory usage
func (d *Daemon) getMemoryInfo() (map[string]interface{}, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, err
	}

	memInfo := make(map[string]interface{})
	lines := strings.Split(string(data), "\n")

	for _, line := range lines {
		if strings.Contains(line, "MemTotal") {
			if val, err := d.parseMemValue(line); err == nil {
				memInfo["total_kb"] = val
			}
		} else if strings.Contains(line, "MemAvailable") {
			if val, err := d.parseMemValue(line); err == nil {
				memInfo["available_kb"] = val
			}
		}
	}

	return memInfo, nil
}

// parseMemValue extracts the numeric value from a /proc/meminfo line
func (d *Daemon) parseMemValue(line string) (int64, error) {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return 0, fmt.Errorf("invalid line format")
	}

	// Remove "kB" suffix if present
	valueStr := strings.TrimSuffix(parts[1], "kB")
	return strconv.ParseInt(valueStr, 10, 64)
}

// getDiskUsage gets disk usage information for a mount point
func (d *Daemon) getDiskUsage(path string) (map[string]interface{}, error) {
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
