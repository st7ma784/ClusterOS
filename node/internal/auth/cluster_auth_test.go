package auth

import (
	"encoding/base64"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		keyB64  string
		wantErr bool
	}{
		{
			name:    "valid key",
			keyB64:  base64.StdEncoding.EncodeToString(make([]byte, 32)),
			wantErr: false,
		},
		{
			name:    "empty key",
			keyB64:  "",
			wantErr: true,
		},
		{
			name:    "invalid base64",
			keyB64:  "not-valid-base64!@#$",
			wantErr: true,
		},
		{
			name:    "key too short",
			keyB64:  base64.StdEncoding.EncodeToString(make([]byte, 16)),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.keyB64)
			if (err != nil) != tt.wantErr {
				t.Errorf("New() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestChallengeResponse(t *testing.T) {
	keyB64 := base64.StdEncoding.EncodeToString(make([]byte, 32))
	ca, err := New(keyB64)
	if err != nil {
		t.Fatalf("Failed to create ClusterAuth: %v", err)
	}

	nodeID := "test-node-123"

	// Generate a challenge
	challenge, err := ca.GenerateChallenge(nodeID)
	if err != nil {
		t.Fatalf("Failed to generate challenge: %v", err)
	}

	if challenge.NodeID != nodeID {
		t.Errorf("Challenge NodeID = %v, want %v", challenge.NodeID, nodeID)
	}

	// Sign the challenge
	response, err := ca.SignChallenge(challenge)
	if err != nil {
		t.Fatalf("Failed to sign challenge: %v", err)
	}

	// Verify the response
	if err := ca.VerifyResponse(response); err != nil {
		t.Errorf("Failed to verify valid response: %v", err)
	}
}

func TestVerifyResponse_Expired(t *testing.T) {
	keyB64 := base64.StdEncoding.EncodeToString(make([]byte, 32))
	ca, err := New(keyB64)
	if err != nil {
		t.Fatalf("Failed to create ClusterAuth: %v", err)
	}

	// Create an expired challenge
	challenge := &Challenge{
		Nonce:     "test-nonce",
		Timestamp: time.Now().Add(-10 * time.Minute), // 10 minutes ago
		NodeID:    "test-node",
	}

	response, err := ca.SignChallenge(challenge)
	if err != nil {
		t.Fatalf("Failed to sign challenge: %v", err)
	}

	// Verify should fail due to expiration
	err = ca.VerifyResponse(response)
	if err == nil {
		t.Error("Expected error for expired challenge, got nil")
	}
}

func TestVerifyResponse_FutureTimestamp(t *testing.T) {
	keyB64 := base64.StdEncoding.EncodeToString(make([]byte, 32))
	ca, err := New(keyB64)
	if err != nil {
		t.Fatalf("Failed to create ClusterAuth: %v", err)
	}

	// Create a challenge with future timestamp
	challenge := &Challenge{
		Nonce:     "test-nonce",
		Timestamp: time.Now().Add(2 * time.Minute), // 2 minutes in future
		NodeID:    "test-node",
	}

	response, err := ca.SignChallenge(challenge)
	if err != nil {
		t.Fatalf("Failed to sign challenge: %v", err)
	}

	// Verify should fail due to future timestamp
	err = ca.VerifyResponse(response)
	if err == nil {
		t.Error("Expected error for future timestamp, got nil")
	}
}

func TestVerifyResponse_WrongKey(t *testing.T) {
	// Create two different keys (exactly 32 bytes each)
	key1B64 := base64.StdEncoding.EncodeToString([]byte("key1-is-32-bytes-long-testkey!!1"))
	key2B64 := base64.StdEncoding.EncodeToString([]byte("key2-is-32-bytes-long-testkey!!2"))

	ca1, err := New(key1B64)
	if err != nil {
		t.Fatalf("Failed to create ClusterAuth 1: %v", err)
	}

	ca2, err := New(key2B64)
	if err != nil {
		t.Fatalf("Failed to create ClusterAuth 2: %v", err)
	}

	// Generate challenge and sign with key1
	challenge, err := ca1.GenerateChallenge("test-node")
	if err != nil {
		t.Fatalf("Failed to generate challenge: %v", err)
	}

	response, err := ca1.SignChallenge(challenge)
	if err != nil {
		t.Fatalf("Failed to sign challenge: %v", err)
	}

	// Try to verify with key2 (should fail)
	err = ca2.VerifyResponse(response)
	if err == nil {
		t.Error("Expected error when verifying with wrong key, got nil")
	}
}

func TestJoinToken(t *testing.T) {
	keyB64 := base64.StdEncoding.EncodeToString(make([]byte, 32))
	ca, err := New(keyB64)
	if err != nil {
		t.Fatalf("Failed to create ClusterAuth: %v", err)
	}

	nodeID := "test-node-123"

	// Create a join token
	token, err := ca.CreateJoinToken(nodeID)
	if err != nil {
		t.Fatalf("Failed to create join token: %v", err)
	}

	if token == "" {
		t.Error("Join token is empty")
	}

	// Verify the join token
	verifiedNodeID, err := ca.VerifyJoinToken(token)
	if err != nil {
		t.Errorf("Failed to verify join token: %v", err)
	}

	if verifiedNodeID != nodeID {
		t.Errorf("Verified NodeID = %v, want %v", verifiedNodeID, nodeID)
	}
}

func TestJoinToken_WrongKey(t *testing.T) {
	// Create two different keys (exactly 32 bytes each)
	key1B64 := base64.StdEncoding.EncodeToString([]byte("key1-is-32-bytes-long-testkey!!1"))
	key2B64 := base64.StdEncoding.EncodeToString([]byte("key2-is-32-bytes-long-testkey!!2"))

	ca1, err := New(key1B64)
	if err != nil {
		t.Fatalf("Failed to create ClusterAuth 1: %v", err)
	}

	ca2, err := New(key2B64)
	if err != nil {
		t.Fatalf("Failed to create ClusterAuth 2: %v", err)
	}

	// Create token with key1
	token, err := ca1.CreateJoinToken("test-node")
	if err != nil {
		t.Fatalf("Failed to create join token: %v", err)
	}

	// Try to verify with key2 (should fail)
	_, err = ca2.VerifyJoinToken(token)
	if err == nil {
		t.Error("Expected error when verifying token with wrong key, got nil")
	}
}

func TestValidateClusterKey(t *testing.T) {
	tests := []struct {
		name    string
		keyB64  string
		wantErr bool
	}{
		{
			name:    "valid 32-byte key",
			keyB64:  base64.StdEncoding.EncodeToString(make([]byte, 32)),
			wantErr: false,
		},
		{
			name:    "valid 64-byte key",
			keyB64:  base64.StdEncoding.EncodeToString(make([]byte, 64)),
			wantErr: false,
		},
		{
			name:    "empty key",
			keyB64:  "",
			wantErr: true,
		},
		{
			name:    "invalid base64",
			keyB64:  "not-valid-base64!@#$",
			wantErr: true,
		},
		{
			name:    "key too short",
			keyB64:  base64.StdEncoding.EncodeToString(make([]byte, 16)),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateClusterKey(tt.keyB64)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateClusterKey() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
