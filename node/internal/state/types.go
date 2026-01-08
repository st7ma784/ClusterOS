package state

import (
	"net"
	"sync"
	"time"
)

// Node represents a cluster node
type Node struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Roles        []string          `json:"roles"`
	Capabilities Capabilities      `json:"capabilities"`
	Status       NodeStatus        `json:"status"`
	Address      string            `json:"address"`
	WireGuardIP  net.IP            `json:"wireguard_ip,omitempty"`
	Tags         map[string]string `json:"tags"`
	LastSeen     time.Time         `json:"last_seen"`
	JoinedAt     time.Time         `json:"joined_at"`
}

// Capabilities describes node hardware capabilities
type Capabilities struct {
	CPU  int    `json:"cpu"`
	RAM  string `json:"ram"`
	GPU  bool   `json:"gpu"`
	Arch string `json:"arch"`
}

// NodeStatus represents the operational status of a node
type NodeStatus string

const (
	StatusAlive   NodeStatus = "alive"
	StatusLeaving NodeStatus = "leaving"
	StatusLeft    NodeStatus = "left"
	StatusFailed  NodeStatus = "failed"
	StatusUnknown NodeStatus = "unknown"
)

// ClusterState holds the current cluster membership and state
type ClusterState struct {
	mu      sync.RWMutex
	nodes   map[string]*Node // keyed by node ID
	leaders map[string]string // keyed by role, value is node ID
}

// NewClusterState creates a new cluster state
func NewClusterState() *ClusterState {
	return &ClusterState{
		nodes:   make(map[string]*Node),
		leaders: make(map[string]string),
	}
}

// AddNode adds or updates a node in the cluster state
func (cs *ClusterState) AddNode(node *Node) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	node.LastSeen = time.Now()
	if existing, ok := cs.nodes[node.ID]; ok {
		// Update existing node, preserve JoinedAt
		node.JoinedAt = existing.JoinedAt
	} else {
		// New node
		node.JoinedAt = time.Now()
	}

	cs.nodes[node.ID] = node
}

// RemoveNode removes a node from the cluster state
func (cs *ClusterState) RemoveNode(nodeID string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	delete(cs.nodes, nodeID)
}

// GetNode retrieves a node by ID
func (cs *ClusterState) GetNode(nodeID string) (*Node, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	node, ok := cs.nodes[nodeID]
	return node, ok
}

// GetAllNodes returns all nodes in the cluster
func (cs *ClusterState) GetAllNodes() []*Node {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	nodes := make([]*Node, 0, len(cs.nodes))
	for _, node := range cs.nodes {
		nodes = append(nodes, node)
	}
	return nodes
}

// GetNodesByRole returns all nodes with a specific role
func (cs *ClusterState) GetNodesByRole(role string) []*Node {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	nodes := make([]*Node, 0)
	for _, node := range cs.nodes {
		for _, r := range node.Roles {
			if r == role {
				nodes = append(nodes, node)
				break
			}
		}
	}
	return nodes
}

// GetAliveNodes returns all nodes with alive status
func (cs *ClusterState) GetAliveNodes() []*Node {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	nodes := make([]*Node, 0)
	for _, node := range cs.nodes {
		if node.Status == StatusAlive {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// NodeCount returns the total number of nodes
func (cs *ClusterState) NodeCount() int {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return len(cs.nodes)
}

// SetLeader sets the leader for a specific role
func (cs *ClusterState) SetLeader(role string, nodeID string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.leaders[role] = nodeID
}

// GetLeader returns the leader node ID for a specific role
func (cs *ClusterState) GetLeader(role string) (string, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	nodeID, ok := cs.leaders[role]
	return nodeID, ok
}

// GetLeaderNode returns the full node object for a role's leader
func (cs *ClusterState) GetLeaderNode(role string) (*Node, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	nodeID, ok := cs.leaders[role]
	if !ok {
		return nil, false
	}

	node, ok := cs.nodes[nodeID]
	return node, ok
}

// RemoveLeader removes the leader for a specific role
func (cs *ClusterState) RemoveLeader(role string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	delete(cs.leaders, role)
}

// UpdateNodeStatus updates the status of a node
func (cs *ClusterState) UpdateNodeStatus(nodeID string, status NodeStatus) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if node, ok := cs.nodes[nodeID]; ok {
		node.Status = status
		node.LastSeen = time.Now()
	}
}

// UpdateNodeTags updates the tags of a node
func (cs *ClusterState) UpdateNodeTags(nodeID string, tags map[string]string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if node, ok := cs.nodes[nodeID]; ok {
		node.Tags = tags
		node.LastSeen = time.Now()
	}
}
