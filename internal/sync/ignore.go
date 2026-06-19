// Package sync implements file monitoring, scanning, and synchronization
// for the gcrypt project.
package sync

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IgnoreMatcher evaluates file paths against a set of ignore patterns,
// supporting gitignore-style syntax including negation, root anchoring,
// and directory-only patterns.
type IgnoreMatcher struct {
	patterns []string
	rootDir  string
}

// NewIgnoreMatcher creates a new IgnoreMatcher rooted at rootDir with the
// given user-provided patterns.
func NewIgnoreMatcher(rootDir string, patterns []string) *IgnoreMatcher {
	return &IgnoreMatcher{
		patterns: patterns,
		rootDir:  filepath.Clean(rootDir),
	}
}

// Match returns true if the given path should be ignored.
//
// Evaluation order:
//  1. Built-in patterns (always ignored)
//  2. User patterns in order — later patterns override earlier ones
//     (negation with "!" prefix re-includes previously excluded files)
func (im *IgnoreMatcher) Match(path string) bool {
	rel, err := filepath.Rel(im.rootDir, filepath.Clean(path))
	if err != nil {
		rel = path
	}
	rel = filepath.ToSlash(rel)

	// Always ignore built-in patterns.
	if im.matchBuiltIn(rel) {
		return true
	}

	// Evaluate user patterns in order; later rules win.
	result := false
	for _, pattern := range im.patterns {
		negate := false
		p := pattern

		if strings.HasPrefix(p, "!") {
			negate = true
			p = p[1:]
		}

		if im.matchPattern(rel, p) {
			result = !negate
		}
	}

	return result
}

// matchBuiltIn checks the path against the hard-coded ignore list that
// cannot be overridden by user patterns.
func (im *IgnoreMatcher) matchBuiltIn(rel string) bool {
	builtIn := []string{
		".gcrypt",
		".gcrypt-ignore",
		"Thumbs.db",
		".DS_Store",
		"desktop.ini",
	}

	name := filepath.Base(rel)
	for _, b := range builtIn {
		if strings.EqualFold(name, b) {
			return true
		}
	}

	// Glob patterns for built-in ignores. These cover transient editor/office
	// artifacts that are never meaningful to sync: temp files (*.tmp, *.swp),
	// MS Office owner files (~$*), editor backup files (*~), and LibreOffice/
	// OpenOffice lock files (.~lock.<name>#).
	globBuiltIn := []string{"*.tmp", "~$*", "*.swp", "*~", ".~lock.*#"}
	for _, pat := range globBuiltIn {
		if matched, _ := filepath.Match(pat, name); matched {
			return true
		}
	}

	// Directory names that are never worth syncing — gcrypt's own metadata,
	// version-control directories, and dependency trees. A file is ignored when
	// ANY component of its path is one of these, so they are skipped at any depth
	// (a previous version only matched them at the sync root, so nested copies
	// such as a sub-project's .git/ or node_modules/ leaked through and flooded
	// the queue). These are built-in and cannot be re-included by user patterns.
	neverSyncDirs := map[string]bool{
		".gcrypt":      true,
		".git":         true,
		".svn":         true,
		".hg":          true,
		"node_modules": true,
	}
	for _, comp := range strings.Split(rel, "/") {
		if neverSyncDirs[comp] {
			return true
		}
	}

	return false
}

// matchPattern evaluates a single gitignore-style pattern against a
// relative path.
func (im *IgnoreMatcher) matchPattern(rel, pattern string) bool {
	p := pattern
	dirOnly := false

	// Trailing / means the pattern only matches directories.
	if strings.HasSuffix(p, "/") {
		dirOnly = true
		p = strings.TrimSuffix(p, "/")
	}

	// Leading / anchors the pattern to the sync root.
	anchored := false
	if strings.HasPrefix(p, "/") {
		anchored = true
		p = p[1:]
	}

	// Handle ** patterns (match any path segment including /).
	if strings.Contains(p, "**") {
		return im.matchDoubleStar(rel, p, anchored, dirOnly)
	}

	// Simple glob matching.
	if anchored {
		// Match from root of sync dir.
		matched, _ := filepath.Match(p, rel)
		if matched {
			return true
		}
		// Also try matching the full relative path prefix.
		matched, _ = filepath.Match(p, filepath.Base(rel))
		return matched
	}

	// Unanchored: match anywhere in the path.
	// Try matching the full relative path first.
	if matched, _ := filepath.Match(p, rel); matched {
		return true
	}

	// Try matching each path component.
	parts := strings.Split(rel, "/")
	for i := 0; i < len(parts); i++ {
		sub := strings.Join(parts[i:], "/")
		if matched, _ := filepath.Match(p, sub); matched {
			return true
		}
	}

	// If dirOnly, also check directory components.
	if dirOnly {
		dir := filepath.Dir(rel)
		if dir != "." && dir != rel {
			if matched, _ := filepath.Match(p, dir); matched {
				return true
			}
		}
	}

	return false
}

// matchDoubleStar handles patterns containing ** which match any number
// of path segments (including zero).
func (im *IgnoreMatcher) matchDoubleStar(rel, pattern string, anchored, dirOnly bool) bool {
	// Split pattern on ** and match segment by segment.
	segments := strings.Split(pattern, "**")
	if len(segments) == 2 {
		prefix := segments[0]
		suffix := segments[1]

		prefix = strings.Trim(prefix, "/")
		suffix = strings.Trim(suffix, "/")

		parts := strings.Split(rel, "/")

		for i := 0; i <= len(parts); i++ {
			var sub string
			if i < len(parts) {
				sub = strings.Join(parts[i:], "/")
			}

			if prefix != "" {
				if i == 0 {
					// Check if rel starts with prefix.
					if !strings.HasPrefix(rel, prefix) && !strings.HasPrefix(rel, strings.TrimSuffix(prefix, "/")+"/") {
						if matched, _ := filepath.Match(prefix, rel); !matched {
							continue
						}
					}
				}
			}

			if suffix != "" && sub != "" {
				if matched, _ := filepath.Match(suffix, sub); matched {
					return true
				}
				// Also try matching suffix against the tail.
				for j := 0; j < len(parts); j++ {
					tail := strings.Join(parts[j:], "/")
					if matched, _ := filepath.Match(suffix, tail); matched {
						return true
					}
				}
			} else if suffix == "" {
				// Pattern ends with **, matches everything after prefix.
				if prefix == "" || strings.HasPrefix(rel, prefix) || strings.HasPrefix(rel, strings.TrimSuffix(prefix, "/")+"/") {
					return true
				}
			}
		}
	}

	return false
}

// LoadIgnoreFile reads a .gcrypt-ignore file and returns the list of
// patterns. Empty lines and lines starting with # are skipped.
func LoadIgnoreFile(path string) ([]string, error) {
	f, err := os.Open(path) // #nosec G304 -- path is the .gcrypt-ignore file inside the configured sync root
	if err != nil {
		return nil, fmt.Errorf("open ignore file: %w", err)
	}
	defer func() { _ = f.Close() }()

	var patterns []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read ignore file: %w", err)
	}

	return patterns, nil
}

// DefaultIgnorePatterns returns the built-in ignore patterns that are
// always applied in addition to any user-provided patterns.
func DefaultIgnorePatterns() []string {
	return []string{
		".gcrypt/",
		".gcrypt-ignore",
		"Thumbs.db",
		".DS_Store",
		"desktop.ini",
		"*.tmp",
		"~$*",
		"*.swp",
		"*~",
		".~lock.*#",
		"*.lock",
		".git/",
	}
}
