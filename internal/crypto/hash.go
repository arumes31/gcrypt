package crypto

import (
	"crypto/sha256"
	"encoding/hex"

	"golang.org/x/crypto/argon2"
)

// HashFile returns the SHA-256 hex digest of the given data.
// This is used for integrity verification and change detection.
func HashFile(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// HashFilePath returns the SHA-256 raw bytes of the given path string.
// This is used as Additional Authenticated Data (AAD) in AES-256-GCM
// to bind ciphertext to the file identity and prevent relocation attacks.
func HashFilePath(path string) []byte {
	h := sha256.Sum256([]byte(path))
	return h[:]
}

// HashPassphrase returns an Argon2id hex digest of the passphrase for
// storage verification. This is separate from key derivation — it uses
// a different salt (derived from the provided salt with a pepper) so
// that the stored hash cannot be used to recover the master key.
//
// The returned string is suitable for storing in configuration for
// passphrase verification at startup.
func HashPassphrase(passphrase string, salt []byte) string {
	// Add a pepper to differentiate this hash from the key derivation
	pepperedSalt := append([]byte("verify:"), salt...)
	// Use lower cost parameters for verification hash since this is
	// only used for comparison, not for producing a cryptographic key
	hash := argon2.IDKey(
		[]byte(passphrase),
		pepperedSalt,
		2,       // time — lower than key derivation
		32*1024, // memory — 32 MB, lower than key derivation
		2,       // parallelism — lower than key derivation
		32,      // key length
	)
	return hex.EncodeToString(hash)
}
