package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arumes31/gcrypt/internal/appstate"
	"github.com/arumes31/gcrypt/internal/config"
	"github.com/arumes31/gcrypt/internal/crypto"
	"github.com/arumes31/gcrypt/internal/drive"
	"github.com/arumes31/gcrypt/internal/models"
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

// ActivityKind classifies a completed sync event for the activity feed.
type ActivityKind string

const (
	ActivityUpload   ActivityKind = "upload"
	ActivityDownload ActivityKind = "download"
	ActivityDelete   ActivityKind = "delete"
	ActivityConflict ActivityKind = "conflict"
)

// ActivityEvent is a single completed sync operation, recorded for the
// Nextcloud-style activity feed in the GUI. Events are kept in a bounded,
// most-recent-first ring buffer per engine.
type ActivityEvent struct {
	Time   time.Time
	Kind   ActivityKind
	Name   string // base file name, e.g. "report.pdf"
	Path   string // local path relative to the sync root
	PairID string
}

// maxRecentEvents bounds the per-engine activity history.
const maxRecentEvents = 100

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

	// recentEvents is a bounded, most-recent-last ring of completed operations
	// for the GUI activity feed. Guarded by mu.
	recentEvents []ActivityEvent

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

	// folderCache maps a slash-separated plaintext relative directory to its
	// Drive folder ID, for the hierarchical layout. Guarded by folderMu. The
	// sync root ("") is always the pair's Drive folder and is resolved directly.
	folderCache map[string]string
	folderMu    sync.Mutex

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
		folderCache:   make(map[string]string),
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

	// --- Initial scan + enqueue (streaming) -----------------------------
	// Uploads begin as files are discovered, before the full walk completes.
	if err := e.scanAndEnqueue(); err != nil {
		e.cancel() // cancel workers we already started
		e.setState(StateError)
		return fmt.Errorf("sync: %w", err)
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

		// Perform the initial scan and enqueue work in a streaming fashion:
		// uploads begin as files are discovered, before the walk completes.
		if err := e.scanAndEnqueue(); err != nil {
			err = fmt.Errorf("sync: %w", err)
			e.sendError(err)
			e.notifyAsyncError(err)
			e.setState(StateError)
			e.notifyAppStateChange(appstate.Scanning, appstate.Error)
			return
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

// scanAndEnqueue performs a streaming initial scan and enqueues the resulting
// sync operations. It loads the previously-synced file set, then streams the
// current directory contents, enqueuing an upload for each new or changed file
// as it is discovered — so uploads begin before the scan finishes. Once the
// walk completes, it enqueues a remote-delete for every previously-synced file
// that no longer exists locally (only determinable once the full current set
// is known).
//
// Enqueueing applies backpressure (see enqueueWork), so the scan self-throttles
// to the rate at which workers drain the queue, keeping memory bounded even for
// very large sync roots.
func (e *Engine) scanAndEnqueue() error {
	previousFiles, err := e.store.ListAll(e.pair.ID)
	if err != nil {
		return fmt.Errorf("list store: %w", err)
	}
	prevMap := make(map[string]*models.SyncFile, len(previousFiles))
	for _, sf := range previousFiles {
		prevMap[sf.LocalPath] = sf
	}

	// Stream the scan: files are hashed in the background and received here as
	// they are produced, so uploads can be enqueued immediately.
	fileCh := make(chan *models.SyncFile, 100)
	scanErrCh := make(chan error, 1)
	go func() {
		err := e.scanner.ScanStream(e.ctx, fileCh)
		close(fileCh)
		scanErrCh <- err
	}()

	seen := make(map[string]struct{}, len(previousFiles))
	for sf := range fileCh {
		seen[sf.LocalPath] = struct{}{}
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

	if err := <-scanErrCh; err != nil {
		return fmt.Errorf("initial scan: %w", err)
	}

	// Files present previously but absent now were deleted locally — delete
	// the corresponding remote copies.
	for path, sf := range prevMap {
		if _, ok := seen[path]; !ok {
			e.enqueueWork(newSyncOperation(sf, models.OpTypeDeleteRemote, e.maxRetries))
		}
	}

	return nil
}

// enqueueWork adds a SyncOperation to the work queue with a blocking send,
// applying natural backpressure: if the queue is full, the caller waits until
// a worker drains a slot rather than buffering the entire backlog in memory.
//
// This is critical for the initial scan, which can produce tens of thousands
// of operations. Spawning a goroutine per overflow item (the previous
// behaviour) piled up one blocked goroutine per file — driving memory into the
// gigabytes for large sync roots. Workers are always started before any work
// is enqueued (see Start/StartAsync) and never call enqueueWork themselves, so
// a blocking send here can never deadlock; it only throttles the producer.
func (e *Engine) enqueueWork(op *models.SyncOperation) {
	// Count the new operation against the backlog. It is decremented once,
	// when the operation terminally completes in the worker. Retry/pause
	// re-enqueues send to the channel directly and must not be counted here.
	atomic.AddInt64(&e.pendingOps, 1)
	select {
	case e.workQueue <- op:
	case <-e.ctx.Done():
		// Engine is shutting down — this operation will never run, so undo
		// the backlog increment to keep the pending counter accurate.
		atomic.AddInt64(&e.pendingOps, -1)
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

	// Deliberately do NOT close the work queue. Detached retry/pause goroutines
	// may still attempt a send, and a send on a closed channel panics. Workers
	// already exit on ctx cancellation, so closing is unnecessary; the queue is
	// simply garbage-collected once no goroutine references it.

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
		Pending: int(atomic.LoadInt64(&e.pendingOps)),
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

// recordEvent appends a completed operation to the activity feed ring buffer,
// dropping the oldest entry once the cap is reached.
func (e *Engine) recordEvent(op *models.SyncOperation) {
	if op == nil {
		return
	}
	kind := activityKindForOp(op.OpType)
	if kind == "" {
		return // not a feed-worthy op type
	}
	var name, path string
	if op.File != nil {
		path = op.File.LocalPath
		name = filepath.Base(path)
	}
	ev := ActivityEvent{
		Time:   time.Now(),
		Kind:   kind,
		Name:   name,
		Path:   path,
		PairID: e.pair.ID,
	}

	e.mu.Lock()
	e.recentEvents = append(e.recentEvents, ev)
	if len(e.recentEvents) > maxRecentEvents {
		// Drop the oldest events, keeping the most recent maxRecentEvents.
		e.recentEvents = e.recentEvents[len(e.recentEvents)-maxRecentEvents:]
	}
	e.mu.Unlock()
}

// RecentEvents returns up to limit most-recent-first activity events. A limit
// <= 0 returns all retained events.
func (e *Engine) RecentEvents(limit int) []ActivityEvent {
	e.mu.RLock()
	defer e.mu.RUnlock()

	n := len(e.recentEvents)
	if n == 0 {
		return nil
	}
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]ActivityEvent, 0, limit)
	// recentEvents is oldest-first; emit most-recent-first.
	for i := n - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, e.recentEvents[i])
	}
	return out
}

// activityKindForOp maps an operation type to an activity feed kind, or "" if
// the operation should not appear in the feed.
func activityKindForOp(t models.OpType) ActivityKind {
	switch t {
	case models.OpTypeUpload:
		return ActivityUpload
	case models.OpTypeDownload:
		return ActivityDownload
	case models.OpTypeDeleteRemote, models.OpTypeDeleteLocal:
		return ActivityDelete
	case models.OpTypeConflict:
		return ActivityConflict
	default:
		return ""
	}
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
				fullPath := filepath.Join(e.pair.LocalDir, ev.Path)
				// Directory events carry no content; the files within generate
				// their own events. Skip them rather than letting ScanSingle fail
				// on a non-regular file and spam the error channel.
				if info, statErr := os.Stat(fullPath); statErr == nil && info.IsDir() {
					continue
				}
				sf, err := e.scanner.ScanSingle(fullPath)
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
					// If the file is already recorded as synced with this exact
					// content, the event carries no real change — most commonly
					// it was triggered by our own write while downloading the
					// file. Skip the redundant upload to avoid re-encrypting and
					// re-pushing every downloaded file.
					if existing.SyncStatus == models.SyncStatusSynced && existing.LocalHash == sf.LocalHash {
						continue
					}
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

				// Upload the new file. Skip directories — the files they contain
				// generate their own rename/create events.
				newFull := filepath.Join(e.pair.LocalDir, ev.Path)
				if info, statErr := os.Stat(newFull); statErr == nil && info.IsDir() {
					continue
				}
				newSF, err := e.scanner.ScanSingle(newFull)
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

			// Respect pause state — requeue after a short delay and wait. Both
			// the delay and the requeue send honour ctx cancellation so this
			// goroutine cannot block forever (or send) after shutdown.
			if e.State() == StatePaused {
				go func(op *models.SyncOperation) {
					select {
					case <-time.After(500 * time.Millisecond):
					case <-e.ctx.Done():
						return
					}
					select {
					case e.workQueue <- op:
					case <-e.ctx.Done():
					}
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
					// Retry with exponential backoff. The operation is still
					// pending, so the backlog counter is left untouched until it
					// terminally completes — except if shutdown discards it.
					delay := backoffDuration(op.Attempts)
					go func(op *models.SyncOperation, d time.Duration) {
						select {
						case <-time.After(d):
							e.workQueue <- op
						case <-e.ctx.Done():
							// Context cancelled during backoff — the op will
							// never run, so drop it from the backlog.
							atomic.AddInt64(&e.pendingOps, -1)
						}
					}(op, delay)
				} else {
					// Max retries exceeded — terminal failure. Mark the file as
					// errored and clear it from the backlog.
					if op.File != nil {
						_ = e.store.UpdateStatus(e.pair.ID, op.File.LocalPath, models.SyncStatusError)
					}
					e.sendError(fmt.Errorf("sync: max retries exceeded for %s %s: %w",
						op.OpType, op.File.LocalPath, err))
					atomic.AddInt64(&e.pendingOps, -1)
				}
			} else {
				// Operation succeeded — terminal. Clear it from the backlog,
				// record it for the activity feed, and update the last sync time.
				atomic.AddInt64(&e.pendingOps, -1)
				e.recordEvent(op)
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

	// 1. Stat the file to get the size for stats and check limit.
	info, err := os.Stat(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			// The file was deleted before we got to upload it. Treat this as a
			// successful no-op so the operation completes immediately (clearing
			// it from the backlog without 5 retries or a bogus error status);
			// any remote copy is removed by the delete event the watcher raises
			// for the same path.
			return nil
		}
		return fmt.Errorf("sync: stat local file %s: %w", localPath, err)
	}

	// Check max file size from app config.
	if e.appCfg.MaxFileSize > 0 && info.Size() > e.appCfg.MaxFileSize {
		return nil // Skip files exceeding the size limit.
	}

	// 2. Compute SHA-256 hash of the file if needed.
	// We use the scanner's ComputeHash which already handles large files in chunks.
	hash, err := e.scanner.ComputeHash(localPath)
	if err != nil {
		return fmt.Errorf("sync: compute hash %s: %w", sf.LocalPath, err)
	}

	// 3. If hash matches and status is synced, skip (no changes).
	if hash == sf.LocalHash && sf.SyncStatus == models.SyncStatusSynced {
		return nil
	}

	// Update the local hash on the SyncFile before proceeding.
	sf.LocalHash = hash

	// 4. Resolve the encrypted parent folder and basename for this file. The
	// local directory tree is mirrored on Drive as a chain of encrypted
	// subfolders, and the Drive name encrypts only the basename — keeping any
	// single Drive folder from growing without bound. Computed before streaming
	// so a failure here doesn't orphan the encrypt goroutine.
	parentID, err := e.resolveRemoteDir(filepath.Dir(sf.LocalPath))
	if err != nil {
		return fmt.Errorf("sync: resolve remote dir for %s: %w", sf.LocalPath, err)
	}
	encryptedName, err := crypto.EncryptFilename(filepath.Base(sf.LocalPath), e.masterKey)
	if err != nil {
		return fmt.Errorf("sync: encrypt filename %s: %w", sf.LocalPath, err)
	}

	// 5. Stream-encrypt the file through a pipe that feeds the upload.
	//
	// The encryptor signals completion via pw.CloseWithError: on success it
	// passes nil, closing the pipe cleanly so the upload sees EOF and finalises;
	// on failure it propagates the error to the reader so the Drive client's
	// Read fails and the upload is aborted, rather than silently finalising a
	// truncated ciphertext. The error is also delivered on errCh (buffered, so
	// the send never blocks) so we can report the precise cause.
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		f, err := os.Open(localPath)
		if err != nil {
			err = fmt.Errorf("open local: %w", err)
			_ = pw.CloseWithError(err)
			errCh <- err
			return
		}
		defer f.Close()

		err = crypto.EncryptStream(f, pw, e.masterKey, sf.LocalPath)
		_ = pw.CloseWithError(err) // nil → clean EOF; non-nil → reader aborts
		errCh <- err
	}()

	// 6. Upload or update on Google Drive.
	var remoteFile *drive.DriveFile
	if sf.RemoteID == "" {
		remoteFile, err = e.driveClient.UploadFile(e.ctx, encryptedName, parentID, pr)
	} else {
		remoteFile, err = e.driveClient.UpdateFile(e.ctx, sf.RemoteID, pr)
	}

	// The upload has returned, so Drive is done with the reader. Closing the
	// read end unblocks the encryptor if the upload aborted mid-stream (e.g. a
	// network error left it blocked on Write), then we wait for it to finish.
	// This makes the result deterministic and guarantees the goroutine can't
	// leak regardless of which side failed first.
	_ = pr.Close()
	encErr := <-errCh

	// A genuine encryption failure is the root cause and the clearest error.
	// When the upload itself failed first, encErr is just io.ErrClosedPipe from
	// the pr.Close above — in that case report the upload error instead.
	if encErr != nil && !errors.Is(encErr, io.ErrClosedPipe) {
		return fmt.Errorf("sync: encrypt %s: %w", sf.LocalPath, encErr)
	}
	if err != nil {
		return fmt.Errorf("sync: upload %s: %w", sf.LocalPath, err)
	}

	// 7. Upsert the store record with the new remote info and synced status.
	// PutSyncFile inserts the row when it does not yet exist (newly-discovered
	// files have no row) and replaces it otherwise. The previous code used
	// UpdateRemoteInfo/UpdateStatus, which fail when no row is present — so new
	// files were uploaded to Drive but never recorded as synced, leaving the
	// operation in a perpetual error/retry loop (and creating duplicate remote
	// files on each retry).
	sf.SyncRootID = e.pair.ID
	sf.RemoteID = remoteFile.ID
	sf.RemoteHash = remoteFile.MD5Hash
	sf.Size = remoteFile.Size
	sf.SyncStatus = models.SyncStatusSynced
	if sf.Version == 0 {
		sf.Version = 1
	}
	if err := e.store.PutSyncFile(sf); err != nil {
		return fmt.Errorf("sync: record uploaded file %s: %w", sf.LocalPath, err)
	}

	// 8. Update stats.
	e.mu.Lock()
	e.stats.FilesUploaded++
	e.stats.BytesUploaded += info.Size()
	e.mu.Unlock()

	return nil
}

