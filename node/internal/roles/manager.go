package roles

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cluster-os/node/internal/state"
	"github.com/sirupsen/logrus"
)

// LeaderElectorManager interface for leader election in role management
// Both Raft-based and Serf-based electors implement this
type LeaderElectorManager interface {
	IsLeader() bool
	RegisterRoleLeadershipObserver(role string) <-chan bool
}

// Manager manages the lifecycle of all roles on a node
type Manager struct {
	registry      *Registry
	roles         map[string]Role
	clusterState  *state.ClusterState
	leaderElector LeaderElectorManager
	logger        *logrus.Logger
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	mu            sync.RWMutex
}

// ManagerConfig contains configuration for the role manager
type ManagerConfig struct {
	Registry      *Registry
	ClusterState  *state.ClusterState
	LeaderElector LeaderElectorManager
	Logger        *logrus.Logger
}

// NewManager creates a new role manager
func NewManager(cfg *ManagerConfig) (*Manager, error) {
	if cfg.Registry == nil {
		return nil, fmt.Errorf("registry is required")
	}

	if cfg.ClusterState == nil {
		return nil, fmt.Errorf("cluster state is required")
	}

	if cfg.Logger == nil {
		cfg.Logger = logrus.New()
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Manager{
		registry:      cfg.Registry,
		roles:         make(map[string]Role),
		clusterState:  cfg.ClusterState,
		leaderElector: cfg.LeaderElector,
		logger:        cfg.Logger,
		ctx:           ctx,
		cancel:        cancel,
	}, nil
}

// StartRole starts a specific role
func (m *Manager) StartRole(name string, config *RoleConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if already running
	if _, exists := m.roles[name]; exists {
		return fmt.Errorf("role %s is already running", name)
	}

	// Create role instance
	role, err := m.registry.Create(name, config)
	if err != nil {
		return fmt.Errorf("failed to create role %s: %w", name, err)
	}

	// If role requires leadership, register for leadership notifications
	if role.IsLeaderRequired() && m.leaderElector != nil {
		m.wg.Add(1)
		go m.monitorLeadership(role)
	}

	// Start the role
	if err := role.Start(m.ctx, m.clusterState); err != nil {
		return fmt.Errorf("failed to start role %s: %w", name, err)
	}

	m.roles[name] = role
	m.logger.Infof("Started role: %s", name)

	return nil
}

// StopRole stops a specific role
func (m *Manager) StopRole(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	role, exists := m.roles[name]
	if !exists {
		return fmt.Errorf("role %s is not running", name)
	}

	// Stop the role
	if err := role.Stop(m.ctx); err != nil {
		m.logger.Warnf("Error stopping role %s: %v", name, err)
	}

	delete(m.roles, name)
	m.logger.Infof("Stopped role: %s", name)

	return nil
}

// RestartRole restarts a specific role
func (m *Manager) RestartRole(name string, config *RoleConfig) error {
	m.logger.Infof("Restarting role: %s", name)

	if err := m.StopRole(name); err != nil && err.Error() != fmt.Sprintf("role %s is not running", name) {
		return err
	}

	time.Sleep(1 * time.Second) // Brief pause

	return m.StartRole(name, config)
}

// ReconfigureRole reconfigures a running role
func (m *Manager) ReconfigureRole(name string) error {
	m.mu.RLock()
	role, exists := m.roles[name]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("role %s is not running", name)
	}

	if err := role.Reconfigure(m.clusterState); err != nil {
		return fmt.Errorf("failed to reconfigure role %s: %w", name, err)
	}

	m.logger.Infof("Reconfigured role: %s", name)
	return nil
}

// ReconfigureAll reconfigures all running roles
func (m *Manager) ReconfigureAll() error {
	m.mu.RLock()
	roleNames := make([]string, 0, len(m.roles))
	for name := range m.roles {
		roleNames = append(roleNames, name)
	}
	m.mu.RUnlock()

	var lastErr error
	for _, name := range roleNames {
		if err := m.ReconfigureRole(name); err != nil {
			m.logger.Errorf("Failed to reconfigure role %s: %v", name, err)
			lastErr = err
		}
	}

	return lastErr
}

