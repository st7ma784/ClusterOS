package state

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/serf/serf"
	"github.com/sirupsen/logrus"
)

// SerfLeaderElector provides stateless leader election using Serf gossip
// This is designed for environments without persistent storage
type SerfLeaderElector struct {
	serf       *serf.Serf
	nodeID     string
	nodeName   string
	logger     *logrus.Logger
	shutdownCh chan struct{}

	// Leadership state
	mu              sync.RWMutex
	isLeader        bool
	currentLeader   string // node name of current leader
	roleLeaderships map[string]bool
	leaderObservers map[string][]chan bool

	// Replicated state (distributed via gossip)
	replicatedState map[string][]byte // key -> value (e.g., munge_key -> key bytes)
	stateVersion    uint64            // monotonic version for state changes

	// Cluster state reference
	clusterState *ClusterState
}

// SerfElectionConfig contains configuration for Serf-based leader election
type SerfElectionConfig struct {
	NodeID       string
	NodeName     string
	Serf         *serf.Serf
	ClusterState *ClusterState
	Logger       *logrus.Logger
}

// NewSerfLeaderElector creates a new Serf-based leader elector
func NewSerfLeaderElector(cfg *SerfElectionConfig) (*SerfLeaderElector, error) {
	if cfg.Logger == nil {
		cfg.Logger = logrus.New()
	}

	if cfg.Serf == nil {
		return nil, fmt.Errorf("serf instance is required")
	}

	le := &SerfLeaderElector{
		serf:            cfg.Serf,
		nodeID:          cfg.NodeID,
		nodeName:        cfg.NodeName,
		logger:          cfg.Logger,
		shutdownCh:      make(chan struct{}),
		roleLeaderships: make(map[string]bool),
		leaderObservers: make(map[string][]chan bool),
		replicatedState: make(map[string][]byte),
		clusterState:    cfg.ClusterState,
	}

	// Start leadership monitor
	go le.monitorLeadership()

	// Start state synchronization
	go le.syncStateLoop()

	return le, nil
}

// monitorLeadership periodically checks and updates leadership status
func (le *SerfLeaderElector) monitorLeadership() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			le.checkLeadership()
		case <-le.shutdownCh:
			return
		}
	}
}

// checkLeadership determines who should be the leader based on Serf membership
func (le *SerfLeaderElector) checkLeadership() {
	le.mu.Lock()
	defer le.mu.Unlock()

	// Get all alive members
	members := le.serf.Members()
	var aliveMembers []serf.Member
	for _, m := range members {
		if m.Status == serf.StatusAlive {
			aliveMembers = append(aliveMembers, m)
		}
	}

	if len(aliveMembers) == 0 {
		le.logger.Warn("No alive members in cluster")
		return
	}

	// Deterministic leader election: lowest lexicographic node name wins
	// This is stable and requires no coordination
	sort.Slice(aliveMembers, func(i, j int) bool {
		return aliveMembers[i].Name < aliveMembers[j].Name
	})

	leaderName := aliveMembers[0].Name
	wasLeader := le.isLeader
	le.currentLeader = leaderName
	le.isLeader = (leaderName == le.nodeName)

	// Notify observers if leadership changed
	if wasLeader != le.isLeader {
		le.logger.Infof("Leadership changed: isLeader=%v (leader is %s)", le.isLeader, leaderName)
		le.notifyLeadershipChange(le.isLeader)
	}
}

// notifyLeadershipChange notifies all role observers of leadership changes
func (le *SerfLeaderElector) notifyLeadershipChange(isLeader bool) {
	for role, observers := range le.leaderObservers {
		le.roleLeaderships[role] = isLeader
		for _, ch := range observers {
			select {
			case ch <- isLeader:
			default:
			}
		}
	}
}

// syncStateLoop periodically synchronizes replicated state across the cluster
func (le *SerfLeaderElector) syncStateLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if le.IsLeader() {
				le.broadcastState()
			}
		case <-le.shutdownCh:
			return
		}
	}
}

