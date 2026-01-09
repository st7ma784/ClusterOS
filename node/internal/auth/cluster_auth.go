package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// ClusterAuth handles authentication for cluster membership
type ClusterAuth struct {
	authKey []byte
}

// New creates a new cluster authentication handler
func New(authKeyBase64 string) (*ClusterAuth, error) {
	if authKeyBase64 == "" {
		return nil, fmt.Errorf("cluster auth key cannot be empty")
	}

	// Decode the base64 key
	authKey, err := base64.StdEncoding.DecodeString(authKeyBase64)
	if err != nil {
		return nil, fmt.Errorf("failed to decode cluster auth key: %w", err)
	}

	// Validate key length (should be at least 32 bytes for security)
	if len(authKey) < 32 {
		return nil, fmt.Errorf("cluster auth key must be at least 32 bytes, got %d", len(authKey))
	}

	return &ClusterAuth{
		authKey: authKey,
	}, nil
}

// Challenge represents an authentication challenge
type Challenge struct {
	Nonce     string    `json:"nonce"`
	Timestamp time.Time `json:"timestamp"`
	NodeID    string    `json:"node_id"`
}

// Response represents an authentication response
type Response struct {
	Challenge Challenge `json:"challenge"`
	Signature string    `json:"signature"`
}

// GenerateChallenge generates a new authentication challenge
func (ca *ClusterAuth) GenerateChallenge(nodeID string) (*Challenge, error) {
	// Generate a random nonce
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	challenge := &Challenge{
		Nonce:     base64.StdEncoding.EncodeToString(nonceBytes),
		Timestamp: time.Now().UTC(),
		NodeID:    nodeID,
	}

	return challenge, nil
}

// SignChallenge signs a challenge with the cluster auth key
func (ca *ClusterAuth) SignChallenge(challenge *Challenge) (*Response, error) {
	// Serialize the challenge
	challengeBytes, err := json.Marshal(challenge)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal challenge: %w", err)
	}

	// Create HMAC-SHA256 signature
	h := hmac.New(sha256.New, ca.authKey)
	h.Write(challengeBytes)
	signature := h.Sum(nil)

	response := &Response{
		Challenge: *challenge,
		Signature: base64.StdEncoding.EncodeToString(signature),
	}

	return response, nil
}

// VerifyResponse verifies an authentication response
func (ca *ClusterAuth) VerifyResponse(response *Response) error {
	// Check timestamp (must be within 5 minutes)
	age := time.Since(response.Challenge.Timestamp)
	if age > 5*time.Minute {
		return fmt.Errorf("challenge expired (age: %v)", age)
	}
	if age < -1*time.Minute {
		return fmt.Errorf("challenge timestamp is in the future")
	}

	// Serialize the challenge
	challengeBytes, err := json.Marshal(response.Challenge)
	if err != nil {
		return fmt.Errorf("failed to marshal challenge: %w", err)
	}

	// Decode the signature
	signature, err := base64.StdEncoding.DecodeString(response.Signature)
	if err != nil {
		return fmt.Errorf("failed to decode signature: %w", err)
	}

	// Verify HMAC-SHA256 signature
	h := hmac.New(sha256.New, ca.authKey)
	h.Write(challengeBytes)
	expectedSignature := h.Sum(nil)

	if !hmac.Equal(signature, expectedSignature) {
		return fmt.Errorf("invalid signature - node does not have correct cluster auth key")
	}

	return nil
}

// CreateJoinToken creates a join token for a node
// This is a self-signed proof that the node has the cluster key
func (ca *ClusterAuth) CreateJoinToken(nodeID string) (string, error) {
	challenge, err := ca.GenerateChallenge(nodeID)
	if err != nil {
		return "", err
	}

	response, err := ca.SignChallenge(challenge)
	if err != nil {
		return "", err
	}

	// Serialize the response to JSON and base64 encode
	responseBytes, err := json.Marshal(response)
	if err != nil {
		return "", fmt.Errorf("failed to marshal response: %w", err)
	}

	token := base64.StdEncoding.EncodeToString(responseBytes)
	return token, nil
}

// VerifyJoinToken verifies a join token
func (ca *ClusterAuth) VerifyJoinToken(token string) (string, error) {
	// Decode the token
	responseBytes, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("failed to decode token: %w", err)
	}

	// Unmarshal the response
	var response Response
	if err := json.Unmarshal(responseBytes, &response); err != nil {
		return "", fmt.Errorf("failed to unmarshal token: %w", err)
	}

	// Verify the response
	if err := ca.VerifyResponse(&response); err != nil {
		return "", err
	}

	return response.Challenge.NodeID, nil
}

// ValidateClusterKey validates that a cluster key is properly formatted
func ValidateClusterKey(authKeyBase64 string) error {
	if authKeyBase64 == "" {
		return fmt.Errorf("cluster auth key cannot be empty")
	}

	// Decode the base64 key
	authKey, err := base64.StdEncoding.DecodeString(authKeyBase64)
	if err != nil {
		return fmt.Errorf("failed to decode cluster auth key: %w", err)
	}

	// Validate key length
	if len(authKey) < 32 {
		return fmt.Errorf("cluster auth key must be at least 32 bytes, got %d", len(authKey))
	}

	return nil
}
