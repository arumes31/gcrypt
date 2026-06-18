package crypto

import (
	"strings"
	"testing"
)

func TestEncryptFilenamePaddedRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	for _, name := range []string{"a", "report.pdf", "a-rather-longer-file-name.txt", strings.Repeat("x", 120)} {
		enc, err := EncryptFilenamePadded(name, key)
		if err != nil {
			t.Fatalf("EncryptFilenamePadded(%q): %v", name, err)
		}
		got, err := DecryptFilename(enc, key)
		if err != nil {
			t.Fatalf("DecryptFilename(padded %q): %v", name, err)
		}
		if got != name {
			t.Fatalf("padded round-trip = %q, want %q", got, name)
		}
	}
}

// TestDecryptFilenameHandlesBothFormats confirms DecryptFilename transparently
// reads both legacy (unpadded) and padded names, so a sync can hold a mix.
func TestDecryptFilenameHandlesBothFormats(t *testing.T) {
	key := make([]byte, 32)
	const name = "mixed-format.txt"

	legacy, err := EncryptFilename(name, key)
	if err != nil {
		t.Fatal(err)
	}
	padded, err := EncryptFilenamePadded(name, key)
	if err != nil {
		t.Fatal(err)
	}
	if legacy == padded {
		t.Fatal("padded and legacy encodings should differ")
	}
	for label, enc := range map[string]string{"legacy": legacy, "padded": padded} {
		got, err := DecryptFilename(enc, key)
		if err != nil {
			t.Fatalf("DecryptFilename(%s): %v", label, err)
		}
		if got != name {
			t.Fatalf("%s decrypt = %q, want %q", label, got, name)
		}
	}
}

// TestPaddedFilenamesHideLength checks that names of different lengths within
// the same padding bucket produce equal-length ciphertext (the point of the
// feature), while remaining deterministic.
func TestPaddedFilenamesHideLength(t *testing.T) {
	key := make([]byte, 32)

	a, _ := EncryptFilenamePadded("a", key)
	b, _ := EncryptFilenamePadded("abcd", key) // same bucket (2+len ≤ 16)
	if len(a) != len(b) {
		t.Fatalf("expected equal ciphertext length within a bucket, got %d vs %d", len(a), len(b))
	}

	// Determinism: same name → same ciphertext.
	a2, _ := EncryptFilenamePadded("a", key)
	if a != a2 {
		t.Fatal("padded filename encryption should be deterministic")
	}
}
