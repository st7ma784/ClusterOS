package roles

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"
)

// Role represents a service that can be health-checked and stopped.
// Startup is handled by the daemon phase machine, not the role itself.
type Role interface {
	Name() string
	HealthCheck() error
	Stop(ctx context.Context) error
}

// BaseRole provides common functionality for all roles
type BaseRole struct {
	name    string
	logger  *logrus.Logger
	running bool
}

// NewBaseRole creates a new base role
func NewBaseRole(name string, logger *logrus.Logger) *BaseRole {
	if logger == nil {
		logger = logrus.New()
	}
	return &BaseRole{name: name, logger: logger}
}

func (br *BaseRole) Name() string { return br.name }

func (br *BaseRole) IsRunning() bool { return br.running }

func (br *BaseRole) SetRunning(running bool) { br.running = running }

func (br *BaseRole) Logger() *logrus.Logger { return br.logger }

// Stop is a default no-op implementation
func (br *BaseRole) Stop(ctx context.Context) error {
	br.running = false
	br.logger.Infof("Role %s stopped", br.name)
	return nil
}

// HealthCheck returns error if the role is not running
func (br *BaseRole) HealthCheck() error {
	if !br.running {
		return fmt.Errorf("role %s is not running", br.name)
	}
	return nil
}

// RoleStatus represents the current status of a role
type RoleStatus struct {
	Name    string
	Running bool
	Healthy bool
	Error   string
}

// GetStatus returns the current status of the base role
func (br *BaseRole) GetStatus() RoleStatus {
	return RoleStatus{
		Name:    br.name,
		Running: br.running,
		Healthy: br.running,
	}
}

// RoleFactory is a function that creates a new role instance
type RoleFactory func(config *RoleConfig, logger *logrus.Logger) (Role, error)

// RoleConfig contains configuration for a role
type RoleConfig struct {
	Name   string
	Config map[string]interface{}
}

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
	return nil
}

// IsRegistered checks if a role is registered
func (r *Registry) IsRegistered(name string) bool {
	_, exists := r.factories[name]
	return exists
}
