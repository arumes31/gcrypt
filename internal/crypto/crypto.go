package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"runtime"
	"unsafe"

	"github.com/daniel/gcrypt/internal/models"
	"golang.org/x/crypto/argon2"
	"golang.org/x/sys/windows"
)

// Argon2id parameters for master key derivation.
const (
	argon2Time     = 3
	argon2Memory   = 64 * 1024 // 64 MB in KiB
	argon2Threads  = 4
	argon2KeyLen   = 32
	argon2SaltSize = 16
)

// AES-GCM nonce size (96-bit / 12 bytes).
const nonceSize = 12

// DEK size (256-bit / 32 bytes).
const dekSize = 32

// EncryptedDEK size: 32 bytes ciphertext + 16 bytes GCM tag = 48 bytes.
const encryptedDEKSize = 48

// ---------------------------------------------------------------------------
// Key Derivation
// ---------------------------------------------------------------------------

// DeriveMasterKey derives a 256-bit master key from the given passphrase
// and salt using Argon2id with the following parameters:
//   - Memory: 64 MiB
//   - Iterations: 3
//   - Parallelism: 4
//   - Key length: 32 bytes (256 bits)
//
// The salt must be exactly 16 bytes.
func DeriveMasterKey(passphrase string, salt []byte) ([]byte, error) {
	if len(salt) != argon2SaltSize {
		return nil, fmt.Errorf("crypto: salt must be %d bytes, got %d", argon2SaltSize, len(salt))
	}
	if len(passphrase) == 0 {
		return nil, fmt.Errorf("crypto: passphrase is empty")
	}

	key := argon2.IDKey(
		[]byte(passphrase),
		salt,
		argon2Time,
		argon2Memory,
		argon2Threads,
		argon2KeyLen,
	)

	return key, nil
}

// GenerateSalt generates a 16-byte random salt suitable for use with
// DeriveMasterKey. It uses crypto/rand as the entropy source.
func GenerateSalt() ([]byte, error) {
	salt := make([]byte, argon2SaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("crypto: failed to generate salt: %w", err)
	}
	return salt, nil
}

// ---------------------------------------------------------------------------
// Per-File Key Management
// ---------------------------------------------------------------------------

// GenerateDEK generates a random 256-bit data encryption key using
// crypto/rand. Each file gets a unique DEK to ensure key isolation.
func GenerateDEK() ([]byte, error) {
	dek := make([]byte, dekSize)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("crypto: failed to generate DEK: %w", err)
	}
	return dek, nil
}

// EncryptDEK encrypts a DEK using AES-256-GCM with the master key.
// Returns the 48-byte encrypted DEK (32-byte ciphertext + 16-byte GCM tag)
// and the 12-byte nonce.
func EncryptDEK(dek []byte, masterKey []byte) (encryptedDEK []byte, dekNonce []byte, err error) {
	if len(dek) != dekSize {
		return nil, nil, fmt.Errorf("crypto: DEK must be %d bytes, got %d", dekSize, len(dek))
	}
	if len(masterKey) != dekSize {
		return nil, nil, fmt.Errorf("crypto: master key must be %d bytes, got %d", dekSize, len(masterKey))
	}

	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, nil, fmt.Errorf("crypto: failed to create AES cipher for DEK: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("crypto: failed to create GCM for DEK: %w", err)
	}

	// Generate random nonce for DEK encryption
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("crypto: failed to generate DEK nonce: %w", err)
	}

	// Encrypt DEK without AAD (the DEK is not bound to a file path)
	encrypted := aead.Seal(nil, nonce, dek, nil)

	return encrypted, nonce, nil
}

// DecryptDEK decrypts a DEK using AES-256-GCM with the master key.
// The encryptedDEK must be 48 bytes (32-byte ciphertext + 16-byte tag)
// and dekNonce must be 12 bytes.
func DecryptDEK(encryptedDEK []byte, dekNonce []byte, masterKey []byte) ([]byte, error) {
	if len(encryptedDEK) != encryptedDEKSize {
		return nil, fmt.Errorf("crypto: encrypted DEK must be %d bytes, got %d", encryptedDEKSize, len(encryptedDEK))
	}
	if len(dekNonce) != nonceSize {
		return nil, fmt.Errorf("crypto: DEK nonce must be %d bytes, got %d", nonceSize, len(dekNonce))
	}
	if len(masterKey) != dekSize {
		return nil, fmt.Errorf("crypto: master key must be %d bytes, got %d", dekSize, len(masterKey))
	}

	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("crypto: failed to create AES cipher for DEK: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: failed to create GCM for DEK: %w", err)
	}

	dek, err := aead.Open(nil, dekNonce, encryptedDEK, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: failed to decrypt DEK: %w", err)
	}

	return dek, nil
}

// ---------------------------------------------------------------------------
// File Encryption/Decryption
// ---------------------------------------------------------------------------