// broadcastState sends the current replicated state to all nodes
func (le *SerfLeaderElector) broadcastState() {
	le.mu.RLock()
	defer le.mu.RUnlock()

	if len(le.replicatedState) == 0 {
		return
	}

	// Create state snapshot
	snapshot := StateSnapshot{
		Version: le.stateVersion,
		State:   make(map[string]string),
	}

	for k, v := range le.replicatedState {
		snapshot.State[k] = base64.StdEncoding.EncodeToString(v)
	}

	payload, err := json.Marshal(snapshot)
	if err != nil {
		le.logger.Errorf("Failed to marshal state snapshot: %v", err)
		return
	}

	// Send as Serf user event
	if err := le.serf.UserEvent("cluster-state", payload, true); err != nil {
		le.logger.Errorf("Failed to broadcast state: %v", err)
	} else {
		le.logger.Debug("Broadcast cluster state to all nodes")
	}
}

// StateSnapshot represents a point-in-time snapshot of replicated state
type StateSnapshot struct {
	Version uint64            `json:"version"`
	State   map[string]string `json:"state"` // base64-encoded values
}

// HandleStateEvent processes incoming state synchronization events
func (le *SerfLeaderElector) HandleStateEvent(payload []byte) error {
	var snapshot StateSnapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return fmt.Errorf("failed to unmarshal state snapshot: %w", err)
	}

	le.mu.Lock()
	defer le.mu.Unlock()

	// Only apply if version is newer
	if snapshot.Version <= le.stateVersion {
		return nil
	}

	le.logger.Infof("Applying state snapshot version %d", snapshot.Version)

	for k, v := range snapshot.State {
		decoded, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			le.logger.Warnf("Failed to decode state value for key %s: %v", k, err)
			continue
		}
		le.replicatedState[k] = decoded
	}

	le.stateVersion = snapshot.Version

	// Apply state to cluster state
	if mungeKey, ok := le.replicatedState["munge_key"]; ok {
		le.clusterState.SetMungeKey(mungeKey, computeHash(mungeKey))
		le.logger.Info("Applied munge key from replicated state")
	}

	return nil
}

// IsLeader returns true if this node is the current leader
func (le *SerfLeaderElector) IsLeader() bool {
	le.mu.RLock()
	defer le.mu.RUnlock()
	return le.isLeader
}

// IsLeaderForRole returns true if this node is the leader for a specific role
func (le *SerfLeaderElector) IsLeaderForRole(role string) bool {
	le.mu.RLock()
	defer le.mu.RUnlock()
	// For Serf-based election, the Raft leader is leader for all roles
	return le.isLeader
}

// GetLeader returns the current leader's node name
func (le *SerfLeaderElector) GetLeader() (string, error) {
	le.mu.RLock()
	defer le.mu.RUnlock()

	if le.currentLeader == "" {
		return "", fmt.Errorf("no leader elected")
	}
	return le.currentLeader, nil
}

