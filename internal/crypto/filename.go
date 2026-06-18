package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"encoding/base64"
	"encoding/binary"
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

	// filenameVerPadded marks the length-padded filename format (a leading
	// version byte before the nonce). Legacy unpadded output has no such byte;
	// DecryptFilename disambiguates by trying the padded interpretation first and
	// falling back to legacy if GCM authentication fails.
	filenameVerPadded = 0x01

	// filenamePadBucket is the granularity (bytes) the plaintext name is padded
	// to, so the ciphertext length only reveals the name length rounded up to a
	// multiple of this — not the exact length.
	filenamePadBucket = 16
)

// roundUpFilename rounds n up to the next multiple of filenamePadBucket.
func roundUpFilename(n int) int {
	return ((n + filenamePadBucket - 1) / filenamePadBucket) * filenamePadBucket
}

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
	return encryptFilename(plaintext, masterKey, false)
}

// EncryptFilenamePadded is like EncryptFilename but pads the plaintext name to a
// fixed bucket size before encryption, so the ciphertext length only reveals the
// name length rounded up to filenamePadBucket rather than exactly. The output is
// self-describing (a version marker), so DecryptFilename reads it transparently
// and a sync can contain a mix of padded and unpadded names.
//
// Note: this addresses length leakage only. Names remain deterministic (the same
// name still encrypts to the same ciphertext), which is inherent to gcrypt's
// design of locating remote files by name without storing a name→ID map.
func EncryptFilenamePadded(plaintext string, masterKey []byte) (string, error) {
	return encryptFilename(plaintext, masterKey, true)
}

// encryptFilename is the shared core for the padded and unpadded variants.
func encryptFilename(plaintext string, masterKey []byte, pad bool) (string, error) {
	if len(masterKey) == 0 {
		return "", fmt.Errorf("crypto: master key is empty")
	}
	if len(plaintext) == 0 {
		return "", fmt.Errorf("crypto: filename is empty")
	}

	// Derive filename encryption key from master key.
	filenameKey, err := DeriveKey(masterKey, filenameKeyInfo, 32)
	if err != nil {
		return "", fmt.Errorf("crypto: failed to derive filename key: %w", err)
	}
	defer WipeBytes(filenameKey)

	// Deterministic nonce = HMAC(filename, filenameKey), based on the ORIGINAL
	// name so the same name always maps to the same ciphertext.
	mac := hmac.New(sha3.NewLegacyKeccak256, filenameKey)
	mac.Write([]byte(plaintext))
	nonce := mac.Sum(nil)[:filenameNonceSize]

	block, err := aes.NewCipher(filenameKey)
	if err != nil {
		return "", fmt.Errorf("crypto: failed to create AES cipher for filename: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("crypto: failed to create GCM for filename: %w", err)
	}

	// Plaintext to seal. For the padded format the sealed payload is
	// [origLen uint16 LE][name bytes][zero padding to a bucket boundary].
	payload := []byte(plaintext)
	if pad {
		nb := []byte(plaintext)
		if len(nb) > 0xFFFF {
			return "", fmt.Errorf("crypto: filename too long to pad (%d bytes, max %d)", len(nb), 0xFFFF)
		}
		padded := make([]byte, roundUpFilename(2+len(nb)))
		binary.LittleEndian.PutUint16(padded[0:2], uint16(len(nb))) // #nosec G115 -- len(nb) is bounded to <= 0xFFFF by the guard above
		copy(padded[2:], nb)
		payload = padded
	}
	ciphertext := aead.Seal(nil, nonce, payload, nil)

	// Encode as [marker?][nonce][ciphertext+tag], base64url without padding.
	combined := make([]byte, 0, 1+filenameNonceSize+len(ciphertext))
	if pad {
		combined = append(combined, filenameVerPadded)
	}
	combined = append(combined, nonce...)
	combined = append(combined, ciphertext...)

	return base64.RawURLEncoding.EncodeToString(combined), nil
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

	// Decode from base64url (no padding).
	data, err := base64.RawURLEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("crypto: failed to decode filename: %w", err)
	}
	if len(data) < filenameNonceSize+16 {
		return "", fmt.Errorf("crypto: encrypted filename too short (%d bytes)", len(data))
	}

	// Derive filename encryption key from master key.
	filenameKey, err := DeriveKey(masterKey, filenameKeyInfo, 32)
	if err != nil {
		return "", fmt.Errorf("crypto: failed to derive filename key: %w", err)
	}
	defer WipeBytes(filenameKey)

	block, err := aes.NewCipher(filenameKey)
	if err != nil {
		return "", fmt.Errorf("crypto: failed to create AES cipher for filename: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("crypto: failed to create GCM for filename: %w", err)
	}

	// Padded format (v1): [marker][nonce][ciphertext of len||name||pad]. Try it
	// first when the marker byte is present; GCM authentication confirms the
	// interpretation, so a legacy name whose first byte happens to equal the
	// marker simply fails here and falls through to the legacy path.
	if data[0] == filenameVerPadded && len(data) >= 1+filenameNonceSize+16 {
		nonce := data[1 : 1+filenameNonceSize]
		enc := data[1+filenameNonceSize:]
		if pt, oerr := aead.Open(nil, nonce, enc, nil); oerr == nil && len(pt) >= 2 {
			n := int(binary.LittleEndian.Uint16(pt[0:2]))
			if 2+n <= len(pt) {
				return string(pt[2 : 2+n]), nil
			}
		}
	}

	// Legacy format (v0): [nonce][ciphertext of name].
	nonce := data[:filenameNonceSize]
	enc := data[filenameNonceSize:]
	plaintext, err := aead.Open(nil, nonce, enc, nil)
	if err != nil {
		return "", fmt.Errorf("crypto: failed to decrypt filename: %w", err)
	}

	return string(plaintext), nil
}
