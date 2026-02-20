package state

import (
	"fmt"
	"sort"

	"github.com/hashicorp/serf/serf"
	"github.com/sirupsen/logrus"
)

// SerfLeaderElector provides stateless leader election using Serf gossip.
// Leader is always the alive member with the lexicographically lowest name.
// No persistent state, no Raft, no events — just sorted member list.
type SerfLeaderElector struct {
	serf     *serf.Serf
	nodeName string
	logger   *logrus.Logger
}

// SerfElectionConfig contains configuration for Serf-based leader election
type SerfElectionConfig struct {
	NodeName string
	Serf     *serf.Serf
	Logger   *logrus.Logger
}

// NewSerfLeaderElector creates a new Serf-based leader elector
func NewSerfLeaderElector(cfg *SerfElectionConfig) (*SerfLeaderElector, error) {
	if cfg.Serf == nil {
		return nil, fmt.Errorf("serf instance is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = logrus.New()
	}
	return &SerfLeaderElector{
		serf:     cfg.Serf,
		nodeName: cfg.NodeName,
		logger:   cfg.Logger,
	}, nil
}

// ComputeLeader returns the name of the current leader (lowest alive member name).
func (le *SerfLeaderElector) ComputeLeader() string {
	var alive []serf.Member
	for _, m := range le.serf.Members() {
		if m.Status == serf.StatusAlive {
			alive = append(alive, m)
		}
	}
	if len(alive) == 0 {
		return ""
	}
	sort.Slice(alive, func(i, j int) bool {
		return alive[i].Name < alive[j].Name
	})
	return alive[0].Name
}

// IsLeader returns true if this node is the current leader.
func (le *SerfLeaderElector) IsLeader() bool {
	leader := le.ComputeLeader()
	return leader != "" && leader == le.nodeName
}

// GetLeader returns the current leader name or error if none.
func (le *SerfLeaderElector) GetLeader() (string, error) {
	leader := le.ComputeLeader()
	if leader == "" {
		return "", fmt.Errorf("no leader: cluster is empty")
	}
	return leader, nil
}

// Shutdown is a no-op (no goroutines to stop).
func (le *SerfLeaderElector) Shutdown() error { return nil }