// ---------------------------------------------------------------------------
// Download
// ---------------------------------------------------------------------------

// downloadFile downloads an encrypted file from Google Drive, decrypts it,
// and writes the plaintext to the local file system.
func (e *Engine) downloadFile(sf *models.SyncFile) error {
	// 1. Download file content as a stream.
	rc, err := e.driveClient.DownloadFile(e.ctx, sf.RemoteID)
	if err != nil {
		return fmt.Errorf("sync: download %s: %w", sf.LocalPath, err)
	}
	defer func() { _ = rc.Close() }()

	// 2. Setup streaming decryption and local write.
	localPath := filepath.Join(e.pair.LocalDir, sf.LocalPath)
	if err := os.MkdirAll(filepath.Dir(localPath), 0750); err != nil {
		return fmt.Errorf("sync: creating directory for %s: %w", sf.LocalPath, err)
	}

	f, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("sync: creating local file %s: %w", localPath, err)
	}
	defer f.Close()

	// Decrypt the stream.
	if err := crypto.DecryptStream(rc, f, e.masterKey, sf.LocalPath); err != nil {
		return fmt.Errorf("sync: decrypt stream %s: %w", sf.LocalPath, err)
	}

	// 3. Re-scan the file to get the final local hash and update stats.
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("sync: stat downloaded file: %w", err)
	}

	hash, err := e.scanner.ComputeHash(localPath)
	if err != nil {
		return fmt.Errorf("sync: compute hash after download: %w", err)
	}

	// 4. Upsert the store record so the downloaded file is tracked. Without an
	// insert here a newly-downloaded remote file would never get a row, and the
	// next poll would treat it as untracked and re-download it endlessly.
	sf.SyncRootID = e.pair.ID
	sf.LocalHash = hash
	sf.SyncStatus = models.SyncStatusSynced
	if sf.Version == 0 {
		sf.Version = 1
	}
	if err := e.store.PutSyncFile(sf); err != nil {
		return fmt.Errorf("sync: record downloaded file %s: %w", sf.LocalPath, err)
	}

	// 5. Update stats.
	e.mu.Lock()
	e.stats.FilesDownloaded++
	e.stats.BytesDownloaded += info.Size()
	e.mu.Unlock()

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

