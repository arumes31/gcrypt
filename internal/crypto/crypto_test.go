package crypto

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/daniel/gcrypt/internal/models"
)

// ---------------------------------------------------------------------------
// TestDeriveMasterKey — Argon2id key derivation
// ---------------------------------------------------------------------------

func TestDeriveMasterKey(t *testing.T) {
	salt, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt failed: %v", err)
	}

	// Derive key from "testpassphrase" with a known salt.
	key, err := DeriveMasterKey("testpassphrase", salt)
	if err != nil {
		t.Fatalf("DeriveMasterKey failed: %v", err)
	}

	// Verify key is 32 bytes.
	if len(key) != 32 {
		t.Errorf("expected key length 32, got %d", len(key))
	}

	// Verify same inputs produce same key (deterministic).
	key2, err := DeriveMasterKey("testpassphrase", salt)
	if err != nil {
		t.Fatalf("DeriveMasterKey (second call) failed: %v", err)
	}
	if !bytes.Equal(key, key2) {
		t.Error("same inputs should produce the same key")
	}

	// Verify different passphrases produce different keys.
	key3, err := DeriveMasterKey("differentpassphrase", salt)
	if err != nil {
		t.Fatalf("DeriveMasterKey (different passphrase) failed: %v", err)
	}
	if bytes.Equal(key, key3) {
		t.Error("different passphrases should produce different keys")
	}

	// Verify different salts produce different keys.
	salt2, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt (second) failed: %v", err)
	}
	key4, err := DeriveMasterKey("testpassphrase", salt2)
	if err != nil {
		t.Fatalf("DeriveMasterKey (different salt) failed: %v", err)
	}
	if bytes.Equal(key, key4) {
		t.Error("different salts should produce different keys")
	}
}

// ---------------------------------------------------------------------------
// TestGenerateSalt — Salt generation
// ---------------------------------------------------------------------------

func TestGenerateSalt(t *testing.T) {
	salt1, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt failed: %v", err)
	}
	salt2, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt (second) failed: %v", err)
	}

	// Verify each is 16 bytes.
	if len(salt1) != 16 {
		t.Errorf("expected salt length 16, got %d", len(salt1))
	}
	if len(salt2) != 16 {
		t.Errorf("expected salt length 16, got %d", len(salt2))
	}

	// Verify they are different (random).
	if bytes.Equal(salt1, salt2) {
		t.Error("two generated salts should not be equal")
	}
}

// ---------------------------------------------------------------------------
// TestGenerateDEK — DEK generation
// ---------------------------------------------------------------------------

func TestGenerateDEK(t *testing.T) {
	dek1, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK failed: %v", err)
	}
	dek2, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK (second) failed: %v", err)
	}

	// Verify each is 32 bytes.
	if len(dek1) != 32 {
		t.Errorf("expected DEK length 32, got %d", len(dek1))
	}
	if len(dek2) != 32 {
		t.Errorf("expected DEK length 32, got %d", len(dek2))
	}

	// Verify they are different (random).
	if bytes.Equal(dek1, dek2) {
		t.Error("two generated DEKs should not be equal")
	}
}

// ---------------------------------------------------------------------------
// TestEncryptDecryptDEK — DEK encryption/decryption
// ---------------------------------------------------------------------------

func TestEncryptDecryptDEK(t *testing.T) {
	// Generate a master key.
	masterKey, err := GenerateDEK() // 32 random bytes works as a master key
	if err != nil {
		t.Fatalf("GenerateDEK (master key) failed: %v", err)
	}

	// Generate a DEK.
	dek, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK failed: %v", err)
	}

	// Encrypt the DEK with master key.
	encryptedDEK, dekNonce, err := EncryptDEK(dek, masterKey)
	if err != nil {
		t.Fatalf("EncryptDEK failed: %v", err)
	}

	// Decrypt the encrypted DEK.
	decryptedDEK, err := DecryptDEK(encryptedDEK, dekNonce, masterKey)
	if err != nil {
		t.Fatalf("DecryptDEK failed: %v", err)
	}

	// Verify decrypted DEK matches original.
	if !bytes.Equal(dek, decryptedDEK) {
		t.Error("decrypted DEK does not match original")
	}

	// Verify wrong master key fails to decrypt.
	wrongKey, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK (wrong key) failed: %v", err)
	}
	_, err = DecryptDEK(encryptedDEK, dekNonce, wrongKey)
	if err == nil {
		t.Error("expected error when decrypting with wrong master key, got nil")
	}
}

// ---------------------------------------------------------------------------
// TestEncryptDecryptFile — File content encryption/decryption
// ---------------------------------------------------------------------------

func TestEncryptDecryptFile(t *testing.T) {
	// Generate a DEK.
	dek, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK failed: %v", err)
	}

	plaintext := []byte("hello world")
	filePath := "test/file.txt"

	// Encrypt "hello world" with DEK and path.
	header, ciphertext, err := EncryptFile(plaintext, dek, filePath)
	if err != nil {
		t.Fatalf("EncryptFile failed: %v", err)
	}

	// Verify header is populated correctly.
	if string(header.Magic[:]) != models.Magic {
		t.Errorf("expected magic %q, got %q", models.Magic, string(header.Magic[:]))
	}
	if header.Version != models.CurrentVersion {
		t.Errorf("expected version %d, got %d", models.CurrentVersion, header.Version)
	}

	// Decrypt with same DEK and path.
	decrypted, err := DecryptFile(ciphertext, header, dek, filePath)
	if err != nil {
		t.Fatalf("DecryptFile failed: %v", err)
	}

	// Verify plaintext matches.
	if !bytes.Equal(plaintext, decrypted) {
		t.Errorf("decrypted plaintext does not match: got %q, want %q", decrypted, plaintext)
	}

	// Verify wrong path (different AAD) fails to decrypt.
	_, err = DecryptFile(ciphertext, header, dek, "wrong/path.txt")
	if err == nil {
		t.Error("expected error when decrypting with wrong path (different AAD), got nil")
	}
}

