package state

import (
	"fmt"
	"net"
	"sync"
	"time"
)

// Node represents a cluster node
type Node struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Roles           []string          `json:"roles"`
	Capabilities    Capabilities      `json:"capabilities"`
	Status          NodeStatus        `json:"status"`
	Address         string            `json:"address"`
	WireGuardIP     net.IP            `json:"wireguard_ip,omitempty"`
	WireGuardPubKey string            `json:"wireguard_pubkey,omitempty"` // Base64-encoded WireGuard public key
	TailscaleIP     string            `json:"tailscale_ip,omitempty"`     // Tailscale IP address
	Tags            map[string]string `json:"tags"`
	LastSeen        time.Time         `json:"last_seen"`
	JoinedAt        time.Time         `json:"joined_at"`
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

// ServiceEndpoint represents a service endpoint
type ServiceEndpoint struct {
	ServiceName string    `json:"service_name"`
	Address     string    `json:"address"`
	Port        int       `json:"port"`
	NodeID      string    `json:"node_id"`
	Status      string    `json:"status"` // "running", "stopped", "error"
	LastSeen    time.Time `json:"last_seen"`
}

// ClusterState holds the current cluster membership and state
type ClusterState struct {
	mu               sync.RWMutex
	nodes            map[string]*Node                // keyed by node ID
	leaders          map[string]string               // keyed by role, value is node ID
	serviceEndpoints map[string]*ServiceEndpoint     // keyed by service name
	secrets          *ClusterSecrets                 // cluster-wide secrets (replicated via Raft)
}

// ClusterSecrets holds cluster-wide secret data
type ClusterSecrets struct {
	MungeKey     []byte    `json:"munge_key"`
	MungeKeyHash string    `json:"munge_key_hash"`
	K3sToken     string    `json:"k3s_token"`
	CreatedAt    time.Time `json:"created_at"`
}

// NewClusterState creates a new cluster state
func NewClusterState() *ClusterState {
	return &ClusterState{
		nodes:            make(map[string]*Node),
		leaders:          make(map[string]string),
		serviceEndpoints: make(map[string]*ServiceEndpoint),
		secrets:          &ClusterSecrets{},
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

// UpdateServiceEndpoint updates or adds a service endpoint
func (cs *ClusterState) UpdateServiceEndpoint(serviceName, address string, port int, nodeID, status string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	
	cs.serviceEndpoints[serviceName] = &ServiceEndpoint{
		ServiceName: serviceName,
		Address:     address,
		Port:        port,
		NodeID:      nodeID,
		Status:      status,
		LastSeen:    time.Now(),
	}
}

// GetServiceEndpoint returns a service endpoint by name
func (cs *ClusterState) GetServiceEndpoint(serviceName string) (*ServiceEndpoint, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	endpoint, ok := cs.serviceEndpoints[serviceName]
	return endpoint, ok
}

// GetAllServiceEndpoints returns all service endpoints
func (cs *ClusterState) GetAllServiceEndpoints() map[string]*ServiceEndpoint {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	
	endpoints := make(map[string]*ServiceEndpoint)
	for k, v := range cs.serviceEndpoints {
		endpoints[k] = v
	}
	return endpoints
}

// RemoveServiceEndpoint removes a service endpoint
func (cs *ClusterState) RemoveServiceEndpoint(serviceName string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	delete(cs.serviceEndpoints, serviceName)
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

// SetMungeKey sets the munge key in cluster secrets
func (cs *ClusterState) SetMungeKey(key []byte, hash string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	cs.secrets.MungeKey = key
	cs.secrets.MungeKeyHash = hash
	cs.secrets.CreatedAt = time.Now()
}

// GetMungeKey retrieves the munge key from cluster secrets
func (cs *ClusterState) GetMungeKey() ([]byte, string, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	if cs.secrets.MungeKey == nil || len(cs.secrets.MungeKey) == 0 {
		return nil, "", fmt.Errorf("munge key not set in cluster state")
	}

	return cs.secrets.MungeKey, cs.secrets.MungeKeyHash, nil
}

// HasMungeKey returns true if a munge key has been set
func (cs *ClusterState) HasMungeKey() bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	return cs.secrets.MungeKey != nil && len(cs.secrets.MungeKey) > 0
}

// SetK3sToken sets the K3s cluster token
func (cs *ClusterState) SetK3sToken(token string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	cs.secrets.K3sToken = token
}

// GetK3sToken retrieves the K3s cluster token
func (cs *ClusterState) GetK3sToken() (string, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	if cs.secrets.K3sToken == "" {
		return "", fmt.Errorf("K3s token not set in cluster state")
	}

	return cs.secrets.K3sToken, nil
}

// GetLocalNode returns the local node from the cluster state
func (cs *ClusterState) GetLocalNode() *Node {
	// This is a placeholder - in a real implementation, this would be
	// determined by the node's identity
	// For now, return nil to indicate not implemented
	return nil
}

// GetWireGuardIPs returns all WireGuard IPs in the cluster
func (cs *ClusterState) GetWireGuardIPs() map[string]net.IP {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	ips := make(map[string]net.IP)
	for id, node := range cs.nodes {
		if node.WireGuardIP != nil {
			ips[id] = node.WireGuardIP
		}
	}
	return ips
}

// SetLocalNodeID sets the local node ID (for identification)
func (cs *ClusterState) SetLocalNodeID(nodeID string) {
	// This is a placeholder - in a real implementation, this would store
	// the local node ID for identification purposes
	// For now, we don't need to store it as it's available through other means
}

// UpdateNodeTailscaleIP updates the Tailscale IP of a node
func (cs *ClusterState) UpdateNodeTailscaleIP(nodeID, tailscaleIP string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if node, ok := cs.nodes[nodeID]; ok {
		node.TailscaleIP = tailscaleIP
		node.LastSeen = time.Now()
	}
}
