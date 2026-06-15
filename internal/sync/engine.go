package sync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daniel/gcrypt/internal/appstate"
	"github.com/daniel/gcrypt/internal/config"
	"github.com/daniel/gcrypt/internal/crypto"
	"github.com/daniel/gcrypt/internal/drive"
	"github.com/daniel/gcrypt/internal/models"
)

// ---------------------------------------------------------------------------
// State enum
// ---------------------------------------------------------------------------

// SyncState represents the current operational state of the sync engine.
type SyncState string

const (
	StateIdle         SyncState = "idle"
	StateScanning     SyncState = "scanning"
	StateSyncing      SyncState = "syncing"
	StateError        SyncState = "error"
	StatePaused       SyncState = "paused"
	StateDisconnected SyncState = "disconnected"
)

// ---------------------------------------------------------------------------
// SyncEngine interface
// ---------------------------------------------------------------------------

// SyncEngine defines the interface for a sync engine. The tray and manager
// depend on this interface rather than the concrete Engine type.
type SyncEngine interface {
	ID() string
	Start() error
	StartAsync() error
	Stop() error
	Pause()
	Resume()
	SyncNow()
	StateChanges() <-chan SyncState
	Errors() <-chan error
	Stats() SyncStats
	State() SyncState
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

// SyncStats holds cumulative counters for sync operations.
type SyncStats struct {
	FilesUploaded   int64
	FilesDownloaded int64
	FilesDeleted    int64
	BytesUploaded   int64
	BytesDownloaded int64
	Errors          int64
	LastSyncTime    time.Time
	LastError       string
}

// SyncActivity is a point-in-time snapshot of in-flight and queued work,
// used to drive the tray's live activity display.
type SyncActivity struct {
	Pending int      // operations waiting in the work queue
	Active  int      // operations currently being processed
	Current []string // short descriptions of the in-flight operations (e.g. "↑ report.pdf")
}

// ---------------------------------------------------------------------------
// Engine
// ---------------------------------------------------------------------------

// Engine is the central orchestrator that connects the file watcher, crypto
// module, and Drive client together to perform end-to-end encrypted
// synchronisation of a local directory with Google Drive.
type Engine struct {
	pair        *config.SyncPair
	appCfg      *config.AppConfig
	driveClient *drive.Client
	store       *drive.Store
	watcher     *Watcher
	scanner     *Scanner
	masterKey   []byte

	state SyncState
	stats SyncStats
	mu    sync.RWMutex

	// activeOps tracks the operation each worker is currently executing,
	// keyed by worker id. Guarded by mu. Used for the live activity display.
	activeOps map[int]string

	// pendingOps counts operations that have been enqueued but not yet
	// terminally completed (queued + in-flight + awaiting retry). It is the
	// real backlog size, independent of the bounded workQueue channel buffer.
	// Accessed atomically.
	pendingOps int64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	workQueue     chan *models.SyncOperation
	errorCh       chan error
	stateChangeCh chan SyncState

	maxRetries  int
	workers     int
	rateLimiter *time.Ticker

	// syncNowCh is used by SyncNow() to trigger an immediate full scan.
	syncNowCh chan struct{}

	// OnError is a channel that receives errors from background operations
	// (e.g. async scan failures). The tray or controller can listen on this
	// channel to be notified of asynchronous errors without polling.
	OnError chan error

	// OnStateChange is an optional callback invoked when the engine's
	// appstate-level state transitions. This allows the tray/controller to
	// react to lifecycle transitions (Scanning → Syncing → Idle, etc.)
	// without watching the SyncState channel.
	OnStateChange func(oldState, newState appstate.State)
}

// EngineOption is a functional option for configuring an Engine.
type EngineOption func(*Engine)

// WithMaxRetries sets the maximum number of retry attempts for failed
// operations. The default is 5.
func WithMaxRetries(n int) EngineOption {
	return func(e *Engine) { e.maxRetries = n }
}

// WithWorkers sets the number of concurrent worker goroutines. The default
// is 3.
func WithWorkers(n int) EngineOption {
	return func(e *Engine) { e.workers = n }
}

// ---------------------------------------------------------------------------
// Constructor
// ---------------------------------------------------------------------------

// NewEngine creates a new sync Engine for the given SyncPair. All parameters
// must be non-nil. Functional options may be supplied to override default
// settings. The engine's sync root ID is derived from pair.ID automatically.
func NewEngine(pair *config.SyncPair, appCfg *config.AppConfig, driveClient *drive.Client, store *drive.Store, masterKey []byte, opts ...EngineOption) (*Engine, error) {
	if pair == nil {
		return nil, fmt.Errorf("sync: pair must not be nil")
	}
	if appCfg == nil {
		return nil, fmt.Errorf("sync: app config must not be nil")
	}
	if driveClient == nil {
		return nil, fmt.Errorf("sync: drive client must not be nil")
	}
	if store == nil {
		return nil, fmt.Errorf("sync: store must not be nil")
	}
	if masterKey == nil {
		return nil, fmt.Errorf("sync: master key must not be nil")
	}

	// Create watcher for the sync directory with ignore patterns from the pair.
	watcher, err := NewWatcher(pair.LocalDir, WithIgnorePatterns(pair.EffectiveIgnorePatterns()), WithSelectedFolders(pair.SelectedFolders))
	if err != nil {
		return nil, fmt.Errorf("sync: create watcher: %w", err)
	}

	// Create scanner for the sync directory.
	scanner := NewScanner(pair.LocalDir, pair.EffectiveIgnorePatterns(), pair.SelectedFolders)

	e := &Engine{
		pair:          pair,
		appCfg:        appCfg,
		driveClient:   driveClient,
		store:         store,
		watcher:       watcher,
		scanner:       scanner,
		masterKey:     masterKey,
		state:         StateIdle,
		activeOps:     make(map[int]string),
		workQueue:     make(chan *models.SyncOperation, 1000),
		errorCh:       make(chan error, 256),
		stateChangeCh: make(chan SyncState, 64),
		syncNowCh:     make(chan struct{}, 1),
		OnError:       make(chan error, 64),
		maxRetries:    5,
		workers:       3,
		rateLimiter:   time.NewTicker(100 * time.Millisecond), // ~10 req/s
	}

	// Apply functional options.
	for _, opt := range opts {
		opt(e)
	}

	return e, nil
}

// ---------------------------------------------------------------------------
// Identity & Immediate Sync
// ---------------------------------------------------------------------------

// ID returns the unique identifier of the sync pair this engine manages.
func (e *Engine) ID() string {
	return e.pair.ID
}

// SyncNow triggers an immediate full scan cycle, similar to what the remote
// poller does on its timer, but one-shot. It is non-blocking; the scan will
// be picked up by the remote poller goroutine.
func (e *Engine) SyncNow() {
	select {
	case e.syncNowCh <- struct{}{}:
	default:
		// A sync-now request is already pending; ignore the duplicate.
	}
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// Start launches the sync engine: starts workers and the event processor
// first, then performs an initial scan and enqueues work items, and finally
// starts the watcher and remote poller. Starting consumers before producers
// prevents a deadlock when the diff loop sends more items than the workQueue
// buffer (cap 1000) can hold.
func (e *Engine) Start() error {
	e.ctx, e.cancel = context.WithCancel(context.Background())

	// --- Start consumers BEFORE producing work items ----------------------
	// This prevents a deadlock when the diff loop sends more items than the
	// workQueue buffer (cap 1000) can hold — the workers must already be
	// running to drain the queue as items are enqueued.

	// Start worker goroutines.
	for i := 0; i < e.workers; i++ {
		e.wg.Add(1)
		go e.runWorker(i)
	}

	// Start the event processor (another consumer of the work queue).
	e.wg.Add(1)
	go e.processEvents()

	e.setState(StateScanning)

	// --- Initial scan ---------------------------------------------------
	currentFiles, err := e.scanner.Scan()
	if err != nil {
		e.cancel() // cancel workers we already started
		e.setState(StateError)
		return fmt.Errorf("sync: initial scan: %w", err)
	}

	previousFiles, err := e.store.ListAll(e.pair.ID)
	if err != nil {
		e.cancel() // cancel workers we already started
		e.setState(StateError)
		return fmt.Errorf("sync: list store: %w", err)
	}

	// Build lookup maps for diffing.
	prevMap := make(map[string]*models.SyncFile, len(previousFiles))
	for _, sf := range previousFiles {
		prevMap[sf.LocalPath] = sf
	}
	curMap := make(map[string]*models.SyncFile, len(currentFiles))
	for _, sf := range currentFiles {
		curMap[sf.LocalPath] = sf
	}

	// Generate SyncOperations from the diff.
	// New files → upload, changed files → upload, deleted files → delete_remote.
	// Uses non-blocking sends via enqueueWork as a safety measure: if the
	// workQueue is temporarily full, a goroutine is spawned to wait for space
	// rather than blocking the caller and risking deadlock.
	for _, sf := range currentFiles {
		prev, existed := prevMap[sf.LocalPath]
		if !existed {
			// New file — upload.
			e.enqueueWork(newSyncOperation(sf, models.OpTypeUpload, e.maxRetries))
			continue
		}
		if sf.LocalHash != prev.LocalHash {
			// Changed file — upload (reuse remote ID from previous record).
			sf.RemoteID = prev.RemoteID
			e.enqueueWork(newSyncOperation(sf, models.OpTypeUpload, e.maxRetries))
		}
	}
	for _, sf := range previousFiles {
		if _, exists := curMap[sf.LocalPath]; !exists {
			// File deleted locally — delete remote.
			e.enqueueWork(newSyncOperation(sf, models.OpTypeDeleteRemote, e.maxRetries))
		}
	}

	// --- Start remaining background goroutines ---------------------------

	// Start the watcher.
	if err := e.watcher.Start(); err != nil {
		e.cancel() // cancel workers we already started
		e.setState(StateError)
		return fmt.Errorf("sync: start watcher: %w", err)
	}

	// Start the remote poller.
	e.wg.Add(1)
	go e.pollRemoteChanges()

	e.setState(StateIdle)
	return nil
}

// StartAsync starts the engine asynchronously. It starts workers and the event
// processor immediately, then runs the initial scan in a background goroutine.
// The caller can monitor progress via the State channel, the OnError channel,
// or the OnStateChange callback.
//
// Unlike Start, this method returns immediately after starting workers — the
// initial scan, diff processing, and watcher/poller startup all happen in a
// background goroutine. Errors from the background scan are logged and sent
// through the OnError channel.
func (e *Engine) StartAsync() error {
	e.ctx, e.cancel = context.WithCancel(context.Background())

	// --- Start consumers BEFORE producing work items ----------------------
	// Same rationale as Start(): workers must be running before we enqueue
	// any work items to prevent deadlock on a full workQueue.

	// Start worker goroutines.
	for i := 0; i < e.workers; i++ {
		e.wg.Add(1)
		go e.runWorker(i)
	}

	// Start the event processor (another consumer of the work queue).
	e.wg.Add(1)
	go e.processEvents()

	e.setState(StateScanning)

	// --- Run initial scan + watcher/poller startup in background ----------
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()

		// Perform the initial scan.
		currentFiles, err := e.scanner.Scan()
		if err != nil {
			err = fmt.Errorf("sync: initial scan: %w", err)
			e.sendError(err)
			e.notifyAsyncError(err)
			e.setState(StateError)
			e.notifyAppStateChange(appstate.Scanning, appstate.Error)
			return
		}

		previousFiles, err := e.store.ListAll(e.pair.ID)
		if err != nil {
			err = fmt.Errorf("sync: list store: %w", err)
			e.sendError(err)
			e.notifyAsyncError(err)
			e.setState(StateError)
			e.notifyAppStateChange(appstate.Scanning, appstate.Error)
			return
		}

		// Build lookup maps for diffing.
		prevMap := make(map[string]*models.SyncFile, len(previousFiles))
		for _, sf := range previousFiles {
			prevMap[sf.LocalPath] = sf
		}
		curMap := make(map[string]*models.SyncFile, len(currentFiles))
		for _, sf := range currentFiles {
			curMap[sf.LocalPath] = sf
		}

		// Generate SyncOperations from the diff.
		for _, sf := range currentFiles {
			prev, existed := prevMap[sf.LocalPath]
			if !existed {
				e.enqueueWork(newSyncOperation(sf, models.OpTypeUpload, e.maxRetries))
				continue
			}
			if sf.LocalHash != prev.LocalHash {
				sf.RemoteID = prev.RemoteID
				e.enqueueWork(newSyncOperation(sf, models.OpTypeUpload, e.maxRetries))
			}
		}
		for _, sf := range previousFiles {
			if _, exists := curMap[sf.LocalPath]; !exists {
				e.enqueueWork(newSyncOperation(sf, models.OpTypeDeleteRemote, e.maxRetries))
			}
		}

		// Start the watcher.
		if err := e.watcher.Start(); err != nil {
			err = fmt.Errorf("sync: start watcher: %w", err)
			e.sendError(err)
			e.notifyAsyncError(err)
			e.setState(StateError)
			e.notifyAppStateChange(appstate.Scanning, appstate.Error)
			return
		}

		// Start the remote poller.
		e.wg.Add(1)
		go e.pollRemoteChanges()

		// Determine final state based on whether there is work to do.
		if len(e.workQueue) > 0 {
			e.setState(StateSyncing)
			e.notifyAppStateChange(appstate.Scanning, appstate.Syncing)
		} else {
			e.setState(StateIdle)
			e.notifyAppStateChange(appstate.Scanning, appstate.Idle)
		}
	}()

	return nil
}

// notifyAsyncError sends an error on the OnError channel (non-blocking).
// This is used for errors that occur in background goroutines (e.g. during
// StartAsync's initial scan) so the tray/controller can be notified.
func (e *Engine) notifyAsyncError(err error) {
	select {
	case e.OnError <- err:
	default:
		// Channel full — log and drop to avoid blocking the goroutine.
		slog.Error("async error dropped (OnError channel full)", "error", err)
	}
}

// notifyAppStateChange invokes the OnStateChange callback if set.
func (e *Engine) notifyAppStateChange(oldState, newState appstate.State) {
	if e.OnStateChange != nil {
		e.OnStateChange(oldState, newState)
	}
}

// enqueueWork adds a SyncOperation to the work queue using a non-blocking
// send. If the queue is full, it spawns a short-lived goroutine that blocks
// until space is available, preventing the caller from deadlocking while still
// guaranteeing that every work item is eventually delivered.
func (e *Engine) enqueueWork(op *models.SyncOperation) {
	// Count the new operation against the backlog. It is decremented once,
	// when the operation terminally completes in the worker. Retry/pause
	// re-enqueues send to the channel directly and must not be counted here.
	atomic.AddInt64(&e.pendingOps, 1)
	select {
	case e.workQueue <- op:
	default:
		// Queue full — hand off to a goroutine that will block until
		// a worker drains a slot. The workers are already running, so
		// this will always complete.
		go func() {
			e.workQueue <- op
		}()
	}
}

// Stop performs a graceful shutdown of the sync engine.
func (e *Engine) Stop() error {
	e.setState(StatePaused)

	// Cancel the context to signal all goroutines.
	if e.cancel != nil {
		e.cancel()
	}

	// Stop the watcher.
	if err := e.watcher.Stop(); err != nil {
		return fmt.Errorf("sync: stop watcher: %w", err)
	}

	// Wait for workers to drain with a 30-second timeout.
	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All goroutines finished.
	case <-time.After(30 * time.Second):
		return fmt.Errorf("sync: timed out waiting for workers to drain")
	}

	// Close the work queue.
	close(e.workQueue)

	// Stop the rate limiter.
	e.rateLimiter.Stop()

	e.setState(StateIdle)
	return nil
}

