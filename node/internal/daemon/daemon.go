package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cluster-os/node/internal/config"
	"github.com/cluster-os/node/internal/discovery"
	"github.com/cluster-os/node/internal/identity"
	"github.com/cluster-os/node/internal/networking"
	"github.com/cluster-os/node/internal/roles"
	"github.com/cluster-os/node/internal/services/kubernetes/k3s"
	slurmauth "github.com/cluster-os/node/internal/services/slurm/auth"
	"github.com/cluster-os/node/internal/services/slurm/controller"
	"github.com/cluster-os/node/internal/services/slurm/worker"
	"github.com/cluster-os/node/internal/state"
	"github.com/sirupsen/logrus"
)

// ClusterPhase is the current phase of the cluster state machine.
type ClusterPhase string

const (
	PhaseDiscovering  ClusterPhase = "discovering"
	PhaseElecting     ClusterPhase = "electing"
	PhaseProvisioning ClusterPhase = "provisioning"
	PhaseJoining      ClusterPhase = "joining"
	PhaseReady        ClusterPhase = "ready"
)

// Daemon represents the node agent daemon.
type Daemon struct {
	config        *config.Config
	identity      *identity.Identity
	clusterState  *state.ClusterState
	discovery     *discovery.SerfDiscovery
	leaderElector *state.SerfLeaderElector
	tailscale     *networking.TailscaleManager
	roleManager   *roles.Manager
	logger        *logrus.Logger
	ctx           context.Context
	cancel        context.CancelFunc

	mu             sync.RWMutex
	phase          ClusterPhase
	isLeader       bool
	leaderName     string
	slurmCtrl      *controller.SLURMController // non-nil only on leader
	failedPeerCache *peerFailCache
}

// Config contains configuration for creating a daemon.
type Config struct {
	Config   *config.Config
	Identity *identity.Identity
	Logger   *logrus.Logger
}

