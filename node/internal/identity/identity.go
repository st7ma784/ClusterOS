package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"

	"github.com/mr-tron/base58"
)

// Identity represents a node's cryptographic identity
type Identity struct {
	PrivateKey ed25519.PrivateKey `json:"private_key"`
	PublicKey  ed25519.PublicKey  `json:"public_key"`
	NodeID     string             `json:"node_id"`
}

// Generate creates a new cryptographic identity using Ed25519
func Generate() (*Identity, error) {
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate Ed25519 keypair: %w", err)
	}

	nodeID := base58.Encode(pubKey)

	return &Identity{
		PrivateKey: privKey,
		PublicKey:  pubKey,
		NodeID:     nodeID,
	}, nil
}

// Verify checks if the identity is valid
func (i *Identity) Verify() error {
	if len(i.PrivateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("invalid private key size: expected %d, got %d",
			ed25519.PrivateKeySize, len(i.PrivateKey))
	}

	if len(i.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid public key size: expected %d, got %d",
			ed25519.PublicKeySize, len(i.PublicKey))
	}

	derivedPubKey := i.PrivateKey.Public().(ed25519.PublicKey)
	if !derivedPubKey.Equal(i.PublicKey) {
		return fmt.Errorf("public key does not match private key")
	}

	expectedNodeID := base58.Encode(i.PublicKey)
	if i.NodeID != expectedNodeID {
		return fmt.Errorf("node ID does not match public key")
	}

	return nil
}

// Sign signs a message with the identity's private key
func (i *Identity) Sign(message []byte) []byte {
	return ed25519.Sign(i.PrivateKey, message)
}

// VerifySignature verifies a signature from another node
func VerifySignature(publicKey ed25519.PublicKey, message, signature []byte) bool {
	return ed25519.Verify(publicKey, message, signature)
}

// String returns a string representation of the identity
func (i *Identity) String() string {
	return fmt.Sprintf("Identity{NodeID: %s, PublicKey: %x...}",
		i.NodeID, i.PublicKey[:8])
}
