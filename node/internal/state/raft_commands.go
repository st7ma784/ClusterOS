package state

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/hashicorp/raft"
)

// CommandType represents the type of Raft command
type CommandType uint8

const (
	// CommandSetMungeKey sets the cluster munge key
	CommandSetMungeKey CommandType = iota
	// CommandAddNode adds a node to the cluster
	CommandAddNode
	// CommandRemoveNode removes a node from the cluster
	CommandRemoveNode
	// CommandSetLeader sets the leader for a role
	CommandSetLeader
)

// RaftCommand represents a command to be applied to the FSM
type RaftCommand struct {
	Type    CommandType     `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// SetMungeKeyPayload is the payload for setting the munge key
type SetMungeKeyPayload struct {
	MungeKey     []byte `json:"munge_key"`
	MungeKeyHash string `json:"munge_key_hash"`
}

// Apply applies a Raft log entry to the FSM
func (f *clusterFSM) Apply(log *raft.Log) interface{} {
	f.logger.Debugf("Applying Raft log entry: index=%d, type=%v", log.Index, log.Type)

	// Decode command
	var cmd RaftCommand
	if err := json.Unmarshal(log.Data, &cmd); err != nil {
		f.logger.Errorf("Failed to unmarshal command: %v", err)
		return fmt.Errorf("failed to unmarshal command: %w", err)
	}

	// Apply based on command type
	switch cmd.Type {
	case CommandSetMungeKey:
		return f.applySetMungeKey(cmd.Payload)
	case CommandAddNode:
		return f.applyAddNode(cmd.Payload)
	case CommandRemoveNode:
		return f.applyRemoveNode(cmd.Payload)
	case CommandSetLeader:
		return f.applySetLeader(cmd.Payload)
	default:
		f.logger.Errorf("Unknown command type: %v", cmd.Type)
		return fmt.Errorf("unknown command type: %v", cmd.Type)
	}
}

// applySetMungeKey applies a SetMungeKey command
func (f *clusterFSM) applySetMungeKey(payload json.RawMessage) interface{} {
	var p SetMungeKeyPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		f.logger.Errorf("Failed to unmarshal SetMungeKey payload: %v", err)
		return fmt.Errorf("failed to unmarshal payload: %w", err)
	}

	f.logger.Infof("Setting munge key in cluster state (hash: %s)", p.MungeKeyHash[:16]+"...")
	f.state.SetMungeKey(p.MungeKey, p.MungeKeyHash)

	return nil
}

// applyAddNode applies an AddNode command
func (f *clusterFSM) applyAddNode(payload json.RawMessage) interface{} {
	var node Node
	if err := json.Unmarshal(payload, &node); err != nil {
		f.logger.Errorf("Failed to unmarshal AddNode payload: %v", err)
		return fmt.Errorf("failed to unmarshal payload: %w", err)
	}

	f.logger.Infof("Adding node to cluster state: %s", node.ID)
	f.state.AddNode(&node)

	return nil
}

// applyRemoveNode applies a RemoveNode command
func (f *clusterFSM) applyRemoveNode(payload json.RawMessage) interface{} {
	var nodeID string
	if err := json.Unmarshal(payload, &nodeID); err != nil {
		f.logger.Errorf("Failed to unmarshal RemoveNode payload: %v", err)
		return fmt.Errorf("failed to unmarshal payload: %w", err)
	}

	f.logger.Infof("Removing node from cluster state: %s", nodeID)
	f.state.RemoveNode(nodeID)

	return nil
}

// applySetLeader applies a SetLeader command
func (f *clusterFSM) applySetLeader(payload json.RawMessage) interface{} {
	var data struct {
		Role   string `json:"role"`
		NodeID string `json:"node_id"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		f.logger.Errorf("Failed to unmarshal SetLeader payload: %v", err)
		return fmt.Errorf("failed to unmarshal payload: %w", err)
	}

	f.logger.Infof("Setting leader for role %s: %s", data.Role, data.NodeID)
	f.state.SetLeader(data.Role, data.NodeID)

	return nil
}

// Snapshot creates a snapshot of the current state
func (f *clusterFSM) Snapshot() (raft.FSMSnapshot, error) {
	f.logger.Info("Creating FSM snapshot")

	// Take a read lock on the state
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()

	// Create snapshot data
	snapshot := &clusterStateSnapshot{
		Nodes:   make(map[string]*Node),
		Leaders: make(map[string]string),
		Secrets: &ClusterSecrets{},
	}

	// Copy nodes
	for id, node := range f.state.nodes {
		// Deep copy node
		nodeCopy := *node
		if node.Tags != nil {
			nodeCopy.Tags = make(map[string]string)
			for k, v := range node.Tags {
				nodeCopy.Tags[k] = v
			}
		}
		snapshot.Nodes[id] = &nodeCopy
	}

	// Copy leaders
	for role, nodeID := range f.state.leaders {
		snapshot.Leaders[role] = nodeID
	}

	// Copy secrets
	if f.state.secrets != nil {
		snapshot.Secrets.MungeKey = make([]byte, len(f.state.secrets.MungeKey))
		copy(snapshot.Secrets.MungeKey, f.state.secrets.MungeKey)
		snapshot.Secrets.MungeKeyHash = f.state.secrets.MungeKeyHash
		snapshot.Secrets.CreatedAt = f.state.secrets.CreatedAt
	}

	f.logger.Infof("Created snapshot with %d nodes, %d leaders", len(snapshot.Nodes), len(snapshot.Leaders))

	return snapshot, nil
}

// Restore restores state from a snapshot
func (f *clusterFSM) Restore(rc io.ReadCloser) error {
	f.logger.Info("Restoring FSM from snapshot")
	defer rc.Close()

	// Decode snapshot
	var snapshot clusterStateSnapshot
	decoder := json.NewDecoder(rc)
	if err := decoder.Decode(&snapshot); err != nil {
		f.logger.Errorf("Failed to decode snapshot: %v", err)
		return fmt.Errorf("failed to decode snapshot: %w", err)
	}

	// Take a write lock on the state
	f.state.mu.Lock()
	defer f.state.mu.Unlock()

	// Restore nodes
	f.state.nodes = snapshot.Nodes

	// Restore leaders
	f.state.leaders = snapshot.Leaders

	// Restore secrets
	if snapshot.Secrets != nil {
		f.state.secrets = snapshot.Secrets
	} else {
		f.state.secrets = &ClusterSecrets{}
	}

	f.logger.Infof("Restored snapshot with %d nodes, %d leaders", len(snapshot.Nodes), len(snapshot.Leaders))

	return nil
}

// clusterStateSnapshot represents a snapshot of cluster state
type clusterStateSnapshot struct {
	Nodes   map[string]*Node `json:"nodes"`
	Leaders map[string]string `json:"leaders"`
	Secrets *ClusterSecrets  `json:"secrets"`
}

// Persist saves the snapshot to the sink
func (s *clusterStateSnapshot) Persist(sink raft.SnapshotSink) error {
	// Encode snapshot as JSON
	encoder := json.NewEncoder(sink)
	if err := encoder.Encode(s); err != nil {
		sink.Cancel()
		return fmt.Errorf("failed to encode snapshot: %w", err)
	}

	// Close the sink
	return sink.Close()
}

// Release is called when the snapshot is no longer needed
func (s *clusterStateSnapshot) Release() {
	// Nothing to release
}