// New creates a new daemon instance.
func New(cfg *Config) (*Daemon, error) {
	if cfg.Logger == nil {
		cfg.Logger = logrus.New()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Daemon{
		config:          cfg.Config,
		identity:        cfg.Identity,
		logger:          cfg.Logger,
		ctx:             ctx,
		cancel:          cancel,
		phase:           PhaseDiscovering,
		failedPeerCache: newPeerFailCache(),
	}, nil
}

// GetClusterState returns the current cluster state.
func (d *Daemon) GetClusterState() *state.ClusterState {
	return d.clusterState
}

// Start initialises all components and launches the phase machine.
func (d *Daemon) Start() error {
	d.logger.Info("Starting Cluster-OS daemon")

	d.clusterState = state.NewClusterState()
	d.clusterState.SetLocalNodeID(d.identity.NodeID)

	// Node name: prefer hostname over generic defaults
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

	wgPubKey, err := d.identity.WireGuardPublicKey()
	if err != nil {
		return fmt.Errorf("get WireGuard public key: %w", err)
	}

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
	d.clusterState.AddNode(localNode)

	// Detect Tailscale IP BEFORE Serf init so we can advertise it as our gossip address.
	// Nodes binding Serf to 0.0.0.0 accept connections from any interface, but peers need
	// to know which IP to contact us on; using the Tailscale IP routes through the encrypted
	// overlay even when direct LAN port 7946 access is unavailable.
	tsAdvertiseIP := d.detectTailscaleIP()
	if tsAdvertiseIP != "" {
		d.logger.Infof("Tailscale IP detected early: %s — using as Serf advertise address", tsAdvertiseIP)
	}

	if err := d.initDiscovery(localNode, tsAdvertiseIP); err != nil {
		return fmt.Errorf("init discovery: %w", err)
	}

	electionCfg := &state.SerfElectionConfig{
		NodeName: d.config.Discovery.NodeName,
		Serf:     d.discovery.GetSerf(),
		Logger:   d.logger,
	}
	elector, err := state.NewSerfLeaderElector(electionCfg)
	if err != nil {
		return fmt.Errorf("init leader elector: %w", err)
	}
	d.leaderElector = elector

	if err := d.initNetworking(); err != nil {
		d.logger.Warnf("Networking init failed: %v (continuing)", err)
	}

	if err := d.setupFirewallRules(); err != nil {
		d.logger.Warnf("Firewall setup failed: %v (continuing)", err)
	}

	if d.tailscale != nil && d.config.Tailscale.APIDiscovery {
		go d.tailscalePeerDiscoveryLoop()
	}

	// Role manager handles health-checking of services started by the phase machine.
	d.roleManager = roles.NewManager(d.logger)
	d.roleManager.StartHealthCheckLoop(30 * time.Second)

	go d.runPhaseMachine()

	d.logger.Info("Daemon started successfully")
	return nil
}

// Run blocks until a signal is received, then shuts down.
func (d *Daemon) Run() error {
	d.logger.Info("Daemon running, waiting for signals")
	d.writeStatusFile()
	go d.statusFileLoop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	d.logger.Infof("Received signal: %s", sig)
	return d.Shutdown()
}

// Shutdown gracefully stops all components.
func (d *Daemon) Shutdown() error {
	d.logger.Info("Shutting down daemon")
	d.cancel()
	if d.roleManager != nil {
		d.roleManager.Shutdown()
	}
	if d.discovery != nil {
		d.discovery.Shutdown()
	}
	d.logger.Info("Daemon shut down")
	return nil
}

// --------------------------------------------------------------------------
// Phase machine
// --------------------------------------------------------------------------

func (d *Daemon) setPhase(p ClusterPhase) {
	d.mu.Lock()
	d.phase = p
	d.mu.Unlock()
	d.logger.Infof("Phase → %s", p)
	d.publishTag("phase", string(p))
}

func (d *Daemon) getPhase() ClusterPhase {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.phase
}

func (d *Daemon) runPhaseMachine() {
	for {
		select {
		case <-d.ctx.Done():
			return
		default:
		}

		switch d.getPhase() {
		case PhaseDiscovering:
			d.runDiscovering()
		case PhaseElecting:
			d.runElecting()
		case PhaseProvisioning:
			if err := d.runProvisioning(); err != nil {
				d.logger.Errorf("Provisioning failed: %v — retrying in 30s", err)
				time.Sleep(30 * time.Second)
				d.setPhase(PhaseElecting)
			}
		case PhaseJoining:
			if err := d.runJoining(); err != nil {
				d.logger.Errorf("Joining failed: %v — retrying in 15s", err)
				time.Sleep(15 * time.Second)
				d.setPhase(PhaseElecting)
			}
		case PhaseReady:
			d.runReady()
		}
	}
}

func (d *Daemon) runDiscovering() {
	d.logger.Info("Discovering cluster members...")
	for {
		select {
		case <-d.ctx.Done():
			return
		default:
		}
		if len(d.discovery.GetAliveMembers()) >= 1 {
			d.logger.Infof("Discovered %d alive member(s)", len(d.discovery.GetAliveMembers()))
			d.setPhase(PhaseElecting)
			return
		}
		time.Sleep(2 * time.Second)
	}
}

func (d *Daemon) runElecting() {
	d.logger.Info("Electing leader...")
	// Brief pause so gossip can stabilise before sorting member list.
	time.Sleep(3 * time.Second)

	leader := d.leaderElector.ComputeLeader()
	isLeader := d.leaderElector.IsLeader()

	d.mu.Lock()
	d.isLeader = isLeader
	d.leaderName = leader
	d.mu.Unlock()

	d.logger.Infof("Leader: %s (I am leader: %v)", leader, isLeader)
	if isLeader {
		d.setPhase(PhaseProvisioning)
	} else {
		d.setPhase(PhaseJoining)
	}
}

// runProvisioning is called on the leader only.
// It starts K3s, generates the munge key, and starts the SLURM controller,
// then publishes everything via Serf tags so workers can join.
func (d *Daemon) runProvisioning() error {
	d.logger.Info("Provisioning cluster (leader role)")

	// Clear any stale worker roles from a previous term.
	d.roleManager.UnregisterRole("k3s-agent")
	d.roleManager.UnregisterRole("slurm-worker")

	nodeIP := d.getLocalIP()

	// --- K3s server ---
	d.logger.Info("Starting K3s server...")
	k3sServer := k3s.NewK3sServerRole(nodeIP, d.logger)
	if err := k3sServer.Start(); err != nil {
		return fmt.Errorf("start k3s server: %w", err)
	}
	d.roleManager.RegisterRole("k3s-server", k3sServer)

	d.logger.Info("Waiting for K3s API to be ready...")
	if err := k3sServer.WaitForAPIReady(5 * time.Minute); err != nil {
		return fmt.Errorf("k3s API ready: %w", err)
	}

	token, err := k3sServer.ReadToken()
	if err != nil {
		return fmt.Errorf("read k3s token: %w", err)
	}

	serverURL := fmt.Sprintf("https://%s:6443", nodeIP)

	// Record in cluster state so ondemand can find the K3s leader.
	d.clusterState.SetLeader("k3s-server", d.identity.NodeID)

	// --- Munge key ---
	mungeManager := slurmauth.NewMungeKeyManager(d.logger)
	mungeKey, err := mungeManager.GenerateMungeKey()
	if err != nil {
		return fmt.Errorf("generate munge key: %w", err)
	}

	// --- SLURM controller ---
	// Build initial worker list from any Serf members already alive (handles re-elections
	// where workers might already be present before the leader finishes provisioning).
	initialWorkers := d.buildWorkerList()
	d.logger.Infof("Starting SLURM controller (initial workers=%d)...", len(initialWorkers))
	slurmCtrl := controller.NewSLURMControllerRole(nodeIP, mungeKey, initialWorkers, "", d.logger)
	if err := slurmCtrl.Start(); err != nil {
		return fmt.Errorf("start slurm controller: %w", err)
	}
	d.roleManager.RegisterRole("slurm-controller", slurmCtrl)
	d.clusterState.SetLeader("slurm-controller", d.identity.NodeID)

	d.mu.Lock()
	d.slurmCtrl = slurmCtrl
	d.mu.Unlock()

	// Watch Serf membership and update the SLURM node list as workers join/leave.
	// slurmctld needs NodeName entries to schedule jobs — we populate them dynamically.
	go d.watchWorkersAndReconfigureSLURM()

	// Publish ALL cluster tags in a single atomic UpdateTags call alongside phase=ready.
	// This is critical: because Serf gossip is asynchronous, workers that see phase=ready
	// via gossip must have all other cluster tags in the SAME member-state snapshot.
	// Publishing them separately would allow a worker to see phase=ready without munge-key
	// if the two gossip messages arrived via different peers or were in different UDP batches.
	if err := d.publishClusterReady(serverURL, token, mungeKey); err != nil {
		return fmt.Errorf("publish cluster ready state: %w", err)
	}

	// Deploy Longhorn, Rancher, nginx-ingress, slurmdbd in the background.
	go k3sServer.DeployClusterServices(mungeKey)

	return nil
}

// runJoining is called on non-leader nodes.
// It waits for the leader to be ready, then reads tags and starts services.
func (d *Daemon) runJoining() error {
	d.logger.Info("Joining cluster (worker role)")

	// Clear any stale leader roles from a previous term.
	d.roleManager.UnregisterRole("k3s-server")
	d.roleManager.UnregisterRole("slurm-controller")

	// Wait for leader to publish phase=ready.
	d.logger.Info("Waiting for leader to reach ready phase...")
	if _, err := d.waitForLeaderTag("phase", "ready", 10*time.Minute); err != nil {
		return fmt.Errorf("wait for leader ready: %w", err)
	}

	// Force an immediate TCP push-pull with the leader so we get all tags in one shot.
	// Without this, gossip propagation of large values (munge-key, k3s-token) may rely
	// solely on periodic push-pull (every 15s) which could be slow or blocked on some
	// Tailscale configurations. The direct TCP connection forces a full state exchange.
	d.syncWithLeader()

	// All cluster tags are published atomically with phase=ready.
	// Use a 5-minute timeout per tag as insurance against slow gossip propagation.
	k3sServerURL, err := d.waitForLeaderTag("k3s-server", "", 5*time.Minute)
	if err != nil {
		d.logger.Errorf("k3s-server tag not found; leader tags visible: %v", d.getLeaderTags())
		return fmt.Errorf("read k3s-server tag: %w", err)
	}
	k3sToken, err := d.waitForLeaderTag("k3s-token", "", 5*time.Minute)
	if err != nil {
		return fmt.Errorf("read k3s-token tag: %w", err)
	}
	mungeKeyB64, err := d.waitForLeaderTag("munge-key", "", 5*time.Minute)
	if err != nil {
		d.logger.Errorf("munge-key tag not found; leader tags visible: %v", d.getLeaderTags())
		return fmt.Errorf("read munge-key tag: %w", err)
	}

	mungeKey, err := base64.StdEncoding.DecodeString(mungeKeyB64)
	if err != nil {
		return fmt.Errorf("decode munge key: %w", err)
	}

	// Extract controller IP from the K3s server URL (https://<IP>:6443).
	controllerIP := d.extractHost(k3sServerURL)
	nodeIP := d.getLocalIP()

	// --- K3s agent ---
	d.logger.Infof("Starting K3s agent → %s", k3sServerURL)
	k3sAgent := k3s.NewK3sAgentRole(k3sServerURL, k3sToken, nodeIP, d.logger)
	if err := k3sAgent.Start(); err != nil {
		return fmt.Errorf("start k3s agent: %w", err)
	}
	d.roleManager.RegisterRole("k3s-agent", k3sAgent)

	// --- SLURM worker ---
	d.logger.Infof("Starting SLURM worker (controller=%s)", controllerIP)
	slurmWorker := worker.NewSLURMWorkerRole(controllerIP, mungeKey, nodeIP, d.logger)
	if err := slurmWorker.Start(); err != nil {
		return fmt.Errorf("start slurm worker: %w", err)
	}
	d.roleManager.RegisterRole("slurm-worker", slurmWorker)

	d.setPhase(PhaseReady)
	return nil
}

// runReady loops, monitoring for leadership changes.
func (d *Daemon) runReady() {
	d.logger.Info("Cluster ready — monitoring leadership")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			nowLeader := d.leaderElector.IsLeader()
			d.mu.RLock()
			wasLeader := d.isLeader
			d.mu.RUnlock()

			if nowLeader != wasLeader {
				d.logger.Infof("Leadership changed: was=%v now=%v", wasLeader, nowLeader)
				d.mu.Lock()
				d.isLeader = nowLeader
				d.mu.Unlock()

				if nowLeader {
					d.logger.Info("Promoted to leader — re-provisioning")
					d.setPhase(PhaseProvisioning)
					return
				}
				// Lost leadership — clear controller reference and re-join as worker.
				d.logger.Info("Lost leadership — re-joining as worker")
				d.mu.Lock()
				d.slurmCtrl = nil
				d.mu.Unlock()
				d.setPhase(PhaseJoining)
				return
			}
		}
	}
}

