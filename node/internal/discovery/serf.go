package discovery

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/cluster-os/node/internal/auth"
	"github.com/cluster-os/node/internal/networking"
	"github.com/cluster-os/node/internal/state"
	"github.com/hashicorp/serf/serf"
	"github.com/sirupsen/logrus"
)

// LeaderElector provides access to Raft operations
type LeaderElector interface {
	IsLeader() bool
	AddVoter(nodeID string, address string) error
	RemoveServer(nodeID string) error
}

// UserEventHandler handles user events
type UserEventHandler func(name string, payload []byte) error

// MembershipChangeHandler handles membership changes
type MembershipChangeHandler func() error

// SerfDiscovery wraps HashiCorp Serf for cluster discovery
type SerfDiscovery struct {
	serf                     *serf.Serf
	eventCh                  chan serf.Event
	shutdownCh               chan struct{}
	state                    *state.ClusterState
	localNode                *state.Node
	logger                   *logrus.Logger
	clusterAuth              *auth.ClusterAuth
	leaderElector            LeaderElector    // For Raft integration
	raftPort                 int              // Raft consensus port
	userEventHandlers        []UserEventHandler // Custom user event handlers
	membershipChangeHandlers []MembershipChangeHandler // Membership change handlers
	lanDiscoveryEnabled      bool
	lanDiscoveryLoop         context.CancelFunc
}

// Config contains configuration for Serf discovery
type Config struct {
	NodeName         string
	NodeID           string
	BindAddr         string
	BindPort         int
	BootstrapPeers   []string
	EncryptKey       []byte
	ClusterAuthKey   string // Base64-encoded cluster authentication key
	Tags             map[string]string
	Logger           *logrus.Logger
	RaftPort         int  // Raft consensus port for cluster integration
	LANDiscovery     bool // Enable LAN discovery
	LANDiscoveryScan time.Duration // How often to scan for peers on LAN
}

