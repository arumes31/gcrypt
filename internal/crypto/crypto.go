package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"runtime"
	"unsafe"

	"github.com/arumes31/gcrypt/internal/models"
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
// In-memory helpers (small values: OAuth token, client secret)
// ---------------------------------------------------------------------------

// EncryptBytes encrypts an in-memory plaintext using the v2 stream format and
// returns the ciphertext. It is a convenience wrapper around EncryptStream for
// small values such as the OAuth token and client secret.
func EncryptBytes(plaintext, masterKey []byte, path string) ([]byte, error) {
	var buf bytes.Buffer
	if err := EncryptStream(bytes.NewReader(plaintext), &buf, masterKey, path); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecryptBytes reverses EncryptBytes, decrypting an in-memory v2-stream
// ciphertext and returning the plaintext.
func DecryptBytes(ciphertext, masterKey []byte, path string) ([]byte, error) {
	var buf bytes.Buffer
	if err := DecryptStream(bytes.NewReader(ciphertext), &buf, masterKey, path); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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

	// [6:8] Version (little-endian uint16).
	header.Version = binary.LittleEndian.Uint16(data[6:8])
	if header.Version != models.CurrentStreamVersion {
		return nil, fmt.Errorf("crypto: unsupported version %d, expected %d", header.Version, models.CurrentStreamVersion)
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
// Streaming Encryption/Decryption
// ---------------------------------------------------------------------------

const (
	// ChunkSize is the size of each encrypted chunk (64 KB).
	ChunkSize = 64 * 1024
	// Overhead is the AES-GCM tag size (16 bytes).
	Overhead = 16
	// EncryptedChunkSize is ChunkSize + Overhead.
	EncryptedChunkSize = ChunkSize + Overhead
)

// streamChunkNonce derives the per-chunk nonce as the content nonce with its
// low 8 bytes XORed by the chunk index, giving every chunk in a file a unique
// nonce (the DEK is itself unique per file).
func streamChunkNonce(contentNonce []byte, chunkIndex uint64) []byte {
	nonce := make([]byte, nonceSize)
	copy(nonce, contentNonce)
	binary.LittleEndian.PutUint64(nonce[0:8], binary.LittleEndian.Uint64(nonce[0:8])^chunkIndex)
	return nonce
}

// streamChunkAAD builds the additional authenticated data for a chunk:
// baseAAD || chunkIndex || finalByte. The trailing "final" byte authenticates
// the position of the last chunk — and therefore the chunk count — defeating
// truncation and extension of the chunk sequence.
func streamChunkAAD(baseAAD []byte, chunkIndex uint64, final bool) []byte {
	aad := make([]byte, 32+8+1)
	copy(aad[0:32], baseAAD)
	binary.LittleEndian.PutUint64(aad[32:40], chunkIndex)
	if final {
		aad[40] = 1
	}
	return aad
}

// EncryptStream encrypts data from src and writes it to dst in chunks using the
// v2 stream format: each chunk's AAD carries its index and whether it is the
// final chunk, so a ciphertext that is truncated or extended at a chunk boundary
// fails to authenticate on decryption.
func EncryptStream(src io.Reader, dst io.Writer, masterKey []byte, filePath string) error {
	// 1. Generate DEK and encrypt it
	dek, err := GenerateDEK()
	if err != nil {
		return err
	}
	defer WipeBytes(dek)

	encryptedDEK, dekNonce, err := EncryptDEK(dek, masterKey)
	if err != nil {
		return err
	}

	// 2. Build and write the header
	contentNonce := make([]byte, nonceSize)
	if _, err := rand.Read(contentNonce); err != nil {
		return err
	}

	header := &models.EncryptedFileHeader{
		Version: models.CurrentStreamVersion,
	}
	copy(header.Magic[:], models.Magic)
	copy(header.EncryptedDEK[:], encryptedDEK)
	copy(header.DEKNonce[:], dekNonce)
	copy(header.ContentNonce[:], contentNonce)

	headerBytes, err := SerializeHeader(header)
	if err != nil {
		return err
	}
	if _, err := dst.Write(headerBytes); err != nil {
		return err
	}

	// 3. Encrypt content in chunks
	block, err := aes.NewCipher(dek)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	baseAAD := HashFilePath(filePath)

	// Encrypt with one chunk of read-ahead so we know which chunk is the last
	// and can mark it final in the AAD. Two buffers are reused and swapped to
	// avoid per-chunk allocation. An empty input still emits exactly one (empty)
	// final chunk so the decryptor always sees a final marker.
	cur := make([]byte, ChunkSize)
	next := make([]byte, ChunkSize)
	curN, err := io.ReadFull(src, cur)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return err
	}

	chunkIndex := uint64(0)
	for {
		nextN, nerr := io.ReadFull(src, next)
		if nerr != nil && nerr != io.EOF && nerr != io.ErrUnexpectedEOF {
			return nerr
		}
		final := nextN == 0

		nonce := streamChunkNonce(contentNonce, chunkIndex)
		aad := streamChunkAAD(baseAAD, chunkIndex, final)
		ciphertext := aead.Seal(nil, nonce, cur[:curN], aad)
		if _, err := dst.Write(ciphertext); err != nil {
			return err
		}
		chunkIndex++

		if final {
			break
		}
		cur, next = next, cur
		curN = nextN
	}

	return nil
}

// DecryptStream decrypts a v2 stream written by EncryptStream. It uses one
// chunk of read-ahead so it knows which chunk should carry the final marker; a
// ciphertext truncated or extended at a chunk boundary makes the computed final
// flag disagree with what was sealed, so Open fails and the tampering is
// detected.
func DecryptStream(src io.Reader, dst io.Writer, masterKey []byte, filePath string) error {
	// 1. Read and parse header
	headerBytes := make([]byte, models.HeaderSize)
	if _, err := io.ReadFull(src, headerBytes); err != nil {
		return err
	}

	header, err := DeserializeHeader(headerBytes)
	if err != nil {
		return err
	}

	// 2. Decrypt DEK
	dek, err := DecryptDEK(header.EncryptedDEK[:], header.DEKNonce[:], masterKey)
	if err != nil {
		return err
	}
	defer WipeBytes(dek)

	// 3. Decrypt content in chunks
	block, err := aes.NewCipher(dek)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}

	baseAAD := HashFilePath(filePath)
	contentNonce := header.ContentNonce[:]

	cur := make([]byte, EncryptedChunkSize)
	next := make([]byte, EncryptedChunkSize)
	curN, err := io.ReadFull(src, cur)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return err
	}

	chunkIndex := uint64(0)
	for {
		nextN, nerr := io.ReadFull(src, next)
		if nerr != nil && nerr != io.EOF && nerr != io.ErrUnexpectedEOF {
			return nerr
		}
		final := nextN == 0

		if curN == 0 {
			// Nothing to open: the stream ended at the header with no final
			// chunk, i.e. it was truncated.
			return fmt.Errorf("crypto: truncated stream (no final chunk)")
		}

		nonce := streamChunkNonce(contentNonce, chunkIndex)
		aad := streamChunkAAD(baseAAD, chunkIndex, final)
		plaintext, oerr := aead.Open(nil, nonce, cur[:curN], aad)
		if oerr != nil {
			return fmt.Errorf("crypto: decrypt chunk %d: %w", chunkIndex, oerr)
		}
		if _, werr := dst.Write(plaintext); werr != nil {
			return werr
		}
		chunkIndex++

		if final {
			break
		}
		cur, next = next, cur
		curN = nextN
	}

	return nil
}

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
	if len(b) == 0 {
		return nil
	}
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
	if len(b) == 0 {
		return nil
	}
	err := windows.VirtualUnlock(
		uintptr(unsafe.Pointer(&b[0])),
		uintptr(len(b)),
	)
	if err != nil {
		return fmt.Errorf("crypto: VirtualUnlock failed: %w", err)
	}
	return nil
}
