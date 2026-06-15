package crypto

import (
	"bytes"
	"testing"
)

// ---------------------------------------------------------------------------
// TestHashFile — File hashing
// ---------------------------------------------------------------------------

func TestHashFile(t *testing.T) {
	data := []byte("hello world")

	// Hash known input.
	hash := HashFile(data)

	// Verify result is 64-char hex string (SHA-256).
	if len(hash) != 64 {
		t.Errorf("expected hash length 64, got %d", len(hash))
	}

	// Verify same input produces same hash.
	hash2 := HashFile(data)
	if hash != hash2 {
		t.Error("same input should produce same hash")
	}

	// Verify different input produces different hash.
	differentData := []byte("goodbye world")
	hash3 := HashFile(differentData)
	if hash == hash3 {
		t.Error("different input should produce different hash")
	}
}

// ---------------------------------------------------------------------------
// TestHashFilePath — Path hashing
// ---------------------------------------------------------------------------

func TestHashFilePath(t *testing.T) {
	path := "docs/report.pdf"

	// Hash a path string.
	hash := HashFilePath(path)

	// Verify result is 32 bytes (raw SHA-256).
	if len(hash) != 32 {
		t.Errorf("expected hash length 32, got %d", len(hash))
	}

	// Verify same input produces same hash.
	hash2 := HashFilePath(path)
	if !bytes.Equal(hash, hash2) {
		t.Error("same path should produce same hash")
	}

	// Verify different path produces different hash.
	hash3 := HashFilePath("other/file.txt")
	if bytes.Equal(hash, hash3) {
		t.Error("different paths should produce different hashes")
	}
}

// ---------------------------------------------------------------------------
// TestHashPassphrase — Passphrase hashing
// ---------------------------------------------------------------------------

func TestHashPassphrase(t *testing.T) {
	salt, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt failed: %v", err)
	}

	// Hash passphrase with salt.
	hash := HashPassphrase("testpassphrase", salt)

	// Verify result is 64-char hex string.
	if len(hash) != 64 {
		t.Errorf("expected hash length 64, got %d", len(hash))
	}

	// Verify same inputs produce same hash.
	hash2 := HashPassphrase("testpassphrase", salt)
	if hash != hash2 {
		t.Error("same inputs should produce same passphrase hash")
	}

	// Verify different salt produces different hash.
	salt2, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt (second) failed: %v", err)
	}
	hash3 := HashPassphrase("testpassphrase", salt2)
	if hash == hash3 {
		t.Error("different salts should produce different passphrase hashes")
	}
}
