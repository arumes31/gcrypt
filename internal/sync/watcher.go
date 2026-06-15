package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/daniel/gcrypt/internal/models"
	"golang.org/x/sys/windows"
)

// Windows notification filter constants for ReadDirectoryChanges.
const (
	notifyFilter = windows.FILE_NOTIFY_CHANGE_FILE_NAME |
		windows.FILE_NOTIFY_CHANGE_DIR_NAME |
		windows.FILE_NOTIFY_CHANGE_ATTRIBUTES |
		windows.FILE_NOTIFY_CHANGE_SIZE |
		windows.FILE_NOTIFY_CHANGE_LAST_WRITE |
		windows.FILE_NOTIFY_CHANGE_CREATION |
		windows.FILE_NOTIFY_CHANGE_SECURITY

	// defaultBufferSize is the size of the buffer used for
	// ReadDirectoryChanges notifications (64 KB).
	defaultBufferSize = 64 * 1024

	// debounceInterval is the time window for debouncing rapid
	// successive changes to the same path.
	debounceInterval = 500 * time.Millisecond
)

// pendingChange holds a debounced file change waiting to be emitted.
type pendingChange struct {
	op      models.ChangeOp
	oldPath string
	time    time.Time
}

// Watcher monitors a local directory tree for file changes using the
// Windows ReadDirectoryChanges API and emits debounced ChangeEvents
// on a channel.
type Watcher struct {
	dir             string
	events          chan models.ChangeEvent
	done            chan struct{}
	mu              sync.Mutex
	ignoreMatcher   *IgnoreMatcher
	ignorePatterns  []string
	selectedFolders []string
	debounceMap     map[string]pendingChange // relative path → pending change
	debounceTimer   *time.Timer
	isRunning       bool
	bufferSize      int
	wg              sync.WaitGroup // tracks background goroutines
}

// WatcherOption is a functional option for configuring a Watcher.
type WatcherOption func(*Watcher)

// WithIgnorePatterns sets the list of gitignore-style patterns for
// files and directories that should be excluded from monitoring.
func WithIgnorePatterns(patterns []string) WatcherOption {
	return func(w *Watcher) {
		w.ignorePatterns = patterns
	}
}

// WithSelectedFolders sets the list of folders that should be monitored for changes.
func WithSelectedFolders(folders []string) WatcherOption {
	return func(w *Watcher) {
		w.selectedFolders = folders
	}
}

// WithBufferSize sets the read buffer size used for ReadDirectoryChanges
// notifications. The default is 64 KB.
func WithBufferSize(size int) WatcherOption {
	return func(w *Watcher) {
		w.bufferSize = size
	}
}

// NewWatcher creates a new Watcher for the given root directory.
// The directory must exist and be a valid directory on disk.
func NewWatcher(dir string, opts ...WatcherOption) (*Watcher, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("watcher: stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("watcher: %s is not a directory", dir)
	}

	w := &Watcher{
		dir:             filepath.Clean(dir),
		events:          make(chan models.ChangeEvent, 256),
		done:            make(chan struct{}),
		ignorePatterns:  DefaultIgnorePatterns(),
		selectedFolders: []string{},
		debounceMap:     make(map[string]pendingChange),
		bufferSize:      defaultBufferSize,
	}

	// Apply functional options.
	for _, opt := range opts {
		opt(w)
	}

	w.ignoreMatcher = NewIgnoreMatcher(w.dir, w.ignorePatterns)

	return w, nil
}

// Start begins watching the directory tree for changes. It launches
// background goroutines for the debounce processor and the main watch
// loop. Call Stop() to shut down.
func (w *Watcher) Start() error {
	w.mu.Lock()
	if w.isRunning {
		w.mu.Unlock()
		return fmt.Errorf("watcher: already running")
	}
	w.isRunning = true
	w.mu.Unlock()

	// Start the debounce processor.
	w.debounceTimer = time.NewTimer(debounceInterval)
	w.wg.Add(1)
	go w.processDebouncedEvents()

	// Start the main watch loop for the root directory.
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.readDirectoryChanges(w.dir)
	}()

	// Start watchers on all subdirectories.
	for _, sub := range w.walkSubdirs() {
		sub := sub // capture for goroutine
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.readDirectoryChanges(sub)
		}()
	}

	return nil
}

