package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	// MungeKeySize is the size of the munge key in bytes.
	// MUNGE's default is 256 bits (32 bytes); we use that to keep the
	// base64-encoded tag well within Serf's 512-byte MetaMaxSize limit.
	MungeKeySize = 32
	// MungeKeyPath is the standard location for the munge key
	MungeKeyPath = "/etc/munge/munge.key"
)

// MungeKeyManager handles munge key generation and local management.
type MungeKeyManager struct {
	logger *logrus.Logger
}

// NewMungeKeyManager creates a new munge key manager
func NewMungeKeyManager(logger *logrus.Logger) *MungeKeyManager {
	return &MungeKeyManager{logger: logger}
}

// GenerateMungeKey generates a cryptographically secure munge key
func (m *MungeKeyManager) GenerateMungeKey() ([]byte, error) {
	key := make([]byte, MungeKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate munge key: %w", err)
	}
	m.logger.Info("Munge key generated")
	return key, nil
}

// HashMungeKey creates a SHA-256 hash of the munge key
func (m *MungeKeyManager) HashMungeKey(key []byte) string {
	hash := sha256.Sum256(key)
	return hex.EncodeToString(hash[:])
}

// WriteMungeKey writes the munge key to disk with correct permissions
func (m *MungeKeyManager) WriteMungeKey(key []byte) error {
	if err := os.MkdirAll("/etc/munge", 0700); err != nil {
		return fmt.Errorf("create munge dir: %w", err)
	}
	if err := os.WriteFile(MungeKeyPath, key, 0400); err != nil {
		return fmt.Errorf("write munge key: %w", err)
	}
	// Set ownership to munge:munge if possible
	if mungeUser, err := user.Lookup("munge"); err == nil {
		uid, _ := strconv.Atoi(mungeUser.Uid)
		gid, _ := strconv.Atoi(mungeUser.Gid)
		os.Chown(MungeKeyPath, uid, gid)
	}
	m.logger.Infof("Munge key written to %s", MungeKeyPath)
	return nil
}

// ReadMungeKey reads the munge key from disk
func (m *MungeKeyManager) ReadMungeKey() ([]byte, error) {
	key, err := os.ReadFile(MungeKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read munge key: %w", err)
	}
	return key, nil
}

// StartMungeDaemon starts the munge authentication daemon.
// It always stops any existing munged first so that the freshly-written munge
// key is loaded.  Without this, a munged left over from a previous run (or from
// a pre-installed systemd munge.service) would continue using its old key,
// causing cross-node authentication failures when the cluster generates a new key.
func (m *MungeKeyManager) StartMungeDaemon() error {
	if exec.Command("pgrep", "-x", "munged").Run() == nil {
		m.logger.Info("Stopping existing munge daemon to reload key")
		exec.Command("pkill", "-TERM", "-x", "munged").Run()
		time.Sleep(500 * time.Millisecond)
		// Remove stale socket so the new munged starts cleanly.
		os.Remove("/var/run/munge/munge.socket.2")
	}

	mungeDirs := []struct {
		path string
		mode os.FileMode
	}{
		{"/var/run/munge", 0755},
		{"/var/log/munge", 0700},
		{"/var/lib/munge", 0700},
		{"/etc/munge", 0700},
	}
	for _, dir := range mungeDirs {
		os.MkdirAll(dir.path, dir.mode)
	}

	// Set ownership if munge user exists
	if mungeUser, err := user.Lookup("munge"); err == nil {
		uid, _ := strconv.Atoi(mungeUser.Uid)
		gid, _ := strconv.Atoi(mungeUser.Gid)
		for _, dir := range mungeDirs {
			os.Chown(dir.path, uid, gid)
		}
		os.Chown(MungeKeyPath, uid, gid)
	}

	if _, err := os.Stat(MungeKeyPath); os.IsNotExist(err) {
		return fmt.Errorf("munge key not found at %s", MungeKeyPath)
	}

	cmd := exec.Command("munged", "--force")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start munged: %w", err)
	}

	// Wait for socket
	socketPath := "/var/run/munge/munge.socket.2"
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		if _, err := os.Stat(socketPath); err == nil {
			m.logger.Infof("Munge daemon started (PID %d)", cmd.Process.Pid)
			return nil
		}
	}

	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		return fmt.Errorf("munged died: %s", stderr.String())
	}

	m.logger.Infof("Munge daemon started (socket pending)")
	return nil
}

// TestMungeAuthentication tests munge by encoding and decoding a credential
func (m *MungeKeyManager) TestMungeAuthentication() error {
	cmd := exec.Command("munge", "-n")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("munge encode: %w", err)
	}
	cmd = exec.Command("unmunge")
	cmd.Stdin = bytes.NewReader(output)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("munge decode: %w", err)
	}
	return nil
}
