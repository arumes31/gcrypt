package crypto

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/arumes31/gcrypt/internal/models"
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
// TestEncryptDecryptStream — Streaming round-trip across sizes
// ---------------------------------------------------------------------------

func TestEncryptDecryptStream(t *testing.T) {
	salt, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt failed: %v", err)
	}
	masterKey, err := DeriveMasterKey("testpassphrase", salt)
	if err != nil {
		t.Fatalf("DeriveMasterKey failed: %v", err)
	}

	filePath := "docs/report.pdf"
	sizes := []int{0, 1, 100, ChunkSize - 1, ChunkSize, ChunkSize + 1, 3*ChunkSize + 7}
	for _, size := range sizes {
		plaintext := bytes.Repeat([]byte("A"), size)

		var ct bytes.Buffer
		if err := EncryptStream(bytes.NewReader(plaintext), &ct, masterKey, filePath); err != nil {
			t.Fatalf("EncryptStream(size=%d) failed: %v", size, err)
		}

		var pt bytes.Buffer
		if err := DecryptStream(bytes.NewReader(ct.Bytes()), &pt, masterKey, filePath); err != nil {
			t.Fatalf("DecryptStream(size=%d) failed: %v", size, err)
		}
		if !bytes.Equal(plaintext, pt.Bytes()) {
			t.Errorf("round-trip mismatch at size=%d", size)
		}

		// Wrong path (different AAD) must fail.
		var bad bytes.Buffer
		if err := DecryptStream(bytes.NewReader(ct.Bytes()), &bad, masterKey, "wrong/path"); err == nil {
			t.Errorf("expected decrypt failure with wrong path at size=%d", size)
		}

		// Wrong key must fail.
		wrongKey, _ := DeriveMasterKey("wrong", salt)
		var bad2 bytes.Buffer
		if err := DecryptStream(bytes.NewReader(ct.Bytes()), &bad2, wrongKey, filePath); err == nil {
			t.Errorf("expected decrypt failure with wrong key at size=%d", size)
		}
	}
}

// ---------------------------------------------------------------------------
// TestDecryptStreamTamperDetected — v2 final-chunk authentication
// ---------------------------------------------------------------------------

func TestDecryptStreamTamperDetected(t *testing.T) {
	salt, _ := GenerateSalt()
	masterKey, _ := DeriveMasterKey("pw", salt)
	filePath := "a/b/c.bin"

	// Exact multiple of ChunkSize so every chunk is a full EncryptedChunkSize.
	plaintext := bytes.Repeat([]byte("X"), 3*ChunkSize)
	var ct bytes.Buffer
	if err := EncryptStream(bytes.NewReader(plaintext), &ct, masterKey, filePath); err != nil {
		t.Fatalf("EncryptStream failed: %v", err)
	}
	full := ct.Bytes()

	// Dropping the final chunk must be detected (the new last chunk was sealed
	// as non-final).
	truncated := full[:len(full)-EncryptedChunkSize]
	var out bytes.Buffer
	if err := DecryptStream(bytes.NewReader(truncated), &out, masterKey, filePath); err == nil {
		t.Fatal("expected truncated stream to fail decryption, got nil error")
	}

	// Appending a bogus extra chunk must be detected (the real final chunk was
	// sealed as final, but now has data after it).
	extended := append(append([]byte{}, full...), make([]byte, EncryptedChunkSize)...)
	var out2 bytes.Buffer
	if err := DecryptStream(bytes.NewReader(extended), &out2, masterKey, filePath); err == nil {
		t.Fatal("expected extended stream to fail decryption, got nil error")
	}
}

// ---------------------------------------------------------------------------
// TestEncryptDecryptBytes — small in-memory helper round-trip
// ---------------------------------------------------------------------------

func TestEncryptDecryptBytes(t *testing.T) {
	salt, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt failed: %v", err)
	}
	masterKey, err := DeriveMasterKey("testpassphrase", salt)
	if err != nil {
		t.Fatalf("DeriveMasterKey failed: %v", err)
	}

	plaintext := bytes.Repeat([]byte("A"), 1024)
	path := "gcrypt://oauth-token"

	ct, err := EncryptBytes(plaintext, masterKey, path)
	if err != nil {
		t.Fatalf("EncryptBytes failed: %v", err)
	}
	if len(ct) < 6 || string(ct[:6]) != models.Magic {
		t.Errorf("ciphertext does not start with magic bytes %q", models.Magic)
	}

	decrypted, err := DecryptBytes(ct, masterKey, path)
	if err != nil {
		t.Fatalf("DecryptBytes failed: %v", err)
	}
	if !bytes.Equal(plaintext, decrypted) {
		t.Error("decrypted plaintext does not match original")
	}

	// Wrong key must fail.
	wrongKey, err := DeriveMasterKey("wrongpassphrase", salt)
	if err != nil {
		t.Fatalf("DeriveMasterKey (wrong passphrase) failed: %v", err)
	}
	if _, err := DecryptBytes(ct, wrongKey, path); err == nil {
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
	header.Version = models.CurrentStreamVersion
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
