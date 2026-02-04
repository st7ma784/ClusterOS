package networking

import (
	"net"
	"testing"
)

func TestAllocateIP(t *testing.T) {
	ipam, err := NewIPAM("10.42.0.0/16")
	if err != nil {
		t.Fatalf("Failed to create IPAM: %v", err)
	}

	// Test deterministic allocation
	ip1, err := ipam.AllocateIP("node-abc123")
	if err != nil {
		t.Fatalf("Failed to allocate IP: %v", err)
	}

	// Same node ID should get same IP
	ip2, err := ipam.AllocateIP("node-abc123")
	if err != nil {
		t.Fatalf("Failed to allocate IP: %v", err)
	}

	if !ip1.Equal(ip2) {
		t.Errorf("Same node ID should get same IP, got %s and %s", ip1, ip2)
	}

	// Different node ID should get different IP (with high probability)
	ip3, err := ipam.AllocateIP("node-xyz789")
	if err != nil {
		t.Fatalf("Failed to allocate IP: %v", err)
	}

	if ip1.Equal(ip3) {
		t.Logf("Warning: Different node IDs got same IP (unlikely but possible): %s", ip1)
	}
}

func TestAllocateIPWithConflictCheck(t *testing.T) {
	ipam, err := NewIPAM("10.42.0.0/16")
	if err != nil {
		t.Fatalf("Failed to create IPAM: %v", err)
	}

	// Allocate first IP
	ip1, err := ipam.AllocateIP("node-1")
	if err != nil {
		t.Fatalf("Failed to allocate IP for node-1: %v", err)
	}

	// Create existing IPs map
	existingIPs := map[string]net.IP{
		"node-1": ip1,
	}

	// Allocate for a different node - should succeed
	ip2, conflictID, err := ipam.AllocateIPWithConflictCheck("node-2", existingIPs)
	if err != nil {
		t.Fatalf("Failed to allocate IP for node-2: %v", err)
	}
	if conflictID != "" {
		t.Errorf("Unexpected conflict with node: %s", conflictID)
	}
	if ip2 == nil {
		t.Error("Expected non-nil IP")
	}

	// Add ip2 to existing IPs
	existingIPs["node-2"] = ip2

	// Try to allocate for node-1 again with conflict check - should be fine (same node)
	ip1b, conflictID, err := ipam.AllocateIPWithConflictCheck("node-1", existingIPs)
	if err != nil {
		t.Fatalf("Unexpected error for same node: %v", err)
	}
	if conflictID != "" {
		t.Errorf("Unexpected conflict for same node: %s", conflictID)
	}
	if !ip1b.Equal(ip1) {
		t.Errorf("Same node should get same IP")
	}
}

func TestAllocateRandomIP(t *testing.T) {
	ipam, err := NewIPAM("10.42.0.0/16")
	if err != nil {
		t.Fatalf("Failed to create IPAM: %v", err)
	}

	// Allocate some IPs to avoid
	avoidIPs := []net.IP{
		net.ParseIP("10.42.1.1"),
		net.ParseIP("10.42.1.2"),
		net.ParseIP("10.42.1.3"),
	}

	// Allocate a random IP
	randomIP, err := ipam.AllocateRandomIP(avoidIPs, "test-salt-1")
	if err != nil {
		t.Fatalf("Failed to allocate random IP: %v", err)
	}

	// Verify it's in the subnet
	if !ipam.ValidateIP(randomIP) {
		t.Errorf("Random IP %s is not in subnet", randomIP)
	}

	// Verify it's not in the avoid list
	for _, avoidIP := range avoidIPs {
		if randomIP.Equal(avoidIP) {
			t.Errorf("Random IP %s should not be in avoid list", randomIP)
		}
	}

	// Allocate another random IP with different salt - should be different
	randomIP2, err := ipam.AllocateRandomIP(avoidIPs, "test-salt-2")
	if err != nil {
		t.Fatalf("Failed to allocate second random IP: %v", err)
	}

	// They might be the same by chance, but log it
	if randomIP.Equal(randomIP2) {
		t.Logf("Note: Two random IPs with different salts were the same (unlikely): %s", randomIP)
	}
}

func TestIPConflictScenario(t *testing.T) {
	// Simulate the cluster merge scenario
	ipam, err := NewIPAM("10.42.0.0/16")
	if err != nil {
		t.Fatalf("Failed to create IPAM: %v", err)
	}

	// Cluster 1 has node-A
	ipA, err := ipam.AllocateIP("node-A")
	if err != nil {
		t.Fatalf("Failed to allocate IP for node-A: %v", err)
	}

	// Cluster 2 has node-B (simulating a node that hashes to same IP)
	// We'll manually create this conflict scenario
	existingIPs := map[string]net.IP{
		"node-A": ipA,
		"node-B": ipA, // Simulated conflict - both have same IP
	}

	// Now we need to resolve - allocate new random IPs for both
	allIPs := []net.IP{}
	for _, ip := range existingIPs {
		allIPs = append(allIPs, ip)
	}

	// Node-A gets new IP
	newIPforA, err := ipam.AllocateRandomIP(allIPs, "node-A-conflict-resolution")
	if err != nil {
		t.Fatalf("Failed to allocate new IP for node-A: %v", err)
	}

	// Update allIPs to include the new IP
	allIPs = append(allIPs, newIPforA)

	// Node-B gets new IP
	newIPforB, err := ipam.AllocateRandomIP(allIPs, "node-B-conflict-resolution")
	if err != nil {
		t.Fatalf("Failed to allocate new IP for node-B: %v", err)
	}

	// Verify all three IPs are different
	if newIPforA.Equal(ipA) {
		t.Error("Node-A's new IP should be different from conflicting IP")
	}
	if newIPforB.Equal(ipA) {
		t.Error("Node-B's new IP should be different from conflicting IP")
	}
	if newIPforA.Equal(newIPforB) {
		t.Error("Node-A and Node-B should have different new IPs")
	}

	t.Logf("Conflict resolution successful:")
	t.Logf("  Original conflicting IP: %s", ipA)
	t.Logf("  Node-A new IP: %s", newIPforA)
	t.Logf("  Node-B new IP: %s", newIPforB)
}
