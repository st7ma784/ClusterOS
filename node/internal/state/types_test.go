package state

import (
	"net"
	"testing"
)

func TestGetTailscaleIPs(t *testing.T) {
	cs := NewClusterState()

	// Add nodes with Tailscale IPs
	cs.AddNode(&Node{
		ID:            "node-1",
		Name:          "test-node-1",
		TailscaleIP:   net.ParseIP("100.64.1.1"),
		TailscaleName: "node-1.cluster",
	})
	cs.AddNode(&Node{
		ID:            "node-2",
		Name:          "test-node-2",
		TailscaleIP:   net.ParseIP("100.64.1.2"),
		TailscaleName: "node-2.cluster",
	})
	cs.AddNode(&Node{
		ID:            "node-3",
		Name:          "test-node-3",
		TailscaleIP:   nil, // No Tailscale IP
		TailscaleName: "",
	})

	ips := cs.GetTailscaleIPs()

	if len(ips) != 2 {
		t.Errorf("Expected 2 IPs, got %d", len(ips))
	}

	if !ips["node-1"].Equal(net.ParseIP("100.64.1.1")) {
		t.Errorf("Wrong IP for node-1: %s", ips["node-1"])
	}

	if !ips["node-2"].Equal(net.ParseIP("100.64.1.2")) {
		t.Errorf("Wrong IP for node-2: %s", ips["node-2"])
	}

	if _, ok := ips["node-3"]; ok {
		t.Error("node-3 should not have an IP entry")
	}
}

func TestGetNodeByTailscaleIP(t *testing.T) {
	cs := NewClusterState()

	cs.AddNode(&Node{
		ID:            "node-1",
		Name:          "test-node-1",
		TailscaleIP:   net.ParseIP("100.64.1.1"),
		TailscaleName: "node-1.cluster",
	})
	cs.AddNode(&Node{
		ID:            "node-2",
		Name:          "test-node-2",
		TailscaleIP:   net.ParseIP("100.64.1.2"),
		TailscaleName: "node-2.cluster",
	})

	// Find node-1 by IP
	node, found := cs.GetNodeByTailscaleIP(net.ParseIP("100.64.1.1"))
	if !found {
		t.Error("Should have found node-1")
	}
	if node.ID != "node-1" {
		t.Errorf("Wrong node found: %s", node.ID)
	}

	// Find node-2 by IP
	node, found = cs.GetNodeByTailscaleIP(net.ParseIP("100.64.1.2"))
	if !found {
		t.Error("Should have found node-2")
	}
	if node.ID != "node-2" {
		t.Errorf("Wrong node found: %s", node.ID)
	}

	// Non-existent IP
	_, found = cs.GetNodeByTailscaleIP(net.ParseIP("100.64.1.99"))
	if found {
		t.Error("Should not have found a node for non-existent IP")
	}
}

func TestUpdateNodeTailscaleIP(t *testing.T) {
	cs := NewClusterState()

	cs.AddNode(&Node{
		ID:            "node-1",
		Name:          "test-node-1",
		TailscaleIP:   net.ParseIP("100.64.1.1"),
		TailscaleName: "node-1.cluster",
	})

	// Update the IP
	cs.UpdateNodeTailscaleIP("node-1", net.ParseIP("100.64.2.100"))

	node, found := cs.GetNode("node-1")
	if !found {
		t.Fatal("Node should exist")
	}

	if !node.TailscaleIP.Equal(net.ParseIP("100.64.2.100")) {
		t.Errorf("IP not updated correctly: %s", node.TailscaleIP)
	}

	// Update non-existent node (should not panic)
	cs.UpdateNodeTailscaleIP("non-existent", net.ParseIP("100.64.3.1"))
}

func TestFindIPConflicts(t *testing.T) {
	cs := NewClusterState()

	// Add nodes with unique IPs
	cs.AddNode(&Node{
		ID:            "node-1",
		Name:          "test-node-1",
		TailscaleIP:   net.ParseIP("100.64.1.1"),
		TailscaleName: "node-1.cluster",
	})
	cs.AddNode(&Node{
		ID:            "node-2",
		Name:          "test-node-2",
		TailscaleIP:   net.ParseIP("100.64.1.2"),
		TailscaleName: "node-2.cluster",
	})

	// No conflicts initially
	conflicts := cs.FindIPConflicts()
	if len(conflicts) != 0 {
		t.Errorf("Expected no conflicts, got %d", len(conflicts))
	}

	// Add a conflicting node
	cs.AddNode(&Node{
		ID:            "node-3",
		Name:          "test-node-3",
		TailscaleIP:   net.ParseIP("100.64.1.1"), // Same as node-1
		TailscaleName: "node-3.cluster",
	})

	conflicts = cs.FindIPConflicts()
	if len(conflicts) != 1 {
		t.Errorf("Expected 1 conflict, got %d", len(conflicts))
	}

	// Verify the conflict is between node-1 and node-3
	if len(conflicts) > 0 {
		conflict := conflicts[0]
		hasNode1 := conflict[0] == "node-1" || conflict[1] == "node-1"
		hasNode3 := conflict[0] == "node-3" || conflict[1] == "node-3"
		if !hasNode1 || !hasNode3 {
			t.Errorf("Conflict should be between node-1 and node-3, got %v", conflict)
		}
	}

	// Add another conflict
	cs.AddNode(&Node{
		ID:            "node-4",
		Name:          "test-node-4",
		TailscaleIP:   net.ParseIP("100.64.1.2"), // Same as node-2
		TailscaleName: "node-4.cluster",
	})

	conflicts = cs.FindIPConflicts()
	if len(conflicts) != 2 {
		t.Errorf("Expected 2 conflicts, got %d", len(conflicts))
	}
}

func TestFindIPConflicts_NilIPs(t *testing.T) {
	cs := NewClusterState()

	// Add nodes with nil IPs
	cs.AddNode(&Node{
		ID:            "node-1",
		Name:          "test-node-1",
		TailscaleIP:   nil,
		TailscaleName: "",
	})
	cs.AddNode(&Node{
		ID:            "node-2",
		Name:          "test-node-2",
		TailscaleIP:   nil,
		TailscaleName: "",
	})

	// Nil IPs should not conflict
	conflicts := cs.FindIPConflicts()
	if len(conflicts) != 0 {
		t.Errorf("Expected no conflicts for nil IPs, got %d", len(conflicts))
	}
}
