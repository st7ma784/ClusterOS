package discovery

import (
	"testing"
)

func TestShortNodeID(t *testing.T) {
	tests := []struct {
		name     string
		fullID   string
		wantLen  int
	}{
		{
			name:    "short ID unchanged",
			fullID:  "abc123",
			wantLen: 6,
		},
		{
			name:    "exactly 16 chars unchanged",
			fullID:  "1234567890123456",
			wantLen: 16,
		},
		{
			name:    "long ID shortened",
			fullID:  "3TtKH8TmVRHk8xnXmQqjg9xX9rvGDW4mM6LdJ8vPnkb5",
			wantLen: 16,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShortNodeID(tt.fullID)
			if len(got) != tt.wantLen {
				t.Errorf("ShortNodeID(%q) = %q (len %d), want len %d", tt.fullID, got, len(got), tt.wantLen)
			}
		})
	}

	// Test uniqueness - different full IDs should produce different short IDs
	id1 := ShortNodeID("3TtKH8TmVRHk8xnXmQqjg9xX9rvGDW4mM6LdJ8vPnkb5")
	id2 := ShortNodeID("4UuLI9UnWSil9yoYnRrkh0yY0swHEX5nN7MeK9wQolc6")
	if id1 == id2 {
		t.Errorf("Different full IDs produced same short ID: %q", id1)
	}

	// Test determinism - same full ID should always produce same short ID
	for i := 0; i < 10; i++ {
		got := ShortNodeID("3TtKH8TmVRHk8xnXmQqjg9xX9rvGDW4mM6LdJ8vPnkb5")
		if got != id1 {
			t.Errorf("ShortNodeID not deterministic: got %q, want %q", got, id1)
		}
	}
}