// HealthCheck performs health checks on all running roles
func (m *Manager) HealthCheck() map[string]error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	results := make(map[string]error)
	for name, role := range m.roles {
		results[name] = role.HealthCheck()
	}

	return results
}

// GetRoleStatus returns the status of a specific role
func (m *Manager) GetRoleStatus(name string) (*RoleStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	role, exists := m.roles[name]
	if !exists {
		return nil, fmt.Errorf("role %s is not running", name)
	}

	if baseRole, ok := role.(*BaseRole); ok {
		status := baseRole.GetStatus()
		return &status, nil
	}

	// Return basic status if not a BaseRole
	return &RoleStatus{
		Name:    name,
		Running: true,
	}, nil
}

// GetAllStatuses returns the status of all running roles
func (m *Manager) GetAllStatuses() []RoleStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	statuses := make([]RoleStatus, 0, len(m.roles))
	for name, role := range m.roles {
		if baseRole, ok := role.(*BaseRole); ok {
			statuses = append(statuses, baseRole.GetStatus())
		} else {
			statuses = append(statuses, RoleStatus{
				Name:    name,
				Running: true,
			})
		}
	}

	return statuses
}

// ListRunningRoles returns the names of all running roles
func (m *Manager) ListRunningRoles() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.roles))
	for name := range m.roles {
		names = append(names, name)
	}
	return names
}

// IsRoleRunning checks if a specific role is running
func (m *Manager) IsRoleRunning(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	_, exists := m.roles[name]
	return exists
}

// monitorLeadership monitors leadership changes for a role
func (m *Manager) monitorLeadership(role Role) {
	defer m.wg.Done()

	if m.leaderElector == nil {
		m.logger.Warnf("No leader elector available for role %s", role.Name())
		return
	}

	// Register for leadership notifications
	leaderCh := m.leaderElector.RegisterRoleLeadershipObserver(role.Name())

	for {
		select {
		case isLeader := <-leaderCh:
			m.logger.Infof("Leadership change for role %s: isLeader=%v", role.Name(), isLeader)

			if err := role.OnLeadershipChange(isLeader); err != nil {
				m.logger.Errorf("Failed to handle leadership change for role %s: %v", role.Name(), err)
			}

		case <-m.ctx.Done():
			return
		}
	}
}

// StartHealthCheckLoop starts a periodic health check loop
func (m *Manager) StartHealthCheckLoop(interval time.Duration) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				results := m.HealthCheck()
				for name, err := range results {
					if err != nil {
						m.logger.Warnf("Health check failed for role %s: %v", name, err)
					}
				}

			case <-m.ctx.Done():
				return
			}
		}
	}()
}

// StartReconfigureLoop starts a periodic reconfiguration loop
func (m *Manager) StartReconfigureLoop(interval time.Duration) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := m.ReconfigureAll(); err != nil {
					m.logger.Warnf("Reconfigure loop encountered errors: %v", err)
				}

			case <-m.ctx.Done():
				return
			}
		}
	}()
}

// Shutdown gracefully shuts down all roles
func (m *Manager) Shutdown() error {
	m.logger.Info("Shutting down role manager")

	// Cancel context to stop all goroutines
	m.cancel()

	// Stop all roles
	m.mu.Lock()
	roleNames := make([]string, 0, len(m.roles))
	for name := range m.roles {
		roleNames = append(roleNames, name)
	}
	m.mu.Unlock()

	var lastErr error
	for _, name := range roleNames {
		if err := m.StopRole(name); err != nil {
			m.logger.Errorf("Failed to stop role %s: %v", name, err)
			lastErr = err
		}
	}

	// Wait for all goroutines to finish
	m.wg.Wait()

	m.logger.Info("Role manager shut down")
	return lastErr
}
