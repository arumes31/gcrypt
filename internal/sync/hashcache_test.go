package sync

import (
	"os"
	"path/filepath"
	"testing"
)

// TestHashCachePersistence verifies the scanner's hash cache survives a
// Save/Load round-trip, which is what lets a cold start skip re-hashing files
// whose size and mtime are unchanged.
func TestHashCachePersistence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "data.bin")
	if err := os.WriteFile(file, []byte("hello world"), 0600); err != nil {
		t.Fatal(err)
	}

	s1 := NewScanner(dir, nil, nil)
	digest, err := s1.ComputeHash(file)
	if err != nil {
		t.Fatalf("ComputeHash: %v", err)
	}

	cachePath := filepath.Join(dir, "cache.json")
	if err := s1.SaveHashCache(cachePath); err != nil {
		t.Fatalf("SaveHashCache: %v", err)
	}

	s2 := NewScanner(dir, nil, nil)
	if err := s2.LoadHashCache(cachePath); err != nil {
		t.Fatalf("LoadHashCache: %v", err)
	}

	abs, _ := filepath.Abs(file)
	entry, ok := s2.hashCache[abs]
	if !ok {
		t.Fatalf("loaded cache missing entry for %s", abs)
	}
	if entry.digest != digest {
		t.Fatalf("loaded digest = %q, want %q", entry.digest, digest)
	}
}

// TestLoadHashCacheMissingFile reports an error but does not panic; the engine
// treats this as a cold start.
func TestLoadHashCacheMissingFile(t *testing.T) {
	s := NewScanner(t.TempDir(), nil, nil)
	if err := s.LoadHashCache(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error loading a missing cache file")
	}
}
