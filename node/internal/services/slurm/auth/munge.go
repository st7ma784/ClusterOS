package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
	"time"

	"github.com/cluster-os/node/internal/state"
	"github.com/sirupsen/logrus"
)

const (
	// MungeKeySize is the size of the munge key in bytes (1024 bits)
	MungeKeySize = 128
	// MungeKeyPath is the standard location for the munge key
	MungeKeyPath = "/etc/munge/munge.key"
	// MungeKeyHashTag is the Serf tag for munge key hash
	MungeKeyHashTag = "munge_key_hash"
	// MungeKeyAvailableTag is the Serf tag indicating key is available
	MungeKeyAvailableTag = "munge_key_available"
)

// MungeKeyManager handles munge key generation and distribution
type MungeKeyManager struct {
	logger *logrus.Logger
}

// NewMungeKeyManager creates a new munge key manager
func NewMungeKeyManager(logger *logrus.Logger) *MungeKeyManager {
	return &MungeKeyManager{
		logger: logger,
	}
}

// GenerateMungeKey generates a cryptographically secure munge key
func (m *MungeKeyManager) GenerateMungeKey() ([]byte, error) {
	m.logger.Info("Generating new munge key")

	key := make([]byte, MungeKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("failed to generate random key: %w", err)
	}

	m.logger.Info("Munge key generated successfully")
	return key, nil
}

// HashMungeKey creates a SHA-256 hash of the munge key for verification
func (m *MungeKeyManager) HashMungeKey(key []byte) string {
	hash := sha256.Sum256(key)
	return hex.EncodeToString(hash[:])
}

// VerifyMungeKey verifies that a key matches the expected hash
func (m *MungeKeyManager) VerifyMungeKey(key []byte, expectedHash string) bool {
	actualHash := m.HashMungeKey(key)
	verified := actualHash == expectedHash

	if verified {
		m.logger.Info("Munge key verification successful")
	} else {
		m.logger.Warn("Munge key verification failed")
	}

	return verified
}

// WriteMungeKey writes the munge key to disk with correct permissions
func (m *MungeKeyManager) WriteMungeKey(key []byte) error {
	m.logger.Infof("Writing munge key to %s", MungeKeyPath)

	// Create /etc/munge directory if it doesn't exist
	mungeDir := "/etc/munge"
	if err := os.MkdirAll(mungeDir, 0700); err != nil {
		return fmt.Errorf("failed to create munge directory: %w", err)
	}

	// Write key to file with restricted permissions
	if err := os.WriteFile(MungeKeyPath, key, 0400); err != nil {
		return fmt.Errorf("failed to write munge key: %w", err)
	}

	// Set ownership to munge:munge
	if err := m.setMungeKeyOwnership(); err != nil {
		m.logger.Warnf("Failed to set munge key ownership: %v (continuing anyway)", err)
		// Don't fail if we can't set ownership - might be in container without munge user
	}

	m.logger.Info("Munge key written successfully")
	return nil
}

// ReadMungeKey reads the munge key from disk
func (m *MungeKeyManager) ReadMungeKey() ([]byte, error) {
	key, err := os.ReadFile(MungeKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read munge key: %w", err)
	}

	m.logger.Infof("Read munge key from %s (%d bytes)", MungeKeyPath, len(key))
	return key, nil
}

// setMungeKeyOwnership sets the munge key file ownership to munge:munge
func (m *MungeKeyManager) setMungeKeyOwnership() error {
	// Look up munge user
	mungeUser, err := user.Lookup("munge")
	if err != nil {
		return fmt.Errorf("munge user not found: %w", err)
	}

	uid, err := strconv.Atoi(mungeUser.Uid)
	if err != nil {
		return fmt.Errorf("invalid munge UID: %w", err)
	}

	gid, err := strconv.Atoi(mungeUser.Gid)
	if err != nil {
		return fmt.Errorf("invalid munge GID: %w", err)
	}

	if err := os.Chown(MungeKeyPath, uid, gid); err != nil {
		return fmt.Errorf("failed to chown munge key: %w", err)
	}

	return nil
}

// StoreInRaft stores the munge key in Raft consensus state
// This is the preferred method as it replicates to all nodes
func (m *MungeKeyManager) StoreInRaft(raftApplier RaftMungeKeyApplier, mungeKey []byte) error {
	m.logger.Info("Storing munge key in Raft consensus state")

	keyHash := m.HashMungeKey(mungeKey)

	// Apply via Raft (this replicates to all nodes)
	if err := raftApplier.ApplySetMungeKey(mungeKey, keyHash); err != nil {
		return fmt.Errorf("failed to apply munge key via Raft: %w", err)
	}

	m.logger.Info("Munge key stored in Raft successfully")
	return nil
}

// FetchFromRaft fetches the munge key from Raft state
// All nodes (including future controllers) can fetch from here
func (m *MungeKeyManager) FetchFromRaft(clusterState *state.ClusterState) ([]byte, string, error) {
	m.logger.Info("Fetching munge key from Raft consensus state")

	key, hash, err := clusterState.GetMungeKey()
	if err != nil {
		return nil, "", fmt.Errorf("failed to get munge key from cluster state: %w", err)
	}

	m.logger.Infof("Fetched munge key from Raft (hash: %s)", hash[:16]+"...")
	return key, hash, nil
}