// ---------------------------------------------------------------------------
// TestEncryptDecryptBlob — Full blob encryption/decryption
// ---------------------------------------------------------------------------

func TestEncryptDecryptBlob(t *testing.T) {
	// Derive master key from passphrase.
	salt, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt failed: %v", err)
	}
	masterKey, err := DeriveMasterKey("testpassphrase", salt)
	if err != nil {
		t.Fatalf("DeriveMasterKey failed: %v", err)
	}

	// Encrypt a larger plaintext (1KB of data).
	plaintext := bytes.Repeat([]byte("A"), 1024)
	filePath := "docs/report.pdf"

	blob, err := EncryptBlob(plaintext, masterKey, filePath)
	if err != nil {
		t.Fatalf("EncryptBlob failed: %v", err)
	}

	// Verify blob starts with magic bytes "GCRYPT".
	if len(blob) < 6 || string(blob[:6]) != "GCRYPT" {
		t.Errorf("blob does not start with magic bytes %q", "GCRYPT")
	}

	// Verify blob is at least 80 bytes (header size).
	if len(blob) < models.HeaderSize {
		t.Errorf("blob is too short: %d bytes, minimum %d", len(blob), models.HeaderSize)
	}

	// Decrypt the blob with same master key and path.
	decrypted, err := DecryptBlob(blob, masterKey, filePath)
	if err != nil {
		t.Fatalf("DecryptBlob failed: %v", err)
	}

	// Verify plaintext matches original.
	if !bytes.Equal(plaintext, decrypted) {
		t.Error("decrypted plaintext does not match original")
	}

	// Verify wrong passphrase fails.
	wrongKey, err := DeriveMasterKey("wrongpassphrase", salt)
	if err != nil {
		t.Fatalf("DeriveMasterKey (wrong passphrase) failed: %v", err)
	}
	_, err = DecryptBlob(blob, wrongKey, filePath)
	if err == nil {
		t.Error("expected error when decrypting with wrong passphrase, got nil")
	}
}

// ---------------------------------------------------------------------------
// TestSerializeDeserializeHeader — Header serialization
// ---------------------------------------------------------------------------

func TestSerializeDeserializeHeader(t *testing.T) {
	// Create an EncryptedFileHeader with known values.
	header := &models.EncryptedFileHeader{}
	copy(header.Magic[:], models.Magic)
	header.Version = models.CurrentVersion
	// Fill remaining fields with test data.
	for i := 0; i < len(header.EncryptedDEK); i++ {
		header.EncryptedDEK[i] = byte(i)
	}
	for i := 0; i < len(header.DEKNonce); i++ {
		header.DEKNonce[i] = byte(i + 100)
	}
	for i := 0; i < len(header.ContentNonce); i++ {
		header.ContentNonce[i] = byte(i + 200)
	}

	// Serialize it.
	serialized, err := SerializeHeader(header)
	if err != nil {
		t.Fatalf("SerializeHeader failed: %v", err)
	}

	// Verify result is 80 bytes.
	if len(serialized) != models.HeaderSize {
		t.Errorf("expected serialized header size %d, got %d", models.HeaderSize, len(serialized))
	}

	// Deserialize it.
	deserialized, err := DeserializeHeader(serialized)
	if err != nil {
		t.Fatalf("DeserializeHeader failed: %v", err)
	}

	// Verify all fields match original.
	if string(deserialized.Magic[:]) != string(header.Magic[:]) {
		t.Error("magic bytes do not match after roundtrip")
	}
	if deserialized.Version != header.Version {
		t.Error("version does not match after roundtrip")
	}
	if !bytes.Equal(deserialized.EncryptedDEK[:], header.EncryptedDEK[:]) {
		t.Error("encrypted DEK does not match after roundtrip")
	}
	if !bytes.Equal(deserialized.DEKNonce[:], header.DEKNonce[:]) {
		t.Error("DEK nonce does not match after roundtrip")
	}
	if !bytes.Equal(deserialized.ContentNonce[:], header.ContentNonce[:]) {
		t.Error("content nonce does not match after roundtrip")
	}

	// Verify deserializing with wrong magic bytes fails.
	badMagic := make([]byte, models.HeaderSize)
	copy(badMagic, serialized)
	copy(badMagic[0:6], "BADXXX")
	_, err = DeserializeHeader(badMagic)
	if err == nil {
		t.Error("expected error when deserializing with wrong magic bytes, got nil")
	}

	// Verify deserializing with wrong version fails.
	badVersion := make([]byte, models.HeaderSize)
	copy(badVersion, serialized)
	badVersion[6] = 0xFF // invalid version
	badVersion[7] = 0xFF
	_, err = DeserializeHeader(badVersion)
	if err == nil {
		t.Error("expected error when deserializing with wrong version, got nil")
	}
}

// ---------------------------------------------------------------------------
// TestWipeBytes — Secure memory wiping
// ---------------------------------------------------------------------------

func TestWipeBytes(t *testing.T) {
	// Create a byte slice with non-zero data.
	data := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

	// Call WipeBytes.
	WipeBytes(data)

	// Verify all bytes are zero.
	for i, b := range data {
		if b != 0 {
			t.Errorf("byte at index %d is %d, expected 0", i, b)
		}
	}
}

// Helper: isValidBase64URL checks if a string is valid base64url without padding.
func isValidBase64URL(s string) bool {
	if strings.Contains(s, "=") {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil
}