// --------------------------------------------------------------------------
// Serf tag helpers
// --------------------------------------------------------------------------

func (d *Daemon) publishTag(key, value string) {
	if d.discovery == nil {
		return
	}
	if err := d.discovery.UpdateTags(map[string]string{key: value}); err != nil {
		d.logger.Warnf("publish tag %s: %v", key, err)
	}
}

// publishClusterReady publishes all cluster coordination tags AND phase=ready in a single
// atomic Serf SetTags call.  Because gossip messages carry a full member-state snapshot,
// workers that see phase=ready will always see the k3s-server/token/munge-key in the
// same snapshot — eliminating the race where phase=ready arrived before munge-key.
func (d *Daemon) publishClusterReady(k3sServerURL, k3sToken string, mungeKey []byte) error {
	if d.discovery == nil {
		return fmt.Errorf("discovery not initialised")
	}
	mungeKeyB64 := base64.StdEncoding.EncodeToString(mungeKey)
	// Log estimated tag payload sizes to aid debugging if SetTags ever fails with
	// "Encoded length of tags exceeds limit of 512 bytes" (memberlist MetaMaxSize).
	d.logger.Infof("publishClusterReady: k3s-token=%d chars, munge-key=%d chars (raw %d bytes)",
		len(k3sToken), len(mungeKeyB64), len(mungeKey))
	tags := map[string]string{
		"k3s-server": k3sServerURL,
		"k3s-token":  k3sToken,
		"munge-key":  mungeKeyB64,
		"phase":      string(PhaseReady),
	}
	if err := d.discovery.UpdateTags(tags); err != nil {
		return fmt.Errorf("serf update tags (hint: total tags must fit in %d bytes): %w", 512, err)
	}
	d.mu.Lock()
	d.phase = PhaseReady
	d.mu.Unlock()
	d.logger.Infof("Phase → %s (k3s-server=%s, munge-key published atomically)", PhaseReady, k3sServerURL)
	return nil
}

