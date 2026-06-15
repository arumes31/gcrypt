package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/daniel/gcrypt/internal/models"
	"golang.org/x/sync/errgroup"
)


// Scanner performs full and single-file directory scans, producing
// SyncFile records with SHA-256 content hashes for change detection.
type Scanner struct {
	dir             string
	ignore          *IgnoreMatcher
	selectedFolders []string
	hashCache       map[string]string // absolute path → hex-encoded SHA-256
}

// NewScanner creates a new Scanner for the given directory root.
// ignorePatterns are used to skip files and directories that should
// not be included in scan results.
func NewScanner(dir string, ignorePatterns []string, selectedFolders []string) *Scanner {
	return &Scanner{
		dir:             filepath.Clean(dir),
		ignore:          NewIgnoreMatcher(dir, ignorePatterns),
		selectedFolders: selectedFolders,
		hashCache:       make(map[string]string),
	}
}

// Scan performs a full recursive walk of the sync directory and returns
// a SyncFile entry for every non-ignored file found.
func (s *Scanner) Scan() ([]*models.SyncFile, error) {
	var files []*models.SyncFile
	var mu sync.Mutex

	g := new(errgroup.Group)
	g.SetLimit(runtime.NumCPU() * 2)

	err := filepath.WalkDir(s.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Handle permission errors gracefully — skip files we can't access.
			if os.IsPermission(err) {
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			return fmt.Errorf("walk %s: %w", path, err)
		}

		// Skip the root directory itself.
		if path == s.dir {
			return nil
		}

		// Check ignore patterns.
		if s.ignore.Match(path) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Check if file is in selected folders (if any are specified)
		if len(s.selectedFolders) > 0 {
			relPath, err := filepath.Rel(s.dir, path)
			if err != nil {
				return nil // Skip if we can't get relative path
			}

			// Convert to slash-separated path for consistent comparison
			relPath = filepath.ToSlash(relPath)

			// Check if the file is in any of the selected folders
			inSelectedFolder := false
			for _, folder := range s.selectedFolders {
				// Normalize the folder path
				normalizedFolder := filepath.ToSlash(filepath.Clean(folder))
				if normalizedFolder == "." {
					normalizedFolder = ""
				}

				// Check if the relative path starts with the selected folder
				if relPath == normalizedFolder || strings.HasPrefix(relPath, normalizedFolder+"/") {
					inSelectedFolder = true
					break
				}
			}

			if !inSelectedFolder {
				// Skip if not in any selected folder
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		// Only process regular files.
		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			// Skip files whose info we cannot read.
			return nil
		}

		// Skip non-regular files (symlinks, devices, etc.).
		if !info.Mode().IsRegular() {
			return nil
		}

		// Schedule file hashing and struct building on the worker pool.
		g.Go(func() error {
			sf, err := s.buildSyncFile(path, info)
			if err != nil {
				// Skip files we cannot hash (e.g. locked by another process).
				return nil
			}

			mu.Lock()
			files = append(files, sf)
			mu.Unlock()
			return nil
		})

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("scan directory walk: %w", err)
	}

	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("scan directory process: %w", err)
	}

	return files, nil
}

// ScanSingle scans a single file and returns its SyncFile record.
func (s *Scanner) ScanSingle(path string) (*models.SyncFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}

	return s.buildSyncFile(path, info)
}

// buildSyncFile creates a SyncFile from the given path and file info.
func (s *Scanner) buildSyncFile(path string, info os.FileInfo) (*models.SyncFile, error) {
	relPath, err := filepath.Rel(s.dir, path)
	if err != nil {
		return nil, fmt.Errorf("relative path for %s: %w", path, err)
	}

	hash, err := s.ComputeHash(path)
	if err != nil {
		return nil, fmt.Errorf("hash %s: %w", path, err)
	}

	return &models.SyncFile{
		LocalPath:  relPath,
		LocalHash:  hash,
		Size:       info.Size(),
		ModTime:    info.ModTime(),
		SyncStatus: models.SyncStatusPending,
	}, nil
}

// ComputeHash computes the SHA-256 hash of a file by reading it in 32 KB
// chunks. The result is cached so that repeated calls for the same path
// return the cached hex-encoded digest without re-reading the file.
func (s *Scanner) ComputeHash(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}

	// Return cached result if available.
	if h, ok := s.hashCache[absPath]; ok {
		return h, nil
	}

	f, err := os.Open(absPath)
	if err != nil {
		return "", fmt.Errorf("open for hash: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	buf := make([]byte, 32*1024) // 32 KB chunks
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return "", fmt.Errorf("read for hash: %w", err)
	}

	digest := hex.EncodeToString(h.Sum(nil))
	s.hashCache[absPath] = digest

	return digest, nil
}

// DiffScan compares two scan snapshots and returns the ChangeEvents
// representing the differences between them.
//
// Detection rules:
//   - File in current but not in previous → create
//   - File in both but hash differs       → modify
//   - File in previous but not in current → delete
func DiffScan(previous, current []*models.SyncFile) ([]*models.ChangeEvent, error) {
	var events []*models.ChangeEvent

	// Build lookup map of previous files by relative path.
	prevMap := make(map[string]*models.SyncFile, len(previous))
	for _, sf := range previous {
		prevMap[sf.LocalPath] = sf
	}

	// Build lookup map of current files by relative path.
	curMap := make(map[string]*models.SyncFile, len(current))
	for _, sf := range current {
		curMap[sf.LocalPath] = sf
	}

	now := time.Now()

	// Detect creates and modifications.
	for _, sf := range current {
		prev, existed := prevMap[sf.LocalPath]
		if !existed {
			events = append(events, &models.ChangeEvent{
				Path:      sf.LocalPath,
				Op:        models.ChangeOpCreate,
				Timestamp: now,
			})
			continue
		}

		if sf.LocalHash != prev.LocalHash {
			events = append(events, &models.ChangeEvent{
				Path:      sf.LocalPath,
				Op:        models.ChangeOpModify,
				Timestamp: now,
			})
		}
	}

	// Detect deletions.
	for _, sf := range previous {
		if _, exists := curMap[sf.LocalPath]; !exists {
			events = append(events, &models.ChangeEvent{
				Path:      sf.LocalPath,
				Op:        models.ChangeOpDelete,
				Timestamp: now,
			})
		}
	}

	return events, nil
}
