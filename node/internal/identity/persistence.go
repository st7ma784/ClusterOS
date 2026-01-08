package identity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// DefaultIdentityPath is the default location for identity storage
	DefaultIdentityPath = "/var/lib/cluster-os/identity.json"

	// FilePermissions for the identity file (read/write for owner only)
	FilePermissions = 0600

	// DirPermissions for the identity directory
	DirPermissions = 0700
)

// Save persists the identity to disk
func (i *Identity) Save(path string) error {
	if path == "" {
		path = DefaultIdentityPath
	}

	// Verify identity before saving
	if err := i.Verify(); err != nil {
		return fmt.Errorf("identity verification failed: %w", err)
	}

	// Create directory if it doesn't exist
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, DirPermissions); err != nil {
		return fmt.Errorf("failed to create identity directory %s: %w", dir, err)
	}

	// Marshal identity to JSON
	data, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal identity: %w", err)
	}

	// Write to temporary file first (atomic write)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, FilePermissions); err != nil {
		return fmt.Errorf("failed to write identity to %s: %w", tmpPath, err)
	}

	// Atomically rename temporary file to actual file
	if err := os.Rename(tmpPath, path); err != nil {
		// Clean up temporary file on error
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename identity file: %w", err)
	}

	return nil
}

// Load reads an identity from disk
func Load(path string) (*Identity, error) {
	if path == "" {
		path = DefaultIdentityPath
	}

	// Check if file exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("identity file does not exist: %s", path)
	}

	// Read file
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read identity file %s: %w", path, err)
	}

	// Unmarshal JSON
	var identity Identity
	if err := json.Unmarshal(data, &identity); err != nil {
		return nil, fmt.Errorf("failed to unmarshal identity: %w", err)
	}

	// Verify loaded identity
	if err := identity.Verify(); err != nil {
		return nil, fmt.Errorf("loaded identity is invalid: %w", err)
	}

	return &identity, nil
}

// Exists checks if an identity file exists at the given path
func Exists(path string) bool {
	if path == "" {
		path = DefaultIdentityPath
	}
	_, err := os.Stat(path)
	return err == nil
}

// LoadOrGenerate loads an existing identity or generates a new one
func LoadOrGenerate(path string) (*Identity, bool, error) {
	if path == "" {
		path = DefaultIdentityPath
	}

	// Try to load existing identity
	if Exists(path) {
		identity, err := Load(path)
		if err != nil {
			return nil, false, fmt.Errorf("failed to load existing identity: %w", err)
		}
		return identity, false, nil
	}

	// Generate new identity
	identity, err := Generate()
	if err != nil {
		return nil, false, fmt.Errorf("failed to generate identity: %w", err)
	}

	// Save new identity
	if err := identity.Save(path); err != nil {
		return nil, false, fmt.Errorf("failed to save new identity: %w", err)
	}

	return identity, true, nil
}

// Delete removes an identity file from disk
func Delete(path string) error {
	if path == "" {
		path = DefaultIdentityPath
	}

	if !Exists(path) {
		return nil // Already deleted
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("failed to delete identity file %s: %w", path, err)
	}

	return nil
}