// EncryptFile encrypts file content using the per-file DEK with AES-256-GCM.
// The filePath is used to compute Additional Authenticated Data (AAD) via
// SHA-256, binding the ciphertext to the file identity and preventing
// relocation attacks.
//
// Returns the encrypted file header (containing the content nonce) and the
// ciphertext (which includes the 16-byte GCM tag appended).
func EncryptFile(plaintext []byte, dek []byte, filePath string) (*models.EncryptedFileHeader, []byte, error) {
	if len(dek) != dekSize {
		return nil, nil, fmt.Errorf("crypto: DEK must be %d bytes, got %d", dekSize, len(dek))
	}

	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, nil, fmt.Errorf("crypto: failed to create AES cipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("crypto: failed to create GCM: %w", err)
	}

	// Generate random 12-byte content nonce
	contentNonce := make([]byte, nonceSize)
	if _, err := rand.Read(contentNonce); err != nil {
		return nil, nil, fmt.Errorf("crypto: failed to generate content nonce: %w", err)
	}

	// Compute AAD = SHA-256(filePath) to bind ciphertext to the file path
	aad := HashFilePath(filePath)

	// Encrypt with AES-256-GCM
	ciphertext := aead.Seal(nil, contentNonce, plaintext, aad)

	// Build the header
	header := &models.EncryptedFileHeader{
		Version: models.CurrentVersion,
	}
	copy(header.Magic[:], models.Magic)
	copy(header.ContentNonce[:], contentNonce)

	return header, ciphertext, nil
}

// DecryptFile decrypts file content using the per-file DEK with AES-256-GCM.
// It recomputes the AAD from the filePath to verify that the ciphertext
// belongs to the claimed file path.
func DecryptFile(ciphertext []byte, header *models.EncryptedFileHeader, dek []byte, filePath string) ([]byte, error) {
	if len(dek) != dekSize {
		return nil, fmt.Errorf("crypto: DEK must be %d bytes, got %d", dekSize, len(dek))
	}
	if header == nil {
		return nil, fmt.Errorf("crypto: header is nil")
	}

	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("crypto: failed to create AES cipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: failed to create GCM: %w", err)
	}

	// Compute AAD = SHA-256(filePath)
	aad := HashFilePath(filePath)

	// Decrypt with AES-256-GCM
	plaintext, err := aead.Open(nil, header.ContentNonce[:], ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("crypto: failed to decrypt file content: %w", err)
	}

	return plaintext, nil
}

// ---------------------------------------------------------------------------
// Full File Operations (combining header + ciphertext)
// ---------------------------------------------------------------------------

// EncryptBlob performs the full encryption pipeline on plaintext data:
//  1. Generate a random DEK
//  2. Encrypt the DEK with the master key
//  3. Encrypt the file content with the DEK (using path-derived AAD)
//  4. Build the file header (magic + version + encrypted DEK + DEK nonce + content nonce)
//  5. Serialize the 80-byte header + ciphertext
//  6. Return the complete encrypted blob
func EncryptBlob(plaintext []byte, masterKey []byte, filePath string) ([]byte, error) {
	if len(masterKey) != dekSize {
		return nil, fmt.Errorf("crypto: master key must be %d bytes, got %d", dekSize, len(masterKey))
	}

	// Step 1: Generate DEK
	dek, err := GenerateDEK()
	if err != nil {
		return nil, fmt.Errorf("crypto: encrypt blob: %w", err)
	}
	defer WipeBytes(dek)

	// Step 2: Encrypt DEK with master key
	encryptedDEK, dekNonce, err := EncryptDEK(dek, masterKey)
	if err != nil {
		return nil, fmt.Errorf("crypto: encrypt blob: %w", err)
	}

	// Step 3: Encrypt file content with DEK
	header, ciphertext, err := EncryptFile(plaintext, dek, filePath)
	if err != nil {
		return nil, fmt.Errorf("crypto: encrypt blob: %w", err)
	}

	// Step 4: Fill in the remaining header fields
	copy(header.EncryptedDEK[:], encryptedDEK)
	copy(header.DEKNonce[:], dekNonce)

	// Step 5: Serialize header + ciphertext
	headerBytes, err := SerializeHeader(header)
	if err != nil {
		return nil, fmt.Errorf("crypto: encrypt blob: %w", err)
	}

	// Step 6: Combine header and ciphertext
	blob := make([]byte, 0, len(headerBytes)+len(ciphertext))
	blob = append(blob, headerBytes...)
	blob = append(blob, ciphertext...)

	return blob, nil
}

