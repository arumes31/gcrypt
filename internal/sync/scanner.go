package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/arumes31/gcrypt/internal/models"
	"golang.org/x/sync/errgroup"
)

// hashCacheEntry is a cached content hash together with the file stamp it was
// computed from, so the cache can be invalidated when the file changes.
type hashCacheEntry struct {
	size    int64
	modTime time.Time
	digest  string
}

// persistedHashEntry is the JSON-serialisable form of hashCacheEntry, used to
// persist the hash cache across restarts so a cold start doesn't re-hash the
// whole tree. ModTime marshals as RFC3339Nano, so it round-trips exactly.
type persistedHashEntry struct {
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	Digest  string    `json:"hash"`
}

// LoadHashCache seeds the in-memory hash cache from a JSON file previously
// written by SaveHashCache. It returns the underlying error when the file is
// missing or unreadable (and on malformed JSON), so the caller can distinguish a
// cold start — a missing cache file — from a corrupt one. Entries are validated
// against the live file stamp on use, so a stale entry can never return a wrong
// hash — at worst it forces a recompute.
func (s *Scanner) LoadHashCache(path string) error {
	data, err := os.ReadFile(path) // #nosec G304 -- path is the app's own hash-cache file under its data dir
	if err != nil {
		return err
	}
	var in map[string]persistedHashEntry
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	s.muCache.Lock()
	for k, v := range in {
		s.hashCache[k] = hashCacheEntry{size: v.Size, modTime: v.ModTime, digest: v.Digest}
	}
	s.muCache.Unlock()
	return nil
}

// SaveHashCache writes the in-memory hash cache to path atomically (temp file +
// rename) so a crash can't leave a half-written cache.
func (s *Scanner) SaveHashCache(path string) error {
	s.muCache.RLock()
	out := make(map[string]persistedHashEntry, len(s.hashCache))
	for k, v := range s.hashCache {
		out[k] = persistedHashEntry{Size: v.size, ModTime: v.modTime, Digest: v.digest}
	}
	s.muCache.RUnlock()

	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Scanner performs full and single-file directory scans, producing
// SyncFile records with SHA-256 content hashes for change detection.
type Scanner struct {
	dir             string
	ignore          *IgnoreMatcher
	selectedFolders []string
	hashCache       map[string]hashCacheEntry // absolute path → cached hash + stamp
	muCache         sync.RWMutex              // protects hashCache
}

// NewScanner creates a new Scanner for the given directory root.
// ignorePatterns are used to skip files and directories that should
// not be included in scan results.
func NewScanner(dir string, ignorePatterns []string, selectedFolders []string) *Scanner {
	return &Scanner{
		dir:             filepath.Clean(dir),
		ignore:          NewIgnoreMatcher(dir, ignorePatterns),
		selectedFolders: selectedFolders,
		hashCache:       make(map[string]hashCacheEntry),
	}
}

// ShouldIgnore reports whether the given path is excluded by this scanner's
// ignore rules — the pair's user patterns plus the built-in never-sync set
// (.git, node_modules, …) matched at any depth. The local walk applies these
// implicitly; exposing them lets the remote reconcile apply the exact same
// exclusions, so a file ignored locally is never pulled back from Drive (the
// old upload path may have pushed now-ignored trees to the cloud). The path may
// be absolute or relative to the sync root; Match normalises it either way.
func (s *Scanner) ShouldIgnore(path string) bool {
	return s.ignore.Match(path)
}

// Scan performs a full recursive walk of the sync directory and returns
// a SyncFile entry for every non-ignored file found.
//
// Scan buffers the entire result in memory. For large sync roots prefer
// ScanStream, which emits files as they are discovered so callers can begin
// processing (e.g. uploading) before the walk completes.
func (s *Scanner) Scan() ([]*models.SyncFile, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan *models.SyncFile, 100)

	var files []*models.SyncFile
	done := make(chan struct{})
	go func() {
		for sf := range out {
			files = append(files, sf)
		}
		close(done)
	}()

	err := s.ScanStream(ctx, out)
	close(out)
	<-done

	if err != nil {
		return nil, fmt.Errorf("scan directory: %w", err)
	}
	return files, nil
}