// Pause suspends sync processing by setting the state to StatePaused.
// Workers will stop processing the work queue while paused.
func (e *Engine) Pause() {
	e.setState(StatePaused)
}

// Resume resumes sync processing after a pause by setting the state to
// StateIdle.
func (e *Engine) Resume() {
	e.setState(StateIdle)
}

// ---------------------------------------------------------------------------
// State Access
// ---------------------------------------------------------------------------

// State returns the current engine state (thread-safe).
func (e *Engine) State() SyncState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state
}

// Stats returns a copy of the current sync statistics (thread-safe).
func (e *Engine) Stats() SyncStats {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.stats
}

// Activity returns a snapshot of in-flight and queued work (thread-safe).
func (e *Engine) Activity() SyncActivity {
	e.mu.RLock()
	defer e.mu.RUnlock()
	act := SyncActivity{
		Pending: len(e.workQueue),
		Active:  len(e.activeOps),
	}
	if len(e.activeOps) > 0 {
		act.Current = make([]string, 0, len(e.activeOps))
		for _, desc := range e.activeOps {
			act.Current = append(act.Current, desc)
		}
		sort.Strings(act.Current) // stable display order
	}
	return act
}

// markActive records that worker id has begun executing op.
func (e *Engine) markActive(id int, op *models.SyncOperation) {
	desc := opDescription(op)
	e.mu.Lock()
	e.activeOps[id] = desc
	e.mu.Unlock()
}