// WaitForLeader waits until a leader is elected (with timeout)
func (le *SerfLeaderElector) WaitForLeader(timeout time.Duration) error {
	start := time.Now()
	for {
		if time.Since(start) > timeout {
			return fmt.Errorf("timeout waiting for leader election")
		}

		le.checkLeadership() // Force a check

		le.mu.RLock()
		hasLeader := le.currentLeader != ""
		le.mu.RUnlock()

		if hasLeader {
			le.logger.Infof("Leader elected: %s", le.currentLeader)
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// RegisterRoleLeadershipObserver registers a channel to be notified of leadership changes
func (le *SerfLeaderElector) RegisterRoleLeadershipObserver(role string) <-chan bool {
	le.mu.Lock()
	defer le.mu.Unlock()

	ch := make(chan bool, 1)
	le.leaderObservers[role] = append(le.leaderObservers[role], ch)

	// Send current state immediately
	ch <- le.isLeader

	return ch
}

// ApplySetMungeKey stores and replicates a munge key across the cluster
func (le *SerfLeaderElector) ApplySetMungeKey(mungeKey []byte, mungeKeyHash string) error {
	if !le.IsLeader() {
		return fmt.Errorf("not the leader, cannot apply munge key")
	}

	le.mu.Lock()
	le.replicatedState["munge_key"] = mungeKey
	le.stateVersion++
	le.mu.Unlock()

	// Store in cluster state
	le.clusterState.SetMungeKey(mungeKey, mungeKeyHash)

	// Broadcast to all nodes
	le.broadcastState()

	le.logger.Info("Successfully applied and broadcast munge key")
	return nil
}

// GetClusterState returns the cluster state
func (le *SerfLeaderElector) GetClusterState() *ClusterState {
	return le.clusterState
}

// AddVoter is a no-op for Serf-based election (no Raft voters)
func (le *SerfLeaderElector) AddVoter(nodeID string, address string) error {
	le.logger.Debugf("AddVoter called for %s (no-op in Serf mode)", nodeID)
	return nil
}

// RemoveServer is a no-op for Serf-based election (no Raft servers)
func (le *SerfLeaderElector) RemoveServer(nodeID string) error {
	le.logger.Debugf("RemoveServer called for %s (no-op in Serf mode)", nodeID)
	return nil
}

// Shutdown gracefully shuts down the leader elector
func (le *SerfLeaderElector) Shutdown() error {
	le.logger.Info("Shutting down Serf leader elector")
	close(le.shutdownCh)
	return nil
}

// RequestState requests current state from the leader
func (le *SerfLeaderElector) RequestState() error {
	le.mu.RLock()
	leader := le.currentLeader
	le.mu.RUnlock()

	if leader == "" || leader == le.nodeName {
		return nil // We are the leader or no leader yet
	}

	// Send a query to the leader requesting state
	payload := []byte("request-state")
	params := &serf.QueryParam{
		FilterNodes: []string{leader},
		RequestAck:  true,
	}

	resp, err := le.serf.Query("get-state", payload, params)
	if err != nil {
		return fmt.Errorf("failed to query leader for state: %w", err)
	}

	// Process responses
	for r := range resp.ResponseCh() {
		if err := le.HandleStateEvent(r.Payload); err != nil {
			le.logger.Warnf("Failed to process state from leader: %v", err)
		}
	}

	return nil
}

// HandleStateQuery responds to state queries from other nodes
func (le *SerfLeaderElector) HandleStateQuery() ([]byte, error) {
	if !le.IsLeader() {
		return nil, fmt.Errorf("not the leader")
	}

	le.mu.RLock()
	defer le.mu.RUnlock()

	snapshot := StateSnapshot{
		Version: le.stateVersion,
		State:   make(map[string]string),
	}

	for k, v := range le.replicatedState {
		snapshot.State[k] = base64.StdEncoding.EncodeToString(v)
	}

	return json.Marshal(snapshot)
}

// GetReplicatedState returns a copy of the replicated state
func (le *SerfLeaderElector) GetReplicatedState() map[string][]byte {
	le.mu.RLock()
	defer le.mu.RUnlock()

	result := make(map[string][]byte)
	for k, v := range le.replicatedState {
		result[k] = append([]byte{}, v...)
	}
	return result
}

// SetReplicatedState sets a value in replicated state (leader only)
func (le *SerfLeaderElector) SetReplicatedState(key string, value []byte) error {
	if !le.IsLeader() {
		return fmt.Errorf("not the leader, cannot set replicated state")
	}

	le.mu.Lock()
	le.replicatedState[key] = value
	le.stateVersion++
	le.mu.Unlock()

	// Broadcast immediately
	le.broadcastState()

	return nil
}

// computeHash computes SHA256 hash of data
func computeHash(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
