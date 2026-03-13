package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"os/signal"
	"runtime"
	"sort"
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
	version       string // binary version string published as Serf tag "ver"
	buildTime     string // binary build timestamp, appended for dirty builds

	mu             sync.RWMutex
	phase          ClusterPhase
	isLeader       bool
	leaderName     string
	slurmCtrl      *controller.SLURMController // non-nil only on leader
	failedPeerCache *peerFailCache
	k3sServerURL   string // URL this worker joined with; empty on leader nodes
}

// Config contains configuration for creating a daemon.
type Config struct {
	Config   *config.Config
	Identity *identity.Identity
	Logger   *logrus.Logger
	// Version is the binary version string (git describe output). Published as a Serf
	// tag so peers can exclude stale-version nodes from leader election.
	Version string
	// BuildTime is the binary build timestamp. For dirty builds it is appended to
	// Version in the Serf tag so binaries built at different times are treated as
	// distinct versions even when the commit hash is the same.
	BuildTime string
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
		version:         cfg.Version,
		buildTime:       cfg.BuildTime,
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

	// WireGuardPubKey is derived here for local state bookkeeping only.
	// It is NOT published to Serf tags (see initSerf — wg_pubkey intentionally omitted)
	// because WireGuard has been replaced by Tailscale for overlay networking.
	// The field is retained in state.Node for compatibility with any code that reads it;
	// it does not affect network connectivity.
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
		WireGuardPubKey: wgPubKey, // legacy field — not used for networking (Tailscale handles overlay)
	}
	d.clusterState.AddNode(localNode)

	// Detect Tailscale IP BEFORE Serf init so we can advertise it as our gossip address.
	// Nodes binding Serf to 0.0.0.0 accept connections from any interface, but peers need
	// to know which IP to contact us on; using the Tailscale IP routes through the encrypted
	// overlay even when direct LAN port 7946 access is unavailable.
	// Tailscale may still be authenticating (OAuth can take 10-30s after boot), so we retry
	// for up to 45 seconds rather than giving up immediately.  A p2p-patched node without
	// its own Tailscale connection will time out here and fall back to advertising its LAN IP
	// (which is still correct — the gateway node handles routing for it).
	tsAdvertiseIP := d.detectTailscaleIPWithRetry(45 * time.Second)
	if tsAdvertiseIP != "" {
		d.logger.Infof("Tailscale IP detected: %s — using as Serf advertise address", tsAdvertiseIP)
	} else {
		d.logger.Warn("Tailscale IP not available at startup — advertising LAN IP; cluster may rely on a gateway node for routing")
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

	// Allow an operator or the patch workflow to temporarily skip firewall
	// modifications. When `CLUSTEROS_SKIP_FIREWALL` is set to "1" or "true",
	// the daemon will not run `setupFirewallRules()` at startup. This is used
	// by `apply-patch.sh` to avoid racing with iptables manipulations during
	// patching and prevents the agent from re-adding redirect rules that would
	// block outbound traffic while the patch is being applied.
	if v := os.Getenv("CLUSTEROS_SKIP_FIREWALL"); v == "1" || strings.ToLower(v) == "true" {
		d.logger.Info("CLUSTEROS_SKIP_FIREWALL set — skipping firewall setup at startup")
	} else {
		if err := d.setupFirewallRules(); err != nil {
			d.logger.Warnf("Firewall setup failed: %v (continuing)", err)
		}
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
				if strings.Contains(err.Error(), "leadership lost") {
					// We are no longer the elected leader — skip re-election and
					// go straight to joining so we don't restart a competing k3s server.
					d.logger.Warnf("Leadership lost during provisioning: %v — switching to joining", err)
					d.mu.Lock()
					d.isLeader = false
					d.leaderName = d.leaderElector.ComputeLeader()
					d.mu.Unlock()
					time.Sleep(5 * time.Second)
					d.setPhase(PhaseJoining)
				} else {
					d.logger.Errorf("Provisioning failed: %v — retrying in 30s", err)
					time.Sleep(30 * time.Second)
					d.setPhase(PhaseElecting)
				}
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
	// Clear stale provisioning tags from any previous session.  If this node was
	// the k3s leader in a prior run and then rebooted, its Serf gossip state may
	// still carry k3s-server / k3s-token / munge-key / phase=ready.  Other nodes
	// check port 6443 on any member that has a k3s-server tag; if k3s hasn't
	// started yet the probe fails and they exclude this node from the election —
	// then they elect a different node that has no k3s state, producing a divergent
	// cluster.  Clearing the tags before the election window prevents this.
	staleTags := []string{"k3s-server", "k3s-token", "munge-key", "k3s-nodes"}
	if err := d.discovery.DeleteTags(staleTags); err != nil {
		d.logger.Debugf("clearStaleTags: %v (non-fatal)", err)
	} else {
		d.logger.Info("Cleared stale provisioning tags (k3s-server, k3s-token, munge-key, k3s-nodes)")
	}
	// Also reset phase to electing so other nodes don't see a stale phase=ready.
	if err := d.discovery.UpdateTags(map[string]string{"phase": string(PhaseElecting)}); err != nil {
		d.logger.Debugf("setPhaseTag(electing): %v (non-fatal)", err)
	}

	d.logger.Info("Electing leader — waiting for peer discovery to stabilise...")
	// Wait for membership to stop growing for 10 consecutive seconds.
	// This gives the Tailscale and LAN peer discovery goroutines time to probe
	// and join all reachable nodes before we elect a leader on a partial view.
	// Minimum: 15s (Tailscale probe runs immediately but TCP+push-pull takes time).
	// Maximum: 30s (avoid blocking indefinitely if the cluster is genuinely small).
	lastCount := 0
	stable := 0
	for start := time.Now(); time.Since(start) < 30*time.Second; {
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		count := len(d.discovery.GetAliveMembers())
		if count > lastCount {
			lastCount = count
			stable = 0
			d.logger.Debugf("Election: membership growing (%d alive) — waiting for stability", count)
		} else {
			stable++
			if stable >= 5 && time.Since(start) >= 15*time.Second {
				// Stable for 10s AND at least 15s have elapsed — safe to elect.
				break
			}
		}
	}
	d.logger.Infof("Election: proceeding with %d alive member(s)", len(d.discovery.GetAliveMembers()))

	// If any alive member already has phase=ready AND a k3s-server URL, they ARE
	// the current cluster leader. Adopt them and join as worker — don't re-elect
	// based on lexicographic rank. This prevents a returning node with a lower
	// hostname from disrupting an established cluster where k3s is already running.
	if existingLeader := d.findRunningLeader(); existingLeader != "" {
		d.logger.Infof("Cluster already has a running leader: %s — joining as worker", existingLeader)
		d.mu.Lock()
		d.isLeader = false
		d.leaderName = existingLeader
		d.mu.Unlock()
		d.setPhase(PhaseJoining)
		return
	}

	// Compute leader from a "reachable alive" set: alive Serf members that either
	// have no k3s-server tag (fresh nodes) OR have a k3s-server URL that's TCP-reachable.
	// This excludes ghost nodes running an old node-agent with stale phase=ready /
	// k3s-server tags whose actual k3s server is gone (e.g. after a cluster wipe,
	// or a node that was skipped during a rolling redeploy and is still on the LAN
	// with its old Tailscale IP unreachable).
	leader, isLeader := d.computeReachableLeader()

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

// computeReachableLeader builds a filtered alive-member list that excludes
// nodes advertising an unreachable k3s-server endpoint (ghost / stale-tag nodes),
// then picks the most network-central node as leader using Serf's Vivaldi
// coordinate system.  The "most central" node has the lowest sum of estimated
// RTTs to all other alive members — this naturally prefers a wired server-room
// node over a laptop on mobile data, and works correctly across WAN/LAN/P2P
// mixed topologies without any manual configuration.
//
// Scoring falls back to lexicographic order when coordinates are not yet
// available (early cluster formation before Vivaldi has converged).
//
// Returns (leaderName, amILeader).
func (d *Daemon) computeReachableLeader() (string, bool) {
	myName := d.discovery.LocalMember().Name
	aliveMembers := d.discovery.GetAliveMembers()

	// Build the candidate set: self always included; peers included only if
	// they do not advertise an unreachable k3s-server (ghost-node defence).
	type candidate struct {
		name string
	}
	candidates := []candidate{{name: myName}}

	myVer := d.version
	// Strip the "-dirty" suffix for version comparison — dirty builds are development
	// snapshots of the same commit and should be treated as equivalent.
	if idx := strings.Index(myVer, "-dirty"); idx >= 0 {
		myVer = myVer[:idx]
	}

	for _, m := range aliveMembers {
		if m.Name == myName {
			continue
		}
		// Exclude peers running a different binary version from the leader election.
		// A node running old code may have stale Serf tags or a missing k3s etcd wipe
		// and should not be elected leader (it would produce a broken cluster or fail
		// to reach phase=ready). We still allow them as WORKERS (joining is version-agnostic).
		if myVer != "" {
			peerVer := m.Tags["ver"]
			if idx := strings.Index(peerVer, "-dirty"); idx >= 0 {
				peerVer = peerVer[:idx]
			}
			if peerVer != "" && peerVer != myVer {
				d.logger.Warnf("Election: excluding %s — version mismatch (peer=%s, local=%s)", m.Name, peerVer, myVer)
				continue
			}
		}
		if k3sURL := m.Tags["k3s-server"]; k3sURL != "" {
			host := d.extractHost(k3sURL)
			if !strings.Contains(host, ":") {
				host = host + ":6443"
			}
			conn, err := net.DialTimeout("tcp", host, 2*time.Second)
			if err != nil {
				d.logger.Warnf("Election: excluding %s — k3s-server %s unreachable (%v)", m.Name, host, err)
				continue
			}
			conn.Close()
		}
		candidates = append(candidates, candidate{name: m.Name})
	}

	// Use lexicographic ordering to pick the leader deterministically.
	// Every node with the same candidate set will reach the same result regardless
	// of when the election runs — this prevents circular deadlocks where node A
	// elects node B and node B elects node A due to different RTT snapshots.
	// Lexicographic is fast, O(n log n), and globally consistent across the cluster.
	names := make([]string, len(candidates))
	for i, c := range candidates {
		names[i] = c.name
	}
	sort.Strings(names)
	leader := names[0]
	d.logger.Infof("Election: lexicographic winner %s (%d candidates)", leader, len(candidates))
	return leader, leader == myName
}

// findRunningLeader returns the name of any alive Serf member that is acting as
// the cluster leader — identified by having both phase=ready AND a non-empty
// k3s-server tag (only the elected leader publishes that tag).
// Workers also reach phase=ready but never publish k3s-server, so they are excluded.
// Additionally confirms the candidate's k3s port (6443) is reachable so we don't
// adopt a ghost leader whose Serf entry is still "alive" but the node is offline.
// Returns "" if no active leader exists (fresh cluster formation).
func (d *Daemon) findRunningLeader() string {
	for _, m := range d.discovery.Members() {
		if m.Status.String() != "alive" {
			continue
		}
		if m.Tags["phase"] != string(PhaseReady) || m.Tags["k3s-server"] == "" {
			continue
		}
		// Confirm the leader is actually reachable before adopting it.
		// This prevents adopting a node whose Serf entry is still "alive" but
		// the machine is offline (e.g. a node that rebooted and hasn't rejoined yet).
		k3sURL := m.Tags["k3s-server"]
		host := strings.TrimPrefix(k3sURL, "https://")
		host = strings.TrimPrefix(host, "http://")
		conn, err := net.DialTimeout("tcp", host, 2*time.Second)
		if err != nil {
			d.logger.Debugf("findRunningLeader: candidate %s k3s port unreachable (%v) — skipping", m.Name, err)
			continue
		}
		conn.Close()
		return m.Name
	}
	return ""
}

// findCompetingLeader returns the name of an alive Serf member that is ALSO
// an established leader (phase=ready + k3s-server tag) to whom we should yield.
// We yield to a competing leader if they have MORE k3s nodes registered (larger
// cluster). Ties are broken by lexicographic name (lower name wins).
// Returns "" if we are the sole/correct leader.
func (d *Daemon) findCompetingLeader() string {
	myName := d.discovery.LocalMember().Name
	myK3sCount := d.countK3sNodes()

	for _, m := range d.discovery.Members() {
		if m.Name == myName {
			continue
		}
		if m.Status.String() != "alive" {
			continue
		}
		if m.Tags["phase"] != string(PhaseReady) || m.Tags["k3s-server"] == "" {
			continue
		}
		// An established leader — compare cluster sizes.
		theirCount, _ := strconv.Atoi(m.Tags["k3s-nodes"])
		if theirCount > myK3sCount {
			// Their cluster is larger — we should yield.
			return m.Name
		}
		if theirCount == myK3sCount && m.Name < myName {
			// Equal size — lower lexicographic name wins as tiebreaker.
			return m.Name
		}
	}
	return ""
}

// countK3sNodes returns the number of nodes registered in the local k3s API.
// Returns 0 if k3s is not running or kubectl fails.
func (d *Daemon) countK3sNodes() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig=/etc/rancher/k3s/k3s.yaml",
		"get", "nodes", "--no-headers", "--ignore-not-found",
	).Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return 0
	}
	return strings.Count(strings.TrimSpace(string(out)), "\n") + 1
}

