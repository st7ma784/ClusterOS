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
	"github.com/cluster-os/node/internal/state"
	"github.com/hashicorp/serf/serf"
	"github.com/sirupsen/logrus"
)

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
	userEventHandlers        []UserEventHandler
	membershipChangeHandlers []MembershipChangeHandler
	lanDiscoveryEnabled      bool
	lanDiscoveryLoop         context.CancelFunc
}

// Config contains configuration for Serf discovery
type Config struct {
	NodeName         string
	NodeID           string
	BindAddr         string
	BindPort         int
	AdvertiseAddr    string // IP to advertise to peers (e.g. Tailscale IP); empty = auto-detect
	BootstrapPeers   []string
	EncryptKey       []byte
	ClusterAuthKey   string
	Tags             map[string]string
	Logger           *logrus.Logger
	LANDiscovery     bool
	LANDiscoveryScan time.Duration
}

// New creates a new Serf discovery instance
func New(cfg *Config, clusterState *state.ClusterState, localNode *state.Node) (*SerfDiscovery, error) {
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
	// Always bind on all interfaces so we accept connections from LAN and Tailscale.
	bindAddr := cfg.BindAddr
	if bindAddr == "" {
		bindAddr = "0.0.0.0"
	}
	serfConfig.MemberlistConfig.BindAddr = bindAddr
	serfConfig.MemberlistConfig.BindPort = cfg.BindPort
	serfConfig.MemberlistConfig.AdvertisePort = cfg.BindPort
	// Advertise Tailscale IP when available so peers connect via Tailscale tunnel.
	if cfg.AdvertiseAddr != "" {
		serfConfig.MemberlistConfig.AdvertiseAddr = cfg.AdvertiseAddr
	}
	serfConfig.EventCh = eventCh
	serfConfig.EnableNameConflictResolution = true

	// Tune for Tailscale overlay networks where UDP may be filtered:
	//   - Shorter push-pull interval so tag changes propagate via TCP more frequently.
	//   - Longer probe timeout so Tailscale DERP relay latency doesn't cause false failures.
	//   - Lower suspicion multiplier for faster dead-node detection.
	serfConfig.ReconnectInterval = 10 * time.Second
	serfConfig.ReconnectTimeout = 2 * time.Hour
	serfConfig.MemberlistConfig.PushPullInterval = 15 * time.Second
	serfConfig.MemberlistConfig.ProbeInterval = 3 * time.Second
	serfConfig.MemberlistConfig.ProbeTimeout = 2 * time.Second
	serfConfig.MemberlistConfig.SuspicionMult = 3
	serfConfig.MemberlistConfig.GossipInterval = 300 * time.Millisecond

	if len(cfg.EncryptKey) > 0 {
		serfConfig.MemberlistConfig.SecretKey = cfg.EncryptKey
	}

	if cfg.Tags == nil {
		cfg.Tags = make(map[string]string)
	}
	cfg.Tags["node_id"] = cfg.NodeID
	cfg.Tags["roles"] = strings.Join(localNode.Roles, ",")
	cfg.Tags["cpu"] = strconv.Itoa(localNode.Capabilities.CPU)
	cfg.Tags["arch"] = localNode.Capabilities.Arch

	// Compact auth token
	joinToken := clusterAuth.CreateCompactJoinToken(cfg.NodeID)
	cfg.Tags["auth"] = joinToken

	serfConfig.Tags = cfg.Tags

	serfInstance, err := serf.Create(serfConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create Serf instance: %w", err)
	}

	sd := &SerfDiscovery{
		serf:        serfInstance,
		eventCh:     eventCh,
		shutdownCh:  shutdownCh,
		state:       clusterState,
		localNode:   localNode,
		logger:      cfg.Logger,
		clusterAuth: clusterAuth,
	}

	// Join bootstrap peers if provided
	if len(cfg.BootstrapPeers) > 0 {
		sd.logger.Infof("Joining cluster via peers: %v", cfg.BootstrapPeers)
		n, err := serfInstance.Join(cfg.BootstrapPeers, true)
		if err != nil {
			sd.logger.Warnf("Failed to join some peers: %v", err)
		} else {
			sd.logger.Infof("Successfully joined %d peers", n)
		}
	} else {
		sd.logger.Info("Bootstrap mode: no peers to join, waiting for Tailscale peer discovery")
	}

	go sd.handleEvents()

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
			}
		case <-sd.shutdownCh:
			return
		}
	}
}

