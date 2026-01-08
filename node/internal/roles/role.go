package roles

import (
	"context"
	"fmt"

	"github.com/cluster-os/node/internal/state"
	"github.com/sirupsen/logrus"
)

// Role represents a service role that can be executed on a node
type Role interface {
	// Name returns the unique name of this role
	Name() string

	// Start starts the role with the given cluster state
	Start(ctx context.Context, clusterState *state.ClusterState) error

	// Stop gracefully stops the role
	Stop(ctx context.Context) error

	// Reconfigure updates the role configuration based on cluster state changes
	Reconfigure(clusterState *state.ClusterState) error

	// HealthCheck verifies the role is functioning correctly
	HealthCheck() error

	// IsLeaderRequired returns true if this role requires leader election
	IsLeaderRequired() bool

	// OnLeadershipChange is called when leadership status changes for this role
	OnLeadershipChange(isLeader bool) error
}

// BaseRole provides common functionality for all roles
type BaseRole struct {
	name     string
	logger   *logrus.Logger
	isLeader bool
	running  bool
}

// NewBaseRole creates a new base role
func NewBaseRole(name string, logger *logrus.Logger) *BaseRole {
	if logger == nil {
		logger = logrus.New()
	}

	return &BaseRole{
		name:     name,
		logger:   logger,
		isLeader: false,
		running:  false,
	}
}

// Name returns the role name
func (br *BaseRole) Name() string {
	return br.name
}

// IsLeader returns true if this node is the leader for this role
func (br *BaseRole) IsLeader() bool {
	return br.isLeader
}

// SetLeader sets the leadership status
func (br *BaseRole) SetLeader(isLeader bool) {
	br.isLeader = isLeader
	br.logger.Infof("Leadership changed for role %s: isLeader=%v", br.name, isLeader)
}

// IsRunning returns true if the role is currently running
func (br *BaseRole) IsRunning() bool {
	return br.running
}

// SetRunning sets the running status
func (br *BaseRole) SetRunning(running bool) {
	br.running = running
}

// Logger returns the role's logger
func (br *BaseRole) Logger() *logrus.Logger {
	return br.logger
}

// Start is a default implementation that does nothing - subclasses should override
func (br *BaseRole) Start(ctx context.Context, clusterState *state.ClusterState) error {
	br.running = true
	br.logger.Infof("Role %s started", br.name)
	return nil
}

// Stop is a default implementation that does nothing - subclasses should override
func (br *BaseRole) Stop(ctx context.Context) error {
	br.running = false
	br.logger.Infof("Role %s stopped", br.name)
	return nil
}

// Reconfigure is a default implementation that does nothing - subclasses should override
func (br *BaseRole) Reconfigure(clusterState *state.ClusterState) error {
	br.logger.Infof("Role %s reconfigured", br.name)
	return nil
}

// IsLeaderRequired returns false by default - subclasses can override
func (br *BaseRole) IsLeaderRequired() bool {
	return false
}

// OnLeadershipChange is a default implementation that does nothing - subclasses should override
func (br *BaseRole) OnLeadershipChange(isLeader bool) error {
	br.isLeader = isLeader
	br.logger.Infof("Leadership changed for role %s: isLeader=%v", br.name, isLeader)
	return nil
}

// RoleConfig contains configuration for a role
type RoleConfig struct {
	Name               string
	Enabled            bool
	RequiresLeadership bool
	Dependencies       []string
	Config             map[string]interface{}
}

// RoleStatus represents the current status of a role
type RoleStatus struct {
	Name      string
	Running   bool
	Healthy   bool
	IsLeader  bool
	Error     string
	StartTime int64
}

// GetStatus returns the current status of the base role
func (br *BaseRole) GetStatus() RoleStatus {
	status := RoleStatus{
		Name:     br.name,
		Running:  br.running,
		IsLeader: br.isLeader,
		Healthy:  false, // Default to unhealthy, subclasses should override
	}

	return status
}

// ValidateConfig validates the role configuration
func (br *BaseRole) ValidateConfig(config map[string]interface{}) error {
	// Base implementation does nothing, subclasses can override
	return nil
}

// DefaultHealthCheck provides a simple health check implementation
func (br *BaseRole) DefaultHealthCheck() error {
	if !br.running {
		return fmt.Errorf("role %s is not running", br.name)
	}
	return nil
}

// HealthCheck verifies the role is functioning correctly
func (br *BaseRole) HealthCheck() error {
	return br.DefaultHealthCheck()
}

// RoleFactory is a function that creates a new role instance
type RoleFactory func(config *RoleConfig, logger *logrus.Logger) (Role, error)

// Registry holds registered role factories
type Registry struct {
	factories map[string]RoleFactory
	logger    *logrus.Logger
}

// NewRegistry creates a new role registry
func NewRegistry(logger *logrus.Logger) *Registry {
	if logger == nil {
		logger = logrus.New()
	}

	return &Registry{
		factories: make(map[string]RoleFactory),
		logger:    logger,
	}
}

// Register registers a role factory
func (r *Registry) Register(name string, factory RoleFactory) error {
	if _, exists := r.factories[name]; exists {
		return fmt.Errorf("role %s is already registered", name)
	}

	r.factories[name] = factory
	r.logger.Infof("Registered role: %s", name)
	return nil
}

// Create creates a new role instance
func (r *Registry) Create(name string, config *RoleConfig) (Role, error) {
	factory, exists := r.factories[name]
	if !exists {
		return nil, fmt.Errorf("role %s is not registered", name)
	}

	role, err := factory(config, r.logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create role %s: %w", name, err)
	}

	return role, nil
}

// List returns all registered role names
func (r *Registry) List() []string {
	names := make([]string, 0, len(r.factories))
	for name := range r.factories {
		names = append(names, name)
	}
	return names
}

// IsRegistered checks if a role is registered
func (r *Registry) IsRegistered(name string) bool {
	_, exists := r.factories[name]
	return exists
}