// DecryptBlob performs the full decryption pipeline on an encrypted blob:
//  1. Parse and validate the 80-byte header (check magic bytes, version)
//  2. Extract encrypted DEK, DEK nonce, and content nonce from the header
//  3. Decrypt the DEK using the master key
//  4. Decrypt the file content using the DEK and path-derived AAD
//  5. Return the plaintext
func DecryptBlob(blob []byte, masterKey []byte, filePath string) ([]byte, error) {
	if len(masterKey) != dekSize {
		return nil, fmt.Errorf("crypto: master key must be %d bytes, got %d", dekSize, len(masterKey))
	}
	if len(blob) < models.HeaderSize {
		return nil, fmt.Errorf("crypto: blob too short (%d bytes, minimum %d)", len(blob), models.HeaderSize)
	}

	// Step 1: Parse and validate header
	header, err := DeserializeHeader(blob[:models.HeaderSize])
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt blob: %w", err)
	}

	// Step 2: Extract fields from header (already done by DeserializeHeader)
	encryptedDEK := header.EncryptedDEK[:]
	dekNonce := header.DEKNonce[:]

	// Step 3: Decrypt DEK with master key
	dek, err := DecryptDEK(encryptedDEK, dekNonce, masterKey)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt blob: %w", err)
	}
	defer WipeBytes(dek)

	// Step 4: Decrypt content with DEK
	ciphertext := blob[models.HeaderSize:]
	plaintext, err := DecryptFile(ciphertext, header, dek, filePath)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt blob: %w", err)
	}

	// Step 5: Return plaintext
	return plaintext, nil
}

// ---------------------------------------------------------------------------
// Header Serialization
// ---------------------------------------------------------------------------

// SerializeHeader converts an EncryptedFileHeader to its 80-byte binary format:
//
//	[0:6]   magic          — 6 bytes ASCII "GCRYPT"
//	[6:8]   version        — 2 bytes little-endian uint16
//	[8:56]  encrypted DEK  — 48 bytes (32-byte ciphertext + 16-byte GCM tag)
//	[56:68] DEK nonce      — 12 bytes
//	[68:80] content nonce  — 12 bytes
func SerializeHeader(h *models.EncryptedFileHeader) ([]byte, error) {
	if h == nil {
		return nil, fmt.Errorf("crypto: header is nil")
	}

	buf := make([]byte, models.HeaderSize)

	// [0:6] Magic
	copy(buf[0:6], h.Magic[:])

	// [6:8] Version (little-endian uint16)
	binary.LittleEndian.PutUint16(buf[6:8], h.Version)

	// [8:56] Encrypted DEK (48 bytes)
	copy(buf[8:56], h.EncryptedDEK[:])

	// [56:68] DEK nonce (12 bytes)
	copy(buf[56:68], h.DEKNonce[:])

	// [68:80] Content nonce (12 bytes)
	copy(buf[68:80], h.ContentNonce[:])

	return buf, nil
}

// DeserializeHeader parses an 80-byte binary header back into an
// EncryptedFileHeader struct. It validates the magic bytes and version.
func DeserializeHeader(data []byte) (*models.EncryptedFileHeader, error) {
	if len(data) < models.HeaderSize {
		return nil, fmt.Errorf("crypto: header data too short (%d bytes, need %d)", len(data), models.HeaderSize)
	}

	header := &models.EncryptedFileHeader{}

	// [0:6] Magic
	copy(header.Magic[:], data[0:6])
	if string(header.Magic[:]) != models.Magic {
		return nil, fmt.Errorf("crypto: invalid magic bytes %q, expected %q", string(header.Magic[:]), models.Magic)
	}

	// [6:8] Version (little-endian uint16)
	header.Version = binary.LittleEndian.Uint16(data[6:8])
	if header.Version != models.CurrentVersion {
		return nil, fmt.Errorf("crypto: unsupported version %d, expected %d", header.Version, models.CurrentVersion)
	}

	// [8:56] Encrypted DEK (48 bytes)
	copy(header.EncryptedDEK[:], data[8:56])

	// [56:68] DEK nonce (12 bytes)
	copy(header.DEKNonce[:], data[56:68])

	// [68:80] Content nonce (12 bytes)
	copy(header.ContentNonce[:], data[68:80])

	return header, nil
}

// ---------------------------------------------------------------------------
// Secure Memory Handling
// ---------------------------------------------------------------------------

// WipeBytes overwrites the provided byte slice with zeros and uses
// runtime.KeepAlive to prevent the compiler from optimizing away the
// overwrite. This should be called on any key material or sensitive
// data that is no longer needed.
func WipeBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
	runtime.KeepAlive(b)
}

// LockMemory calls VirtualLock on Windows to prevent the specified
// memory region from being swapped to disk. This should be used to
// protect sensitive key material residing in memory.
//
// Returns an error if the VirtualLock call fails or if the platform
// is not Windows.
func LockMemory(b []byte) error {
	err := windows.VirtualLock(
		uintptr(unsafe.Pointer(&b[0])),
		uintptr(len(b)),
	)
	if err != nil {
		return fmt.Errorf("crypto: VirtualLock failed: %w", err)
	}
	return nil
}

// UnlockMemory calls VirtualUnlock on Windows to release a memory
// lock previously established by LockMemory.
func UnlockMemory(b []byte) error {
	err := windows.VirtualUnlock(
		uintptr(unsafe.Pointer(&b[0])),
		uintptr(len(b)),
	)
	if err != nil {
		return fmt.Errorf("crypto: VirtualUnlock failed: %w", err)
	}
	return nil
}