// syncWithLeader forces an immediate TCP push-pull state exchange with the current leader.
// This ensures that large tag values (munge-key, k3s-token) that may not fit in a UDP
// gossip packet are fetched via TCP before we try to read them.
func (d *Daemon) syncWithLeader() {
	leaderName := d.leaderElector.ComputeLeader()
	for _, m := range d.discovery.Members() {
		if m.Name != leaderName {
			continue
		}
		// serf.Member.Port is the memberlist port (same as BindPort / 7946).
		addr := fmt.Sprintf("%s:%d", m.Addr.String(), m.Port)
		d.logger.Infof("Forcing TCP state sync with leader %s at %s", leaderName, addr)
		if n, err := d.discovery.Join([]string{addr}); err != nil {
			d.logger.Debugf("Leader TCP sync: %v (synced %d)", err, n)
		}
		// Brief pause so the push-pull goroutine can complete before we read tags.
		time.Sleep(3 * time.Second)
		return
	}
	d.logger.Warn("syncWithLeader: leader not found in member list — skipping")
}

// getLeaderTags returns all tags from the leader member for diagnostics.
func (d *Daemon) getLeaderTags() map[string]string {
	leaderName := d.leaderElector.ComputeLeader()
	for _, m := range d.discovery.Members() {
		if m.Name == leaderName {
			return m.Tags
		}
	}
	return nil
}

// buildWorkerList returns the current list of alive non-leader Serf members as WorkerInfo.
func (d *Daemon) buildWorkerList() []controller.WorkerInfo {
	leaderName := d.leaderElector.ComputeLeader()
	var workers []controller.WorkerInfo
	for _, m := range d.discovery.GetAliveMembers() {
		if m.Name == leaderName {
			continue
		}
		phase := m.Tags["phase"]
		if phase == string(PhaseDiscovering) || phase == string(PhaseElecting) {
			continue // not yet participating
		}
		cpu, _ := strconv.Atoi(m.Tags["cpu"])
		if cpu < 1 {
			cpu = 1
		}
		// Use the Serf member address as NodeAddr; Name as NodeName.
		// The node name matches what slurmd registers with (its Serf node name).
		workers = append(workers, controller.WorkerInfo{
			Name: m.Name,
			Addr: m.Addr.String(),
			CPUs: cpu,
		})
	}
	return workers
}

// watchWorkersAndReconfigureSLURM polls Serf membership every 15 s and reconfigures
// slurmctld when the worker set changes.  This ensures the SLURM node list stays in sync
// without requiring a controller restart — slurmctld is sent SIGHUP + scontrol reconfigure.
func (d *Daemon) watchWorkersAndReconfigureSLURM() {
	d.logger.Info("Starting SLURM worker watcher")
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	lastHash := ""
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
		}

		d.mu.RLock()
		ctrl := d.slurmCtrl
		d.mu.RUnlock()
		if ctrl == nil {
			return
		}

		workers := d.buildWorkerList()

		// Build a simple hash of worker names to detect changes without spurious reconfigures.
		hash := ""
		for _, w := range workers {
			hash += w.Name + "," + w.Addr + ";"
		}
		if hash == lastHash {
			continue
		}
		lastHash = hash

		d.logger.Infof("SLURM worker list changed — reconfiguring (%d workers)", len(workers))
		if err := ctrl.Reconfigure(workers); err != nil {
			d.logger.Warnf("SLURM reconfigure: %v", err)
		}
	}
}

// detectTailscaleIP runs 'tailscale ip -4' and returns the IP string, or "" on failure.
// This is called early in Start() so the IP is available before Serf is initialised.
func (d *Daemon) detectTailscaleIP() string {
	cmd := exec.Command("tailscale", "ip", "-4")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(string(out))
	if net.ParseIP(ip) == nil {
		return ""
	}
	return ip
}

// getLeaderTag returns the value of a tag from the current leader's Serf member.
func (d *Daemon) getLeaderTag(key string) (string, bool) {
	leaderName := d.leaderElector.ComputeLeader()
	for _, m := range d.discovery.Members() {
		if m.Name == leaderName {
			val, ok := m.Tags[key]
			return val, ok
		}
	}
	return "", false
}

