package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"github.com/mr-tron/base58"
	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/curve25519"
)

// Identity represents a node's cryptographic identity
type Identity struct {
	PrivateKey ed25519.PrivateKey `json:"private_key"`
	PublicKey  ed25519.PublicKey  `json:"public_key"`
	NodeID     string             `json:"node_id"`
}

// Generate creates a new cryptographic identity using Ed25519
func Generate() (*Identity, error) {
	// Generate Ed25519 keypair
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate Ed25519 keypair: %w", err)
	}

	// Derive node ID from public key using base58 encoding
	nodeID := base58.Encode(pubKey)

	return &Identity{
		PrivateKey: privKey,
		PublicKey:  pubKey,
		NodeID:     nodeID,
	}, nil
}

// DeriveWireGuardKey derives a WireGuard private key from the identity
// This ensures the WireGuard key is deterministic and tied to node identity
func (i *Identity) DeriveWireGuardKey() ([]byte, error) {
	// Use BLAKE2b to derive a 32-byte WireGuard private key from the Ed25519 private key
	hash, err := blake2b.New256(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create BLAKE2b hash: %w", err)
	}

	// Hash the private key with a domain separator
	hash.Write([]byte("wireguard-key-derivation"))
	hash.Write(i.PrivateKey.Seed())

	return hash.Sum(nil), nil
}

// WireGuardKeyString returns the WireGuard private key as a base64 string
func (i *Identity) WireGuardKeyString() (string, error) {
	key, err := i.DeriveWireGuardKey()
	if err != nil {
		return "", err
	}
	// WireGuard keys are typically base64 encoded, but we'll return hex for now
	// and convert in the WireGuard module
	return hex.EncodeToString(key), nil
}

// WireGuardPublicKey returns the WireGuard public key derived from the private key
// The key is clamped per Curve25519 requirements and returned as base64
func (i *Identity) WireGuardPublicKey() (string, error) {
	key, err := i.DeriveWireGuardKey()
	if err != nil {
		return "", err
	}

	// Clamp the private key for Curve25519
	key[0] &= 248
	key[31] &= 127
	key[31] |= 64

	// Derive public key using scalar base multiplication
	var publicKey [32]byte
	var privKey [32]byte
	copy(privKey[:], key)

	curve25519.ScalarBaseMult(&publicKey, &privKey)

	return base64.StdEncoding.EncodeToString(publicKey[:]), nil
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

	// Verify that the public key matches the private key
	derivedPubKey := i.PrivateKey.Public().(ed25519.PublicKey)
	if !derivedPubKey.Equal(i.PublicKey) {
		return fmt.Errorf("public key does not match private key")
	}

	// Verify that the node ID matches the public key
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
