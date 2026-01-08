package identity

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerate(t *testing.T) {
	identity, err := Generate()
	if err != nil {
		t.Fatalf("Generate() failed: %v", err)
	}

	if identity == nil {
		t.Fatal("Generate() returned nil identity")
	}

	if len(identity.PrivateKey) != ed25519.PrivateKeySize {
		t.Errorf("Invalid private key size: expected %d, got %d",
			ed25519.PrivateKeySize, len(identity.PrivateKey))
	}

	if len(identity.PublicKey) != ed25519.PublicKeySize {
		t.Errorf("Invalid public key size: expected %d, got %d",
			ed25519.PublicKeySize, len(identity.PublicKey))
	}

	if identity.NodeID == "" {
		t.Error("NodeID is empty")
	}
}

func TestVerify(t *testing.T) {
	identity, err := Generate()
	if err != nil {
		t.Fatalf("Generate() failed: %v", err)
	}

	if err := identity.Verify(); err != nil {
		t.Errorf("Verify() failed for valid identity: %v", err)
	}

	// Test with corrupted identity
	badIdentity := *identity
	badIdentity.NodeID = "invalid"
	if err := badIdentity.Verify(); err == nil {
		t.Error("Verify() should fail for invalid NodeID")
	}
}

func TestDeriveWireGuardKey(t *testing.T) {
	identity, err := Generate()
	if err != nil {
		t.Fatalf("Generate() failed: %v", err)
	}

	wgKey, err := identity.DeriveWireGuardKey()
	if err != nil {
		t.Fatalf("DeriveWireGuardKey() failed: %v", err)
	}

	if len(wgKey) != 32 {
		t.Errorf("WireGuard key should be 32 bytes, got %d", len(wgKey))
	}

	// Verify derivation is deterministic
	wgKey2, err := identity.DeriveWireGuardKey()
	if err != nil {
		t.Fatalf("DeriveWireGuardKey() failed on second call: %v", err)
	}

	if string(wgKey) != string(wgKey2) {
		t.Error("WireGuard key derivation is not deterministic")
	}
}

func TestSignAndVerify(t *testing.T) {
	identity, err := Generate()
	if err != nil {
		t.Fatalf("Generate() failed: %v", err)
	}

	message := []byte("test message")
	signature := identity.Sign(message)

	if len(signature) != ed25519.SignatureSize {
		t.Errorf("Invalid signature size: expected %d, got %d",
			ed25519.SignatureSize, len(signature))
	}

	// Verify with correct public key
	if !VerifySignature(identity.PublicKey, message, signature) {
		t.Error("Signature verification failed for valid signature")
	}

	// Verify with wrong message
	wrongMessage := []byte("wrong message")
	if VerifySignature(identity.PublicKey, wrongMessage, signature) {
		t.Error("Signature verification should fail for wrong message")
	}

	// Verify with wrong public key
	otherIdentity, _ := Generate()
	if VerifySignature(otherIdentity.PublicKey, message, signature) {
		t.Error("Signature verification should fail for wrong public key")
	}
}

func TestSaveAndLoad(t *testing.T) {
	// Create temporary directory
	tempDir, err := os.MkdirTemp("", "identity-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	identityPath := filepath.Join(tempDir, "identity.json")

	// Generate and save identity
	originalIdentity, err := Generate()
	if err != nil {
		t.Fatalf("Generate() failed: %v", err)
	}

	if err := originalIdentity.Save(identityPath); err != nil {
		t.Fatalf("Save() failed: %v", err)
	}

	// Check file exists
	if !Exists(identityPath) {
		t.Error("Identity file does not exist after Save()")
	}

	// Load identity
	loadedIdentity, err := Load(identityPath)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Compare identities
	if loadedIdentity.NodeID != originalIdentity.NodeID {
		t.Error("Loaded NodeID does not match original")
	}

	if string(loadedIdentity.PrivateKey) != string(originalIdentity.PrivateKey) {
		t.Error("Loaded PrivateKey does not match original")
	}

	if string(loadedIdentity.PublicKey) != string(originalIdentity.PublicKey) {
		t.Error("Loaded PublicKey does not match original")
	}
}

func TestLoadOrGenerate(t *testing.T) {
	// Create temporary directory
	tempDir, err := os.MkdirTemp("", "identity-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	identityPath := filepath.Join(tempDir, "identity.json")

	// First call should generate new identity
	identity1, wasGenerated, err := LoadOrGenerate(identityPath)
	if err != nil {
		t.Fatalf("LoadOrGenerate() failed: %v", err)
	}

	if !wasGenerated {
		t.Error("LoadOrGenerate() should report identity was generated")
	}

	if identity1 == nil {
		t.Fatal("LoadOrGenerate() returned nil identity")
	}

	// Second call should load existing identity
	identity2, wasGenerated, err := LoadOrGenerate(identityPath)
	if err != nil {
		t.Fatalf("LoadOrGenerate() failed on second call: %v", err)
	}

	if wasGenerated {
		t.Error("LoadOrGenerate() should report identity was loaded, not generated")
	}

	// Should be the same identity
	if identity1.NodeID != identity2.NodeID {
		t.Error("LoadOrGenerate() returned different identities")
	}
}

func TestDelete(t *testing.T) {
	// Create temporary directory
	tempDir, err := os.MkdirTemp("", "identity-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	identityPath := filepath.Join(tempDir, "identity.json")

	// Generate and save identity
	identity, err := Generate()
	if err != nil {
		t.Fatalf("Generate() failed: %v", err)
	}

	if err := identity.Save(identityPath); err != nil {
		t.Fatalf("Save() failed: %v", err)
	}

	// Delete identity
	if err := Delete(identityPath); err != nil {
		t.Errorf("Delete() failed: %v", err)
	}

	// Verify file is deleted
	if Exists(identityPath) {
		t.Error("Identity file still exists after Delete()")
	}

	// Delete should be idempotent
	if err := Delete(identityPath); err != nil {
		t.Errorf("Delete() failed on non-existent file: %v", err)
	}
}
