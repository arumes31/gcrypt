package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoggerRotationKeepsCorrectBackups verifies that rotation shifts backups
// correctly and keeps exactly maxBackups files, with the most recent rotated
// content in .1 (regression test for the off-by-one shift that dropped a
// backup and overwrote another).
func TestLoggerRotationKeepsCorrectBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	logger, err := NewLogger(path)
	if err != nil {
		t.Fatalf("NewLogger failed: %v", err)
	}
	defer func() { _ = logger.Close() }()

	// Force a rotation on every write after the first, keeping 2 backups.
	logger.SetMaxSize(1)
	logger.SetMaxBackups(2)

	// Write five distinguishable lines: L0..L4.
	for i := 0; i < 5; i++ {
		logger.Info("L" + string(rune('0'+i)))
	}

	read := func(p string) string {
		b, err := os.ReadFile(p) // #nosec G304 -- p is a test temp file
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		return string(b)
	}

	// Current log holds the newest line; .1 the previous; .2 the one before.
	if got := read(path); !strings.Contains(got, "L4") {
		t.Errorf("current log should contain L4, got %q", got)
	}
	if got := read(path + ".1"); !strings.Contains(got, "L3") {
		t.Errorf("%s.1 should contain L3, got %q", path, got)
	}
	if got := read(path + ".2"); !strings.Contains(got, "L2") {
		t.Errorf("%s.2 should contain L2, got %q", path, got)
	}

	// Only maxBackups (2) backups are kept — no .3.
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Errorf("expected no %s.3 backup, stat err = %v", path, err)
	}
}

// TestLoggerLevelFiltering verifies that messages below the configured level
// are dropped.
func TestLoggerLevelFiltering(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "level.log")

	logger, err := NewLogger(path)
	if err != nil {
		t.Fatalf("NewLogger failed: %v", err)
	}
	defer func() { _ = logger.Close() }()

	logger.SetLevel("warn")
	logger.Debug("dbg-message")
	logger.Info("inf-message")
	logger.Warn("wrn-message")
	logger.Error("err-message")

	b, err := os.ReadFile(path) // #nosec G304 -- path is a test temp file
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	content := string(b)

	if strings.Contains(content, "dbg-message") {
		t.Error("debug message should be filtered out at warn level")
	}
	if strings.Contains(content, "inf-message") {
		t.Error("info message should be filtered out at warn level")
	}
	if !strings.Contains(content, "wrn-message") {
		t.Error("warn message should be present at warn level")
	}
	if !strings.Contains(content, "err-message") {
		t.Error("error message should be present at warn level")
	}
}
