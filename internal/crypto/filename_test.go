package crypto

import (
	"bytes"
	"testing"
)

// ---------------------------------------------------------------------------
// TestEncryptDecryptFilename — Filename encryption
// ---------------------------------------------------------------------------

func TestEncryptDecryptFilename(t *testing.T) {
	// Derive a master key.
	salt, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt failed: %v", err)
	}
	masterKey, err := DeriveMasterKey("testpassphrase", salt)
	if err != nil {
		t.Fatalf("DeriveMasterKey failed: %v", err)
	}

	filename := "documents/report.pdf"

	// Encrypt the filename.
	encrypted, err := EncryptFilename(filename, masterKey)
	if err != nil {
		t.Fatalf("EncryptFilename failed: %v", err)
	}

	// Verify result is a valid base64url string (no padding).
	if !isValidBase64URL(encrypted) {
		t.Errorf("encrypted filename is not valid base64url (no padding): %q", encrypted)
	}

	// Decrypt the result.
	decrypted, err := DecryptFilename(encrypted, masterKey)
	if err != nil {
		t.Fatalf("DecryptFilename failed: %v", err)
	}

	// Verify it matches original filename.
	if decrypted != filename {
		t.Errorf("decrypted filename %q does not match original %q", decrypted, filename)
	}

	// Verify same input produces same output (deterministic).
	encrypted2, err := EncryptFilename(filename, masterKey)
	if err != nil {
		t.Fatalf("EncryptFilename (second) failed: %v", err)
	}
	if encrypted != encrypted2 {
		t.Error("same input should produce same encrypted output (deterministic)")
	}

	// Verify different filenames produce different outputs.
	otherFilename := "documents/other.pdf"
	encryptedOther, err := EncryptFilename(otherFilename, masterKey)
	if err != nil {
		t.Fatalf("EncryptFilename (other) failed: %v", err)
	}
	if encrypted == encryptedOther {
		t.Error("different filenames should produce different encrypted outputs")
	}
}

// ---------------------------------------------------------------------------
// TestEncryptFilenameSpecialChars — Special characters in filenames
// ---------------------------------------------------------------------------

func TestEncryptFilenameSpecialChars(t *testing.T) {
	// Derive a master key.
	salt, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt failed: %v", err)
	}
	masterKey, err := DeriveMasterKey("testpassphrase", salt)
	if err != nil {
		t.Fatalf("DeriveMasterKey failed: %v", err)
	}

	testCases := []struct {
		name     string
		filename string
	}{
		{"spaces", "my documents/quarterly report.pdf"},
		{"unicode", "文档/报告.pdf"},
		{"special chars", "files/data-2024_(final).xlsx"},
		{"emoji", "📁/📄.txt"},
		{"mixed", "hello world/测试 file (1).docx"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			encrypted, err := EncryptFilename(tc.filename, masterKey)
			if err != nil {
				t.Fatalf("EncryptFilename failed for %q: %v", tc.filename, err)
			}

			decrypted, err := DecryptFilename(encrypted, masterKey)
			if err != nil {
				t.Fatalf("DecryptFilename failed for %q: %v", tc.filename, err)
			}

			if decrypted != tc.filename {
				t.Errorf("roundtrip failed: got %q, want %q", decrypted, tc.filename)
			}

			// Verify encrypted result is valid base64url.
			if !isValidBase64URL(encrypted) {
				t.Errorf("encrypted filename is not valid base64url: %q", encrypted)
			}
		})
	}

	// Also verify that different special filenames produce different ciphertexts.
	var encryptedResults []string
	for _, tc := range testCases {
		enc, err := EncryptFilename(tc.filename, masterKey)
		if err != nil {
			t.Fatalf("EncryptFilename failed: %v", err)
		}
		encryptedResults = append(encryptedResults, enc)
	}
	for i := 0; i < len(encryptedResults); i++ {
		for j := i + 1; j < len(encryptedResults); j++ {
			if encryptedResults[i] == encryptedResults[j] {
				t.Errorf("different filenames produced same ciphertext at indices %d and %d", i, j)
			}
		}
	}

	// Suppress unused warning for bytes.
	_ = bytes.Equal(nil, nil)
}
