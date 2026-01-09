package networking

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"text/template"
	"time"

	"github.com/cluster-os/node/internal/identity"
	"github.com/cluster-os/node/internal/state"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/curve25519"
)

// WireGuardManager manages the WireGuard mesh network
type WireGuardManager struct {
	identity      *identity.Identity
	ipam          *IPAM
	clusterState  *state.ClusterState
	localIP       net.IP
	interfaceName string
	listenPort    int
	configPath    string
	logger        *logrus.Logger
}

// WireGuardConfig contains WireGuard configuration
type WireGuardConfig struct {
	Identity      *identity.Identity
	IPAM          *IPAM
	ClusterState  *state.ClusterState
	InterfaceName string
	ListenPort    int
	ConfigPath    string
	Logger        *logrus.Logger
}

// Peer represents a WireGuard peer
type Peer struct {
	PublicKey           string
	Endpoint            string
	AllowedIPs          []string
	PersistentKeepalive int
}

// NewWireGuardManager creates a new WireGuard manager
func NewWireGuardManager(cfg *WireGuardConfig) (*WireGuardManager, error) {
	if cfg.Logger == nil {
		cfg.Logger = logrus.New()
	}

	if cfg.ConfigPath == "" {
		cfg.ConfigPath = fmt.Sprintf("/etc/wireguard/%s.conf", cfg.InterfaceName)
	}

	// Allocate IP for local node
	localIP, err := cfg.IPAM.AllocateIP(cfg.Identity.NodeID)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate IP: %w", err)
	}

	cfg.Logger.Infof("Allocated WireGuard IP: %s", localIP)

	wgm := &WireGuardManager{
		identity:      cfg.Identity,
		ipam:          cfg.IPAM,
		clusterState:  cfg.ClusterState,
		localIP:       localIP,
		interfaceName: cfg.InterfaceName,
		listenPort:    cfg.ListenPort,
		configPath:    cfg.ConfigPath,
		logger:        cfg.Logger,
	}

	return wgm, nil
}

// GetLocalIP returns the local WireGuard IP
func (wgm *WireGuardManager) GetLocalIP() net.IP {
	return wgm.localIP
}

// GenerateConfig generates the WireGuard configuration
func (wgm *WireGuardManager) GenerateConfig() (string, error) {
	// Get WireGuard private key from identity
	wgPrivateKey, err := wgm.deriveWireGuardPrivateKey()
	if err != nil {
		return "", fmt.Errorf("failed to derive WireGuard private key: %w", err)
	}

	// Get all peers from cluster state
	peers := wgm.buildPeerList()

	// Template data
	data := struct {
		PrivateKey string
		Address    string
		ListenPort int
		Peers      []Peer
	}{
		PrivateKey: base64.StdEncoding.EncodeToString(wgPrivateKey),
		Address:    fmt.Sprintf("%s/%d", wgm.localIP.String(), wgm.ipam.GetCIDRBits()),
		ListenPort: wgm.listenPort,
		Peers:      peers,
	}

	// Parse template
	tmpl, err := template.New("wireguard").Parse(configTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %w", err)
	}

	// Execute template
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return buf.String(), nil
}

// buildPeerList builds the list of WireGuard peers from cluster state
func (wgm *WireGuardManager) buildPeerList() []Peer {
	peers := []Peer{}

	// Get all alive nodes except ourselves
	for _, node := range wgm.clusterState.GetAliveNodes() {
		if node.ID == wgm.identity.NodeID {
			continue // Skip ourselves
		}

		// Allocate IP for peer
		peerIP, err := wgm.ipam.AllocateIP(node.ID)
		if err != nil {
			wgm.logger.Warnf("Failed to allocate IP for node %s: %v", node.ID, err)
			continue
		}

		// Get peer's public key from cluster state (exchanged via Serf tags)
		peerPublicKey := node.WireGuardPubKey
		if peerPublicKey == "" {
			wgm.logger.Warnf("No WireGuard public key for node %s, skipping peer", node.Name)
			continue
		}

		// Create peer
		peer := Peer{
			PublicKey:           peerPublicKey,
			Endpoint:            fmt.Sprintf("%s:%d", node.Address, wgm.listenPort),
			AllowedIPs:          []string{fmt.Sprintf("%s/32", peerIP.String())},
			PersistentKeepalive: 25, // NAT traversal
		}

		peers = append(peers, peer)
	}

	return peers
}

// deriveWireGuardPrivateKey derives a WireGuard private key from the identity
func (wgm *WireGuardManager) deriveWireGuardPrivateKey() ([]byte, error) {
	key, err := wgm.identity.DeriveWireGuardKey()
	if err != nil {
		return nil, err
	}

	// Ensure key is clamped for Curve25519
	key[0] &= 248
	key[31] &= 127
	key[31] |= 64

	return key, nil
}

// deriveWireGuardPublicKey derives a WireGuard public key from the private key
func (wgm *WireGuardManager) deriveWireGuardPublicKey(privateKey []byte) []byte {
	var publicKey [32]byte
	var privKey [32]byte
	copy(privKey[:], privateKey)

	curve25519.ScalarBaseMult(&publicKey, &privKey)

	return publicKey[:]
}

