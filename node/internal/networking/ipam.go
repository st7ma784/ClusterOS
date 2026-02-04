package networking

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
)

// IPAM handles IP address allocation for the WireGuard mesh
type IPAM struct {
	subnet *net.IPNet
}

// NewIPAM creates a new IPAM instance
func NewIPAM(subnetStr string) (*IPAM, error) {
	_, subnet, err := net.ParseCIDR(subnetStr)
	if err != nil {
		return nil, fmt.Errorf("invalid subnet: %w", err)
	}

	return &IPAM{
		subnet: subnet,
	}, nil
}

// AllocateIP allocates a deterministic IP address for a node based on its ID
// This ensures the same node always gets the same IP address
func (ipam *IPAM) AllocateIP(nodeID string) (net.IP, error) {
	// Hash the node ID to get a deterministic value
	hash := sha256.Sum256([]byte(nodeID))

	// Use the first 4 bytes of the hash for IPv4
	hashValue := binary.BigEndian.Uint32(hash[:4])

	// Get the network address and mask
	networkIP := ipam.subnet.IP.To4()
	if networkIP == nil {
		return nil, fmt.Errorf("only IPv4 subnets are currently supported")
	}

	mask := ipam.subnet.Mask
	ones, bits := mask.Size()

	// Calculate the number of available IPs (excluding network and broadcast)
	availableIPs := uint32(1 << uint(bits-ones))

	// Reserve first and last IP (network and broadcast)
	// Map hash to available IP range (starting from .1)
	ipOffset := (hashValue % (availableIPs - 2)) + 1

	// Convert network IP to uint32
	networkInt := binary.BigEndian.Uint32(networkIP)

	// Calculate target IP
	targetIP := networkInt + ipOffset

	// Convert back to IP
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, targetIP)

	// Verify IP is within subnet
	if !ipam.subnet.Contains(ip) {
		return nil, fmt.Errorf("calculated IP %s is outside subnet %s", ip, ipam.subnet)
	}

	return ip, nil
}

// AllocateIPWithOffset allocates an IP with a specific offset (for testing)
func (ipam *IPAM) AllocateIPWithOffset(offset uint32) (net.IP, error) {
	networkIP := ipam.subnet.IP.To4()
	if networkIP == nil {
		return nil, fmt.Errorf("only IPv4 subnets are currently supported")
	}

	mask := ipam.subnet.Mask
	ones, bits := mask.Size()
	availableIPs := uint32(1 << uint(bits-ones))

	// Validate offset
	if offset == 0 || offset >= availableIPs-1 {
		return nil, fmt.Errorf("invalid offset: must be between 1 and %d", availableIPs-2)
	}

	// Convert network IP to uint32
	networkInt := binary.BigEndian.Uint32(networkIP)

	// Calculate target IP
	targetIP := networkInt + offset

	// Convert back to IP
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, targetIP)

	return ip, nil
}

// GetSubnet returns the configured subnet
func (ipam *IPAM) GetSubnet() *net.IPNet {
	return ipam.subnet
}

// GetSubnetString returns the subnet as a string
func (ipam *IPAM) GetSubnetString() string {
	return ipam.subnet.String()
}

// GetNetworkIP returns the network address
func (ipam *IPAM) GetNetworkIP() net.IP {
	return ipam.subnet.IP
}

// GetMask returns the subnet mask
func (ipam *IPAM) GetMask() net.IPMask {
	return ipam.subnet.Mask
}

// GetCIDRBits returns the CIDR prefix length
func (ipam *IPAM) GetCIDRBits() int {
	ones, _ := ipam.subnet.Mask.Size()
	return ones
}

// GetAvailableIPCount returns the number of available IPs in the subnet
func (ipam *IPAM) GetAvailableIPCount() uint32 {
	ones, bits := ipam.subnet.Mask.Size()
	// Subtract 2 for network and broadcast addresses
	return uint32(1<<uint(bits-ones)) - 2
}

// ValidateIP checks if an IP is within the managed subnet
func (ipam *IPAM) ValidateIP(ip net.IP) bool {
	return ipam.subnet.Contains(ip)
}