// Stop shuts down the watcher. It signals all goroutines to exit and
// waits for them to finish before closing the events channel.
func (w *Watcher) Stop() error {
	w.mu.Lock()
	if !w.isRunning {
		w.mu.Unlock()
		return nil
	}
	w.isRunning = false
	w.mu.Unlock()

	close(w.done)
	w.debounceTimer.Stop()

	// Wait for all goroutines to finish.
	w.wg.Wait()

	// Flush any remaining debounced events before closing.
	w.flushDebouncedEvents()

	close(w.events)
	return nil
}

// Events returns the read-only channel on which change events are
// emitted. Consumers should drain this channel to prevent backpressure.
func (w *Watcher) Events() <-chan models.ChangeEvent {
	return w.events
}

// readDirectoryChanges is the core watch loop for a single directory.
// It opens a handle to the directory, calls ReadDirectoryChanges in a
// loop, and parses the resulting FILE_NOTIFY_INFORMATION records.
func (w *Watcher) readDirectoryChanges(dir string) {
	// Convert directory path to a UTF16 pointer for Windows API.
	dirPtr, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return
	}

	// Open the directory handle with the required access rights.
	handle, err := windows.CreateFile(
		dirPtr,
		windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED,
		0,
	)
	if err != nil {
		return
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	// Allocate the notification buffer.
	buf := make([]byte, w.bufferSize)
	var overlapped windows.Overlapped
	var bytesReturned uint32

	// Create an event for overlapped I/O completion.
	overlapped.HEvent, err = windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return
	}
	defer func() { _ = windows.CloseHandle(overlapped.HEvent) }()

	for {
		// Issue the asynchronous ReadDirectoryChanges call.
		err := windows.ReadDirectoryChanges(
			handle,
			&buf[0],
			uint32(len(buf)),
			true, // watch subtree
			notifyFilter,
			&bytesReturned,
			&overlapped,
			0,
		)
		if err != nil {
			return
		}

		// Wait for the I/O to complete or for shutdown.
		waitResult, err := windows.WaitForSingleObject(overlapped.HEvent, 500)
		if err != nil {
			return
		}

		// Check for shutdown signal.
		select {
		case <-w.done:
			return
		default:
		}

		if waitResult != windows.WAIT_OBJECT_0 {
			// Timeout or error — retry the loop.
			continue
		}

		// Get the number of bytes transferred.
		var bytesTransferred uint32
		err = windows.GetOverlappedResult(handle, &overlapped, &bytesTransferred, false)
		if err != nil || bytesTransferred == 0 {
			// Reset the overlapped event for the next call.
			_ = windows.ResetEvent(overlapped.HEvent)
			continue
		}

		// Parse the notification records from the buffer.
		w.parseNotifyBuffer(buf[:bytesTransferred], dir)

		// Reset the overlapped event for the next call.
		_ = windows.ResetEvent(overlapped.HEvent)
	}
}

// parseNotifyBuffer parses a buffer of FILE_NOTIFY_INFORMATION records
// produced by ReadDirectoryChanges and feeds each change into the
// debounce map.
//
// The FILE_NOTIFY_INFORMATION structure is variable-length:
//
//	typedef struct _FILE_NOTIFY_INFORMATION {
//	    DWORD NextEntryOffset;
//	    DWORD Action;
//	    DWORD FileNameLength;   // in bytes
//	    WCHAR FileName[1];      // variable-length
//	} FILE_NOTIFY_INFORMATION;
func (w *Watcher) parseNotifyBuffer(buf []byte, watchDir string) {
	// Track rename pairs: old name → old relative path.
	var renameOldPath string

	offset := 0
	for {
		if offset+12 > len(buf) {
			break
		}

		// Read the fixed header fields.
		nextEntryOffset := *(*uint32)(unsafe.Pointer(&buf[offset]))
		action := *(*uint32)(unsafe.Pointer(&buf[offset+4]))
		fileNameLength := *(*uint32)(unsafe.Pointer(&buf[offset+8]))

		if offset+12+int(fileNameLength) > len(buf) {
			break
		}

		// Read the variable-length file name (UTF-16).
		fileNameBytes := buf[offset+12 : offset+12+int(fileNameLength)]
		fileNameUTF16 := make([]uint16, fileNameLength/2)
		for i := range fileNameUTF16 {
			fileNameUTF16[i] = *(*uint16)(unsafe.Pointer(&fileNameBytes[i*2]))
		}
		fileName := string(utf16.Decode(fileNameUTF16))

		// Build the full path and relative path.
		fullPath := filepath.Join(watchDir, fileName)
		relPath, err := filepath.Rel(w.dir, fullPath)
		if err != nil {
			relPath = fullPath
		}
		relPath = filepath.ToSlash(relPath)

		// Map the Windows action to a ChangeOp.
		switch action {
		case windows.FILE_ACTION_ADDED:
			w.addDebouncedChange(relPath, models.ChangeOpCreate, "")

		case windows.FILE_ACTION_REMOVED:
			w.addDebouncedChange(relPath, models.ChangeOpDelete, "")

		case windows.FILE_ACTION_MODIFIED:
			w.addDebouncedChange(relPath, models.ChangeOpModify, "")

		case windows.FILE_ACTION_RENAMED_OLD_NAME:
			// Store the old path; the next record should be
			// FILE_ACTION_RENAMED_NEW_NAME.
			renameOldPath = relPath

		case windows.FILE_ACTION_RENAMED_NEW_NAME:
			oldPath := renameOldPath
			renameOldPath = ""
			w.addDebouncedChange(relPath, models.ChangeOpRename, oldPath)
		}

		// Advance to the next entry.
		if nextEntryOffset == 0 {
			break
		}
		offset += int(nextEntryOffset)
	}
}