// resolveConflict reconciles a file that changed remotely. If the local copy is
// untouched since the last sync it simply pulls the remote update; if the local
// copy also changed it applies last-write-wins, preserving the local copy as a
// backup when the remote version wins.
func (e *Engine) resolveConflict(sf *models.SyncFile) error {
	localPath := filepath.Join(e.pair.LocalDir, sf.LocalPath)

	// If the local copy is gone, re-create it from the remote. A genuine local
	// deletion is reconciled separately via delete operations; pulling the
	// remote here is the safe default and never loses data.
	if _, statErr := os.Stat(localPath); os.IsNotExist(statErr) {
		return e.downloadFile(sf)
	}

	// If the local copy is unchanged since our last sync (sf.LocalHash is the
	// content hash recorded then), there is no real conflict — just pull the
	// remote update without creating a backup.
	if curHash, err := e.scanner.ComputeHash(localPath); err == nil && curHash == sf.LocalHash {
		return e.downloadFile(sf)
	}

	// Both sides diverged — apply last-write-wins by modification time.
	localModTime := sf.ModTime
	if info, err := os.Stat(localPath); err == nil {
		localModTime = info.ModTime()
	}
	var remoteModTime time.Time
	if sf.RemoteID != "" {
		if remoteFile, err := e.driveClient.GetFile(e.ctx, sf.RemoteID); err == nil {
			remoteModTime = remoteFile.ModTime
		}
	}

	// Local strictly newer → push the local version over the remote.
	if localModTime.After(remoteModTime) {
		return e.uploadFile(sf)
	}

	// Remote newer (or identical timestamp) → remote wins: preserve the local
	// copy as a timestamped backup, then download the remote version.
	// downloadFile records the new remote hash, so the conflict does not
	// re-trigger on every subsequent poll. The backup is best-effort and is
	// streamed so large files are not loaded fully into memory.
	backupPath := fmt.Sprintf("%s.conflict.%s", localPath, time.Now().Format("20060102-150405"))
	if src, err := os.Open(localPath); err == nil {
		if dst, err := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600); err == nil {
			_, _ = io.Copy(dst, src)
			_ = dst.Close()
		}
		_ = src.Close()
	}
	return e.downloadFile(sf)
}