// handleMemberEvent handles node join/leave/update/failed events
func (sd *SerfDiscovery) handleMemberEvent(event serf.MemberEvent) {
	for _, member := range event.Members {
		nodeID := member.Tags["node_id"]
		if nodeID == "" {
			sd.logger.Warnf("Member %s has no node_id tag", member.Name)
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
			sd.state.UpdateNodeStatus(nodeID, state.StatusLeft)

		case serf.EventMemberFailed:
			sd.logger.Warnf("Node failed: %s (ID: %s)", member.Name, nodeID)
			sd.state.UpdateNodeStatus(nodeID, state.StatusFailed)

		case serf.EventMemberReap:
			sd.logger.Infof("Node reaped: %s (ID: %s)", member.Name, nodeID)
			sd.state.RemoveNode(nodeID)
			sd.notifyMembershipChange()
		}
	}
}

// handleMemberJoin processes a member join event
func (sd *SerfDiscovery) handleMemberJoin(member serf.Member) {
	nodeID := member.Tags["node_id"]
	if nodeID == sd.localNode.ID {
		return
	}

	// Validate authentication token
	authToken := member.Tags["auth"]
	if authToken == "" {
		sd.logger.Warnf("Node %s attempted to join without auth token", member.Name)
		return
	}
	if err := sd.clusterAuth.VerifyCompactJoinToken(nodeID, authToken); err != nil {
		sd.logger.Warnf("Node %s failed authentication: %v", member.Name, err)
		return
	}

	node := sd.memberToNode(member)
	node.Status = state.StatusAlive
	sd.state.AddNode(node)

	sd.notifyMembershipChange()
}

// handleMemberUpdate processes a member update event
func (sd *SerfDiscovery) handleMemberUpdate(member serf.Member) {
	node := sd.memberToNode(member)
	node.Status = state.StatusAlive
	sd.state.AddNode(node)
	sd.notifyMembershipChange()
}

// RegisterMembershipChangeHandler registers a callback for cluster membership changes
func (sd *SerfDiscovery) RegisterMembershipChangeHandler(handler MembershipChangeHandler) {
	sd.membershipChangeHandlers = append(sd.membershipChangeHandlers, handler)
}

func (sd *SerfDiscovery) notifyMembershipChange() {
	for _, handler := range sd.membershipChangeHandlers {
		go handler()
	}
}

// handleUserEvent processes custom user events
func (sd *SerfDiscovery) handleUserEvent(event serf.UserEvent) {
	for _, handler := range sd.userEventHandlers {
		if err := handler(event.Name, event.Payload); err != nil {
			sd.logger.Warnf("User event handler error: %v", err)
		}
	}
}

// RegisterUserEventHandler registers a handler for user events
func (sd *SerfDiscovery) RegisterUserEventHandler(handler UserEventHandler) {
	sd.userEventHandlers = append(sd.userEventHandlers, handler)
}

// handleQuery processes Serf queries (no-op for now)
func (sd *SerfDiscovery) handleQuery(query *serf.Query) {}

// memberToNode converts a Serf member to a cluster node
func (sd *SerfDiscovery) memberToNode(member serf.Member) *state.Node {
	nodeID := member.Tags["node_id"]
	cpu, _ := strconv.Atoi(member.Tags["cpu"])
	return &state.Node{
		ID:      nodeID,
		Name:    member.Name,
		Roles:   strings.Split(member.Tags["roles"], ","),
		Address: member.Addr.String(),
		Capabilities: state.Capabilities{
			CPU:  cpu,
			Arch: member.Tags["arch"],
		},
		TailscaleIP:     member.Tags["wgip"],
		WireGuardPubKey: member.Tags["wg_pubkey"],
		Tags:            member.Tags,
	}
}