// waitForLeaderTag polls until the leader has the tag set to a non-empty value.
// If wantValue is non-empty, it waits until the tag equals that value exactly.
func (d *Daemon) waitForLeaderTag(key, wantValue string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-d.ctx.Done():
			return "", fmt.Errorf("context cancelled")
		default:
		}
		if val, ok := d.getLeaderTag(key); ok && val != "" {
			if wantValue == "" || val == wantValue {
				return val, nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return "", fmt.Errorf("timeout waiting for leader tag %q after %v", key, timeout)
}

// extractHost parses the host from a URL like "https://IP:port".
func (d *Daemon) extractHost(rawURL string) string {
	s := strings.TrimPrefix(rawURL, "https://")
	s = strings.TrimPrefix(s, "http://")
	if idx := strings.LastIndex(s, ":"); idx != -1 {
		return s[:idx]
	}
	return s
}

// getLocalIP returns the Tailscale IP if available, otherwise the first usable IP.
func (d *Daemon) getLocalIP() string {
	if d.tailscale != nil {
		if ip := d.tailscale.GetLocalIP(); ip != nil {
			return ip.String()
		}
	}
	addrs, err := d.getUsableAddresses()
	if err == nil && len(addrs) > 0 {
		parts := strings.Fields(addrs[0])
		if len(parts) > 0 {
			return parts[0]
		}
	}
	return ""
}

// --------------------------------------------------------------------------
// Discovery initialisation
// --------------------------------------------------------------------------

func (d *Daemon) initDiscovery(localNode *state.Node, advertiseAddr string) error {
	d.logger.Info("Initialising Serf discovery")

	var encryptKey []byte
	if d.config.Discovery.EncryptKey != "" {
		var err error
		encryptKey, err = discovery.ParseEncryptKey(d.config.Discovery.EncryptKey)
		if err != nil {
			return fmt.Errorf("parse encryption key: %w", err)
		}
	}

	// wg_pubkey intentionally omitted: WireGuard is replaced by Tailscale and the
	// pubkey (~55 bytes base64) needlessly consumes Serf's 512-byte MetaMaxSize budget.
	tags := map[string]string{}

	// Always bind on 0.0.0.0 so we accept gossip from both LAN and Tailscale interfaces.
	// Tailscale nodes that block port 7946 may still reach us via LAN, and vice versa.
	discoveryCfg := &discovery.Config{
		NodeName:       d.config.Discovery.NodeName,
		NodeID:         d.identity.NodeID,
		BindAddr:       "0.0.0.0",
		BindPort:       d.config.Discovery.BindPort,
		AdvertiseAddr:  advertiseAddr, // Tailscale IP when available
		BootstrapPeers: d.config.Discovery.BootstrapPeers,
		EncryptKey:     encryptKey,
		ClusterAuthKey: d.config.Cluster.AuthKey,
		Tags:           tags,
		Logger:         d.logger,
	}

	disc, err := discovery.New(discoveryCfg, d.clusterState, localNode)
	if err != nil {
		return fmt.Errorf("create discovery: %w", err)
	}
	d.discovery = disc
	d.logger.Infof("Discovery initialised (cluster size: %d)", disc.GetClusterSize())
	return nil
}

// --------------------------------------------------------------------------
// Networking (Tailscale)
// --------------------------------------------------------------------------

func (d *Daemon) initNetworking() error {
	d.logger.Info("Initialising networking (Tailscale)")

	if !networking.IsTailscaleAvailable() {
		d.logger.Warn("Tailscale not detected — cluster networking may be limited")
		return nil
	}

	ts, err := networking.NewTailscaleManager(&networking.TailscaleConfig{Logger: d.logger})
	if err != nil {
		d.logger.Warnf("Tailscale manager init failed: %v (continuing without Tailscale)", err)
		return nil
	}
	d.tailscale = ts

	d.clusterState.UpdateNodeTailscaleIP(d.identity.NodeID, ts.GetLocalIP().String())

	tailscaleIP := ts.GetLocalIP().String()
	if tailscaleIP != "" && tailscaleIP != "<nil>" {
		oldName := d.config.Discovery.NodeName
		d.config.Discovery.NodeName = tailscaleIP
		d.logger.Infof("Updated node name from %s to Tailscale IP: %s", oldName, tailscaleIP)
		if localNode, found := d.clusterState.GetNode(d.identity.NodeID); found {
			localNode.Name = tailscaleIP
			d.clusterState.AddNode(localNode)
		}
	}

	if d.discovery != nil {
		if err := d.updateSerfTailscaleIP(ts.GetLocalIP()); err != nil {
			d.logger.Warnf("Failed to update Serf tags with Tailscale IP: %v", err)
		}
	}

	if err := d.setUniqueHostname(ts.GetLocalIP()); err != nil {
		d.logger.Warnf("Failed to set unique hostname: %v (continuing)", err)
	}

	d.logger.Infof("Tailscale networking initialised, local IP: %s", ts.GetLocalIP())
	return nil
}

func (d *Daemon) updateSerfTailscaleIP(ip net.IP) error {
	if d.discovery == nil {
		return nil
	}
	return d.discovery.UpdateTags(map[string]string{"wgip": ip.String()})
}

func (d *Daemon) setUniqueHostname(tsIP net.IP) error {
	if tsIP == nil {
		return fmt.Errorf("no Tailscale IP")
	}
	ip4 := tsIP.To4()
	if ip4 == nil {
		return fmt.Errorf("not an IPv4 address: %s", tsIP)
	}
	newHostname := fmt.Sprintf("node-%d-%d", ip4[2], ip4[3])

	currentHostname, _ := os.Hostname()
	if currentHostname == newHostname {
		d.logger.Infof("Hostname already %s", newHostname)
		return nil
	}
	d.logger.Infof("Setting hostname: %s → %s (Tailscale IP %s)", currentHostname, newHostname, tsIP)

	os.WriteFile("/etc/hostname", []byte(newHostname+"\n"), 0644)

	cmd := exec.Command("hostnamectl", "set-hostname", newHostname)
	if err := cmd.Run(); err != nil {
		cmd = exec.Command("hostname", newHostname)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("set hostname: %w", err)
		}
	}

	d.updateHostsFile(newHostname, tsIP)
	d.config.Discovery.NodeName = newHostname
	d.logger.Infof("Hostname set to %s", newHostname)
	return nil
}