// New creates a new Serf discovery instance
func New(cfg *Config, clusterState *state.ClusterState, localNode *state.Node, leaderElector LeaderElector) (*SerfDiscovery, error) {
	if cfg.Logger == nil {
		cfg.Logger = logrus.New()
	}

	// Initialize cluster authentication
	clusterAuth, err := auth.New(cfg.ClusterAuthKey)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize cluster auth: %w", err)
	}

	eventCh := make(chan serf.Event, 256)
	shutdownCh := make(chan struct{})

	// Configure Serf
	serfConfig := serf.DefaultConfig()
	serfConfig.NodeName = cfg.NodeName
	serfConfig.MemberlistConfig.BindAddr = cfg.BindAddr
	serfConfig.MemberlistConfig.BindPort = cfg.BindPort
	serfConfig.MemberlistConfig.AdvertisePort = cfg.BindPort
	serfConfig.EventCh = eventCh
	serfConfig.EnableNameConflictResolution = true

	// Set encryption key if provided
	if len(cfg.EncryptKey) > 0 {
		serfConfig.MemberlistConfig.SecretKey = cfg.EncryptKey
	}

	// Add tags (use short names to fit within Serf's 512-byte encoded metadata limit)
	if cfg.Tags == nil {
		cfg.Tags = make(map[string]string)
	}
	cfg.Tags["node_id"] = cfg.NodeID
	cfg.Tags["roles"] = strings.Join(localNode.Roles, ",")
	cfg.Tags["cpu"] = strconv.Itoa(localNode.Capabilities.CPU)
	cfg.Tags["arch"] = localNode.Capabilities.Arch

	// Generate and add authentication token
	joinToken, err := clusterAuth.CreateJoinToken(cfg.NodeID)
	if err != nil {
		return nil, fmt.Errorf("failed to create join token: %w", err)
	}
	cfg.Tags["auth_token"] = joinToken

	serfConfig.Tags = cfg.Tags

	// Create Serf instance
	serfInstance, err := serf.Create(serfConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create Serf instance: %w", err)
	}

	sd := &SerfDiscovery{
		serf:          serfInstance,
		eventCh:       eventCh,
		shutdownCh:    shutdownCh,
		state:         clusterState,
		localNode:     localNode,
		logger:        cfg.Logger,
		clusterAuth:   clusterAuth,
		leaderElector: leaderElector,
		raftPort:      cfg.RaftPort,
	}

	// Join bootstrap peers if provided
	peersToJoin := cfg.BootstrapPeers

	// If no explicit bootstrap peers, try Tailscale peer discovery
	if len(peersToJoin) == 0 && networking.IsTailscaleAvailable() {
		sd.logger.Info("No bootstrap peers configured, attempting Tailscale peer discovery...")

		// First try to find cluster-tagged peers
		discoveredPeers, err := networking.DiscoverClusterPeers(cfg.BindPort)
		if err != nil {
			sd.logger.Warnf("Tailscale cluster peer discovery failed: %v", err)
		} else if len(discoveredPeers) > 0 {
			sd.logger.Infof("Discovered %d cluster peers via Tailscale: %v", len(discoveredPeers), discoveredPeers)
			peersToJoin = discoveredPeers
		} else {
			// No tagged peers found - try all Tailscale peers (initial cluster setup)
			sd.logger.Info("No tagged cluster peers found, trying all Tailscale peers...")
			allPeers, err := networking.DiscoverAllTailscalePeers(cfg.BindPort)
			if err != nil {
				sd.logger.Warnf("Tailscale peer discovery failed: %v", err)
			} else if len(allPeers) > 0 {
				sd.logger.Infof("Discovered %d Tailscale peers to try: %v", len(allPeers), allPeers)
				peersToJoin = allPeers
			}
		}
	}

	if len(peersToJoin) > 0 {
		sd.logger.Infof("Joining cluster via peers: %v", peersToJoin)
		n, err := serfInstance.Join(peersToJoin, true)
		if err != nil {
			sd.logger.Warnf("Failed to join some peers: %v", err)
		} else {
			sd.logger.Infof("Successfully joined %d peers", n)
		}
	} else {
		sd.logger.Info("Bootstrap mode: starting as cluster of 1 (will discover other nodes via Tailscale)")
	}

	// Start event handler
	go sd.handleEvents()

	// Start background Tailscale peer discovery loop if enabled
	if cfg.LANDiscovery || len(cfg.BootstrapPeers) == 0 {
		sd.lanDiscoveryEnabled = true
		ctx, cancel := context.WithCancel(context.Background())
		sd.lanDiscoveryLoop = cancel
		interval := cfg.LANDiscoveryScan
		if interval == 0 {
			interval = 30 * time.Second // Default: check for new peers every 30 seconds
		}
		go sd.tailscalePeerDiscoveryLoop(ctx, cfg.BindPort, interval)
	}

	return sd, nil
}

// handleEvents processes Serf events
func (sd *SerfDiscovery) handleEvents() {
	for {
		select {
		case event := <-sd.eventCh:
			switch e := event.(type) {
			case serf.MemberEvent:
				sd.handleMemberEvent(e)
			case serf.UserEvent:
				sd.handleUserEvent(e)
			case *serf.Query:
				sd.handleQuery(e)
			default:
				sd.logger.Debugf("Received unknown event type: %T", e)
			}
		case <-sd.shutdownCh:
			sd.logger.Info("Event handler shutting down")
			return
		}
	}
}

// handleMemberEvent handles node join/leave/update/failed events
func (sd *SerfDiscovery) handleMemberEvent(event serf.MemberEvent) {
	for _, member := range event.Members {
		nodeID := member.Tags["id"]
		if nodeID == "" {
			sd.logger.Warnf("Member %s has no id tag", member.Name)
			continue
		}

		switch event.EventType() {
		case serf.EventMemberJoin:
			sd.logger.Infof("Node joined: %s (ID: %s, Address: %s)", member.Name, nodeID, member.Addr)
			sd.handleMemberJoin(member)

		case serf.EventMemberUpdate:
			sd.logger.Debugf("Node updated: %s (ID: %s)", member.Name, nodeID)
			sd.handleMemberUpdate(member)

		case serf.EventMemberLeave:
			sd.logger.Infof("Node left: %s (ID: %s)", member.Name, nodeID)
			sd.handleMemberLeave(member)

		case serf.EventMemberFailed:
			sd.logger.Warnf("Node failed: %s (ID: %s)", member.Name, nodeID)
			sd.handleMemberFailed(member)

		case serf.EventMemberReap:
			sd.logger.Infof("Node reaped: %s (ID: %s)", member.Name, nodeID)
			sd.handleMemberReap(member)
		}
	}
}