// addDebouncedChange inserts or updates a pending change in the debounce
// map. If a change for the same path already exists and is older than
// the debounce window, it is replaced with the newer change type.
func (w *Watcher) addDebouncedChange(relPath string, op models.ChangeOp, oldPath string) {
	// Check ignore patterns.
	if w.shouldIgnore(relPath) {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// Special collapse: if we see a DELETE followed by a CREATE on the
	// same path within the debounce window, collapse into MODIFY.
	if op == models.ChangeOpCreate {
		if existing, ok := w.debounceMap[relPath]; ok && existing.op == models.ChangeOpDelete {
			// Collapse DELETE + CREATE → MODIFY.
			w.debounceMap[relPath] = pendingChange{
				op:   models.ChangeOpModify,
				time: time.Now(),
			}
			return
		}
	}

	w.debounceMap[relPath] = pendingChange{
		op:      op,
		oldPath: oldPath,
		time:    time.Now(),
	}
}

// processDebouncedEvents runs as a goroutine, periodically checking the
// debounce map for entries that have matured past the debounce window
// and emitting them as ChangeEvents on the events channel.
func (w *Watcher) processDebouncedEvents() {
	defer w.wg.Done()

	for {
		select {
		case <-w.done:
			return
		case <-w.debounceTimer.C:
			w.flushDebouncedEvents()
			w.mu.Lock()
			if w.isRunning {
				w.debounceTimer.Reset(debounceInterval)
			}
			w.mu.Unlock()
		}
	}
}

// flushDebouncedEvents emits all pending changes that are older than
// the debounce interval and removes them from the debounce map.
func (w *Watcher) flushDebouncedEvents() {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := time.Now()
	for relPath, pc := range w.debounceMap {
		if now.Sub(pc.time) >= debounceInterval {
			evt := models.ChangeEvent{
				Path:      relPath,
				Op:        pc.op,
				OldPath:   pc.oldPath,
				Timestamp: pc.time,
			}

			select {
			case w.events <- evt:
			default:
				// Channel full — drop the event to avoid blocking.
				// In production this should log a warning.
			}

			delete(w.debounceMap, relPath)
		}
	}
}

// shouldIgnore returns true if the given relative path matches any
// ignore pattern and should be excluded from monitoring.
func (w *Watcher) shouldIgnore(relPath string) bool {
	// Convert back to OS-specific path for the matcher.
	osPath := filepath.FromSlash(relPath)
	fullPath := filepath.Join(w.dir, osPath)

	// Check ignore patterns first
	if w.ignoreMatcher.Match(fullPath) {
		return true
	}

	// If selected folders are specified, check if the path is in one of them
	if len(w.selectedFolders) > 0 {
		// Check if the file is in any of the selected folders
		inSelectedFolder := false
		for _, folder := range w.selectedFolders {
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

		// If not in any selected folder, ignore it
		if !inSelectedFolder {
			return true
		}
	}

	return false
}

// walkSubdirs returns a list of all subdirectory paths under the
// watcher's root directory.
func (w *Watcher) walkSubdirs() []string {
	var dirs []string

	_ = filepath.WalkDir(w.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsPermission(err) && d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip the root itself.
		if path == w.dir {
			return nil
		}

		if !d.IsDir() {
			return nil
		}

		// Skip ignored directories.
		if w.ignoreMatcher.Match(path) {
			return filepath.SkipDir
		}

		dirs = append(dirs, path)
		return nil
	})

	return dirs
}
