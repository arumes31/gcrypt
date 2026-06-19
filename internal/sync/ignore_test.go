package sync

import (
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// TestIgnoreMatcherBuiltIn — Built-in patterns
// ---------------------------------------------------------------------------

func TestIgnoreMatcherBuiltIn(t *testing.T) {
	rootDir := t.TempDir()
	im := NewIgnoreMatcher(rootDir, nil)

	builtInIgnored := []string{
		filepath.Join(rootDir, "Thumbs.db"),
		filepath.Join(rootDir, ".DS_Store"),
		filepath.Join(rootDir, "desktop.ini"),
		filepath.Join(rootDir, "subdir", "Thumbs.db"),
		filepath.Join(rootDir, ".gcrypt"),
		filepath.Join(rootDir, ".gcrypt-ignore"),
		filepath.Join(rootDir, "test.tmp"),
		filepath.Join(rootDir, "~$document.docx"),
		filepath.Join(rootDir, "file.swp"),
		filepath.Join(rootDir, ".git"),
		filepath.Join(rootDir, ".git", "config"),
		// Never-sync directories must be ignored at ANY depth, not just the root.
		filepath.Join(rootDir, "node_modules"),
		filepath.Join(rootDir, "node_modules", "left-pad", "index.js"),
		filepath.Join(rootDir, "proj", "node_modules", "x", "y.js"),
		filepath.Join(rootDir, "proj", ".git", "FETCH_HEAD"),
		filepath.Join(rootDir, "proj", ".svn", "entries"),
		filepath.Join(rootDir, "proj", ".hg", "store"),
		// Case variants must still match on case-insensitive filesystems.
		filepath.Join(rootDir, ".Git", "config"),
		filepath.Join(rootDir, "Node_Modules", "left-pad", "index.js"),
		filepath.Join(rootDir, "proj", "NODE_MODULES", "x.js"),
	}

	for _, path := range builtInIgnored {
		if !im.Match(path) {
			t.Errorf("expected built-in pattern to match %q", path)
		}
	}

	// Paths that should NOT be ignored.
	notIgnored := []string{
		filepath.Join(rootDir, "document.pdf"),
		filepath.Join(rootDir, "photo.jpg"),
		filepath.Join(rootDir, "subdir", "report.docx"),
	}

	for _, path := range notIgnored {
		if im.Match(path) {
			t.Errorf("expected %q to NOT be ignored", path)
		}
	}
}

// ---------------------------------------------------------------------------
// TestIgnoreMatcherUserPatterns — User-provided glob patterns
// ---------------------------------------------------------------------------

func TestIgnoreMatcherUserPatterns(t *testing.T) {
	rootDir := t.TempDir()
	patterns := []string{"*.log", "build/", "temp_*"}
	im := NewIgnoreMatcher(rootDir, patterns)

	userIgnored := []string{
		filepath.Join(rootDir, "app.log"),
		filepath.Join(rootDir, "subdir", "debug.log"),
		filepath.Join(rootDir, "build"),
		filepath.Join(rootDir, "build", "output.exe"),
		filepath.Join(rootDir, "temp_cache"),
	}

	for _, path := range userIgnored {
		if !im.Match(path) {
			t.Errorf("expected user pattern to match %q", path)
		}
	}

	// Paths that should NOT be ignored by user patterns.
	notIgnored := []string{
		filepath.Join(rootDir, "document.pdf"),
		filepath.Join(rootDir, "src", "main.go"),
	}

	for _, path := range notIgnored {
		if im.Match(path) {
			t.Errorf("expected %q to NOT be ignored by user patterns", path)
		}
	}
}

// ---------------------------------------------------------------------------
// TestIgnoreMatcherNegation — Negation patterns
// ---------------------------------------------------------------------------

func TestIgnoreMatcherNegation(t *testing.T) {
	rootDir := t.TempDir()
	// Ignore all .log files, but re-include important.log.
	patterns := []string{"*.log", "!important.log"}
	im := NewIgnoreMatcher(rootDir, patterns)

	// Regular .log files should be ignored.
	if !im.Match(filepath.Join(rootDir, "debug.log")) {
		t.Error("expected debug.log to be ignored")
	}

	// important.log should NOT be ignored (negated).
	if im.Match(filepath.Join(rootDir, "important.log")) {
		t.Error("expected important.log to NOT be ignored (negation pattern)")
	}

	// Non-log files should not be ignored.
	if im.Match(filepath.Join(rootDir, "document.pdf")) {
		t.Error("expected document.pdf to NOT be ignored")
	}
}

// ---------------------------------------------------------------------------
// TestLoadIgnoreFile — Loading .gcrypt-ignore file from disk
// ---------------------------------------------------------------------------

func TestLoadIgnoreFile(t *testing.T) {
	// Create a temp .gcrypt-ignore file.
	tmpDir := t.TempDir()
	ignorePath := filepath.Join(tmpDir, ".gcrypt-ignore")

	content := `# This is a comment
*.log
build/
!important.log

# Another comment
*.bak
`
	if err := os.WriteFile(ignorePath, []byte(content), 0600); err != nil {
		t.Fatalf("failed to write ignore file: %v", err)
	}

	patterns, err := LoadIgnoreFile(ignorePath)
	if err != nil {
		t.Fatalf("LoadIgnoreFile failed: %v", err)
	}

	expected := []string{"*.log", "build/", "!important.log", "*.bak"}
	if len(patterns) != len(expected) {
		t.Fatalf("expected %d patterns, got %d: %v", len(expected), len(patterns), patterns)
	}

	for i, exp := range expected {
		if patterns[i] != exp {
			t.Errorf("pattern[%d]: got %q, want %q", i, patterns[i], exp)
		}
	}

	// Test loading non-existent file.
	_, err = LoadIgnoreFile(filepath.Join(tmpDir, "nonexistent"))
	if err == nil {
		t.Error("expected error when loading non-existent ignore file, got nil")
	}
}