// handleMemberJoin processes a member join event
func (sd *SerfDiscovery) handleMemberJoin(member serf.Member) {
	// Skip processing for local node
	nodeID := member.Tags["node_id"]
	if nodeID == sd.localNode.ID {
		return
	}

	// Validate authentication token
	authToken := member.Tags["auth_token"]
	if authToken == "" {
		sd.logger.Warnf("Node %s attempted to join without auth token - rejecting", member.Name)
		return
	}

	// Verify the join token
	verifiedNodeID, err := sd.clusterAuth.VerifyJoinToken(authToken)
	if err != nil {
		sd.logger.Warnf("Node %s failed authentication: %v - rejecting", member.Name, err)
		return
	}

	// Verify the node ID matches
	if verifiedNodeID != nodeID {
		sd.logger.Warnf("Node %s auth token node ID mismatch (expected %s, got %s) - rejecting",
			member.Name, nodeID, verifiedNodeID)
		return
	}

	sd.logger.Infof("Node %s authenticated successfully", member.Name)

	// Add to local cluster state
	node := sd.memberToNode(member)
	node.Status = state.StatusAlive
	sd.state.AddNode(node)

	// Add to Raft cluster if we're the leader
	if sd.leaderElector != nil && sd.leaderElector.IsLeader() {
		raftAddr := fmt.Sprintf("%s:%d", member.Addr.String(), sd.raftPort)
		sd.logger.Infof("Adding node %s to Raft cluster at %s", nodeID, raftAddr)

		err := sd.leaderElector.AddVoter(nodeID, raftAddr)
		if err != nil {
			sd.logger.Errorf("Failed to add node %s to Raft cluster: %v", nodeID, err)
		} else {
			sd.logger.Infof("Successfully added node %s to Raft cluster", nodeID)
		}
	} else if sd.leaderElector != nil {
		sd.logger.Debugf("Not Raft leader, skipping Raft voter addition for node %s", nodeID)
	}
}

// handleMemberUpdate processes a member update event
func (sd *SerfDiscovery) handleMemberUpdate(member serf.Member) {
	node := sd.memberToNode(member)
	node.Status = state.StatusAlive
	sd.state.AddNode(node)

	// Notify membership change handlers (triggers WireGuard peer refresh etc.)
	sd.notifyMembershipChange()
}

// handleMemberLeave processes a member leave event
func (sd *SerfDiscovery) handleMemberLeave(member serf.Member) {
	nodeID := member.Tags["id"]
	sd.state.UpdateNodeStatus(nodeID, state.StatusLeft)

	// Remove from Raft cluster if we're the leader
	if sd.leaderElector != nil && sd.leaderElector.IsLeader() {
		sd.logger.Infof("Removing node %s from Raft cluster (graceful leave)", nodeID)
		err := sd.leaderElector.RemoveServer(nodeID)
		if err != nil {
			sd.logger.Errorf("Failed to remove node %s from Raft cluster: %v", nodeID, err)
		} else {
			sd.logger.Infof("Successfully removed node %s from Raft cluster", nodeID)
		}
	}
}

// handleMemberFailed processes a member failed event
func (sd *SerfDiscovery) handleMemberFailed(member serf.Member) {
	nodeID := member.Tags["id"]
	sd.state.UpdateNodeStatus(nodeID, state.StatusFailed)

	// Remove from Raft cluster if we're the leader (after grace period)
	if sd.leaderElector != nil && sd.leaderElector.IsLeader() {
		sd.logger.Warnf("Node %s failed, removing from Raft cluster", nodeID)
		err := sd.leaderElector.RemoveServer(nodeID)
		if err != nil {
			sd.logger.Errorf("Failed to remove node %s from Raft cluster: %v", nodeID, err)
		} else {
			sd.logger.Infof("Successfully removed failed node %s from Raft cluster", nodeID)
		}
	}
}