// assertStillLeader returns an error if another node now ranks lower (lexicographically)
// than us in the alive member list, meaning we should yield leadership.
// This is called at slow checkpoints inside runProvisioning so we don't race to
// start a k3s server when another node has already become the elected leader.
func (d *Daemon) assertStillLeader() error {
	if !d.leaderElector.IsLeader() {
		newLeader := d.leaderElector.ComputeLeader()
		return fmt.Errorf("leadership lost to %s — aborting provisioning", newLeader)
	}
	return nil
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

	// Distribute the actual running cluster CA cert via Serf user event.
	// This eliminates the x509 "certificate signed by unknown authority" error
	// when workers were patched with a different k3s-ca.crt than what the leader
	// is actually using.  Workers install this cert before starting the k3s agent.
	if caCert, err := k3sServer.ReadCACert(); err != nil {
		d.logger.Warnf("Could not read cluster CA cert for distribution: %v", err)
	} else if d.discovery != nil {
		d.logger.Infof("Broadcasting cluster CA cert via Serf user event (%d bytes)", len(caCert))
		if err := d.discovery.SendEvent("k3s-ca-cert", caCert, false); err != nil {
			d.logger.Warnf("Failed to send k3s-ca-cert user event: %v", err)
		}
	}

	// Label this k8s node with its extra disk count so Longhorn registration
	// can read it without needing SSH or Serf tag budget for path strings.
	if paths := readExtraDiskPaths(); len(paths) > 0 {
		hostname, _ := os.Hostname()
		label := fmt.Sprintf("clusteros-ndisks=%d", len(paths))
		if out, err := exec.Command("k3s", "kubectl", "label", "node", hostname, label, "--overwrite").CombinedOutput(); err != nil {
			d.logger.Warnf("Could not label node with disk count: %v — %s", err, string(out))
		} else {
			d.logger.Infof("Labelled k8s node %s with %s", hostname, label)
		}
	}

	serverURL := fmt.Sprintf("https://%s:6443", nodeIP)

	// Record in cluster state so ondemand can find the K3s leader.
	d.clusterState.SetLeader("k3s-server", d.identity.NodeID)

	// --- Munge key ---
	mungeManager := slurmauth.NewMungeKeyManager(d.logger)
	var mungeKey []byte

	// Prefer an existing local key (persistent across reboots/elections).
	if k, err := mungeManager.ReadMungeKey(); err == nil && len(k) == slurmauth.MungeKeySize {
		d.logger.Info("Found existing /etc/munge/munge.key — reusing")
		mungeKey = k
	} else if d.discovery != nil {
		// If no local key, check our Serf tags (could have been published earlier).
		if tag, ok := d.getLeaderTag("munge-key"); ok && tag != "" {
			if k2, err := base64.StdEncoding.DecodeString(tag); err == nil && len(k2) == slurmauth.MungeKeySize {
				d.logger.Info("Found munge-key in Serf tags — using and persisting to disk")
				mungeKey = k2
				if err := mungeManager.WriteMungeKey(mungeKey); err != nil {
					d.logger.Warnf("Failed to persist munge key from tag: %v", err)
				}
			} else {
				d.logger.Warn("munge-key tag present but failed to decode or wrong size")
			}
		}
	}

	// If still empty, generate a fresh key.
	if mungeKey == nil {
		mungeKey, err = mungeManager.GenerateMungeKey()
		if err != nil {
			return fmt.Errorf("generate munge key: %w", err)
		}
	}

	// Publish the munge key immediately so workers can start `munged` as soon
	// as the leader is elected. The full cluster-ready payload is still
	// published atomically later by publishClusterReady.
	// NOTE: do NOT publish extra diagnostic tags (e.g. munge-key-hash) here —
	// the Serf MetaMaxSize is 512 bytes across all 11 tags; every extra tag
	// risks pushing the publishClusterReady call over the limit.
	if d.discovery != nil {
		mungeKeyB64 := base64.StdEncoding.EncodeToString(mungeKey)
		d.logger.Infof("Publishing early munge-key (len=%d)", len(mungeKeyB64))
		d.publishTag("munge-key", mungeKeyB64)
		if err := d.discovery.SendEvent("munge-key", []byte(mungeKeyB64), false); err != nil {
			d.logger.Warnf("Failed to send munge-key user event: %v", err)
		} else {
			d.logger.Debug("Sent munge-key as Serf user event for reliable delivery")
		}
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

	// Start slurmd on the leader so it participates as a compute node.
	// This is essential for single-node clusters and allows jobs to run on the leader.
	// Uses NewSLURMWorkerRoleNoConfig to avoid overwriting the controller's full slurm.conf.
	// Register with the role manager BEFORE Start() so that if the first attempt fails
	// (e.g. slurmctld still initializing), the 30s health-check loop will keep retrying.
	d.logger.Info("Starting slurmd on leader node...")
	leaderWorker := worker.NewSLURMWorkerRoleNoConfig(nodeIP, mungeKey, nodeIP, d.logger)
	d.roleManager.RegisterRole("slurm-worker", leaderWorker)
	if err := leaderWorker.Start(); err != nil {
		d.logger.Warnf("slurmd on leader failed to start: %v — will retry via HealthCheck", err)
	} else {
		d.logger.Info("slurmd running on leader — leader participates as compute node")
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
	var mungeKey []byte
	mungeKeyB64, err := d.waitForLeaderTag("munge-key", "", 5*time.Minute)
	if err != nil {
		d.logger.Warnf("munge-key tag not found; leader tags visible: %v — will try local munge.key or user-event delivery", d.getLeaderTags())
		// Attempt to read a locally-applied munge key (might have arrived via user event)
		mungeManager := slurmauth.NewMungeKeyManager(d.logger)
		if k, err2 := mungeManager.ReadMungeKey(); err2 == nil {
			d.logger.Info("Found local /etc/munge/munge.key written by user event; using it")
			mungeKey = k
		} else {
			// No local key — fail with original error
			return fmt.Errorf("read munge-key tag: %w", err)
		}
	} else {
		mungeKey, err = base64.StdEncoding.DecodeString(mungeKeyB64)
		if err != nil {
			return fmt.Errorf("decode munge key: %w", err)
		}
	}

	// Extract controller IP from the K3s server URL (https://<IP>:6443).
	controllerIP := d.extractHost(k3sServerURL)
	nodeIP := d.getLocalIP()

	// Remember which server URL we joined with so runReady() can detect changes.
	d.mu.Lock()
	d.k3sServerURL = k3sServerURL
	d.mu.Unlock()

	// Wait for Tailscale before attempting to join k3s.
	// The k3s server URL is a Tailscale IP (e.g. https://100.x.x.x:6443).
	// On a freshly patched node, Tailscale OAuth can take 30-120s to authenticate.
	// We waited 45s at startup; give it an additional 90s here (total ~2.5 min).
	// If Tailscale is already up, this returns instantly.
	if tsIP := d.detectTailscaleIPWithRetry(90 * time.Second); tsIP != "" {
		d.logger.Infof("Tailscale ready (%s) — proceeding with k3s join", tsIP)
		// Update wgip Serf tag so other nodes know our Tailscale IP.
		_ = d.discovery.UpdateTags(map[string]string{"wgip": tsIP})
		// Use Tailscale IP for SLURM (which registers by nodeIP with the controller).
		nodeIP = tsIP
	} else {
		d.logger.Warn("Tailscale not available after 2.5min — attempting k3s join via LAN routing")
	}

	// --- K3s agent ---
	// Non-fatal: if k3s agent fails (binary missing, version mismatch, TLS error),
	// we log a warning and continue — SLURM must still start regardless.
	d.logger.Infof("Starting K3s agent → %s", k3sServerURL)
	k3sAgent := k3s.NewK3sAgentRole(k3sServerURL, k3sToken, nodeIP, d.logger)
	if err := k3sAgent.Start(); err != nil {
		d.logger.Warnf("k3s agent failed to start: %v — SLURM will still start", err)
	} else {
		d.roleManager.RegisterRole("k3s-agent", k3sAgent)
		// Label this worker node with its extra disk count so the leader's Longhorn
		// registration goroutine can read it from the k8s API without SSH.
		if paths := readExtraDiskPaths(); len(paths) > 0 {
			hostname, _ := os.Hostname()
			label := fmt.Sprintf("clusteros-ndisks=%d", len(paths))
			// Wait briefly for agent registration before labelling.
			time.Sleep(30 * time.Second)
			if out, err := exec.Command("k3s", "kubectl",
				"--server", k3sServerURL,
				"label", "node", hostname, label, "--overwrite").CombinedOutput(); err != nil {
				d.logger.Warnf("Could not label worker node with disk count: %v — %s", err, string(out))
			} else {
				d.logger.Infof("Labelled worker k8s node %s with %s", hostname, label)
			}
		}
	}

	// --- SLURM worker ---
	// Always attempt, even if k3s failed above.
	d.logger.Infof("Starting SLURM worker (controller=%s)", controllerIP)
	slurmWorker := worker.NewSLURMWorkerRole(controllerIP, mungeKey, nodeIP, d.logger)
	if err := slurmWorker.Start(); err != nil {
		return fmt.Errorf("start slurm worker: %w", err)
	}
	d.roleManager.RegisterRole("slurm-worker", slurmWorker)

	d.setPhase(PhaseReady)
	return nil
}

// runReady loops, monitoring for leadership changes and remote k3s server availability.
func (d *Daemon) runReady() {
	d.logger.Info("Cluster ready — monitoring leadership")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// serverUnreachable counts how many consecutive 30s checks have found the
	// k3s server at joinedURL unreachable.  After 10 checks (~5 min) we force a
	// re-join.  This catches the case where the old leader's Serf member is still
	// in "suspected" state (not yet marked failed), so ComputeLeader() keeps
	// returning the old name and getLeaderTag returns the same URL as joinedURL —
	// meaning the URL-change check below never fires even though a new leader with
	// a different k3s server has been elected.
	serverUnreachable := 0

	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.mu.RLock()
			wasLeader := d.isLeader
			joinedURL := d.k3sServerURL
			d.mu.RUnlock()

			if wasLeader {
				// Publish current k3s node count so any competing leader in a merging
				// Serf sub-cluster can compare cluster sizes and decide who yields.
				if count := d.countK3sNodes(); count > 0 {
					d.publishTag("k3s-nodes", strconv.Itoa(count))
				}

				// Split-brain merge: when Tailscale/LAN discovery joins two previously
				// isolated Serf sub-clusters, both leaders suddenly see each other.
				// The leader of the SMALLER cluster yields to the larger one so running
				// workloads are preserved. Equal size → lower lexicographic name wins.
				// We only yield to an *established* leader (phase=ready + k3s-server tag),
				// never to a fresh node that merely has a lower name.
				if competing := d.findCompetingLeader(); competing != "" {
					myName := d.discovery.LocalMember().Name
					d.logger.Warnf("Split-brain detected: competing leader %s (we are %s) has larger/preferred cluster — yielding", competing, myName)
					d.mu.Lock()
					d.isLeader = false
					d.leaderName = competing
					d.mu.Unlock()
					// Remove our k3s-nodes leadership tag before stepping down.
					d.discovery.DeleteTags([]string{"k3s-nodes"})
					d.setPhase(PhaseJoining)
					return
				}
			}

			if !wasLeader && joinedURL != "" {
				// Check 1: URL change in Serf tags.
				// Fires when the leader publishes a new k3s-server tag (new leader elected
				// and Serf gossip has propagated the change).
				if currentURL, ok := d.getLeaderTag("k3s-server"); ok && currentURL != "" && currentURL != joinedURL {
					d.logger.Warnf("k3s server URL changed (%s → %s) — re-joining", joinedURL, currentURL)
					serverUnreachable = 0
					d.setPhase(PhaseJoining)
					return
				}

				// Check 2: Direct TCP reachability of port 6443 on the server we joined.
				// This catches the case where the old leader's Serf member is still
				// "suspected" (not yet failed) so ComputeLeader() keeps returning the same
				// name and Check 1 never fires, even though a new leader has been elected
				// and is already running k3s at a different IP.
				// After ~5 minutes of unreachability we re-join so the worker reads
				// whatever the CURRENT Serf leader is publishing, regardless of whether
				// the old member has been marked failed yet.
				serverHost := net.JoinHostPort(d.extractHost(joinedURL), "6443")
				conn, dialErr := net.DialTimeout("tcp", serverHost, 3*time.Second)
				if dialErr == nil {
					conn.Close()
					serverUnreachable = 0
				} else {
					serverUnreachable++
					d.logger.Debugf("k3s server %s unreachable (check %d/10): %v", joinedURL, serverUnreachable, dialErr)
					if serverUnreachable >= 10 {
						d.logger.Warnf("k3s server %s unreachable for %d consecutive checks (~5 min) — forcing re-join to refresh leader", joinedURL, serverUnreachable)
						serverUnreachable = 0
						d.setPhase(PhaseJoining)
						return
					}
				}
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

	// Purge stale diagnostic tags that may have been published by a previous
	// provisioning attempt in the same session.  These are not needed for cluster
	// operation and consume budget from the 512-byte Serf MetaMaxSize limit.
	if err := d.discovery.DeleteTags([]string{"munge-key-hash"}); err != nil {
		d.logger.Debugf("DeleteTags(munge-key-hash): %v (non-fatal)", err)
	}

	tags := map[string]string{
		"k3s-server": k3sServerURL,
		"k3s-token":  k3sToken,
		"munge-key":  mungeKeyB64,
		"phase":      string(PhaseReady),
	}
	if err := d.discovery.UpdateTags(tags); err != nil {
		// A broadcast timeout means there are no other Serf members to ack the
		// update yet — this is non-fatal. Our own local state IS updated (Serf
		// persists tags locally before broadcasting). Workers that do a TCP
		// push-pull via syncWithLeader() will receive the tags directly.
		// Retry in the background so gossip propagates as more peers join.
		d.logger.Warnf("publishClusterReady: UpdateTags broadcast timeout (%v) — continuing; will retry in background", err)
		go d.retryPublishTags(tags)
	}
	d.mu.Lock()
	d.phase = PhaseReady
	d.mu.Unlock()
	d.logger.Infof("Phase → %s (k3s-server=%s, munge-key published atomically)", PhaseReady, k3sServerURL)
	return nil
}

// retryPublishTags keeps retrying UpdateTags until it succeeds or the context ends.
// Called as a goroutine when publishClusterReady's first UpdateTags times out due to
// no Serf peers being present yet. Once peers join, the retry will succeed and they
// will receive the cluster tags via gossip.
func (d *Daemon) retryPublishTags(tags map[string]string) {
	for attempt := 1; ; attempt++ {
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
		if d.discovery == nil {
			return
		}
		if err := d.discovery.UpdateTags(tags); err != nil {
			d.logger.Debugf("retryPublishTags attempt %d: %v", attempt, err)
			continue
		}
		d.logger.Infof("retryPublishTags: cluster tags published successfully on attempt %d", attempt)
		return
	}
}

// storedLeaderName returns the leader name that was explicitly stored during election
// (either from findRunningLeader or ComputeLeader). This is the name workers should use
// when looking up leader tags — not d.leaderElector.ComputeLeader(), which recomputes
// from live Serf membership and can return a different node than the one we adopted.
func (d *Daemon) storedLeaderName() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.leaderName
}

// syncWithLeader forces an immediate TCP push-pull state exchange with the current leader.
// This ensures that large tag values (munge-key, k3s-token) that may not fit in a UDP
// gossip packet are fetched via TCP before we try to read them.
func (d *Daemon) syncWithLeader() {
	leaderName := d.storedLeaderName()
	if leaderName == "" {
		leaderName = d.leaderElector.ComputeLeader()
	}
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
	leaderName := d.storedLeaderName()
	if leaderName == "" {
		leaderName = d.leaderElector.ComputeLeader()
	}
	for _, m := range d.discovery.Members() {
		if m.Name == leaderName {
			return m.Tags
		}
	}
	return nil
}

// buildWorkerList returns the current list of alive Serf members that should be
// SLURM compute nodes, excluding only the controller's own IP.
//
// We do NOT exclude the Serf leader name — the Serf leader (lexicographic winner)
// may be a different node from the k3s/SLURM controller.  Excluding the Serf leader
// would remove a valid worker from slurm.conf, causing "lookup failure for node" errors.
// The controller template already adds the controller's own IP, so we only skip localIP.
func (d *Daemon) buildWorkerList() []controller.WorkerInfo {
	localIP := d.getLocalIP()
	var workers []controller.WorkerInfo
	for _, m := range d.discovery.GetAliveMembers() {
		// Exclude the local node — the controller template already registers this
		// node's IP as a compute node in slurm.conf.
		memberIP := m.Addr.String()
		if wgip, ok := m.Tags["wgip"]; ok && wgip != "" {
			memberIP = wgip
		}
		if memberIP == localIP {
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
		// TmpDisk: derive from ndisks tag — standardised paths /mnt/clusteros/disk-N.
		// Each extra disk contributes its usable space; SLURM uses this for job temp files.
		tmpDisk := 0
		if nstr, ok := m.Tags["ndisks"]; ok {
			if n, err := strconv.Atoi(nstr); err == nil && n > 0 {
				tmpDisk = extraDiskTotalMB(n)
			}
		}
		memMB, _ := strconv.Atoi(m.Tags["ram"])
		gpus, _ := strconv.Atoi(m.Tags["gpu"])
		workers = append(workers, controller.WorkerInfo{
			Name:    memberIP, // must match slurmd's -N flag (Tailscale IP)
			Addr:    memberIP,
			CPUs:    cpu,
			MemMB:   memMB,
			GPUs:    gpus,
			TmpDisk: tmpDisk,
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

		// Sort by name so the hash is stable regardless of Serf member iteration order.
		// Without sorting, non-deterministic Go map/slice order causes a new hash on every
		// poll even when the worker set is unchanged, triggering spurious reconfigurations.
		sort.Slice(workers, func(i, j int) bool { return workers[i].Name < workers[j].Name })

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

// detectTailscaleIPWithRetry polls for a Tailscale IP until timeout.
// Useful at startup when Tailscale may still be authenticating (OAuth takes ~10-30s).
func (d *Daemon) detectTailscaleIPWithRetry(timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ip := d.detectTailscaleIP(); ip != "" {
			return ip
		}
		select {
		case <-d.ctx.Done():
			return ""
		case <-time.After(2 * time.Second):
		}
	}
	return ""
}

// advertiseNewMemberSubnet is called when a Serf member joins with a non-Tailscale
// LAN address.  If this node has Tailscale, it calls `tailscale set --advertise-routes`
// to include the new member's /24 subnet so the rest of the mesh can reach it.
// This handles nodes that are only reachable via a direct Ethernet patch.
func (d *Daemon) advertiseNewMemberSubnet(memberAddr net.IP) {
	if memberAddr == nil {
		return
	}
	// Only act if this node has Tailscale.
	if d.detectTailscaleIP() == "" {
		return
	}
	// Skip Tailscale IPs (100.64.0.0/10) — they're already in the mesh.
	tailscaleRange := &net.IPNet{
		IP:   net.ParseIP("100.64.0.0"),
		Mask: net.CIDRMask(10, 32),
	}
	if tailscaleRange.Contains(memberAddr) {
		return
	}
	// Build a /24 covering the member's address.
	b := memberAddr.To4()
	if b == nil {
		return // IPv6 address — skip (no /24 subnet to advertise)
	}
	subnet := fmt.Sprintf("%d.%d.%d.0/24", b[0], b[1], b[2])
	d.logger.Infof("[lan-route] Advertising subnet %s via Tailscale for new p2p member %s", subnet, memberAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Collect existing advertised routes to merge (tailscale set replaces the full list).
	existing := d.currentTailscaleRoutes()
	routes := mergeRoute(existing, subnet)
	args := []string{"set", "--advertise-routes=" + strings.Join(routes, ",")}
	if out, err := exec.CommandContext(ctx, "tailscale", args...).CombinedOutput(); err != nil {
		d.logger.Debugf("[lan-route] tailscale set --advertise-routes: %v (%s)", err, strings.TrimSpace(string(out)))
	}
}

func (d *Daemon) currentTailscaleRoutes() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
	if err != nil {
		return nil
	}
	var status struct {
		Self struct {
			PrimaryRoutes []string `json:"PrimaryRoutes"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return nil
	}
	return status.Self.PrimaryRoutes
}

func mergeRoute(existing []string, newRoute string) []string {
	for _, r := range existing {
		if r == newRoute {
			return existing
		}
	}
	return append(existing, newRoute)
}

// getLeaderTag returns the value of a tag from the current leader's Serf member.
// Uses d.leaderName (the explicitly adopted leader) rather than ComputeLeader()
// (the lexicographic minimum alive member) to avoid a mismatch when the adopted
// leader was found via findRunningLeader but is not the lexico-minimum.
func (d *Daemon) getLeaderTag(key string) (string, bool) {
	leaderName := d.storedLeaderName()
	if leaderName == "" {
		leaderName = d.leaderElector.ComputeLeader()
	}
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
// Every 30s it forces a TCP push-pull with the leader so stale gossip state
// doesn't cause an indefinite wait.
func (d *Daemon) waitForLeaderTag(key, wantValue string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	// Force an immediate sync so we have fresh tags from the leader on first poll.
	d.syncWithLeader()
	lastSync := time.Now()
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
		// Periodically re-sync so freshly published tags propagate quickly.
		if time.Since(lastSync) > 30*time.Second {
			leader := d.storedLeaderName()
			if leader == "" {
				leader = d.leaderElector.ComputeLeader()
			}
			d.logger.Debugf("waitForLeaderTag(%q): still waiting, forcing sync with leader %s", key, leader)
			d.syncWithLeader()
			lastSync = time.Now()
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

	// Advertise the number of extra (non-boot) disks mounted by apply-patch.sh.
	// The leader reads this from every member's Serf tags to register disk paths
	// with Longhorn after the cluster reaches ready. Tag is tiny: "ndisks=2".
	if diskPaths := readExtraDiskPaths(); len(diskPaths) > 0 {
		tags["ndisks"] = strconv.Itoa(len(diskPaths))
	}

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
		LANDiscovery:   true, // probe local physical subnets for Serf peers
		Version:        d.version,
		BuildTime:      d.buildTime,
	}

	disc, err := discovery.New(discoveryCfg, d.clusterState, localNode)
	if err != nil {
		return fmt.Errorf("create discovery: %w", err)
	}
	d.discovery = disc
	// Register a handler to receive munge-key user events.  Serf tags are
	// limited to ~512 bytes, so leaders may choose to broadcast the full
	// munge key as a user event instead of relying solely on tags.  Handlers
	// write the key to disk and start munged so workers can authenticate.
	// Install k3s cluster CA cert when received from the leader.
	// This fires on every node (including the leader itself, harmless) and writes
	// the cert to the locations k3s agent reads before connecting to the server.
	// Prevents "x509: certificate signed by unknown authority" when nodes were
	// patched at different times and have different CA bundles.
	d.discovery.RegisterUserEventHandler(func(name string, payload []byte) error {
		if name != "k3s-ca-cert" {
			return nil
		}
		d.logger.Infof("Received k3s-ca-cert user event (%d bytes) — installing", len(payload))
		caPaths := []string{
			"/var/lib/rancher/k3s/agent/server-ca.crt",
			"/var/lib/rancher/k3s/server/tls/server-ca.crt",
		}
		for _, p := range caPaths {
			if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
				d.logger.Warnf("mkdir %s: %v", filepath.Dir(p), err)
				continue
			}
			if err := os.WriteFile(p, payload, 0644); err != nil {
				d.logger.Warnf("Write k3s CA to %s: %v", p, err)
			} else {
				d.logger.Debugf("Installed k3s CA at %s", p)
			}
		}
		return nil
	})

	d.discovery.RegisterUserEventHandler(func(name string, payload []byte) error {
		if name != "munge-key" {
			return nil
		}
		d.logger.Infof("Received munge-key user event (len=%d)", len(payload))
		// payload is base64-encoded
		kb64 := string(payload)
		key, err := base64.StdEncoding.DecodeString(kb64)
		if err != nil {
			d.logger.Warnf("Failed to decode munge-key payload: %v", err)
			return err
		}
		mungeManager := slurmauth.NewMungeKeyManager(d.logger)
		if err := mungeManager.WriteMungeKey(key); err != nil {
			d.logger.Warnf("Failed to write munge key from user event: %v", err)
			return err
		}
		if err := mungeManager.StartMungeDaemon(); err != nil {
			d.logger.Warnf("Failed to start munged after user event: %v", err)
			return err
		}
		d.logger.Info("Munge key applied from user event and munged started")
		return nil
	})
	// When a new member joins via LAN (non-Tailscale IP), advertise their subnet
	// via Tailscale so the rest of the mesh can route to them.  This handles
	// nodes connected via a direct Ethernet patch to this node (p2p) that have
	// no Tailscale of their own — they rely on us as a gateway.
	d.discovery.RegisterMembershipChangeHandler(func() error {
		tailscaleRange := &net.IPNet{
			IP:   net.ParseIP("100.64.0.0"),
			Mask: net.CIDRMask(10, 32),
		}
		for _, m := range d.discovery.GetAliveMembers() {
			if m.Addr == nil {
				continue
			}
			if !tailscaleRange.Contains(m.Addr) && !m.Addr.IsLoopback() {
				go d.advertiseNewMemberSubnet(m.Addr)
			}
		}
		return nil
	})

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

	// Re-affirm exit-node advertisement on every start.
	// --advertise-exit-node makes this node available as an internet gateway for
	// cluster peers whose LAN path is temporarily broken.
	// --accept-routes lets this node use routes (including exit nodes) advertised by peers.
	// This is a best-effort fire-and-forget; failures are non-fatal.
	// NOTE: on tailscale.com the exit node must still be approved in the admin console
	// (or via ACL autoApprovers).  On Headscale add autoApprovers to your policy.
	go func() {
		args := []string{"set", "--advertise-exit-node=true", "--accept-routes=true"}
		if out, err := exec.Command("tailscale", args...).CombinedOutput(); err != nil {
			d.logger.Debugf("tailscale set --advertise-exit-node: %v (%s)", err, strings.TrimSpace(string(out)))
		} else {
			d.logger.Info("Tailscale: advertising as exit node, accepting peer routes")
		}
	}()

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
		{"80", "tcp", "HTTP (nginx-ingress hostNetwork)"},
		{"443", "tcp", "HTTPS (nginx-ingress hostNetwork)"},
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

	// Remove stale REDIRECT rules for ports 80/443 from both the OUTPUT and
	// PREROUTING nat chains.  Old daemon versions added these rules to redirect
	// HTTP/HTTPS traffic to ingress-nginx NodePorts (30080/30443), but they break
	// outbound connections (apt, containerd, curl) with ECONNREFUSED.
	//
	// We flush the entire OUTPUT chain (no kube-proxy rules live there) but use
	// targeted deletion for PREROUTING to avoid disturbing kube-proxy's
	// KUBE-SERVICES jump rule which also lives in PREROUTING.
	d.logger.Info("Flushing nat OUTPUT chain and removing stale PREROUTING REDIRECTs")
	exec.Command("iptables", "-t", "nat", "-F", "OUTPUT").Run() //nolint:errcheck
	for _, pair := range [][2]string{{"80", "30080"}, {"443", "30443"}} {
		from, to := pair[0], pair[1]
		for {
			if exec.Command("iptables", "-t", "nat", "-C", "PREROUTING",
				"-p", "tcp", "--dport", from, "-j", "REDIRECT", "--to-ports", to).Run() != nil {
				break
			}
			exec.Command("iptables", "-t", "nat", "-D", "PREROUTING", //nolint:errcheck
				"-p", "tcp", "--dport", from, "-j", "REDIRECT", "--to-ports", to).Run()
		}
	}

	// Persist the now-clean iptables state so iptables-persistent/netfilter-persistent
	// cannot restore stale rules from /etc/iptables/rules.v4 on the next reboot.
	os.MkdirAll("/etc/iptables", 0755)
	if f, err := os.Create("/etc/iptables/rules.v4"); err == nil {
		saveCmd := exec.Command("iptables-save")
		saveCmd.Stdout = f
		saveCmd.Run()
		f.Close()
		d.logger.Info("Saved iptables state to /etc/iptables/rules.v4")
	} else {
		d.logger.Warnf("Could not write /etc/iptables/rules.v4: %v", err)
	}

	// UFW FORWARD policy — must be ACCEPT for pod NodePort traffic to route correctly.
	// kube-proxy DNAT's NodePort traffic to pod IPs; UFW's default DROP blocks this.
	if data, err := os.ReadFile("/etc/default/ufw"); err == nil {
		if strings.Contains(string(data), `DEFAULT_FORWARD_POLICY="DROP"`) {
			newData := strings.ReplaceAll(string(data),
				`DEFAULT_FORWARD_POLICY="DROP"`,
				`DEFAULT_FORWARD_POLICY="ACCEPT"`)
			if os.WriteFile("/etc/default/ufw", []byte(newData), 0644) == nil {
				exec.Command("ufw", "reload").Run()
				d.logger.Info("Fixed UFW FORWARD policy: DROP → ACCEPT (required for pod routing)")
			}
		}
	}

	// IP forwarding — required for routing between LAN, Tailscale, and pod network.
	exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").Run()

	// Pod CIDR FORWARD rules — allow Flannel-routed packets through the FORWARD chain.
	for _, cidr := range []string{"10.42.0.0/16", "10.43.0.0/16"} {
		if exec.Command("iptables", "-C", "FORWARD", "-d", cidr, "-j", "ACCEPT").Run() != nil {
			exec.Command("iptables", "-I", "FORWARD", "1", "-d", cidr, "-j", "ACCEPT").Run()
		}
		if exec.Command("iptables", "-C", "FORWARD", "-s", cidr, "-j", "ACCEPT").Run() != nil {
			exec.Command("iptables", "-I", "FORWARD", "1", "-s", cidr, "-j", "ACCEPT").Run()
		}
	}

	// Ingress NodePort aliases: redirect incoming :30080 → :80 and :30443 → :443.
	// ingress-nginx runs with hostNetwork=true and binds directly to ports 80/443.
	// kube-proxy's NodePort DNAT doesn't apply to hostNetwork pods, so we add
	// explicit PREROUTING REDIRECT rules so external traffic on 30080/30443 reaches nginx.
	for _, pair := range [][2]string{{"30080", "80"}, {"30443", "443"}} {
		from, to := pair[0], pair[1]
		if exec.Command("iptables", "-t", "nat", "-C", "PREROUTING",
			"-p", "tcp", "--dport", from, "-j", "REDIRECT", "--to-ports", to).Run() != nil {
			exec.Command("iptables", "-t", "nat", "-A", "PREROUTING", //nolint:errcheck
				"-p", "tcp", "--dport", from, "-j", "REDIRECT", "--to-ports", to).Run()
			d.logger.Infof("Added PREROUTING REDIRECT :%s → :%s (ingress NodePort alias)", from, to)
		}
		// Also handle loopback (OUTPUT chain) so curl http://localhost:30080 works.
		if exec.Command("iptables", "-t", "nat", "-C", "OUTPUT",
			"-p", "tcp", "--dport", from, "-j", "REDIRECT", "--to-ports", to).Run() != nil {
			exec.Command("iptables", "-t", "nat", "-A", "OUTPUT", //nolint:errcheck
				"-p", "tcp", "--dport", from, "-j", "REDIRECT", "--to-ports", to).Run()
		}
	}

	d.logger.Info("Firewall rules set (including Tailscale interface trust and pod CIDR forwarding)")
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

	// Extra disk paths
	if paths := readExtraDiskPaths(); len(paths) > 0 {
		status["extra_disks"] = paths
	}

	return status
}

// readExtraDiskPaths reads /etc/clusteros/extra-disks written by apply-patch.sh.
// Returns the list of mounted extra disk paths (e.g. ["/mnt/clusteros/disk-0"]).
func readExtraDiskPaths() []string {
	const manifest = "/etc/clusteros/extra-disks"
	data, err := os.ReadFile(manifest)
	if err != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths
}

// extraDiskTotalMB returns the total usable space in MB across n standardised
// extra disk mount points (/mnt/clusteros/disk-0 … disk-N-1).
// Used to populate SLURM's TmpDisk so the scheduler knows how much scratch
// space is available on each node.
func extraDiskTotalMB(n int) int {
	totalMB := 0
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("/mnt/clusteros/disk-%d", i)
		var st syscall.Statfs_t
		if err := syscall.Statfs(path, &st); err == nil {
			mb := int(st.Bavail * uint64(st.Bsize) / (1024 * 1024))
			totalMB += mb
		}
	}
	return totalMB
}