// UpdateTags updates the local node's tags in Serf.
// Critical tags (node_id, roles) are always preserved.
func (sd *SerfDiscovery) UpdateTags(tags map[string]string) error {
	// Merge with existing tags to avoid clobbering critical ones
	existing := sd.serf.LocalMember().Tags
	merged := make(map[string]string, len(existing)+len(tags))
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range tags {
		merged[k] = v
	}
	if err := sd.serf.SetTags(merged); err != nil {
		return fmt.Errorf("failed to update tags: %w", err)
	}
	return nil
}

// GetAliveMembers returns all currently alive Serf members
func (sd *SerfDiscovery) GetAliveMembers() []serf.Member {
	var alive []serf.Member
	for _, m := range sd.serf.Members() {
		if m.Status == serf.StatusAlive {
			alive = append(alive, m)
		}
	}
	return alive
}

// Members returns all Serf members (any status)
func (sd *SerfDiscovery) Members() []serf.Member {
	return sd.serf.Members()
}

// LocalMember returns the local Serf member
func (sd *SerfDiscovery) LocalMember() serf.Member {
	return sd.serf.LocalMember()
}

// GetSerf returns the underlying Serf instance
func (sd *SerfDiscovery) GetSerf() *serf.Serf {
	return sd.serf
}

// GetClusterSize returns the number of alive nodes in the cluster
func (sd *SerfDiscovery) GetClusterSize() int {
	return len(sd.GetAliveMembers())
}

// Join attempts to join the cluster via the specified peers
func (sd *SerfDiscovery) Join(peers []string) (int, error) {
	n, err := sd.serf.Join(peers, true)
	if err != nil {
		return n, fmt.Errorf("failed to join peers: %w", err)
	}
	return n, nil
}

// GetMemberByNodeID finds a Serf member by node ID
func (sd *SerfDiscovery) GetMemberByNodeID(nodeID string) (*serf.Member, bool) {
	for _, member := range sd.serf.Members() {
		if member.Tags["node_id"] == nodeID {
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
	return 0
}

// GetAdvertiseAddr returns the advertised address for this node
func (sd *SerfDiscovery) GetAdvertiseAddr() (string, int) {
	lm := sd.serf.LocalMember()
	return lm.Addr.String(), int(lm.Port)
}

// IsBootstrap returns true if this is the only node in the cluster
func (sd *SerfDiscovery) IsBootstrap() bool {
	members := sd.serf.Members()
	return len(members) == 1 && members[0].Name == sd.serf.LocalMember().Name
}

// Addr returns the bind address of the Serf agent
func (sd *SerfDiscovery) Addr() net.Addr {
	lm := sd.serf.LocalMember()
	return &net.TCPAddr{IP: lm.Addr, Port: int(lm.Port)}
}

// Leave gracefully leaves the cluster
func (sd *SerfDiscovery) Leave() error {
	return sd.serf.Leave()
}

// Shutdown shuts down the discovery layer
func (sd *SerfDiscovery) Shutdown() error {
	close(sd.shutdownCh)
	return sd.serf.Shutdown()
}

// SendEvent sends a custom user event to the cluster
func (sd *SerfDiscovery) SendEvent(name string, payload []byte, coalesce bool) error {
	return sd.serf.UserEvent(name, payload, coalesce)
}

// ParseEncryptKey parses a base64-encoded encryption key
func ParseEncryptKey(keyStr string) ([]byte, error) {
	if keyStr == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(keyStr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode encryption key: %w", err)
	}
	keyLen := len(key)
	if keyLen != 16 && keyLen != 24 && keyLen != 32 {
		return nil, fmt.Errorf("encryption key must be 16, 24, or 32 bytes, got %d", keyLen)
	}
	return key, nil
}