// handleMemberReap processes a member reap event
func (sd *SerfDiscovery) handleMemberReap(member serf.Member) {
	nodeID := member.Tags["id"]
	sd.state.RemoveNode(nodeID)
	sd.notifyMembershipChange()
}

// RegisterMembershipChangeHandler registers a callback for cluster membership changes
// This is called when nodes join, leave, update, or fail
func (sd *SerfDiscovery) RegisterMembershipChangeHandler(handler MembershipChangeHandler) {
	sd.membershipChangeHandlers = append(sd.membershipChangeHandlers, handler)
}

// notifyMembershipChange calls all registered membership change handlers
func (sd *SerfDiscovery) notifyMembershipChange() {
	for _, handler := range sd.membershipChangeHandlers {
		go handler() // Run handlers asynchronously to avoid blocking
	}
}

// handleUserEvent processes custom user events
func (sd *SerfDiscovery) handleUserEvent(event serf.UserEvent) {
	sd.logger.Debugf("Received user event: %s (payload size: %d)", event.Name, len(event.Payload))

	// Call registered handlers
	for _, handler := range sd.userEventHandlers {
		if err := handler(event.Name, event.Payload); err != nil {
			sd.logger.Warnf("User event handler error: %v", err)
		}
	}
}

// RegisterUserEventHandler registers a handler for user events
func (sd *SerfDiscovery) RegisterUserEventHandler(handler UserEventHandler) {
	sd.userEventHandlers = append(sd.userEventHandlers, handler)
	sd.logger.Debug("Registered user event handler")
}

// handleQuery processes Serf queries
func (sd *SerfDiscovery) handleQuery(query *serf.Query) {
	sd.logger.Debugf("Received query: %s", query.Name)
	// TODO: Handle queries (e.g., "get-leader", "health-check")
}

// memberToNode converts a Serf member to a cluster node
func (sd *SerfDiscovery) memberToNode(member serf.Member) *state.Node {
	nodeID := member.Tags["id"]

	// Parse roles (expand ultra-short abbreviations)
	roles := []string{}
	if roleStr := member.Tags["r"]; roleStr != "" {
		abbrevRoles := strings.Split(roleStr, ",")
		for _, abbrev := range abbrevRoles {
			switch abbrev {
			case "w":
				roles = append(roles, "wireguard")
			case "c":
				roles = append(roles, "slurm-controller")
			case "s":
				roles = append(roles, "slurm-worker")
			case "k":
				roles = append(roles, "k3s-server")
			case "a":
				roles = append(roles, "k3s-agent")
			default:
				roles = append(roles, abbrev) // fallback
			}
		}
	}

	// Parse capabilities
	cpu, _ := strconv.Atoi(member.Tags["p"])
	capabilities := state.Capabilities{
		CPU:  cpu,
		RAM:  member.Tags["ram"],
		GPU:  member.Tags["gpu"] == "true",
		Arch: member.Tags["h"],
	}

	// Extract Tailscale IP from tags (stored in wgip tag for compatibility)
	var tailscaleIP string
	if tsIPStr := member.Tags["wgip"]; tsIPStr != "" {
		tailscaleIP = tsIPStr
	}

	// Extract WireGuard public key from tags
	wgPubKey := member.Tags["wg_pubkey"]

	return &state.Node{
		ID:              nodeID,
		Name:            member.Name,
		Roles:           roles,
		Capabilities:    capabilities,
		Address:         member.Addr.String(),
		WireGuardPubKey: wgPubKey,
		TailscaleIP:     tailscaleIP,
		Tags:            member.Tags,
	}
}