// ScanStream performs a full recursive walk of the sync directory and sends a
// SyncFile entry for every non-ignored regular file to out as it is discovered
// and hashed. This lets callers begin processing files before the walk
// finishes, rather than waiting for a complete in-memory slice.
//
// ScanStream returns when the walk and all hashing complete, or when ctx is
// cancelled. The caller retains ownership of out and is responsible for closing
// it after ScanStream returns. Sends to out are subject to ctx cancellation, so
// a caller applying backpressure (a slow reader) naturally throttles the scan.
func (s *Scanner) ScanStream(ctx context.Context, out chan<- *models.SyncFile) error {
	g, ctx := errgroup.WithContext(ctx)

	// workCh buffers files to be hashed.
	type scanWork struct {
		path string
		info os.FileInfo
	}
	workCh := make(chan scanWork, 100)

	// Start a bounded worker pool for hashing files.
	numWorkers := runtime.NumCPU() * 2
	for i := 0; i < numWorkers; i++ {
		g.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case w, ok := <-workCh:
					if !ok {
						return nil
					}
					sf, err := s.buildSyncFile(w.path, w.info)
					if err != nil {
						// Skip files we cannot hash (e.g. locked by another process).
						continue
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case out <- sf:
					}
				}
			}
		})
	}

	// Producer: walk the directory and enqueue work.
	g.Go(func() error {
		defer close(workCh)
		return filepath.WalkDir(s.dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if os.IsPermission(err) {
					if d != nil && d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				return fmt.Errorf("walk %s: %w", path, err)
			}

			if path == s.dir {
				return nil
			}

			if s.ignore.Match(path) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			if len(s.selectedFolders) > 0 {
				relPath, err := filepath.Rel(s.dir, path)
				if err != nil {
					return nil
				}
				relPath = filepath.ToSlash(relPath)
				inSelectedFolder := false
				for _, folder := range s.selectedFolders {
					normalizedFolder := filepath.ToSlash(filepath.Clean(folder))
					if normalizedFolder == "." {
						normalizedFolder = ""
					}
					if relPath == normalizedFolder || strings.HasPrefix(relPath, normalizedFolder+"/") {
						inSelectedFolder = true
						break
					}
				}
				if !inSelectedFolder {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
			}

			if d.IsDir() {
				return nil
			}

			info, err := d.Info()
			if err != nil {
				return nil
			}

			if !info.Mode().IsRegular() {
				return nil
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case workCh <- scanWork{path: path, info: info}:
			}

			return nil
		})
	})

	return g.Wait()
}

// inSelectedFolder reports whether the slash-relative path falls within the
// configured selected folders. With none configured, everything is in scope.
func (s *Scanner) inSelectedFolder(rel string) bool {
	if len(s.selectedFolders) == 0 {
		return true
	}
	for _, folder := range s.selectedFolders {
		nf := filepath.ToSlash(filepath.Clean(folder))
		if nf == "." {
			nf = ""
		}
		if nf == "" || rel == nf || strings.HasPrefix(rel, nf+"/") {
			return true
		}
	}
	return false
}

// EmptyDirs returns the relative, slash-separated paths of directories under the
// sync root that contain no non-ignored regular files anywhere in their subtree.
// Such directories would otherwise never be represented remotely, because remote
// folders are normally created only as a side effect of uploading a file. It
// honours the same ignore and selected-folder rules as ScanStream.
func (s *Scanner) EmptyDirs() ([]string, error) {
	dirs := make(map[string]struct{})
	withFile := make(map[string]struct{})

	walkErr := filepath.WalkDir(s.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsPermission(err) {
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			return err
		}
		if p == s.dir {
			return nil
		}
		if s.ignore.Match(p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(s.dir, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		if !s.inSelectedFolder(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			dirs[rel] = struct{}{}
			return nil
		}
		if info, ierr := d.Info(); ierr != nil || !info.Mode().IsRegular() {
			return nil
		}
		// Mark every ancestor directory as containing a file.
		for a := path.Dir(rel); a != "." && a != "/" && a != ""; a = path.Dir(a) {
			withFile[a] = struct{}{}
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	var empty []string
	for dir := range dirs {
		if _, has := withFile[dir]; !has {
			empty = append(empty, dir)
		}
	}
	return empty, nil
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
// chunks. The result is cached, keyed by the file's size and modification time
// so that a cached digest is reused only while the file is unchanged. A cache
// keyed by path alone would return a stale hash after the file was modified,
// causing later changes to go undetected.
func (s *Scanner) ComputeHash(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}

	// Stat the file so the cache entry can be validated against the current
	// size and modification time.
	info, statErr := os.Stat(absPath)
	if statErr == nil {
		s.muCache.RLock()
		entry, ok := s.hashCache[absPath]
		s.muCache.RUnlock()
		if ok && entry.size == info.Size() && entry.modTime.Equal(info.ModTime()) {
			return entry.digest, nil
		}
	}

	f, err := os.Open(absPath) // #nosec G304 -- absPath is a file within the configured sync root being hashed
	if err != nil {
		return "", fmt.Errorf("open for hash: %w", err)
	}
	defer func() { _ = f.Close() }()

	hash := sha256.New()
	buf := make([]byte, 32*1024) // 32 KB chunks
	if _, err := io.CopyBuffer(hash, f, buf); err != nil {
		return "", fmt.Errorf("read for hash: %w", err)
	}

	digest := hex.EncodeToString(hash.Sum(nil))

	if statErr == nil {
		s.muCache.Lock()
		s.hashCache[absPath] = hashCacheEntry{size: info.Size(), modTime: info.ModTime(), digest: digest}
		s.muCache.Unlock()
	}

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