// --------------------------------------------------------------------------
// Tailscale failed-peer cache
// --------------------------------------------------------------------------

// peerFailCache prevents hammering unreachable Tailscale peers.
// Peers that fail a Serf join attempt are suppressed for failedPeerTTL before
// being retried, avoiding repeated 10-second TCP timeouts every discovery tick.
type peerFailCache struct {
	mu    sync.Mutex
	peers map[string]time.Time
}

const failedPeerTTL = 5 * time.Minute

// peerProbeTimeout is the TCP dial timeout used to pre-check whether a
// Tailscale peer has port 7946 open before handing it to Serf's Join.
// A short value (2 s) prevents the 10-second memberlist TCP timeout from
// stalling the discovery loop when peers are Tailscale-online but not
// running node-agent (phones, laptops, powered-off nodes, etc.).
const peerProbeTimeout = 2 * time.Second

func newPeerFailCache() *peerFailCache {
	return &peerFailCache{peers: make(map[string]time.Time)}
}

func (c *peerFailCache) shouldSkip(addr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.peers[addr]
	if !ok {
		return false
	}
	if time.Since(t) > failedPeerTTL {
		delete(c.peers, addr)
		return false
	}
	return true
}

func (c *peerFailCache) markFailed(addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.peers[addr] = time.Now()
}

// probePeers dials each address in parallel with peerProbeTimeout.
// Only addresses where the TCP handshake succeeds are returned.
// Unreachable addresses are immediately recorded as failed in the cache so
// subsequent discovery ticks skip them for failedPeerTTL.
func (d *Daemon) probePeers(addrs []string) []string {
	if len(addrs) == 0 {
		return nil
	}
	type result struct {
		addr string
		ok   bool
	}
	ch := make(chan result, len(addrs))
	for _, addr := range addrs {
		go func(a string) {
			conn, err := net.DialTimeout("tcp", a, peerProbeTimeout)
			if err == nil {
				conn.Close()
				ch <- result{a, true}
			} else {
				ch <- result{a, false}
			}
		}(addr)
	}
	var reachable []string
	for range addrs {
		r := <-ch
		if r.ok {
			reachable = append(reachable, r.addr)
		} else {
			d.failedPeerCache.markFailed(r.addr)
			d.logger.Debugf("Tailscale peer %s unreachable — skipping and caching for %v", r.addr, failedPeerTTL)
		}
	}
	return reachable
}

func (d *Daemon) tailscalePeerDiscoveryLoop() {
	d.discoverAndJoinTailscalePeers()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			d.discoverAndJoinTailscalePeers()
		case <-d.ctx.Done():
			return
		}
	}
}

