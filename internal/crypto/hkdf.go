// Package crypto implements all cryptographic operations for gcrypt:
// key derivation, AES-256-GCM encryption/decryption, filename encryption,
// hashing, and secure memory handling.
package crypto

import (
	"crypto/sha256"
	"fmt"

	"golang.org/x/crypto/hkdf"
)

// hkdfFixedSalt is the fixed salt used for HKDF-Expand operations.
const hkdfFixedSalt = "gcrypt-hkdf-v1"

// DeriveKey derives a key of the specified length from the master key using
// HKDF-Expand with SHA-256, a fixed salt of "gcrypt-hkdf-v1", and the
// provided info string.
//
// This is used to derive purpose-specific subkeys (e.g., filename encryption
// key) from the master key without reducing overall security.
func DeriveKey(masterKey []byte, info string, length int) ([]byte, error) {
	if len(masterKey) == 0 {
		return nil, fmt.Errorf("crypto: master key is empty")
	}
	if length <= 0 {
		return nil, fmt.Errorf("crypto: invalid key length %d", length)
	}
	if length > sha256.Size*255 {
		return nil, fmt.Errorf("crypto: key length %d exceeds HKDF maximum", length)
	}

	// Use hkdf.New (Extract+Expand) with the fixed salt for proper
	// key separation. The info parameter provides domain separation
	// between different derived keys.
	prkReader := hkdf.New(sha256.New, masterKey, []byte(hkdfFixedSalt), []byte(info))

	out := make([]byte, length)
	if _, err := prkReader.Read(out); err != nil {
		return nil, fmt.Errorf("crypto: HKDF key derivation failed: %w", err)
	}

	return out, nil
}