// PublishMungeKeyHash publishes the munge key hash to cluster state via Serf tags
// DEPRECATED: Use StoreInRaft instead for Raft-based replication
func (m *MungeKeyManager) PublishMungeKeyHash(clusterState *state.ClusterState, nodeID string, keyHash string) error {
	m.logger.Infof("Publishing munge key hash to cluster state for node %s", nodeID)

	// Get current node
	node, ok := clusterState.GetNode(nodeID)
	if !ok {
		return fmt.Errorf("node %s not found in cluster state", nodeID)
	}

	// Update tags with munge key hash
	if node.Tags == nil {
		node.Tags = make(map[string]string)
	}
	node.Tags[MungeKeyHashTag] = keyHash
	node.Tags[MungeKeyAvailableTag] = "true"

	clusterState.UpdateNodeTags(nodeID, node.Tags)

	m.logger.Info("Munge key hash published successfully")
	return nil
}

// RaftMungeKeyApplier is an interface for applying munge keys via Raft
type RaftMungeKeyApplier interface {
	ApplySetMungeKey(mungeKey []byte, mungeKeyHash string) error
}

// FetchMungeKeyFromController retrieves the munge key from the controller node
// For Docker testing, this uses the shared volume approach
// For production, this would use a secure RPC or file distribution mechanism
func (m *MungeKeyManager) FetchMungeKeyFromController(clusterState *state.ClusterState) ([]byte, string, error) {
	m.logger.Info("Fetching munge key from controller")

	// Get the SLURM controller leader
	controllerNode, ok := clusterState.GetLeaderNode("slurm-controller")
	if !ok {
		return nil, "", fmt.Errorf("no SLURM controller found in cluster")
	}

	m.logger.Infof("Found SLURM controller: %s", controllerNode.ID)

	// Get the published key hash
	keyHash, ok := controllerNode.Tags[MungeKeyHashTag]
	if !ok {
		return nil, "", fmt.Errorf("controller has not published munge key hash")
	}

	// For Docker testing with shared volumes, the key will be available at the standard path
	// In production, this would fetch from the controller via secure RPC

	// Check if key is already available (shared volume scenario)
	if _, err := os.Stat(MungeKeyPath); err == nil {
		m.logger.Info("Munge key found at standard path (shared volume)")
		key, err := m.ReadMungeKey()
		if err != nil {
			return nil, "", err
		}
		return key, keyHash, nil
	}

	// If not available, this would be where we'd implement secure key transfer
	// For now, return an error indicating the key needs to be distributed
	return nil, keyHash, fmt.Errorf("munge key not yet available - waiting for distribution")
}

// EncodeMungeKey encodes a munge key to base64 for transfer
func (m *MungeKeyManager) EncodeMungeKey(key []byte) string {
	return base64.StdEncoding.EncodeToString(key)
}

// DecodeMungeKey decodes a base64-encoded munge key
func (m *MungeKeyManager) DecodeMungeKey(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("failed to decode munge key: %w", err)
	}
	return key, nil
}

// StartMungeDaemon starts the munge authentication daemon
func (m *MungeKeyManager) StartMungeDaemon() error {
	m.logger.Info("Starting munge daemon")

	// Check if munged is already running
	if err := exec.Command("pgrep", "-x", "munged").Run(); err == nil {
		m.logger.Info("Munge daemon already running")
		return nil
	}

	// Ensure /var/run/munge exists with correct permissions
	if err := os.MkdirAll("/var/run/munge", 0755); err != nil {
		return fmt.Errorf("failed to create /var/run/munge: %w", err)
	}

	// Ensure /var/log/munge exists
	if err := os.MkdirAll("/var/log/munge", 0700); err != nil {
		return fmt.Errorf("failed to create /var/log/munge: %w", err)
	}

	// Set ownership of directories to munge user if possible
	if mungeUser, err := user.Lookup("munge"); err == nil {
		uid, _ := strconv.Atoi(mungeUser.Uid)
		gid, _ := strconv.Atoi(mungeUser.Gid)
		os.Chown("/var/run/munge", uid, gid)
		os.Chown("/var/log/munge", uid, gid)
		os.Chown("/var/lib/munge", uid, gid)
		os.Chmod("/var/lib/munge", 0700)
	}

	// Start munged with --force to bypass strict security checks in containers
	// This is safe because we're running in an isolated container environment
	cmd := exec.Command("munged", "--force")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start munged: %w", err)
	}

	// Wait a moment for the socket to be created
	time.Sleep(500 * time.Millisecond)

	m.logger.Infof("Munge daemon started with PID %d", cmd.Process.Pid)
	return nil
}

// TestMungeAuthentication tests munge authentication by encoding and decoding a credential
func (m *MungeKeyManager) TestMungeAuthentication() error {
	m.logger.Info("Testing munge authentication")

	// Try to encode a test credential
	cmd := exec.Command("munge", "-n")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("munge encode failed: %w", err)
	}

	// Try to decode the credential
	cmd = exec.Command("unmunge")
	cmd.Stdin = bytes.NewReader(output)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("munge decode failed: %w", err)
	}

	m.logger.Info("Munge authentication test successful")
	return nil
}