// ---------------------------------------------------------------------------
// Remote Poller
// ---------------------------------------------------------------------------

// pollRemoteChanges periodically polls Google Drive for remote changes and
// generates download or delete_local operations as needed. It also listens
// on syncNowCh for immediate scan requests triggered by SyncNow().
func (e *Engine) pollRemoteChanges() {
	defer e.wg.Done()

	// Determine the sync interval from the pair, flooring it at 5 seconds so a
	// misconfigured tiny value can't hammer the Drive API. (Previously a value
	// below 5s was bumped all the way up to 5 minutes, which surprisingly made a
	// "fast" interval the slowest.)
	interval := time.Duration(e.pair.EffectiveSyncInterval()) * time.Second
	if interval < 5*time.Second {
		interval = 5 * time.Second
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
	// Enumerate all remote files with their decrypted local paths (flat or
	// hierarchical layout, transparently).
	remoteFiles, err := e.collectRemoteFiles()
	if err != nil {
		return fmt.Errorf("list remote files: %w", err)
	}

	// Build a set of remote IDs currently tracked in the store.
	trackedFiles, err := e.store.ListAll(e.pair.ID)
	if err != nil {
		return fmt.Errorf("list store files: %w", err)
	}

	remoteByID := make(map[string]*models.SyncFile, len(trackedFiles))
	for _, sf := range trackedFiles {
		if sf.RemoteID != "" {
			remoteByID[sf.RemoteID] = sf
		}
	}

	// Skip both download and delete-local generation in forward-only mode.
	if e.pair.ForwardOnly {
		return nil
	}

	for _, rf := range remoteFiles {
		tracked, isTracked := remoteByID[rf.ID]
		if !isTracked {
			// New remote file → download.
			sf := &models.SyncFile{
				LocalPath:  rf.LocalPath,
				RemoteID:   rf.ID,
				RemoteHash: rf.RemoteHash,
				Size:       rf.Size,
				ModTime:    rf.ModTime,
			}
			e.enqueueWork(newSyncOperation(sf, models.OpTypeDownload, e.maxRetries))
			continue
		}

		// Already tracked: detect a remote modification (e.g. another machine
		// edited the file) by comparing the Drive content hash against what we
		// recorded at the last sync. Without this, edits to existing files never
		// propagate — only new files and deletions would. A change is reconciled
		// via conflict resolution, which pulls the remote update when the local
		// copy is untouched and otherwise backs up the local copy first.
		if rf.RemoteHash != "" && tracked.RemoteHash != "" && rf.RemoteHash != tracked.RemoteHash {
			sf := &models.SyncFile{
				LocalPath:  tracked.LocalPath,
				RemoteID:   rf.ID,
				RemoteHash: rf.RemoteHash,
				LocalHash:  tracked.LocalHash,
				Size:       rf.Size,
				ModTime:    rf.ModTime,
			}
			e.enqueueWork(newSyncOperation(sf, models.OpTypeConflict, e.maxRetries))
		}
	}

	// For each store file not in the remote list → create a delete_local operation.
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

	return nil
}

// remoteFileInfo describes a remote file together with the local relative path
// recovered from its (encrypted) name and folder location.
type remoteFileInfo struct {
	ID         string
	RemoteHash string
	Size       int64
	ModTime    time.Time
	LocalPath  string
}

// collectRemoteFiles enumerates every file in the pair's Drive folder by walking
// the encrypted folder tree, returning each file with its decrypted local
// relative path. Objects whose names cannot be decrypted (not created by gcrypt,
// or a different key) are skipped.
func (e *Engine) collectRemoteFiles() ([]remoteFileInfo, error) {
	var acc []remoteFileInfo
	if err := e.collectRemoteHierarchical(e.pair.DriveFolderID, "", &acc); err != nil {
		return nil, err
	}
	return acc, nil
}

// collectRemoteHierarchical recursively walks the encrypted Drive folder tree
// rooted at folderID, which maps to the plaintext relative directory relDir
// ("" = the sync root). It appends decrypted file entries to acc and caches the
// IDs of folders it discovers so later uploads can reuse them.
func (e *Engine) collectRemoteHierarchical(folderID, relDir string, acc *[]remoteFileInfo) error {
	pageToken := ""
	for {
		files, next, err := e.driveClient.ListFiles(e.ctx, folderID, pageToken)
		if err != nil {
			return fmt.Errorf("list folder %s: %w", folderID, err)
		}
		for _, rf := range files {
			name, err := crypto.DecryptFilename(rf.Name, e.masterKey)
			if err != nil {
				e.sendError(fmt.Errorf("sync: decrypt remote name %q: %w", rf.Name, err))
				continue
			}
			childRel := name
			if relDir != "" {
				childRel = relDir + "/" + name
			}
			if rf.IsFolder() {
				e.cacheRemoteDir(childRel, rf.ID)
				if err := e.collectRemoteHierarchical(rf.ID, childRel, acc); err != nil {
					return err
				}
				continue
			}
			*acc = append(*acc, remoteFileInfo{
				ID:         rf.ID,
				RemoteHash: rf.MD5Hash,
				Size:       rf.Size,
				ModTime:    rf.ModTime,
				LocalPath:  filepath.FromSlash(childRel),
			})
		}
		if next == "" {
			break
		}
		pageToken = next
	}
	return nil
}

// resolveRemoteDir returns the Drive folder ID for the given plaintext relative
// directory, creating the encrypted folder chain on Drive as needed. The sync
// root (".", "/" or "") maps directly to the pair's Drive folder. Results are
// cached, and the lock is held across creation so concurrent workers cannot
// create duplicate folders for the same path.
func (e *Engine) resolveRemoteDir(relDir string) (string, error) {
	slashDir := path.Clean(filepath.ToSlash(relDir))
	if slashDir == "." || slashDir == "/" || slashDir == "" {
		return e.pair.DriveFolderID, nil
	}

	e.folderMu.Lock()
	defer e.folderMu.Unlock()

	if id, ok := e.folderCache[slashDir]; ok {
		return id, nil
	}

	parentID := e.pair.DriveFolderID
	cum := ""
	for _, comp := range strings.Split(slashDir, "/") {
		if comp == "" {
			continue
		}
		if cum == "" {
			cum = comp
		} else {
			cum += "/" + comp
		}
		if id, ok := e.folderCache[cum]; ok {
			parentID = id
			continue
		}
		encName, err := crypto.EncryptFilename(comp, e.masterKey)
		if err != nil {
			return "", fmt.Errorf("encrypt dir component %q: %w", comp, err)
		}
		id, err := e.driveClient.EnsureFolderUnder(e.ctx, parentID, encName)
		if err != nil {
			return "", fmt.Errorf("ensure remote folder %q: %w", cum, err)
		}
		e.folderCache[cum] = id
		parentID = id
	}
	return parentID, nil
}

// cacheRemoteDir records the Drive folder ID for a plaintext relative directory
// (slash-separated) discovered while walking the remote tree.
func (e *Engine) cacheRemoteDir(relDir, folderID string) {
	e.folderMu.Lock()
	e.folderCache[relDir] = folderID
	e.folderMu.Unlock()
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