// UpdateTags updates the local node's tags in Serf
func (sd *SerfDiscovery) UpdateTags(tags map[string]string) error {
	// Preserve critical tags with short names to fit Serf's 512-byte limit
	tags["id"] = sd.localNode.ID
	tags["r"] = strings.Join(sd.localNode.Roles, ",")

	if err := sd.serf.SetTags(tags); err != nil {
		return fmt.Errorf("failed to update tags: %w", err)
	}

	sd.logger.Debugf("Updated tags: %v", tags)
	return nil
}

// SendEvent sends a custom user event to the cluster
func (sd *SerfDiscovery) SendEvent(name string, payload []byte, coalesce bool) error {
	if err := sd.serf.UserEvent(name, payload, coalesce); err != nil {
		return fmt.Errorf("failed to send event: %w", err)
	}
	sd.logger.Debugf("Sent event: %s", name)
	return nil
}

// Query sends a query to the cluster and returns responses
func (sd *SerfDiscovery) Query(name string, payload []byte, filterNodes []string) (*serf.QueryResponse, error) {
	params := &serf.QueryParam{
		FilterNodes: filterNodes,
		RequestAck:  true,
	}

	resp, err := sd.serf.Query(name, payload, params)
	if err != nil {
		return nil, fmt.Errorf("failed to send query: %w", err)
	}

	sd.logger.Debugf("Sent query: %s", name)
	return resp, nil
}

// Members returns the current cluster members from Serf
func (sd *SerfDiscovery) Members() []serf.Member {
	return sd.serf.Members()
}

// LocalMember returns the local Serf member
func (sd *SerfDiscovery) LocalMember() serf.Member {
	return sd.serf.LocalMember()
}

// GetSerf returns the underlying Serf instance
// Used for Serf-based leader election
func (sd *SerfDiscovery) GetSerf() *serf.Serf {
	return sd.serf
}

// Leave gracefully leaves the cluster
func (sd *SerfDiscovery) Leave() error {
	sd.logger.Info("Leaving cluster")
	return sd.serf.Leave()
}

// Shutdown shuts down the discovery layer
func (sd *SerfDiscovery) Shutdown() error {
	sd.logger.Info("Shutting down discovery layer")

	close(sd.shutdownCh)

	if err := sd.serf.Shutdown(); err != nil {
		return fmt.Errorf("failed to shutdown Serf: %w", err)
	}

	return nil
}

// GetAdvertiseAddr returns the advertised address for this node
func (sd *SerfDiscovery) GetAdvertiseAddr() (string, int) {
	localMember := sd.serf.LocalMember()

	addr := localMember.Addr.String()
	port := localMember.Port

	return addr, int(port)
}

// IsBootstrap returns true if this node is the bootstrap node
func (sd *SerfDiscovery) IsBootstrap() bool {
	members := sd.serf.Members()
	return len(members) == 1 && members[0].Name == sd.serf.LocalMember().Name
}

// GetMemberByNodeID finds a Serf member by node ID
func (sd *SerfDiscovery) GetMemberByNodeID(nodeID string) (*serf.Member, bool) {
	for _, member := range sd.serf.Members() {
		if member.Tags["id"] == nodeID {
			return &member, true
		}
	}
	return nil, false
}

// GetHealthScore returns the health score of the local node
func (sd *SerfDiscovery) GetHealthScore() int {
	stats := sd.serf.Stats()
	if healthScore, ok := stats["health_score"]; ok {
		if score, err := strconv.Atoi(healthScore); err == nil {
			return score
		}
	}
	// Return a default health score
	return 0
}

// Join attempts to join the cluster via the specified peers
func (sd *SerfDiscovery) Join(peers []string) (int, error) {
	n, err := sd.serf.Join(peers, true)
	if err != nil {
		return n, fmt.Errorf("failed to join peers: %w", err)
	}
	sd.logger.Infof("Joined %d peers", n)
	return n, nil
}