func (d *Daemon) discoverAndJoinTailscalePeers() {
	cmd := exec.Command("tailscale", "status", "--json")
	output, err := cmd.Output()
	if err != nil {
		d.logger.Debugf("tailscale status: %v", err)
		return
	}

	var tsStatus struct {
		Self *struct {
			TailscaleIPs []string `json:"TailscaleIPs"`
		} `json:"Self"`
		Peer map[string]*struct {
			HostName     string   `json:"HostName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
			Online       bool     `json:"Online"`
		} `json:"Peer"`
	}
	if err := json.Unmarshal(output, &tsStatus); err != nil {
		d.logger.Debugf("parse tailscale status: %v", err)
		return
	}

	var selfIP string
	if tsStatus.Self != nil && len(tsStatus.Self.TailscaleIPs) > 0 {
		selfIP = tsStatus.Self.TailscaleIPs[0]
	}

	serfPort := d.config.Discovery.BindPort
	if serfPort == 0 {
		serfPort = 7946
	}

	knownAddrs := make(map[string]bool)
	for _, m := range d.discovery.Members() {
		knownAddrs[m.Addr.String()] = true
	}

	var peersToJoin []string
	for _, peer := range tsStatus.Peer {
		if !peer.Online || len(peer.TailscaleIPs) == 0 {
			continue
		}
		peerIP := peer.TailscaleIPs[0]
		if peerIP == selfIP || knownAddrs[peerIP] {
			continue
		}
		peerAddr := fmt.Sprintf("%s:%d", peerIP, serfPort)
		// Skip peers that recently failed — avoids a 10-second TCP timeout
		// on every discovery tick for devices that are Tailscale-online but
		// not running node-agent (e.g., phones, laptops, offline nodes).
		if d.failedPeerCache.shouldSkip(peerAddr) {
			d.logger.Debugf("Tailscale peer %s recently failed — skipping for %v", peerAddr, failedPeerTTL)
			continue
		}
		peersToJoin = append(peersToJoin, peerAddr)
	}

	if len(peersToJoin) == 0 {
		return
	}

	// Probe in parallel before handing to Serf so unreachable peers don't
	// cause sequential 10-second TCP timeouts inside memberlist's Join.
	peersToJoin = d.probePeers(peersToJoin)
	if len(peersToJoin) == 0 {
		return
	}

	d.logger.Infof("Tailscale discovery: joining %d peer(s): %v", len(peersToJoin), peersToJoin)
	n, err := d.discovery.Join(peersToJoin)
	if err != nil {
		d.logger.Debugf("Tailscale peer join: %v (joined %d)", err, n)
		// Mark peers that did not end up in the Serf member list as failed
		// so we don't retry them on every 15-second tick.
		current := make(map[string]bool)
		for _, m := range d.discovery.Members() {
			current[m.Addr.String()] = true
		}
		for _, peerAddr := range peersToJoin {
			host, _, _ := net.SplitHostPort(peerAddr)
			if !current[host] {
				d.failedPeerCache.markFailed(peerAddr)
				d.logger.Debugf("Peer %s cached as failed for %v", peerAddr, failedPeerTTL)
			}
		}
	}
	if n > 0 {
		d.logger.Infof("Tailscale discovery: joined %d new peer(s)", n)
	}
}

func (d *Daemon) updateHostsFile(hostname string, ip net.IP) {
	hostsEntry := fmt.Sprintf("%s\t%s\n", ip.String(), hostname)
	content, err := os.ReadFile("/etc/hosts")
	if err != nil {
		return
	}
	if strings.Contains(string(content), hostname) {
		return
	}
	f, err := os.OpenFile("/etc/hosts", os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(hostsEntry)
}

// --------------------------------------------------------------------------
// Status file
// --------------------------------------------------------------------------

func (d *Daemon) statusFileLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			d.writeStatusFile()
		case <-d.ctx.Done():
			return
		}
	}
}

func (d *Daemon) writeStatusFile() {
	statusDir := "/run/clusteros"
	statusFile := statusDir + "/status.json"
	if err := os.MkdirAll(statusDir, 0755); err != nil {
		return
	}

	type memberInfo struct {
		Name   string `json:"name"`
		Addr   string `json:"addr"`
		Status string `json:"status"`
		NodeID string `json:"node_id"`
		Phase  string `json:"phase,omitempty"`
	}

	var members []memberInfo
	aliveCount := 0
	if d.discovery != nil {
		for _, m := range d.discovery.Members() {
			status := "unknown"
			switch m.Status {
			case 1:
				status = "alive"
				aliveCount++
			case 2:
				status = "leaving"
			case 3:
				status = "left"
			case 4:
				status = "failed"
			}
			members = append(members, memberInfo{
				Name:   m.Name,
				Addr:   m.Addr.String(),
				Status: status,
				NodeID: m.Tags["node_id"],
				Phase:  m.Tags["phase"],
			})
		}
	}

	type statusData struct {
		Phase       string       `json:"phase"`
		IsLeader    bool         `json:"is_leader"`
		Leader      string       `json:"leader"`
		Joined      bool         `json:"joined"`
		MemberCount int          `json:"member_count"`
		Members     []memberInfo `json:"members"`
		UpdatedAt   string       `json:"updated_at"`
	}

	d.mu.RLock()
	sd := statusData{
		Phase:       string(d.phase),
		IsLeader:    d.isLeader,
		Leader:      d.leaderName,
		Joined:      aliveCount > 0,
		MemberCount: aliveCount,
		Members:     members,
		UpdatedAt:   time.Now().Format(time.RFC3339),
	}
	d.mu.RUnlock()

	// Refresh leader from elector in case it changed.
	if d.leaderElector != nil {
		if leader, err := d.leaderElector.GetLeader(); err == nil {
			sd.Leader = leader
		}
		sd.IsLeader = d.leaderElector.IsLeader()
	}

	jsonData, err := json.MarshalIndent(sd, "", "  ")
	if err != nil {
		return
	}
	tmpFile := statusFile + ".tmp"
	if err := os.WriteFile(tmpFile, jsonData, 0644); err != nil {
		return
	}
	os.Rename(tmpFile, statusFile)
}

// --------------------------------------------------------------------------
// Firewall
// --------------------------------------------------------------------------

func (d *Daemon) setupFirewallRules() error {
	d.logger.Info("Setting up firewall rules")
	rules := []struct{ port, proto, comment string }{
		{"22", "tcp", "SSH"},
		{"7946", "tcp", "Serf TCP"},
		{"7946", "udp", "Serf UDP"},
		{"6443", "tcp", "K3s API"},
		{"6817", "tcp", "SLURM slurmctld"},
		{"6818", "tcp", "SLURM slurmd"},
		{"6819", "tcp", "SLURM slurmdbd"},
		{"10250", "tcp", "Kubelet API"},
		{"2379:2380", "tcp", "etcd"},
		{"30000:32767", "tcp", "K8s NodePort"},
		{"30000:32767", "udp", "K8s NodePort"},
	}
	for _, rule := range rules {
		cmd := exec.Command("ufw", "allow", fmt.Sprintf("%s/%s", rule.port, rule.proto))
		if _, err := cmd.CombinedOutput(); err != nil {
			ipt := exec.Command("iptables", "-A", "INPUT", "-p", rule.proto,
				"--dport", rule.port, "-j", "ACCEPT")
			ipt.CombinedOutput()
		}
	}

	// Trust the Tailscale interface — traffic arriving on tailscale0/ts0 has already
	// been authenticated by Tailscale, so we don't need per-port rules for overlay traffic.
	for _, iface := range []string{"tailscale0", "ts0"} {
		exec.Command("ufw", "allow", "in", "on", iface).CombinedOutput()
	}
	// iptables CGNAT range fallback for kernels where interface-based rules don't apply.
	cmd := exec.Command("iptables", "-C", "INPUT", "-s", "100.64.0.0/10", "-j", "ACCEPT")
	if cmd.Run() != nil {
		exec.Command("iptables", "-I", "INPUT", "-s", "100.64.0.0/10", "-j", "ACCEPT").Run()
	}

	d.logger.Info("Firewall rules set (including Tailscale interface trust)")
	return nil
}

// --------------------------------------------------------------------------
// Network helpers
// --------------------------------------------------------------------------

func (d *Daemon) waitForNetwork() error {
	maxWait := 90 * time.Second
	interval := 2 * time.Second
	start := time.Now()
	d.logger.Info("Waiting for network...")
	for {
		addrs, err := d.getUsableAddresses()
		if err == nil && len(addrs) > 0 {
			d.logger.Infof("Network available: %d usable address(es)", len(addrs))
			return nil
		}
		elapsed := time.Since(start)
		if elapsed >= maxWait {
			d.logNetworkState()
			return fmt.Errorf("timeout waiting for network after %v", elapsed)
		}
		d.logger.Infof("No usable address yet (%v remaining)", (maxWait - elapsed).Round(time.Second))
		time.Sleep(interval)
	}
}

func (d *Daemon) getUsableAddresses() ([]string, error) {
	var usable []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}
			usable = append(usable, fmt.Sprintf("%s (%s)", ip.String(), iface.Name))
		}
	}
	return usable, nil
}

func (d *Daemon) logNetworkState() {
	d.logger.Warn("Network state dump:")
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		flags := ""
		if iface.Flags&net.FlagUp != 0 {
			flags += "UP "
		}
		if iface.Flags&net.FlagLoopback != 0 {
			flags += "LOOPBACK "
		}
		d.logger.Infof("  %s: [%s]", iface.Name, flags)
	}
}

// --------------------------------------------------------------------------
// Status / diagnostics
// --------------------------------------------------------------------------

// ServiceStatus represents the status of a system service.
type ServiceStatus struct {
	Name    string
	Status  string
	Message string
}

func (d *Daemon) checkSystemdService(serviceName string) ServiceStatus {
	cmd := exec.Command("systemctl", "is-active", serviceName)
	output, err := cmd.Output()
	ss := ServiceStatus{Name: serviceName}
	if err != nil {
		ss.Status = "error"
		ss.Message = fmt.Sprintf("check failed: %v", err)
		return ss
	}
	switch strings.TrimSpace(string(output)) {
	case "active":
		ss.Status = "running"
	case "inactive":
		ss.Status = "stopped"
	case "failed":
		ss.Status = "error"
	default:
		ss.Status = "unknown"
	}
	return ss
}

func (d *Daemon) getResourceUsage() map[string]interface{} {
	res := map[string]interface{}{"cpu_cores": runtime.NumCPU()}
	if memInfo, err := d.getMemoryInfo(); err == nil {
		res["memory"] = memInfo
	}
	if diskInfo, err := d.getDiskUsage("/"); err == nil {
		res["disk"] = diskInfo
	}
	return res
}

func (d *Daemon) getMemoryInfo() (map[string]interface{}, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	info := make(map[string]interface{})
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "MemTotal") {
			if v, err := d.parseMemValue(line); err == nil {
				info["total_kb"] = v
			}
		} else if strings.Contains(line, "MemAvailable") {
			if v, err := d.parseMemValue(line); err == nil {
				info["available_kb"] = v
			}
		}
	}
	return info, nil
}

func (d *Daemon) parseMemValue(line string) (int64, error) {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return 0, fmt.Errorf("invalid line")
	}
	return strconv.ParseInt(strings.TrimSuffix(parts[1], "kB"), 10, 64)
}

func (d *Daemon) getDiskUsage(path string) (map[string]interface{}, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return nil, err
	}
	total := stat.Blocks * uint64(stat.Bsize)
	avail := stat.Bavail * uint64(stat.Bsize)
	used := total - avail
	return map[string]interface{}{
		"total_bytes":     total,
		"available_bytes": avail,
		"used_bytes":      used,
		"used_percent":    float64(used) / float64(total) * 100,
	}, nil
}

// GetComprehensiveStatus returns a map suitable for the status API.
func (d *Daemon) GetComprehensiveStatus() map[string]interface{} {
	status := map[string]interface{}{
		"node": map[string]interface{}{
			"id":      d.identity.NodeID,
			"name":    d.config.Discovery.NodeName,
			"cluster": d.config.Cluster.Name,
		},
	}

	d.mu.RLock()
	status["phase"] = string(d.phase)
	status["is_leader"] = d.isLeader
	status["leader"] = d.leaderName
	d.mu.RUnlock()

	netStatus := map[string]interface{}{"type": "tailscale"}
	if d.tailscale != nil {
		if ip := d.tailscale.GetLocalIP(); ip != nil {
			netStatus["tailscale_ip"] = ip.String()
			netStatus["connected"] = true
		}
	}
	status["networking"] = netStatus
	status["resources"] = d.getResourceUsage()
	return status
}