// IsReservedIP checks if an IP is reserved (network or broadcast)
func (ipam *IPAM) IsReservedIP(ip net.IP) bool {
	if !ipam.subnet.Contains(ip) {
		return false
	}

	// Check if it's the network address
	if ip.Equal(ipam.subnet.IP) {
		return true
	}

	// Check if it's the broadcast address
	broadcast := make(net.IP, len(ipam.subnet.IP))
	copy(broadcast, ipam.subnet.IP)

	for i := range broadcast {
		broadcast[i] |= ^ipam.subnet.Mask[i]
	}

	if ip.Equal(broadcast) {
		return true
	}

	return false
}

// GetIPRange returns the first and last usable IP in the subnet
func (ipam *IPAM) GetIPRange() (net.IP, net.IP) {
	networkIP := ipam.subnet.IP.To4()
	ones, bits := ipam.subnet.Mask.Size()

	networkInt := binary.BigEndian.Uint32(networkIP)
	maxOffset := uint32(1<<uint(bits-ones)) - 1

	// First usable IP (.1)
	firstIP := make(net.IP, 4)
	binary.BigEndian.PutUint32(firstIP, networkInt+1)

	// Last usable IP (.254 or similar)
	lastIP := make(net.IP, 4)
	binary.BigEndian.PutUint32(lastIP, networkInt+maxOffset-1)

	return firstIP, lastIP
}

// AllocateIPWithConflictCheck allocates an IP for a node, checking for conflicts
// against already assigned IPs. If a conflict is detected, it returns the conflicting
// node ID along with an error.
func (ipam *IPAM) AllocateIPWithConflictCheck(nodeID string, existingIPs map[string]net.IP) (net.IP, string, error) {
	ip, err := ipam.AllocateIP(nodeID)
	if err != nil {
		return nil, "", err
	}

	// Check if this IP is already assigned to another node
	for otherNodeID, otherIP := range existingIPs {
		if otherNodeID != nodeID && ip.Equal(otherIP) {
			return ip, otherNodeID, fmt.Errorf("IP conflict: %s is already assigned to node %s", ip, otherNodeID)
		}
	}

	return ip, "", nil
}

// AllocateRandomIP allocates a random IP within the subnet, avoiding specified IPs
// This is used for conflict resolution when two nodes hash to the same IP
func (ipam *IPAM) AllocateRandomIP(avoidIPs []net.IP, salt string) (net.IP, error) {
	networkIP := ipam.subnet.IP.To4()
	if networkIP == nil {
		return nil, fmt.Errorf("only IPv4 subnets are currently supported")
	}

	mask := ipam.subnet.Mask
	ones, bits := mask.Size()
	availableIPs := uint32(1 << uint(bits-ones))

	// Convert network IP to uint32
	networkInt := binary.BigEndian.Uint32(networkIP)

	// Try different salts to find an available IP
	for attempt := 0; attempt < 1000; attempt++ {
		// Create a unique hash using the salt and attempt number
		hashInput := fmt.Sprintf("%s-%d", salt, attempt)
		hash := sha256.Sum256([]byte(hashInput))
		hashValue := binary.BigEndian.Uint32(hash[:4])

		// Map hash to available IP range (excluding network and broadcast)
		ipOffset := (hashValue % (availableIPs - 2)) + 1
		targetIP := networkInt + ipOffset

		// Convert to IP
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, targetIP)

		// Check if this IP conflicts with any avoided IPs
		conflict := false
		for _, avoidIP := range avoidIPs {
			if ip.Equal(avoidIP) {
				conflict = true
				break
			}
		}

		if !conflict {
			return ip, nil
		}
	}

	return nil, fmt.Errorf("failed to allocate random IP after 1000 attempts")
}

// ParseIP parses an IP address string
func ParseIP(ipStr string) (net.IP, error) {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return nil, fmt.Errorf("invalid IP address: %s", ipStr)
	}
	return ip, nil
}

// IPToString converts an IP to string with CIDR notation
func IPToString(ip net.IP, cidrBits int) string {
	return fmt.Sprintf("%s/%d", ip.String(), cidrBits)
}