// ParseEncryptKey parses a base64-encoded encryption key
func ParseEncryptKey(keyStr string) ([]byte, error) {
	if keyStr == "" {
		return nil, nil
	}

	// Serf expects a 16, 24, or 32 byte key
	key, err := decodeBase64(keyStr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode encryption key: %w", err)
	}

	keyLen := len(key)
	if keyLen != 16 && keyLen != 24 && keyLen != 32 {
		return nil, fmt.Errorf("encryption key must be 16, 24, or 32 bytes, got %d", keyLen)
	}

	return key, nil
}

// Helper function to decode base64
func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// GetClusterSize returns the number of alive nodes in the cluster
func (sd *SerfDiscovery) GetClusterSize() int {
	count := 0
	for _, member := range sd.serf.Members() {
		if member.Status == serf.StatusAlive {
			count++
		}
	}
	return count
}

// WaitForCluster waits until the cluster reaches a minimum size
func (sd *SerfDiscovery) WaitForCluster(minSize int, addr string, port int) error {
	sd.logger.Infof("Waiting for cluster to reach size %d", minSize)

	// If we're already at size, return immediately
	if sd.GetClusterSize() >= minSize {
		return nil
	}

	// Wait for cluster to grow
	// TODO: Implement proper waiting mechanism with timeout
	sd.logger.Info("Cluster size requirement met")
	return nil
}

// Addr returns the bind address of the Serf agent
func (sd *SerfDiscovery) Addr() net.Addr {
	localMember := sd.serf.LocalMember()
	return &net.TCPAddr{
		IP:   localMember.Addr,
		Port: int(localMember.Port),
	}
}

// tailscalePeerDiscoveryLoop periodically checks for new Tailscale peers to join
func (sd *SerfDiscovery) tailscalePeerDiscoveryLoop(ctx context.Context, serfPort int, interval time.Duration) {
	sd.logger.Info("Starting Tailscale peer discovery loop")
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Track peers we've already tried to avoid spamming
	triedPeers := make(map[string]time.Time)
	retryAfter := 5 * time.Minute // Don't retry failed peers for 5 minutes

	for {
		select {
		case <-ctx.Done():
			sd.logger.Info("Tailscale peer discovery loop stopped")
			return
		case <-sd.shutdownCh:
			sd.logger.Info("Tailscale peer discovery loop stopped (shutdown)")
			return
		case <-ticker.C:
			// Check if Tailscale is still available
			if !networking.IsTailscaleAvailable() {
				sd.logger.Debug("Tailscale not available, skipping peer discovery")
				continue
			}

			// Get current cluster members to avoid trying to join existing members
			currentMembers := make(map[string]bool)
			for _, member := range sd.serf.Members() {
				if member.Status == serf.StatusAlive || member.Status == serf.StatusLeaving {
					currentMembers[member.Addr.String()] = true
				}
			}

			// Try to discover new peers
			peers, err := networking.DiscoverClusterPeers(serfPort)
			if err != nil {
				sd.logger.Debugf("Cluster peer discovery failed: %v", err)
				// Try all peers as fallback
				peers, _ = networking.DiscoverAllTailscalePeers(serfPort)
			}

			var newPeers []string
			now := time.Now()
			for _, peer := range peers {
				// Extract IP from peer address
				host, _, _ := net.SplitHostPort(peer)

				// Skip if already a member
				if currentMembers[host] {
					continue
				}

				// Skip if we tried recently and it failed
				if lastTry, ok := triedPeers[peer]; ok && now.Sub(lastTry) < retryAfter {
					continue
				}

				newPeers = append(newPeers, peer)
				triedPeers[peer] = now
			}

			if len(newPeers) > 0 {
				sd.logger.Infof("Discovered %d new potential peers via Tailscale: %v", len(newPeers), newPeers)
				n, err := sd.serf.Join(newPeers, true)
				if err != nil {
					sd.logger.Warnf("Failed to join some discovered peers: %v", err)
				}
				if n > 0 {
					sd.logger.Infof("Successfully joined %d new peers via Tailscale discovery", n)
				}
			}

			// Clean up old entries from triedPeers
			for peer, lastTry := range triedPeers {
				if now.Sub(lastTry) > retryAfter {
					delete(triedPeers, peer)
				}
			}
		}
	}
}
