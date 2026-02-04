package state

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"github.com/sirupsen/logrus"
)

// LeaderElector manages per-role leader election using Raft (persistent consensus)
// 
// RESILIENCE ARCHITECTURE:
// ========================
// This implementation provides automatic failover and re-election without service disruption:
//
// 1. QUORUM-BASED SAFETY:
//    - Requires majority of nodes to reach consensus (no split-brain)
//    - If leader fails, new leader elected from remaining healthy nodes
//    - A node losing connection to leader doesn't trigger re-election if quorum still healthy
//
// 2. HEARTBEAT MONITORING (1 second interval):
//    - If leader fails to send heartbeat within 1 second, followers detect failure
//    - New election automatically triggered (1 second timeout + 1 second election = ~2s failover)
//    - No manual intervention needed
//
// 3. MESH NETWORK RESILIENCE (Tailscale with built-in redundancy):
//    - Multiple paths between nodes ensure redundancy
//    - If one peer connection lost, others remain active
//    - Persistent keepalive packets prevent NAT timeout issues
//    - Only total loss of leader (all network paths down) triggers re-election
//
// 4. STATE PERSISTENCE:
//    - Raft state persisted to disk in BoltDB
//    - Surviving nodes can recover leadership and state after crash
//
// TOPOLOGY:
//    Leader Election: Raft consensus (persistent, cross-node coordination)
//    Service Discovery: Serf gossip (membership tracking)
//    Networking: Tailscale mesh (CGNAT 100.64.0.0/10 with global connectivity)
type LeaderElector struct {
	raft       *raft.Raft
	fsm        *clusterFSM
	transport  *raft.NetworkTransport
	state      *ClusterState
	nodeID     string
	dataDir    string
	logger     *logrus.Logger
	shutdownCh chan struct{}

	// Role-specific leadership tracking
	mu              sync.RWMutex
	roleLeaderships map[string]bool // role -> am I leader?
	leaderObservers map[string][]chan bool
}

// ElectionConfig contains configuration for leader election
type ElectionConfig struct {
	NodeID        string
	NodeAddr      string
	DataDir       string
	BindAddr      string
	BindPort      int
	BootstrapNode bool
	Logger        *logrus.Logger
}

