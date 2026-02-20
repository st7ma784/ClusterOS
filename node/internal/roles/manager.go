package roles

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Manager tracks running roles and runs periodic health checks.
// It does NOT start services — that is done by the daemon phase machine.
type Manager struct {
	roles  map[string]Role
	logger *logrus.Logger
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.RWMutex
}

// NewManager creates a new role manager
func NewManager(logger *logrus.Logger) *Manager {
	if logger == nil {
		logger = logrus.New()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		roles:  make(map[string]Role),
		logger: logger,
		ctx:    ctx,
		cancel: cancel,
	}
}

// RegisterRole adds an already-started role for health monitoring
func (m *Manager) RegisterRole(name string, role Role) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roles[name] = role
}

// UnregisterRole removes a role from health monitoring
func (m *Manager) UnregisterRole(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.roles, name)
}

// IsRoleRunning checks if a specific role is registered
func (m *Manager) IsRoleRunning(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, exists := m.roles[name]
	return exists
}

// HealthCheck performs health checks on all registered roles
func (m *Manager) HealthCheck() map[string]error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	results := make(map[string]error, len(m.roles))
	for name, role := range m.roles {
		results[name] = role.HealthCheck()
	}
	return results
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
				for name, err := range m.HealthCheck() {
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

// Shutdown stops all goroutines and stops all registered roles
func (m *Manager) Shutdown() {
	m.cancel()
	m.mu.Lock()
	roles := make(map[string]Role, len(m.roles))
	for k, v := range m.roles {
		roles[k] = v
	}
	m.mu.Unlock()

	for name, role := range roles {
		if err := role.Stop(context.Background()); err != nil {
			m.logger.Errorf("Failed to stop role %s: %v", name, err)
		}
	}
	m.wg.Wait()
}
