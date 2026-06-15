package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"encoding/base64"
	"fmt"

	"golang.org/x/crypto/sha3"
)

const (
	// filenameKeyInfo is the HKDF info string used to derive the
	// filename encryption key from the master key.
	filenameKeyInfo = "gcrypt-filename"

	// filenameNonceSize is the size of the GCM nonce used for
	// filename encryption (12 bytes).
	filenameNonceSize = 12
)

// EncryptFilename encrypts a plaintext filename deterministically using
// the master key. The same filename always encrypts to the same ciphertext,
// enabling change detection without storing a separate filename map.
//
// Algorithm:
//  1. Derive a 256-bit filename encryption key from masterKey using
//     HKDF-SHA256 with info="gcrypt-filename"
//  2. Derive a deterministic 12-byte nonce from HMAC-SHA256(filename, filenameKey)
//     — this ensures the same filename always produces the same ciphertext
//  3. Encrypt with AES-256-GCM using the derived key and deterministic nonce
//  4. Prepend the nonce to the ciphertext, then Base64url-encode (no padding)
//     for use as a Google Drive filename
//
// The output is deterministic: the same (plaintext, masterKey) pair always
// produces the same encoded string. The GCM authentication tag ensures
// integrity during decryption.
func EncryptFilename(plaintext string, masterKey []byte) (string, error) {
	if len(masterKey) == 0 {
		return "", fmt.Errorf("crypto: master key is empty")
	}
	if len(plaintext) == 0 {
		return "", fmt.Errorf("crypto: filename is empty")
	}

	// Step 1: Derive filename encryption key from master key
	filenameKey, err := DeriveKey(masterKey, filenameKeyInfo, 32)
	if err != nil {
		return "", fmt.Errorf("crypto: failed to derive filename key: %w", err)
	}
	defer WipeBytes(filenameKey)

	// Step 2: Derive deterministic nonce from HMAC-SHA256(filename, filenameKey).
	// Using the filename key as the HMAC key and the plaintext filename as the
	// message ensures that the same filename always maps to the same nonce,
	// producing deterministic ciphertext.
	mac := hmac.New(sha3.NewLegacyKeccak256, filenameKey)
	mac.Write([]byte(plaintext))
	nonce := mac.Sum(nil)[:filenameNonceSize]

	// Step 3: Encrypt with AES-256-GCM
	block, err := aes.NewCipher(filenameKey)
	if err != nil {
		return "", fmt.Errorf("crypto: failed to create AES cipher for filename: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("crypto: failed to create GCM for filename: %w", err)
	}

	// No AAD for filename encryption — the deterministic nonce already
	// binds the output to the input filename.
	ciphertext := aead.Seal(nil, nonce, []byte(plaintext), nil)

	// Step 4: Prepend nonce to ciphertext, then Base64url-encode without padding.
	// Format: [nonce 12 bytes][ciphertext + GCM tag]
	combined := make([]byte, 0, filenameNonceSize+len(ciphertext))
	combined = append(combined, nonce...)
	combined = append(combined, ciphertext...)

	encoded := base64.RawURLEncoding.EncodeToString(combined)

	return encoded, nil
}

// DecryptFilename decrypts a filename that was encrypted with EncryptFilename.
// It reverses the deterministic encryption process:
//  1. Base64url-decode the ciphertext
//  2. Extract the 12-byte nonce from the beginning
//  3. Derive the same filename encryption key from masterKey
//  4. Decrypt with AES-256-GCM using the extracted nonce and derived key
//  5. Return the plaintext filename
func DecryptFilename(ciphertext string, masterKey []byte) (string, error) {
	if len(masterKey) == 0 {
		return "", fmt.Errorf("crypto: master key is empty")
	}
	if len(ciphertext) == 0 {
		return "", fmt.Errorf("crypto: encrypted filename is empty")
	}

	// Step 1: Decode from base64url (no padding)
	data, err := base64.RawURLEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("crypto: failed to decode filename: %w", err)
	}

	// Step 2: Extract nonce from the beginning of the data.
	// Format: [nonce 12 bytes][ciphertext + GCM tag 16+ bytes]
	// Minimum size: 12 (nonce) + 16 (GCM tag) = 28 bytes
	if len(data) < filenameNonceSize+16 {
		return "", fmt.Errorf("crypto: encrypted filename too short (%d bytes)", len(data))
	}

	nonce := data[:filenameNonceSize]
	encryptedName := data[filenameNonceSize:]

	// Step 3: Derive filename encryption key from master key
	filenameKey, err := DeriveKey(masterKey, filenameKeyInfo, 32)
	if err != nil {
		return "", fmt.Errorf("crypto: failed to derive filename key: %w", err)
	}
	defer WipeBytes(filenameKey)

	// Step 4: Decrypt with AES-256-GCM
	block, err := aes.NewCipher(filenameKey)
	if err != nil {
		return "", fmt.Errorf("crypto: failed to create AES cipher for filename: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("crypto: failed to create GCM for filename: %w", err)
	}

	plaintext, err := aead.Open(nil, nonce, encryptedName, nil)
	if err != nil {
		return "", fmt.Errorf("crypto: failed to decrypt filename: %w", err)
	}

	return string(plaintext), nil
}