// NewLeaderElector creates a new Raft-based leader elector
func NewLeaderElector(cfg *ElectionConfig, clusterState *ClusterState) (*LeaderElector, error) {
	if cfg.Logger == nil {
		cfg.Logger = logrus.New()
	}

	if cfg.DataDir == "" {
		cfg.DataDir = "/var/lib/cluster-os/raft"
	}

	// Ensure data directory exists
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create raft data directory: %w", err)
	}

	fsm := &clusterFSM{
		state:  clusterState,
		logger: cfg.Logger,
	}

	le := &LeaderElector{
		fsm:             fsm,
		state:           clusterState,
		nodeID:          cfg.NodeID,
		dataDir:         cfg.DataDir,
		logger:          cfg.Logger,
		shutdownCh:      make(chan struct{}),
		roleLeaderships: make(map[string]bool),
		leaderObservers: make(map[string][]chan bool),
	}

	// Configure Raft
	raftConfig := raft.DefaultConfig()
	raftConfig.LocalID = raft.ServerID(cfg.NodeID)
	raftConfig.LogOutput = io.Discard // Use logger instead
	raftConfig.HeartbeatTimeout = 1000 * time.Millisecond
	raftConfig.ElectionTimeout = 1000 * time.Millisecond
	raftConfig.CommitTimeout = 500 * time.Millisecond
	raftConfig.LeaderLeaseTimeout = 500 * time.Millisecond

	// Create transport
	// Bind to all interfaces
	bindAddr := fmt.Sprintf("%s:%d", cfg.BindAddr, cfg.BindPort)

	// Use NodeAddr for advertising (this is the address other nodes will use to contact us)
	advertiseAddr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:%d", cfg.NodeAddr, cfg.BindPort))
	if err != nil {
		return nil, fmt.Errorf("failed to resolve advertise address: %w", err)
	}

	transportConfig := &raft.NetworkTransportConfig{
		ServerAddressProvider: nil,
		MaxPool:               3,
		Timeout:               10 * time.Second,
		Stream:                nil,
		Logger:                nil,
	}

	transport, err := raft.NewTCPTransportWithConfig(bindAddr, advertiseAddr, transportConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create Raft transport: %w", err)
	}
	le.transport = transport

	// Create log store
	logStore, err := raftboltdb.NewBoltStore(filepath.Join(cfg.DataDir, "raft-log.db"))
	if err != nil {
		return nil, fmt.Errorf("failed to create log store: %w", err)
	}

	// Create stable store
	stableStore, err := raftboltdb.NewBoltStore(filepath.Join(cfg.DataDir, "raft-stable.db"))
	if err != nil {
		return nil, fmt.Errorf("failed to create stable store: %w", err)
	}

	// Create snapshot store
	snapshotStore, err := raft.NewFileSnapshotStore(cfg.DataDir, 3, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot store: %w", err)
	}

	// Create Raft instance
	raftInstance, err := raft.NewRaft(raftConfig, fsm, logStore, stableStore, snapshotStore, transport)
	if err != nil {
		return nil, fmt.Errorf("failed to create Raft instance: %w", err)
	}
	le.raft = raftInstance

	// Bootstrap if this is the first node
	if cfg.BootstrapNode {
		configuration := raft.Configuration{
			Servers: []raft.Server{
				{
					ID:      raft.ServerID(cfg.NodeID),
					Address: transport.LocalAddr(),
				},
			},
		}
		future := raftInstance.BootstrapCluster(configuration)
		if err := future.Error(); err != nil {
			// Ignore error if already bootstrapped
			if err != raft.ErrCantBootstrap {
				return nil, fmt.Errorf("failed to bootstrap cluster: %w", err)
			}
		} else {
			cfg.Logger.Info("Bootstrapped Raft cluster")
		}
	}

	// Start leadership monitor
	go le.monitorLeadership()

	return le, nil
}

// monitorLeadership watches for leadership changes
func (le *LeaderElector) monitorLeadership() {
	leaderCh := le.raft.LeaderCh()

	for {
		select {
		case isLeader := <-leaderCh:
			le.logger.Infof("Leadership changed: isLeader=%v", isLeader)
			le.handleLeadershipChange(isLeader)
		case <-le.shutdownCh:
			return
		}
	}
}

// handleLeadershipChange handles Raft leadership changes
func (le *LeaderElector) handleLeadershipChange(isLeader bool) {
	le.mu.Lock()
	defer le.mu.Unlock()

	// Notify all role observers
	for role, observers := range le.leaderObservers {
		// Check if we should be leader for this role
		// For now, if we're Raft leader, we're leader for all roles
		le.roleLeaderships[role] = isLeader

		// Notify observers
		for _, ch := range observers {
			select {
			case ch <- isLeader:
			default:
			}
		}
	}
}

// IsLeader returns true if this node is the Raft leader
func (le *LeaderElector) IsLeader() bool {
	return le.raft.State() == raft.Leader
}

// IsLeaderForRole returns true if this node is the leader for a specific role
func (le *LeaderElector) IsLeaderForRole(role string) bool {
	le.mu.RLock()
	defer le.mu.RUnlock()
	return le.roleLeaderships[role]
}

