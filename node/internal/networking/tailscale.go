package networking

import (
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"

	"github.com/sirupsen/logrus"
)

// TailscaleManager manages Tailscale network integration
// Instead of running our own WireGuard mesh, we use Tailscale's existing mesh
type TailscaleManager struct {
	localIP net.IP
	logger  *logrus.Logger
}

// TailscaleConfig contains configuration for Tailscale integration
type TailscaleConfig struct {
	Logger *logrus.Logger
}

// NewTailscaleManager creates a new Tailscale manager
// It detects the local Tailscale IP and uses it for cluster communication
func NewTailscaleManager(cfg *TailscaleConfig) (*TailscaleManager, error) {
	if cfg.Logger == nil {
		cfg.Logger = logrus.New()
	}

	// Detect Tailscale IP
	tailscaleIP, err := DetectTailscaleIP()
	if err != nil {
		return nil, fmt.Errorf("failed to detect Tailscale IP: %w", err)
	}

	cfg.Logger.Infof("Using Tailscale IP for cluster communication: %s", tailscaleIP)

	return &TailscaleManager{
		localIP: tailscaleIP,
		logger:  cfg.Logger,
	}, nil
}

// GetLocalIP returns the local Tailscale IP
func (tm *TailscaleManager) GetLocalIP() net.IP {
	return tm.localIP
}

// DetectTailscaleIP finds the Tailscale IP address on this machine
// Tailscale uses the CGNAT range 100.64.0.0/10 (100.64.0.0 - 100.127.255.255)
func DetectTailscaleIP() (net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("failed to list interfaces: %w", err)
	}

	// First, try to find a Tailscale-named interface
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}

		// Check for Tailscale interface names
		isTailscaleIface := strings.HasPrefix(iface.Name, "tailscale") ||
			strings.HasPrefix(iface.Name, "ts") ||
			iface.Name == "utun" // macOS

		if !isTailscaleIface {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok {
				ip4 := ipNet.IP.To4()
				if ip4 != nil && isTailscaleIP(ip4) {
					return ip4, nil
				}
			}
		}
	}

	// Fallback: scan all interfaces for Tailscale CGNAT range
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		// Skip WireGuard interfaces (our old mesh)
		if strings.HasPrefix(iface.Name, "wg") {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok {
				ip4 := ipNet.IP.To4()
				if ip4 != nil && isTailscaleIP(ip4) {
					return ip4, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("no Tailscale IP found - ensure Tailscale is running and connected")
}

// isTailscaleIP checks if an IP is in the Tailscale CGNAT range (100.64.0.0/10)
func isTailscaleIP(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	// 100.64.0.0/10 = 100.64.0.0 - 100.127.255.255
	// First octet must be 100, second octet must be 64-127 (0x40-0x7F)
	return ip4[0] == 100 && (ip4[1]&0xC0) == 64
}

// IsTailscaleAvailable checks if Tailscale is running and has an IP
func IsTailscaleAvailable() bool {
	ip, err := DetectTailscaleIP()
	return err == nil && ip != nil
}

// TailscalePeer represents a peer on the Tailscale network
type TailscalePeer struct {
	ID       string   `json:"ID"`
	HostName string   `json:"HostName"`
	DNSName  string   `json:"DNSName"`
	OS       string   `json:"OS"`
	TailAddr string   `json:"TailscaleIPs"`
	Online   bool     `json:"Online"`
	Tags     []string `json:"Tags"`
}

// tailscaleStatus represents the JSON output of `tailscale status --json`
type tailscaleStatus struct {
	Self *tailscalePeerStatus            `json:"Self"`
	Peer map[string]*tailscalePeerStatus `json:"Peer"`
}

type tailscalePeerStatus struct {
	ID           string   `json:"ID"`
	HostName     string   `json:"HostName"`
	DNSName      string   `json:"DNSName"`
	OS           string   `json:"OS"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	Online       bool     `json:"Online"`
	Tags         []string `json:"Tags"`
}

// DiscoverClusterPeers finds other ClusterOS nodes on the same Tailscale network
// It uses `tailscale status --json` to get the list of peers and filters for
// nodes with the "tag:cluster-node" tag or cluster-* hostname patterns
func DiscoverClusterPeers(serfPort int) ([]string, error) {
	// Run tailscale status --json
	cmd := exec.Command("tailscale", "status", "--json")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get tailscale status: %w", err)
	}

	var status tailscaleStatus
	if err := json.Unmarshal(output, &status); err != nil {
		return nil, fmt.Errorf("failed to parse tailscale status: %w", err)
	}

	var peers []string
	selfID := ""
	if status.Self != nil {
		selfID = status.Self.ID
	}

	for _, peer := range status.Peer {
		// Skip ourselves
		if peer.ID == selfID {
			continue
		}

		// Skip offline peers
		if !peer.Online {
			continue
		}

		// Check if this is a cluster node
		isClusterNode := false

		// Check for cluster-node tag
		for _, tag := range peer.Tags {
			if tag == "tag:cluster-node" || strings.Contains(tag, "cluster") {
				isClusterNode = true
				break
			}
		}

		// Also check hostname patterns that suggest a cluster node
		hostname := strings.ToLower(peer.HostName)
		if strings.HasPrefix(hostname, "cluster-") ||
			strings.HasPrefix(hostname, "node-") ||
			strings.Contains(hostname, "clusteros") {
			isClusterNode = true
		}

		if !isClusterNode {
			continue
		}

		// Get the Tailscale IP
		if len(peer.TailscaleIPs) > 0 {
			// Use the first IPv4 address
			for _, ip := range peer.TailscaleIPs {
				if !strings.Contains(ip, ":") { // Skip IPv6
					peerAddr := fmt.Sprintf("%s:%d", ip, serfPort)
					peers = append(peers, peerAddr)
					break
				}
			}
		}
	}

	return peers, nil
}

// DiscoverAllTailscalePeers returns all online Tailscale peers (not just cluster nodes)
// This is useful when tag-based filtering isn't set up yet
func DiscoverAllTailscalePeers(serfPort int) ([]string, error) {
	cmd := exec.Command("tailscale", "status", "--json")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get tailscale status: %w", err)
	}

	var status tailscaleStatus
	if err := json.Unmarshal(output, &status); err != nil {
		return nil, fmt.Errorf("failed to parse tailscale status: %w", err)
	}

	var peers []string
	selfID := ""
	if status.Self != nil {
		selfID = status.Self.ID
	}

	for _, peer := range status.Peer {
		if peer.ID == selfID {
			continue
		}
		if !peer.Online {
			continue
		}
		if len(peer.TailscaleIPs) > 0 {
			for _, ip := range peer.TailscaleIPs {
				if !strings.Contains(ip, ":") {
					peerAddr := fmt.Sprintf("%s:%d", ip, serfPort)
					peers = append(peers, peerAddr)
					break
				}
			}
		}
	}

	return peers, nil
}