// markDone clears the active-operation record for worker id.
func (e *Engine) markDone(id int) {
	e.mu.Lock()
	delete(e.activeOps, id)
	e.mu.Unlock()
}

// opDescription renders a short, human-readable label for an operation, e.g.
// "↑ report.pdf" for an upload.
func opDescription(op *models.SyncOperation) string {
	name := ""
	if op.File != nil {
		name = filepath.Base(op.File.LocalPath)
	}
	var icon string
	switch op.OpType {
	case models.OpTypeUpload:
		icon = "↑"
	case models.OpTypeDownload:
		icon = "↓"
	case models.OpTypeDeleteRemote, models.OpTypeDeleteLocal:
		icon = "🗑"
	case models.OpTypeConflict:
		icon = "⚠"
	default:
		icon = "•"
	}
	if name == "" {
		return icon
	}
	return icon + " " + name
}

// Errors returns a read-only channel that receives sync errors.
func (e *Engine) Errors() <-chan error {
	return e.errorCh
}

// StateChanges returns a read-only channel that receives state transitions.
func (e *Engine) StateChanges() <-chan SyncState {
	return e.stateChangeCh
}

// ---------------------------------------------------------------------------
// Core Sync Logic — Event Processing
// ---------------------------------------------------------------------------

// processEvents reads from the watcher's event channel and pushes
// SyncOperations onto the work queue.
func (e *Engine) processEvents() {
	defer e.wg.Done()

	for {
		select {
		case <-e.ctx.Done():
			return

		case ev, ok := <-e.watcher.Events():
			if !ok {
				// Channel closed — watcher stopped.
				return
			}

			// Skip processing while paused.
			if e.State() == StatePaused {
				continue
			}

			e.setState(StateSyncing)

			switch ev.Op {
			case models.ChangeOpCreate, models.ChangeOpModify:
				sf, err := e.scanner.ScanSingle(filepath.Join(e.pair.LocalDir, ev.Path))
				if err != nil {
					e.sendError(fmt.Errorf("sync: scan %s: %w", ev.Path, err))
					continue
				}
				// Check max file size.
				if e.appCfg.MaxFileSize > 0 && sf.Size > e.appCfg.MaxFileSize {
					continue
				}
				// Try to preserve existing remote ID from the store.
				existing, err := e.store.GetSyncFile(e.pair.ID, ev.Path)
				if err == nil && existing != nil {
					sf.RemoteID = existing.RemoteID
				}
				e.enqueueWork(newSyncOperation(sf, models.OpTypeUpload, e.maxRetries))

			case models.ChangeOpDelete:
				sf := &models.SyncFile{
					LocalPath: ev.Path,
				}
				// Try to get the remote ID from the store.
				existing, err := e.store.GetSyncFile(e.pair.ID, ev.Path)
				if err == nil && existing != nil {
					sf.RemoteID = existing.RemoteID
				}
				e.enqueueWork(newSyncOperation(sf, models.OpTypeDeleteRemote, e.maxRetries))

			case models.ChangeOpRename:
				// Delete the old remote entry.
				oldSF := &models.SyncFile{
					LocalPath: ev.OldPath,
				}
				existing, err := e.store.GetSyncFile(e.pair.ID, ev.OldPath)
				if err == nil && existing != nil {
					oldSF.RemoteID = existing.RemoteID
				}
				e.enqueueWork(newSyncOperation(oldSF, models.OpTypeDeleteRemote, e.maxRetries))

				// Upload the new file.
				newSF, err := e.scanner.ScanSingle(filepath.Join(e.pair.LocalDir, ev.Path))
				if err != nil {
					e.sendError(fmt.Errorf("sync: scan renamed %s: %w", ev.Path, err))
					continue
				}
				// Check max file size.
				if e.appCfg.MaxFileSize > 0 && newSF.Size > e.appCfg.MaxFileSize {
					continue
				}
				e.enqueueWork(newSyncOperation(newSF, models.OpTypeUpload, e.maxRetries))
			}

			// When the work queue is empty, transition back to idle.
			if len(e.workQueue) == 0 {
				e.setState(StateIdle)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Core Sync Logic — Worker
// ---------------------------------------------------------------------------

// runWorker is a goroutine that processes SyncOperations from the work queue.
func (e *Engine) runWorker(id int) {
	defer e.wg.Done()

	for {
		select {
		case <-e.ctx.Done():
			// Drain remaining operations before exiting.
			for {
				select {
				case op, ok := <-e.workQueue:
					if !ok {
						return
					}
					if err := e.executeOperation(op); err != nil {
						e.sendError(fmt.Errorf("sync: worker %d drain: %w", id, err))
					}
					atomic.AddInt64(&e.pendingOps, -1)
				default:
					return
				}
			}

		case op, ok := <-e.workQueue:
			if !ok {
				return
			}

			// Respect pause state — requeue and wait.
			if e.State() == StatePaused {
				go func(op *models.SyncOperation) {
					time.Sleep(500 * time.Millisecond)
					e.workQueue <- op
				}(op)
				continue
			}

			// Wait for rate limiter token.
			select {
			case <-e.rateLimiter.C:
			case <-e.ctx.Done():
				// Put the operation back and exit.
				go func(op *models.SyncOperation) {
					e.workQueue <- op
				}(op)
				return
			}

			e.markActive(id, op)
			err := e.executeOperation(op)
			e.markDone(id)
			if err != nil {
				op.Attempts++
				op.LastError = err.Error()

				if op.Attempts < op.MaxAttempts {
					// Retry with exponential backoff.
					delay := backoffDuration(op.Attempts)
					go func(op *models.SyncOperation, d time.Duration) {
						select {
						case <-time.After(d):
							e.workQueue <- op
						case <-e.ctx.Done():
							// Context cancelled during backoff — discard.
						}
					}(op, delay)
				} else {
					// Max retries exceeded — mark file as error in store.
					if op.File != nil {
						_ = e.store.UpdateStatus(e.pair.ID, op.File.LocalPath, models.SyncStatusError)
					}
					e.sendError(fmt.Errorf("sync: max retries exceeded for %s %s: %w",
						op.OpType, op.File.LocalPath, err))
				}
			} else {
				// Operation succeeded — update last sync time.
				e.mu.Lock()
				e.stats.LastSyncTime = time.Now()
				e.mu.Unlock()

				// If work queue is now empty, transition to idle.
				if len(e.workQueue) == 0 {
					e.setState(StateIdle)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Core Sync Logic — Operation Execution
// ---------------------------------------------------------------------------

// executeOperation dispatches a single SyncOperation to the appropriate
// handler based on its OpType.
func (e *Engine) executeOperation(op *models.SyncOperation) error {
	// In forward-only mode, skip download and delete_local operations
	if e.pair.ForwardOnly {
		switch op.OpType {
		case models.OpTypeDownload:
			// Skip download in forward-only mode
			return nil
		case models.OpTypeDeleteLocal:
			// Skip delete local in forward-only mode
			return nil
		}
	}

	switch op.OpType {
	case models.OpTypeUpload:
		return e.uploadFile(op.File)
	case models.OpTypeDownload:
		return e.downloadFile(op.File)
	case models.OpTypeDeleteRemote:
		return e.deleteRemote(op.File)
	case models.OpTypeDeleteLocal:
		return e.deleteLocal(op.File)
	case models.OpTypeConflict:
		return e.resolveConflict(op.File)
	default:
		return fmt.Errorf("sync: unknown operation type %q", op.OpType)
	}
}

// ---------------------------------------------------------------------------
// Upload
// ---------------------------------------------------------------------------

// uploadFile reads a local file, encrypts it, and uploads it to Google Drive.
func (e *Engine) uploadFile(sf *models.SyncFile) error {
	localPath := filepath.Join(e.pair.LocalDir, sf.LocalPath)

	// 1. Read the local file contents.
	plaintext, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("sync: read local file %s: %w", localPath, err)
	}

	// Check max file size from app config.
	if e.appCfg.MaxFileSize > 0 && int64(len(plaintext)) > e.appCfg.MaxFileSize {
		crypto.WipeBytes(plaintext)
		return nil // Skip files exceeding the size limit.
	}

	// 2. Compute SHA-256 hash of plaintext.
	hash := crypto.HashFile(plaintext)

	// 3. If hash matches and status is synced, skip (no changes).
	if hash == sf.LocalHash && sf.SyncStatus == models.SyncStatusSynced {
		crypto.WipeBytes(plaintext)
		return nil
	}

	// Update the local hash on the SyncFile before proceeding.
	sf.LocalHash = hash

	// 4. Encrypt the file.
	ciphertext, err := crypto.EncryptBlob(plaintext, e.masterKey, sf.LocalPath)
	if err != nil {
		crypto.WipeBytes(plaintext)
		return fmt.Errorf("sync: encrypt %s: %w", sf.LocalPath, err)
	}

	// 5. Encrypt the filename.
	encryptedName, err := crypto.EncryptFilename(sf.LocalPath, e.masterKey)
	if err != nil {
		crypto.WipeBytes(plaintext)
		return fmt.Errorf("sync: encrypt filename %s: %w", sf.LocalPath, err)
	}

	// 6. Create io.Reader from encrypted content.
	reader := bytes.NewReader(ciphertext)

	// 7/8. Upload or update on Google Drive.
	var remoteFile *drive.DriveFile
	if sf.RemoteID == "" {
		remoteFile, err = e.driveClient.UploadFile(e.ctx, encryptedName, e.pair.DriveFolderID, reader)
	} else {
		remoteFile, err = e.driveClient.UpdateFile(e.ctx, sf.RemoteID, reader)
	}
	if err != nil {
		crypto.WipeBytes(plaintext)
		return fmt.Errorf("sync: upload %s: %w", sf.LocalPath, err)
	}

	// 9. Update store: remote info.
	if err := e.store.UpdateRemoteInfo(e.pair.ID, sf.LocalPath, remoteFile.ID, remoteFile.MD5Hash, remoteFile.Size); err != nil {
		crypto.WipeBytes(plaintext)
		return fmt.Errorf("sync: update remote info %s: %w", sf.LocalPath, err)
	}

	// 10. Update store: status → synced.
	if err := e.store.UpdateStatus(e.pair.ID, sf.LocalPath, models.SyncStatusSynced); err != nil {
		crypto.WipeBytes(plaintext)
		return fmt.Errorf("sync: update status %s: %w", sf.LocalPath, err)
	}

	// 11. Update stats.
	e.mu.Lock()
	e.stats.FilesUploaded++
	e.stats.BytesUploaded += int64(len(plaintext))
	e.mu.Unlock()

	// 12. Wipe plaintext buffer.
	crypto.WipeBytes(plaintext)

	return nil
}

// ---------------------------------------------------------------------------
// Download
// ---------------------------------------------------------------------------

// downloadFile downloads an encrypted file from Google Drive, decrypts it,
// and writes the plaintext to the local file system.
func (e *Engine) downloadFile(sf *models.SyncFile) error {
	// 1. Download file content.
	rc, err := e.driveClient.DownloadFile(e.ctx, sf.RemoteID)
	if err != nil {
		return fmt.Errorf("sync: download %s: %w", sf.LocalPath, err)
	}
	defer func() { _ = rc.Close() }()

	// 2. Read all content from the ReadCloser.
	ciphertext, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("sync: read download %s: %w", sf.LocalPath, err)
	}

	// 3. Decrypt the file.
	plaintext, err := crypto.DecryptBlob(ciphertext, e.masterKey, sf.LocalPath)
	if err != nil {
		return fmt.Errorf("sync: decrypt %s: %w", sf.LocalPath, err)
	}

	// 4. Compute SHA-256 hash of decrypted content.
	hash := crypto.HashFile(plaintext)

	// 5. Write plaintext to local file path (create parent dirs if needed).
	localPath := filepath.Join(e.pair.LocalDir, sf.LocalPath)
	if err := os.MkdirAll(filepath.Dir(localPath), 0750); err != nil {
		crypto.WipeBytes(plaintext)
		return fmt.Errorf("sync: creating directory for %s: %w", sf.LocalPath, err)
	}

	if err := os.WriteFile(localPath, plaintext, 0600); err != nil {
		crypto.WipeBytes(plaintext)
		return fmt.Errorf("sync: writing %s: %w", sf.LocalPath, err)
	}

	// 6. Update store: set local hash, update status to synced.
	sf.LocalHash = hash
	if err := e.store.UpdateRemoteInfo(e.pair.ID, sf.LocalPath, sf.RemoteID, sf.RemoteHash, sf.Size); err != nil {
		crypto.WipeBytes(plaintext)
		return fmt.Errorf("sync: update remote info for download %s: %w", sf.LocalPath, err)
	}
	if err := e.store.UpdateStatus(e.pair.ID, sf.LocalPath, models.SyncStatusSynced); err != nil {
		crypto.WipeBytes(plaintext)
		return fmt.Errorf("sync: update status for download %s: %w", sf.LocalPath, err)
	}

	// 7. Update stats.
	e.mu.Lock()
	e.stats.FilesDownloaded++
	e.stats.BytesDownloaded += int64(len(plaintext))
	e.mu.Unlock()

	// 8. Wipe plaintext buffer.
	crypto.WipeBytes(plaintext)

	return nil
}

// ---------------------------------------------------------------------------
// Delete Remote
// ---------------------------------------------------------------------------

// deleteRemote removes a file from Google Drive and deletes its local store
// record.
func (e *Engine) deleteRemote(sf *models.SyncFile) error {
	// 1. If RemoteID is not empty, delete from Drive.
	if sf.RemoteID != "" {
		if err := e.driveClient.DeleteFile(e.ctx, sf.RemoteID); err != nil {
			return fmt.Errorf("sync: delete remote %s: %w", sf.LocalPath, err)
		}
	}

	// 2. Delete from store.
	if err := e.store.DeleteSyncFile(e.pair.ID, sf.LocalPath); err != nil {
		return fmt.Errorf("sync: delete store record %s: %w", sf.LocalPath, err)
	}

	// 3. Update stats.
	e.mu.Lock()
	e.stats.FilesDeleted++
	e.mu.Unlock()

	return nil
}

// ---------------------------------------------------------------------------
// Delete Local
// ---------------------------------------------------------------------------

// deleteLocal removes a local file and its store record.
func (e *Engine) deleteLocal(sf *models.SyncFile) error {
	// 1. Delete the local file.
	localPath := filepath.Join(e.pair.LocalDir, sf.LocalPath)
	if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("sync: delete local file %s: %w", localPath, err)
	}

	// 2. Delete from store.
	if err := e.store.DeleteSyncFile(e.pair.ID, sf.LocalPath); err != nil {
		return fmt.Errorf("sync: delete store record %s: %w", sf.LocalPath, err)
	}

	// 3. Update stats.
	e.mu.Lock()
	e.stats.FilesDeleted++
	e.mu.Unlock()

	return nil
}

// ---------------------------------------------------------------------------
// Conflict Resolution
// ---------------------------------------------------------------------------

// resolveConflict implements last-write-wins conflict resolution.
func (e *Engine) resolveConflict(sf *models.SyncFile) error {
	// 1. Compare local and remote modification times.
	localModTime := sf.ModTime
	// Use the current file's mod time from disk if available.
	localPath := filepath.Join(e.pair.LocalDir, sf.LocalPath)
	if info, err := os.Stat(localPath); err == nil {
		localModTime = info.ModTime()
	}

	// Get remote modification time.
	var remoteModTime time.Time
	if sf.RemoteID != "" {
		if remoteFile, err := e.driveClient.GetFile(e.ctx, sf.RemoteID); err == nil {
			remoteModTime = remoteFile.ModTime
		}
	}

	// 2. If local is newer: upload local version (overwrite remote).
	if localModTime.After(remoteModTime) {
		return e.uploadFile(sf)
	}

	// 3. If remote is newer: save a conflict backup, then download.
	if remoteModTime.After(localModTime) {
		// Save current local file as a conflict backup.
		backupPath := fmt.Sprintf("%s.conflict.%s", localPath, time.Now().Format("20060102-150405"))
		if data, err := os.ReadFile(localPath); err == nil {
			_ = os.WriteFile(backupPath, data, 0600)
		}
		// Download the remote version.
		return e.downloadFile(sf)
	}

	// Same timestamp — treat as synced.
	if err := e.store.UpdateStatus(e.pair.ID, sf.LocalPath, models.SyncStatusSynced); err != nil {
		return fmt.Errorf("sync: update status after conflict %s: %w", sf.LocalPath, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Remote Poller
// ---------------------------------------------------------------------------

// pollRemoteChanges periodically polls Google Drive for remote changes and
// generates download or delete_local operations as needed. It also listens
// on syncNowCh for immediate scan requests triggered by SyncNow().
func (e *Engine) pollRemoteChanges() {
	defer e.wg.Done()

	// Determine the sync interval from the pair (default 5 minutes).
	interval := time.Duration(e.pair.EffectiveSyncInterval()) * time.Second
	if interval < 5*time.Second {
		interval = 5 * time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	consecutiveErrors := 0

	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			if e.State() == StatePaused {
				continue
			}

			if err := e.pollOnce(); err != nil {
				consecutiveErrors++
				e.sendError(fmt.Errorf("sync: remote poll: %w", err))

				// Exponential backoff on API errors.
				backoff := backoffDuration(consecutiveErrors)
				select {
				case <-time.After(backoff):
				case <-e.ctx.Done():
					return
				}
			} else {
				consecutiveErrors = 0
			}
		case <-e.syncNowCh:
			// Immediate scan triggered by SyncNow().
			if e.State() == StatePaused {
				continue
			}

			if err := e.pollOnce(); err != nil {
				e.sendError(fmt.Errorf("sync: sync-now poll: %w", err))
			}
		}
	}
}

// pollOnce performs a single remote poll cycle.
func (e *Engine) pollOnce() error {
	// List all remote files in the gcrypt folder.
	remoteFiles, err := e.listAllRemote()
	if err != nil {
		return fmt.Errorf("list remote files: %w", err)
	}

	// Build a set of remote IDs currently tracked in the store.
	trackedFiles, err := e.store.ListAll(e.pair.ID)
	if err != nil {
		return fmt.Errorf("list store files: %w", err)
	}

	remoteIDSet := make(map[string]*models.SyncFile, len(trackedFiles))
	for _, sf := range trackedFiles {
		if sf.RemoteID != "" {
			remoteIDSet[sf.RemoteID] = sf
		}
	}

	// For each remote file not in the store → create a download operation.
	// Skip in forward-only mode
	if !e.pair.ForwardOnly {
		for _, rf := range remoteFiles {
			if _, tracked := remoteIDSet[rf.ID]; !tracked {
				sf := &models.SyncFile{
					LocalPath:  rf.Name, // Will be resolved during decryption
					RemoteID:   rf.ID,
					RemoteHash: rf.MD5Hash,
					Size:       rf.Size,
					ModTime:    rf.ModTime,
				}
				e.enqueueWork(newSyncOperation(sf, models.OpTypeDownload, e.maxRetries))
			}
		}
	}

	// For each store file not in the remote list → create a delete_local operation.
	// Skip in forward-only mode
	if !e.pair.ForwardOnly {
		remoteIDLookup := make(map[string]struct{}, len(remoteFiles))
		for _, rf := range remoteFiles {
			remoteIDLookup[rf.ID] = struct{}{}
		}
		for _, sf := range trackedFiles {
			if sf.RemoteID != "" {
				if _, exists := remoteIDLookup[sf.RemoteID]; !exists {
					e.enqueueWork(newSyncOperation(sf, models.OpTypeDeleteLocal, e.maxRetries))
				}
			}
		}
	}

	return nil
}

// listAllRemote paginates through all files in the Drive folder.
func (e *Engine) listAllRemote() ([]*drive.DriveFile, error) {
	var all []*drive.DriveFile
	pageToken := ""

	for {
		files, nextToken, err := e.driveClient.ListFiles(e.ctx, e.pair.DriveFolderID, pageToken)
		if err != nil {
			return nil, fmt.Errorf("list files page: %w", err)
		}
		all = append(all, files...)

		if nextToken == "" {
			break
		}
		pageToken = nextToken
	}

	return all, nil
}

// ---------------------------------------------------------------------------
// Helper Functions
// ---------------------------------------------------------------------------

// setState updates the engine state and notifies the stateChangeCh
// (thread-safe).
func (e *Engine) setState(state SyncState) {
	e.mu.Lock()
	e.state = state
	e.mu.Unlock()

	// Non-blocking send to state change channel.
	select {
	case e.stateChangeCh <- state:
	default:
		// Channel full — drop the notification to avoid blocking.
	}
}

// sendError sends an error to the error channel (non-blocking).
func (e *Engine) sendError(err error) {
	e.mu.Lock()
	e.stats.Errors++
	e.stats.LastError = err.Error()
	e.mu.Unlock()

	select {
	case e.errorCh <- err:
	default:
		// Channel full — drop the error to avoid blocking.
	}
}

// newSyncOperation creates a SyncOperation with sensible defaults.
func newSyncOperation(file *models.SyncFile, opType models.OpType, maxAttempts int) *models.SyncOperation {
	return &models.SyncOperation{
		File:        file,
		OpType:      opType,
		Priority:    0,
		Attempts:    0,
		MaxAttempts: maxAttempts,
		CreatedAt:   time.Now(),
	}
}

// backoffDuration calculates exponential backoff: 2^attempts * 2 seconds,
// capped at 32 seconds.
func backoffDuration(attempts int) time.Duration {
	d := time.Duration(1<<uint(attempts)) * 2 * time.Second
	if d > 32*time.Second {
		d = 32 * time.Second
	}
	return d
}