// WaitForLeader waits until a leader is elected (with timeout)
func (le *LeaderElector) WaitForLeader(timeout time.Duration) error {
	start := time.Now()
	for {
		if time.Since(start) > timeout {
			return fmt.Errorf("timeout waiting for leader election")
		}

		if leader := le.raft.Leader(); leader != "" {
			le.logger.Infof("Leader elected: %s", leader)
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// GetLeader returns the current Raft leader address
func (le *LeaderElector) GetLeader() (string, error) {
	leaderAddr, leaderID := le.raft.LeaderWithID()
	if leaderAddr == "" {
		return "", fmt.Errorf("no leader elected")
	}
	return string(leaderID), nil
}

// AddVoter adds a new voting member to the Raft cluster
func (le *LeaderElector) AddVoter(nodeID string, address string) error {
	if !le.IsLeader() {
		return fmt.Errorf("not the leader, cannot add voter")
	}

	serverID := raft.ServerID(nodeID)
	serverAddr := raft.ServerAddress(address)

	future := le.raft.AddVoter(serverID, serverAddr, 0, 0)
	if err := future.Error(); err != nil {
		return fmt.Errorf("failed to add voter: %w", err)
	}

	le.logger.Infof("Added voter: %s (%s)", nodeID, address)
	return nil
}

// RemoveServer removes a server from the Raft cluster
func (le *LeaderElector) RemoveServer(nodeID string) error {
	if !le.IsLeader() {
		return fmt.Errorf("not the leader, cannot remove server")
	}

	serverID := raft.ServerID(nodeID)
	future := le.raft.RemoveServer(serverID, 0, 0)
	if err := future.Error(); err != nil {
		return fmt.Errorf("failed to remove server: %w", err)
	}

	le.logger.Infof("Removed server: %s", nodeID)
	return nil
}

// GetConfiguration returns the current Raft configuration
func (le *LeaderElector) GetConfiguration() ([]string, error) {
	future := le.raft.GetConfiguration()
	if err := future.Error(); err != nil {
		return nil, fmt.Errorf("failed to get configuration: %w", err)
	}

	config := future.Configuration()
	var servers []string
	for _, server := range config.Servers {
		servers = append(servers, string(server.ID))
	}

	return servers, nil
}

// RegisterRoleLeadershipObserver registers a channel to be notified of leadership changes for a role
func (le *LeaderElector) RegisterRoleLeadershipObserver(role string) <-chan bool {
	le.mu.Lock()
	defer le.mu.Unlock()

	ch := make(chan bool, 1)
	le.leaderObservers[role] = append(le.leaderObservers[role], ch)

	// Send current state immediately
	if isLeader, ok := le.roleLeaderships[role]; ok {
		ch <- isLeader
	} else {
		ch <- le.IsLeader()
	}

	return ch
}

// ApplySetMungeKey applies a SetMungeKey command via Raft
func (le *LeaderElector) ApplySetMungeKey(mungeKey []byte, mungeKeyHash string) error {
	if !le.IsLeader() {
		return fmt.Errorf("not the leader, cannot apply munge key")
	}

	// Create command payload
	payload := SetMungeKeyPayload{
		MungeKey:     mungeKey,
		MungeKeyHash: mungeKeyHash,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	// Create Raft command
	cmd := RaftCommand{
		Type:    CommandSetMungeKey,
		Payload: payloadBytes,
	}

	cmdBytes, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("failed to marshal command: %w", err)
	}

	// Apply to Raft with timeout
	future := le.raft.Apply(cmdBytes, 10*time.Second)
	if err := future.Error(); err != nil {
		return fmt.Errorf("failed to apply munge key via Raft: %w", err)
	}

	le.logger.Info("Successfully applied munge key to Raft cluster")
	return nil
}

// GetClusterState returns the cluster state
func (le *LeaderElector) GetClusterState() *ClusterState {
	return le.state
}

// Shutdown gracefully shuts down the leader elector
func (le *LeaderElector) Shutdown() error {
	le.logger.Info("Shutting down leader elector")

	close(le.shutdownCh)

	// Step down if leader
	if le.IsLeader() {
		le.logger.Info("Stepping down as leader")
		if err := le.raft.LeadershipTransfer().Error(); err != nil {
			le.logger.Warnf("Failed to transfer leadership: %v", err)
		}
	}

	// Shutdown Raft
	future := le.raft.Shutdown()
	if err := future.Error(); err != nil {
		return fmt.Errorf("failed to shutdown Raft: %w", err)
	}

	// Close transport
	if err := le.transport.Close(); err != nil {
		return fmt.Errorf("failed to close transport: %w", err)
	}

	return nil
}

// clusterFSM is the finite state machine for Raft
// Implementation is in raft_commands.go
type clusterFSM struct {
	state  *ClusterState
	logger *logrus.Logger
}