// ApplyConfig applies the WireGuard configuration
func (wgm *WireGuardManager) ApplyConfig() error {
	wgm.logger.Info("Applying WireGuard configuration")

	// Generate config
	config, err := wgm.GenerateConfig()
	if err != nil {
		return fmt.Errorf("failed to generate config: %w", err)
	}

	// Write config to temporary file first
	tempPath := wgm.configPath + ".tmp"
	if err := os.WriteFile(tempPath, []byte(config), 0600); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	// Atomically move to final location
	if err := os.Rename(tempPath, wgm.configPath); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("failed to move config: %w", err)
	}

	// Apply configuration using wg-quick
	if err := wgm.bringUpInterface(); err != nil {
		return fmt.Errorf("failed to bring up interface: %w", err)
	}

	wgm.logger.Info("WireGuard configuration applied successfully")
	return nil
}

// bringUpInterface brings up the WireGuard interface with retry logic
func (wgm *WireGuardManager) bringUpInterface() error {
	// Check if interface already exists
	if wgm.isInterfaceUp() {
		wgm.logger.Info("Interface already up, reloading configuration")
		return wgm.reloadInterface()
	}

	// Retry logic for bringing up the interface
	const maxRetries = 5
	const retryDelay = 2 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		wgm.logger.Infof("Bringing up interface %s (attempt %d/%d)", wgm.interfaceName, attempt, maxRetries)

		// Bring up interface using wg-quick
		cmd := exec.Command("wg-quick", "up", wgm.interfaceName)
		output, err := cmd.CombinedOutput()
		if err == nil {
			wgm.logger.Infof("Interface %s brought up successfully", wgm.interfaceName)
			
			// Wait a bit for the interface to stabilize
			time.Sleep(500 * time.Millisecond)
			
			// Verify interface is actually up
			if wgm.isInterfaceUp() {
				wgm.logger.Infof("Interface %s verified to be operational", wgm.interfaceName)
				return nil
			}
			
			wgm.logger.Warnf("Interface %s brought up but verification failed, will retry", wgm.interfaceName)
		} else {
			wgm.logger.Warnf("wg-quick up failed (attempt %d/%d): %v\nOutput: %s", attempt, maxRetries, err, string(output))
		}

		// Don't sleep after the last attempt
		if attempt < maxRetries {
			wgm.logger.Infof("Waiting %v before retry...", retryDelay)
			time.Sleep(retryDelay)
		}
	}

	return fmt.Errorf("failed to bring up interface %s after %d attempts", wgm.interfaceName, maxRetries)
}

// reloadInterface reloads the WireGuard configuration
func (wgm *WireGuardManager) reloadInterface() error {
	// Sync configuration using wg syncconf
	cmd := exec.Command("wg", "syncconf", wgm.interfaceName, wgm.configPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("wg syncconf failed: %w\nOutput: %s", err, string(output))
	}

	wgm.logger.Infof("Interface %s configuration synced", wgm.interfaceName)
	return nil
}

// isInterfaceUp checks if the WireGuard interface is up
func (wgm *WireGuardManager) isInterfaceUp() bool {
	cmd := exec.Command("ip", "link", "show", wgm.interfaceName)
	err := cmd.Run()
	return err == nil
}

// BringDownInterface brings down the WireGuard interface
func (wgm *WireGuardManager) BringDownInterface() error {
	if !wgm.isInterfaceUp() {
		wgm.logger.Info("Interface already down")
		return nil
	}

	cmd := exec.Command("wg-quick", "down", wgm.interfaceName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("wg-quick down failed: %w\nOutput: %s", err, string(output))
	}

	wgm.logger.Infof("Interface %s brought down", wgm.interfaceName)
	return nil
}

// GetInterfaceStatus returns the status of the WireGuard interface
func (wgm *WireGuardManager) GetInterfaceStatus() (string, error) {
	if !wgm.isInterfaceUp() {
		return "down", nil
	}

	cmd := exec.Command("wg", "show", wgm.interfaceName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("wg show failed: %w", err)
	}

	return string(output), nil
}

// StartMaintenance starts a maintenance loop that periodically reconfigures WireGuard
func (wgm *WireGuardManager) StartMaintenance(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		if err := wgm.ApplyConfig(); err != nil {
			wgm.logger.Errorf("Failed to apply config during maintenance: %v", err)
		}
	}
}

// Shutdown gracefully shuts down the WireGuard manager
func (wgm *WireGuardManager) Shutdown() error {
	wgm.logger.Info("Shutting down WireGuard manager")
	return wgm.BringDownInterface()
}

// configTemplate is the WireGuard configuration template
const configTemplate = `[Interface]
PrivateKey = {{.PrivateKey}}
Address = {{.Address}}
ListenPort = {{.ListenPort}}

{{range .Peers}}
[Peer]
PublicKey = {{.PublicKey}}
Endpoint = {{.Endpoint}}
AllowedIPs = {{range $i, $ip := .AllowedIPs}}{{if $i}}, {{end}}{{$ip}}{{end}}
PersistentKeepalive = {{.PersistentKeepalive}}

{{end}}`
