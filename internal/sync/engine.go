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

	"golang.org/x/sync/singleflight"
	"golang.org/x/text/unicode/norm"

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
	Pending      int      // operations waiting in the work queue
	PendingBytes int64    // total bytes of queued + in-flight transfer ops (uploads/downloads)
	Active       int      // operations currently being processed
	Current      []string // short descriptions of the in-flight operations (e.g. "↑ report.pdf")
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

	// pendingBytes mirrors pendingOps but sums the byte size of outstanding
	// transfer operations (uploads/downloads), so the UI can show byte-based
	// progress and an ETA rather than a file-count ratio that jumps when a large
	// file follows many tiny ones. Accessed atomically; incremented/decremented
	// in lock-step with pendingOps.
	pendingBytes int64

	// liveBytesUp/liveBytesDown count transfer bytes as they stream over the
	// wire, incremented from the up/download data path (not in one lump at file
	// completion). The live transfer-rate display samples deltas of these, so a
	// large file shows real throughput throughout instead of 0 B/s during the
	// transfer and a single end-of-file spike. Accessed atomically. They include
	// retransmitted bytes on retry (correct for a rate), so they are a throughput
	// odometer, not a count of distinct bytes stored.
	liveBytesUp   int64
	liveBytesDown int64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	workQueue     chan *models.SyncOperation
	errorCh       chan error
	stateChangeCh chan SyncState

	maxRetries  int
	workers     int
	rateLimiter *time.Ticker
	// largeUploadSem bounds concurrent large-file uploads (each buffers up to one
	// upload chunk) so a high worker count stays memory-safe. Buffered to
	// maxConcurrentLargeUploads; small files don't acquire it.
	largeUploadSem chan struct{}

	// folderCache maps a slash-separated plaintext relative directory to its
	// Drive folder ID, for the hierarchical layout. Guarded by folderMu. The
	// sync root ("") is always the pair's Drive folder and is resolved directly.
	folderCache map[string]string
	folderMu    sync.Mutex
	// folderGroup deduplicates concurrent resolution/creation of the same remote
	// directory so parallel uploads never create duplicate Drive folders, while
	// still allowing different directories to be created concurrently.
	folderGroup singleflight.Group

	// syncNowCh is used by SyncNow() to trigger an immediate full scan.
	syncNowCh chan struct{}

	// changePageToken holds the Drive changes-feed cursor between remote polls.
	// Empty means "no baseline yet": the next poll establishes one and does a
	// full reconcile. It is only ever read/written from the pollRemoteChanges
	// goroutine (the sole caller of pollOnce), so it needs no lock.
	changePageToken string

	// OnError is a channel that receives errors from background operations
	// (e.g. async scan failures). The tray or controller can listen on this
	// channel to be notified of asynchronous errors without polling.
	OnError chan error

	// OnStateChange is an optional callback invoked when the engine's
	// appstate-level state transitions. This allows the tray/controller to
	// react to lifecycle transitions (Scanning → Syncing → Idle, etc.)
	// without watching the SyncState channel.
	OnStateChange func(oldState, newState appstate.State)

	// conflicts holds unresolved conflicts queued for manual resolution.
	conflicts conflictQueue
}

// EngineOption is a functional option for configuring an Engine.
type EngineOption func(*Engine)

// WithMaxRetries sets the maximum number of retry attempts for failed
// operations. The default is 5.
func WithMaxRetries(n int) EngineOption {
	return func(e *Engine) { e.maxRetries = n }
}

// defaultWorkers is the worker-pool size per engine when not overridden by
// app.upload_workers. Small-file throughput is bound by per-request latency
// (~1s per small upload against Drive), so throughput ≈ workers ÷ latency and
// concurrency is the main lever: to actually approach the request-rate cap you
// need roughly maxRequestsPerSec workers in flight. The pool is load-adaptive by
// nature — idle workers just block on the (empty) queue costing ~nothing, so
// only as many as there is work run concurrently (it scales from ~0 up to this
// many), which is why a high default is safe for light workloads. Large files
// are separately capped (see maxConcurrentLargeUploads) so a high count stays
// memory-safe; small files only buffer their own (small) size.
const defaultWorkers = 160

// maxRequestsPerSec caps the per-engine Drive request rate. Google's per-user
// limit is ~200 requests/s (12,000/min); we aim for ~90% of that so a saturated
// worker pool uses nearly the full budget while leaving headroom for bursts and
// for the metadata/listing calls, with 429 retry+backoff as the backstop. NOTE:
// this cap is per engine (per sync pair). With multiple pairs the combined rate
// can exceed Google's per-user limit, so lower app.upload_workers when running
// many pairs (429s are retried regardless).
const maxRequestsPerSec = 180

// largeUploadThreshold is the file size at or above which an upload buffers a
// full upload chunk in memory. Concurrent uploads of such files are gated by
// maxConcurrentLargeUploads so a high worker count can't blow up memory; smaller
// files buffer only their own size and run with full worker concurrency.
const largeUploadThreshold = 8 * 1024 * 1024 // 8 MiB (matches drive uploadChunkSize)

// maxConcurrentLargeUploads bounds how many large-file uploads run at once,
// independent of the worker count, capping large-upload memory at roughly
// maxConcurrentLargeUploads * 8 MiB and keeping a handful of big files from
// monopolising bandwidth.
const maxConcurrentLargeUploads = 3

// largeRequeueDelay is how long a worker waits before re-queuing a large upload
// that couldn't get a large-upload slot, so it doesn't spin while big files are
// saturated.
const largeRequeueDelay = 250 * time.Millisecond

// WithWorkers sets the number of concurrent worker goroutines, overriding the
// default. Note: the request-rate limiter is sized from the worker count at
// construction, so prefer app.upload_workers for production tuning.
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

	// Concurrency: uploads now run in parallel (the Drive client is no longer
	// serialised), so the worker count is the main throughput lever — important
	// for syncing large trees of small files. Tunable via app.upload_workers.
	workers := defaultWorkers
	if appCfg.UploadWorkers > 0 {
		workers = appCfg.UploadWorkers
	}
	// Size the request-rate limiter so it does not throttle below the worker
	// concurrency, while still capping bursts to stay clear of Drive's per-user
	// rate limits (429s are retried with backoff regardless).
	reqPerSec := workers * 10
	if reqPerSec > maxRequestsPerSec {
		reqPerSec = maxRequestsPerSec
	}

	e := &Engine{
		pair:           pair,
		appCfg:         appCfg,
		driveClient:    driveClient,
		store:          store,
		watcher:        watcher,
		scanner:        scanner,
		masterKey:      masterKey,
		state:          StateIdle,
		activeOps:      make(map[int]string),
		folderCache:    make(map[string]string),
		workQueue:      make(chan *models.SyncOperation, 1000),
		errorCh:        make(chan error, 256),
		stateChangeCh:  make(chan SyncState, 64),
		syncNowCh:      make(chan struct{}, 1),
		OnError:        make(chan error, 64),
		maxRetries:     5,
		workers:        workers,
		rateLimiter:    time.NewTicker(time.Second / time.Duration(reqPerSec)),
		largeUploadSem: make(chan struct{}, maxConcurrentLargeUploads),
	}

	// Apply functional options.
	for _, opt := range opts {
		opt(e)
	}

	// Warm the scanner's hash cache from the previous run so a cold start doesn't
	// re-hash the whole tree. Best-effort: a missing/old cache just means a
	// regular (re-hashing) first scan.
	if p := e.hashCachePath(); p != "" {
		_ = e.scanner.LoadHashCache(p)
	}

	return e, nil
}

// hashCachePath returns the on-disk location of this pair's persistent hash
// cache. It lives alongside the log file (the gcrypt app-data dir), keyed by
// pair ID, so it is never placed inside the synced tree. Returns "" if no base
// directory can be determined.
func (e *Engine) hashCachePath() string {
	base := filepath.Dir(e.appCfg.LogPath)
	if base == "" || base == "." {
		return ""
	}
	return filepath.Join(base, "hashcache-"+e.pair.ID+".json")
}

// ---------------------------------------------------------------------------
// Identity & Immediate Sync
// ---------------------------------------------------------------------------

// ID returns the unique identifier of the sync pair this engine manages.
func (e *Engine) ID() string {
	return e.pair.ID
}

// encName encrypts a plaintext path component to its remote (Drive) name, using
// the padded format when the pair has filename padding enabled. Decryption
// auto-detects the format, so a pair can hold a mix of padded and unpadded names.
func (e *Engine) encName(name string) (string, error) {
	if e.pair.PadFilenames {
		return crypto.EncryptFilenamePadded(name, e.masterKey)
	}
	return crypto.EncryptFilename(name, e.masterKey)
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
	previousFiles, err := e.store.ListAll(e.ctx, e.pair.ID)
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

	// In download-only mode the local tree never pushes upward, so skip upload
	// and remote-delete generation entirely (but still drain the scan).
	canUpload := e.allowsUpload()

	seen := make(map[string]struct{}, len(previousFiles))
	for sf := range fileCh {
		seen[sf.LocalPath] = struct{}{}
		if !canUpload {
			continue
		}
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

	// Files present previously but absent now were deleted locally — delete the
	// corresponding remote copies, unless the direction forbids remote deletes
	// (download-only never deletes remotely; mirror/backup keeps deleted files).
	if e.allowsRemoteDelete() {
		// Collect first so a bulk-delete guard can veto what looks like an
		// accidental mass deletion (e.g. the local sync folder is on an unmounted
		// drive and scans as empty) before any remote files are trashed. Online-only
		// records have no local copy by design, so they are never delete candidates.
		var toDelete []*models.SyncFile
		for path, sf := range prevMap {
			if sf.SyncStatus == models.SyncStatusOnlineOnly {
				continue
			}
			if _, ok := seen[path]; !ok {
				toDelete = append(toDelete, sf)
			}
		}
		// Only veto deletions when the local sync root itself is missing or
		// unreadable (unmounted drive / reassigned drive letter) — the case where
		// every tracked file falsely looks deleted. When the root is present, a
		// genuine bulk delete propagates in full: deleting a big folder is allowed.
		if len(toDelete) > 0 && !e.localRootAvailable() {
			e.sendError(fmt.Errorf("sync: local sync folder %q is missing or unreadable; skipping %d remote deletion(s) until it is back",
				e.pair.LocalDir, len(toDelete)))
			return nil
		}
		for _, sf := range toDelete {
			e.enqueueWork(newSyncOperation(sf, models.OpTypeDeleteRemote, e.maxRetries))
		}
	}

	// Mirror empty local directories to Drive so the folder structure — not just
	// files — is preserved (and reproduced on other machines). Only when uploads
	// are allowed for this direction.
	if canUpload {
		if emptyDirs, derr := e.scanner.EmptyDirs(); derr == nil {
			for _, d := range emptyDirs {
				if _, rerr := e.resolveRemoteDir(d); rerr != nil {
					e.sendError(fmt.Errorf("sync: ensure remote folder %s: %w", d, rerr))
				}
			}
		}
	}

	// Persist the hash cache so the next startup scan can skip re-hashing files
	// whose size and mtime are unchanged. Best-effort.
	if p := e.hashCachePath(); p != "" {
		_ = e.scanner.SaveHashCache(p)
	}

	return nil
}

// localRootAvailable reports whether the pair's local sync root currently exists
// as a readable directory. Used to distinguish a genuine bulk deletion (root
// present, files removed) from an unmounted/reassigned drive (root gone), so the
// former propagates and the latter is held back.
func (e *Engine) localRootAvailable() bool {
	info, err := os.Stat(longPath(e.pair.LocalDir))
	return err == nil && info.IsDir()
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
	atomic.AddInt64(&e.pendingBytes, opBytes(op))
	select {
	case e.workQueue <- op:
	case <-e.ctx.Done():
		// Engine is shutting down — this operation will never run, so undo
		// the backlog increment to keep the pending counter accurate.
		atomic.AddInt64(&e.pendingOps, -1)
		atomic.AddInt64(&e.pendingBytes, -opBytes(op))
	}
}

// opBytes returns the byte size a transfer operation contributes to the pending
// backlog. Only uploads and downloads move bytes; deletes/conflicts count as 0.
func opBytes(op *models.SyncOperation) int64 {
	if op == nil || op.File == nil {
		return 0
	}
	switch op.OpType {
	case models.OpTypeUpload, models.OpTypeDownload:
		return op.File.Size
	default:
		return 0
	}
}

// enqueueDetached schedules a brand-new operation without ever blocking the
// caller. It MUST be used instead of enqueueWork whenever the caller is itself a
// worker goroutine (e.g. deleteRemote/deleteLocal resolving a delete/edit
// conflict): enqueueWork does a blocking send, so a worker calling it while the
// queue is full would stall the very goroutines that drain the queue — a
// deadlock that manifests as uploads no longer being processed. Like
// enqueueWork it counts the op against the backlog (it is a new op, not a
// retry), but the send happens on a detached goroutine and honours ctx
// cancellation, decrementing the counter if the op never makes it onto the queue.
func (e *Engine) enqueueDetached(op *models.SyncOperation) {
	atomic.AddInt64(&e.pendingOps, 1)
	atomic.AddInt64(&e.pendingBytes, opBytes(op))
	go func() {
		select {
		case e.workQueue <- op:
		case <-e.ctx.Done():
			atomic.AddInt64(&e.pendingOps, -1)
			atomic.AddInt64(&e.pendingBytes, -opBytes(op))
		}
	}()
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

// --- Sync-direction gates ---------------------------------------------------
// These translate the pair's direction policy (two-way / upload-only /
// download-only / mirror) into the four primitive permissions the engine acts
// on. Mirror is upload-only that never deletes on the remote (backup mode).

// allowsUpload reports whether local creations/modifications push to the remote.
func (e *Engine) allowsUpload() bool {
	switch e.pair.EffectiveDirection() {
	case config.SyncDirTwoWay, config.SyncDirUploadOnly, config.SyncDirMirror:
		return true
	}
	return false
}

// allowsDownload reports whether remote creations/modifications pull to local.
func (e *Engine) allowsDownload() bool {
	switch e.pair.EffectiveDirection() {
	case config.SyncDirTwoWay, config.SyncDirDownloadOnly:
		return true
	}
	return false
}

// allowsRemoteDelete reports whether a local deletion is propagated to the
// remote. Mirror (backup) and download-only never delete on the remote.
func (e *Engine) allowsRemoteDelete() bool {
	switch e.pair.EffectiveDirection() {
	case config.SyncDirTwoWay, config.SyncDirUploadOnly:
		return true
	}
	return false
}

// allowsLocalDelete reports whether a remote deletion is propagated locally.
func (e *Engine) allowsLocalDelete() bool {
	switch e.pair.EffectiveDirection() {
	case config.SyncDirTwoWay, config.SyncDirDownloadOnly:
		return true
	}
	return false
}

// Stats returns a copy of the current sync statistics (thread-safe). The byte
// counters are sourced from the live atomic odometers so the figure advances
// during a transfer rather than jumping only when each file finishes.
func (e *Engine) Stats() SyncStats {
	e.mu.RLock()
	s := e.stats
	e.mu.RUnlock()
	s.BytesUploaded = atomic.LoadInt64(&e.liveBytesUp)
	s.BytesDownloaded = atomic.LoadInt64(&e.liveBytesDown)
	return s
}

// countingReader wraps an io.Reader, atomically adding the number of bytes read
// to *counter as the stream is consumed. Wrapping the upload/download data
// stream with this makes the live transfer-rate reflect bytes as they move over
// the wire, instead of the whole file size landing in a single sample at
// completion (which showed 0 B/s during a large transfer, then a huge spike).
type countingReader struct {
	r       io.Reader
	counter *int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 {
		atomic.AddInt64(cr.counter, int64(n))
	}
	return n, err
}

// Activity returns a snapshot of in-flight and queued work (thread-safe).
func (e *Engine) Activity() SyncActivity {
	e.mu.RLock()
	defer e.mu.RUnlock()
	act := SyncActivity{
		Pending:      int(atomic.LoadInt64(&e.pendingOps)),
		PendingBytes: atomic.LoadInt64(&e.pendingBytes),
		Active:       len(e.activeOps),
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

// ErroredFile describes a tracked file stuck in the error state, for the GUI's
// Issues view.
type ErroredFile struct {
	PairID    string
	LocalPath string
}

// ListErrored returns the pair's tracked files currently in the error state
// (max retries exhausted). Best-effort: returns nil if the store can't be read.
func (e *Engine) ListErrored() []ErroredFile {
	files, err := e.store.ListAll(e.ctx, e.pair.ID)
	if err != nil {
		return nil
	}
	var out []ErroredFile
	for _, sf := range files {
		if sf.SyncStatus == models.SyncStatusError {
			out = append(out, ErroredFile{PairID: e.pair.ID, LocalPath: sf.LocalPath})
		}
	}
	return out
}

// RetryFailed re-enqueues every errored file for another attempt: an upload when
// the local copy still exists, otherwise a download of the remote copy. Returns
// the number of operations re-enqueued. It may block briefly applying queue
// backpressure, so callers should not hold locks across it.
func (e *Engine) RetryFailed() int {
	files, err := e.store.ListAll(e.ctx, e.pair.ID)
	if err != nil {
		return 0
	}
	n := 0
	for _, sf := range files {
		if sf.SyncStatus != models.SyncStatusError {
			continue
		}
		localPath := filepath.Join(e.pair.LocalDir, sf.LocalPath)
		switch _, statErr := os.Stat(localPath); {
		case statErr == nil:
			e.enqueueWork(newSyncOperation(sf, models.OpTypeUpload, e.maxRetries))
		case sf.RemoteID != "":
			e.enqueueWork(newSyncOperation(sf, models.OpTypeDownload, e.maxRetries))
		default:
			continue
		}
		n++
	}
	return n
}

// PendingConflicts returns a snapshot of all unresolved manual conflicts.
func (e *Engine) PendingConflicts() []ConflictItem {
	return e.conflicts.List()
}

// ResolveConflictAction resolves a manually-queued conflict by applying the
// given action. It removes the conflict from the queue and enqueues the
// appropriate sync operation. Returns an error if the conflict is not found.
func (e *Engine) ResolveConflictAction(localPath string, action config.ConflictPolicy) error {
	if !e.conflicts.Remove(e.pair.ID, localPath) {
		return fmt.Errorf("sync: no pending conflict for %s", localPath)
	}

	existing, err := e.store.GetSyncFile(e.ctx, e.pair.ID, localPath)
	if err != nil || existing == nil {
		return fmt.Errorf("sync: no store record for %s", localPath)
	}

	switch action {
	case config.ConflictPolicyKeepLocal:
		e.enqueueWork(newSyncOperation(existing, models.OpTypeUpload, e.maxRetries))
	case config.ConflictPolicyKeepBoth:
		// Keep the local copy as a timestamped backup, then pull the remote.
		// Done directly (not via an OpTypeConflict op) so it doesn't re-enter the
		// manual-policy path and re-queue itself.
		e.backupLocalConflictCopy(filepath.Join(e.pair.LocalDir, localPath))
		e.enqueueWork(newSyncOperation(existing, models.OpTypeDownload, e.maxRetries))
	default: // keep_remote (and any unknown action) → pull the remote copy.
		e.enqueueWork(newSyncOperation(existing, models.OpTypeDownload, e.maxRetries))
	}
	return nil
}

// backupLocalConflictCopy copies the local file to a timestamped ".conflict"
// sibling, best-effort, so a remote-wins resolution preserves the local version.
func (e *Engine) backupLocalConflictCopy(localPath string) {
	backupPath := fmt.Sprintf("%s.conflict.%s", localPath, time.Now().Format("20060102-150405"))
	if src, err := os.Open(longPath(localPath)); err == nil {
		if dst, err := os.OpenFile(longPath(backupPath), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600); err == nil {
			_, _ = io.Copy(dst, src)
			_ = dst.Close()
		}
		_ = src.Close()
	}
}

// OnlineOnlyCount returns how many online-only placeholder files this pair has
// (tracked remote files that have not been downloaded).
func (e *Engine) OnlineOnlyCount() int {
	files, err := e.store.ListAll(e.ctx, e.pair.ID)
	if err != nil {
		return 0
	}
	n := 0
	for _, sf := range files {
		if sf.SyncStatus == models.SyncStatusOnlineOnly {
			n++
		}
	}
	return n
}

// MakeAvailableOffline enqueues downloads for every online-only placeholder in
// this pair, materialising them on disk. Returns the number queued. May block
// briefly on queue backpressure, so callers should not hold locks across it.
func (e *Engine) MakeAvailableOffline() int {
	files, err := e.store.ListAll(e.ctx, e.pair.ID)
	if err != nil {
		return 0
	}
	n := 0
	for _, sf := range files {
		if sf.SyncStatus == models.SyncStatusOnlineOnly && sf.RemoteID != "" {
			e.enqueueWork(newSyncOperation(sf, models.OpTypeDownload, e.maxRetries))
			n++
		}
	}
	return n
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
				existing, err := e.store.GetSyncFile(e.ctx, e.pair.ID, ev.Path)
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
				existing, err := e.store.GetSyncFile(e.ctx, e.pair.ID, ev.Path)
				if err == nil && existing != nil {
					sf.RemoteID = existing.RemoteID
				}
				e.enqueueWork(newSyncOperation(sf, models.OpTypeDeleteRemote, e.maxRetries))

			case models.ChangeOpRename:
				// Delete the old remote entry.
				oldSF := &models.SyncFile{
					LocalPath: ev.OldPath,
				}
				existing, err := e.store.GetSyncFile(e.ctx, e.pair.ID, ev.OldPath)
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

			// Respect pause state, quiet hours, and metered network — requeue
			// after a short delay and wait. Both the delay and the requeue send
			// honour ctx cancellation so this goroutine cannot block forever.
			shouldDefer := e.State() == StatePaused
			if !shouldDefer && e.appCfg.Schedule.QuietHoursEnabled {
				shouldDefer = IsQuietHours(e.appCfg.Schedule.QuietHoursStart, e.appCfg.Schedule.QuietHoursEnd)
			}
			if !shouldDefer && e.appCfg.Schedule.PauseOnMetered {
				shouldDefer = IsMeteredNetwork()
			}
			if shouldDefer {
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

			// Large-file gate: keep at most maxConcurrentLargeUploads big uploads
			// in flight (they each buffer a chunk and hog bandwidth). A worker that
			// can't get a slot re-queues the large op after a short delay and moves
			// on to the next item — so a burst of large files never starves the
			// many small-file uploads, which keep running at full concurrency.
			isLarge := op.OpType == models.OpTypeUpload && op.File != nil && op.File.Size >= largeUploadThreshold
			if isLarge {
				select {
				case e.largeUploadSem <- struct{}{}:
					// Acquired a large-upload slot.
				default:
					go func(op *models.SyncOperation) {
						select {
						case <-time.After(largeRequeueDelay):
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
			}

			e.markActive(id, op)
			err := e.executeOperation(op)
			e.markDone(id)
			if isLarge {
				<-e.largeUploadSem
			}
			if err != nil {
				// A full-Drive error won't fix itself on retry — exhaust the retry
				// budget immediately so we fail fast with a clear message instead of
				// hammering a full account five times with backoff.
				var quotaErr *drive.QuotaExceededError
				if errors.As(err, &quotaErr) {
					op.Attempts = op.MaxAttempts
				}

				op.Attempts++
				op.LastError = err.Error()

				// Surface the failure so a stalled/looping sync isn't silent. Per
				// attempt is a WARN; the terminal give-up below is reported via the
				// error channel.
				opPath := ""
				if op.File != nil {
					opPath = op.File.LocalPath
				}
				slog.Warn("sync operation failed; scheduling retry",
					"op", op.OpType, "file", opPath,
					"attempt", op.Attempts, "max", op.MaxAttempts, "error", err)

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
							atomic.AddInt64(&e.pendingBytes, -opBytes(op))
						}
					}(op, delay)
				} else {
					// Max retries exceeded — terminal failure. Mark the file as
					// errored and clear it from the backlog.
					if op.File != nil {
						_ = e.store.UpdateStatus(e.ctx, e.pair.ID, op.File.LocalPath, models.SyncStatusError)
					}
					e.sendError(fmt.Errorf("sync: max retries exceeded for %s %s: %w",
						op.OpType, op.File.LocalPath, err))
					atomic.AddInt64(&e.pendingOps, -1)
					atomic.AddInt64(&e.pendingBytes, -opBytes(op))
				}
			} else {
				// Operation succeeded — terminal. Clear it from the backlog,
				// record it for the activity feed, and update the last sync time.
				atomic.AddInt64(&e.pendingOps, -1)
				atomic.AddInt64(&e.pendingBytes, -opBytes(op))
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
	// Direction-aware filtering: skip operations the pair's data-flow policy
	// forbids (defence in depth — generation already gates these). Uses the
	// gate helpers so the legacy ForwardOnly flag is honoured too.
	switch op.OpType {
	case models.OpTypeUpload:
		if !e.allowsUpload() {
			return nil
		}
	case models.OpTypeDownload:
		if !e.allowsDownload() {
			return nil
		}
	case models.OpTypeDeleteRemote:
		if !e.allowsRemoteDelete() {
			return nil
		}
	case models.OpTypeDeleteLocal:
		if !e.allowsLocalDelete() {
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
	info, err := os.Stat(longPath(localPath))
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
	encryptedName, err := e.encName(filepath.Base(sf.LocalPath))
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
		f, err := os.Open(longPath(localPath))
		if err != nil {
			err = fmt.Errorf("open local: %w", err)
			_ = pw.CloseWithError(err)
			errCh <- err
			return
		}
		defer func() { _ = f.Close() }()

		err = crypto.EncryptStream(f, pw, e.masterKey, sf.LocalPath)
		_ = pw.CloseWithError(err) // nil → clean EOF; non-nil → reader aborts
		errCh <- err
	}()

	// 6. Upload or update on Google Drive.
	//
	// For a file we have never recorded (RemoteID == ""), first check whether a
	// file with this encrypted name already exists in the target folder. This
	// closes a crash-recovery gap: if a previous run uploaded the file but was
	// killed before recording it in the store, a plain Create here would silently
	// create a *second* copy on Drive. Re-using the existing file via Update keeps
	// the upload idempotent across restarts. Encrypted names are deterministic, so
	// the lookup reliably finds the prior copy. The extra lookup only runs for
	// new/untracked files; already-tracked files go straight to Update.
	remoteID := sf.RemoteID
	if remoteID == "" {
		if existing, serr := e.driveClient.SearchByName(e.ctx, encryptedName, parentID); serr == nil {
			for _, ef := range existing {
				if !ef.IsFolder() {
					remoteID = ef.ID
					break
				}
			}
		}
		// A failed lookup is non-fatal: fall through to Create. The worst case is
		// the pre-existing duplicate behaviour, never a lost or corrupt file.
	}

	// Count ciphertext bytes as Drive pulls them from the pipe, so the live
	// transfer rate tracks real upload progress for this (possibly large) file.
	countingPR := &countingReader{r: pr, counter: &e.liveBytesUp}

	var remoteFile *drive.DriveFile
	if remoteID == "" {
		remoteFile, err = e.driveClient.UploadFile(e.ctx, encryptedName, parentID, countingPR, info.ModTime())
	} else {
		remoteFile, err = e.driveClient.UpdateFile(e.ctx, remoteID, countingPR, info.ModTime())
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

	// TOCTOU guard: the file is hashed (step 2) and then re-opened and streamed
	// separately, so it can change in between. If its size or mtime moved, the
	// bytes now on Drive may not match `hash` — fail the op so it retries with a
	// fresh hash rather than recording a hash that doesn't describe the uploaded
	// content. The retry re-uploads via the existing remote ID (or the dedup
	// lookup), so no duplicate is created.
	if cur, statErr := os.Stat(longPath(localPath)); statErr == nil {
		if cur.Size() != info.Size() || !cur.ModTime().Equal(info.ModTime()) {
			return fmt.Errorf("sync: %s changed during upload; will retry", sf.LocalPath)
		}
	}

	// TOCTOU guard phase 2: re-hash the file after the upload stream completed.
	// The size/mtime check above catches most mutations, but an in-place
	// overwrite can change content without updating size or mtime (rare, but
	// possible). Comparing the post-upload hash to the pre-upload hash recorded
	// in sf.LocalHash catches this case. The retry re-uploads via the existing
	// remote ID (or dedup lookup), so no duplicate is created.
	if reHash, rhErr := e.scanner.ComputeHash(localPath); rhErr == nil && reHash != sf.LocalHash {
		return fmt.Errorf("sync: %s content changed during upload (hash mismatch); will retry", sf.LocalPath)
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
	if err := e.store.PutSyncFile(e.ctx, sf); err != nil {
		return fmt.Errorf("sync: record uploaded file %s: %w", sf.LocalPath, err)
	}

	// 8. Update stats. Byte throughput is tallied live via countingPR above; here
	// we only bump the completed-file counter.
	e.mu.Lock()
	e.stats.FilesUploaded++
	e.mu.Unlock()

	return nil
}

// ---------------------------------------------------------------------------
// Download
// ---------------------------------------------------------------------------

// diskSpaceHeadroom is the free space kept in reserve beyond a download's own
// size, so a sync never fills the volume to the last byte.
const diskSpaceHeadroom = 64 * 1024 * 1024 // 64 MiB

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
	dir := filepath.Dir(localPath)
	if err := os.MkdirAll(longPath(dir), 0750); err != nil {
		return fmt.Errorf("sync: creating directory for %s: %w", sf.LocalPath, err)
	}

	// Refuse to start the write if the target volume can't hold the file, rather
	// than filling the disk and failing partway. sf.Size is the ciphertext size
	// (a slight overestimate of the plaintext), and we keep a small headroom.
	if sf.Size > 0 {
		if avail, derr := availableDiskBytes(dir); derr == nil {
			needed := uint64(sf.Size) + diskSpaceHeadroom
			if avail < needed {
				return fmt.Errorf("sync: not enough disk space for %s: need ~%d MiB, %d MiB free",
					sf.LocalPath, needed/(1024*1024), avail/(1024*1024))
			}
		}
	}

	// Decrypt into a temporary file in the same directory, then atomically
	// rename it over the target. This keeps a process kill (or a decrypt failure)
	// mid-download from ever leaving a truncated, readable file at the real path:
	// the partial plaintext only lives in a ".tmp" file — which the watcher and
	// scanner ignore (see DefaultIgnorePatterns) so it is never mistaken for a
	// user file — and the real path is replaced in a single atomic os.Rename.
	// Placing the temp file in the same directory guarantees the rename stays on
	// one volume (cross-device renames fail and aren't atomic).
	tmp, err := os.CreateTemp(longPath(dir), filepath.Base(localPath)+".gcrypt-part-*.tmp")
	if err != nil {
		return fmt.Errorf("sync: creating temp file for %s: %w", sf.LocalPath, err)
	}
	tmpPath := tmp.Name()
	// On any failure below, discard the temp file so partial data never lingers.
	// The success path sets tmp = nil first, making this a no-op.
	defer func() {
		if tmp != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	// Decrypt the stream into the temp file. Count ciphertext bytes as they
	// arrive so the live transfer rate tracks real download progress for this
	// (possibly large) file rather than spiking only when it finishes.
	countingRC := &countingReader{r: rc, counter: &e.liveBytesDown}
	if err := crypto.DecryptStream(countingRC, tmp, e.masterKey, sf.LocalPath); err != nil {
		return fmt.Errorf("sync: decrypt stream %s: %w", sf.LocalPath, err)
	}

	// Flush to disk before the rename so the on-disk file is complete the moment
	// it appears under the real name, then close it (Windows can't rename over /
	// from an open handle reliably).
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync: flushing %s: %w", sf.LocalPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("sync: closing temp file for %s: %w", sf.LocalPath, err)
	}

	// Atomically move the completed plaintext into place (replacing any existing
	// file). os.Rename uses MoveFileEx(MOVEFILE_REPLACE_EXISTING) on Windows.
	if err := os.Rename(tmpPath, longPath(localPath)); err != nil {
		return fmt.Errorf("sync: finalizing %s: %w", sf.LocalPath, err)
	}
	tmp = nil // rename succeeded — disarm the cleanup defer

	// Restore the file's modification time from the remote metadata (uploads
	// preserve it on Drive), so a downloaded file keeps its original timestamp
	// instead of showing the download time. Best-effort: a failure here doesn't
	// invalidate the otherwise-complete download.
	if !sf.ModTime.IsZero() {
		_ = os.Chtimes(longPath(localPath), time.Now(), sf.ModTime)
	}

	// 3. Re-scan the file to get the final local hash and update stats. The stat
	// confirms the finalised file is present before we record it as synced.
	if _, err := os.Stat(longPath(localPath)); err != nil {
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
	if err := e.store.PutSyncFile(e.ctx, sf); err != nil {
		return fmt.Errorf("sync: record downloaded file %s: %w", sf.LocalPath, err)
	}

	// 5. Update stats. Byte throughput is tallied live via countingRC above; here
	// we only bump the completed-file counter.
	e.mu.Lock()
	e.stats.FilesDownloaded++
	e.mu.Unlock()

	return nil
}

// ---------------------------------------------------------------------------
// Delete Remote
// ---------------------------------------------------------------------------

// deleteRemote removes a file from Google Drive and deletes its local store
// record. The remote file is moved to Drive's trash (not permanently deleted),
// so a propagated deletion stays recoverable from the Drive trash.
func (e *Engine) deleteRemote(sf *models.SyncFile) error {
	// Delete/edit conflict: the local copy was removed, but if the remote copy
	// was edited since our last sync, trashing it would lose those remote
	// changes. In a two-way sync the safe resolution is "edit beats delete" —
	// pull the remote version back locally instead of deleting it. (In
	// upload-only mode the local side is authoritative, so we still delete.)
	if sf.RemoteID != "" && e.allowsDownload() {
		if rf, err := e.driveClient.GetFile(e.ctx, sf.RemoteID); err == nil {
			if rf.MD5Hash != "" && sf.RemoteHash != "" && rf.MD5Hash != sf.RemoteHash {
				slog.Warn("delete/edit conflict: remote edited after local delete; restoring local copy",
					"file", sf.LocalPath)
				dl := *sf
				dl.RemoteHash = rf.MD5Hash
				dl.Size = rf.Size
				dl.ModTime = rf.ModTime
				e.enqueueDetached(newSyncOperation(&dl, models.OpTypeDownload, e.maxRetries))
				return nil
			}
		}
	}

	// 1. If RemoteID is not empty, move the remote copy to trash.
	if sf.RemoteID != "" {
		if err := e.driveClient.TrashFile(e.ctx, sf.RemoteID); err != nil {
			return fmt.Errorf("sync: trash remote %s: %w", sf.LocalPath, err)
		}
	}

	// 2. Delete from store.
	if err := e.store.DeleteSyncFile(e.ctx, e.pair.ID, sf.LocalPath); err != nil {
		return fmt.Errorf("sync: delete store record %s: %w", sf.LocalPath, err)
	}

	// 3. Update stats.
	e.mu.Lock()
	e.stats.FilesDeleted++
	e.mu.Unlock()

	// 4. Best-effort: remove now-empty encrypted parent folders on Drive so a
	// deletion doesn't leave empty folders behind. Only meaningful when the file
	// actually lived on Drive (had a RemoteID).
	if sf.RemoteID != "" {
		e.pruneEmptyRemoteDirs(path.Dir(filepath.ToSlash(sf.LocalPath)))
	}

	return nil
}

// ---------------------------------------------------------------------------
// Delete Local
// ---------------------------------------------------------------------------

// deleteLocal removes a local file and its store record.
func (e *Engine) deleteLocal(sf *models.SyncFile) error {
	localPath := filepath.Join(e.pair.LocalDir, sf.LocalPath)

	// Delete/edit conflict: the remote copy is gone, but if the local file has
	// unsynced edits since our last sync, deleting it would lose them. "Edit
	// beats delete" — re-upload the local copy to recreate it remotely instead.
	// (Only when uploads are allowed; download-only treats remote as authoritative.)
	if e.allowsUpload() {
		if cur, err := e.scanner.ComputeHash(localPath); err == nil && sf.LocalHash != "" && cur != sf.LocalHash {
			slog.Warn("delete/edit conflict: local edited after remote delete; restoring remote copy",
				"file", sf.LocalPath)
			up := *sf
			up.RemoteID = "" // old remote was deleted/trashed; recreate it
			up.LocalHash = cur
			e.enqueueDetached(newSyncOperation(&up, models.OpTypeUpload, e.maxRetries))
			return nil
		}
	}

	// 1. Delete the local file.
	if err := os.Remove(longPath(localPath)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("sync: delete local file %s: %w", localPath, err)
	}

	// 2. Delete from store.
	if err := e.store.DeleteSyncFile(e.ctx, e.pair.ID, sf.LocalPath); err != nil {
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

// resolveConflict reconciles a file that changed remotely. The resolution
// strategy depends on the pair's ConflictPolicy:
//   - auto: last-write-wins (original behaviour)
//   - keep_local: always push the local copy
//   - keep_remote: always pull the remote copy
//   - keep_both: download remote and keep the local copy as a backup
//   - manual: queue for resolution via the GUI (no automatic action)
func (e *Engine) resolveConflict(sf *models.SyncFile) error {
	localPath := filepath.Join(e.pair.LocalDir, sf.LocalPath)

	// If the local copy is gone, re-create it from the remote regardless of
	// policy. A genuine local deletion is reconciled separately via delete
	// operations; pulling the remote here is the safe default.
	if _, statErr := os.Stat(longPath(localPath)); os.IsNotExist(statErr) {
		return e.downloadFile(sf)
	}

	policy := e.pair.ConflictPolicy
	if policy == "" {
		policy = config.ConflictPolicyAuto
	}

	switch policy {
	case config.ConflictPolicyKeepLocal:
		return e.uploadFile(sf)

	case config.ConflictPolicyKeepRemote:
		return e.downloadFile(sf)

	case config.ConflictPolicyKeepBoth:
		// Back up local, then download remote.
		e.backupLocalConflictCopy(localPath)
		return e.downloadFile(sf)

	case config.ConflictPolicyManual:
		// Queue for manual resolution — don't touch the file.
		var localModTime time.Time
		if info, err := os.Stat(longPath(localPath)); err == nil {
			localModTime = info.ModTime()
		}
		var remoteModTime time.Time
		if sf.RemoteID != "" {
			if remoteFile, err := e.driveClient.GetFile(e.ctx, sf.RemoteID); err == nil {
				remoteModTime = remoteFile.ModTime
			}
		}
		e.conflicts.Add(ConflictItem{
			PairID:        e.pair.ID,
			LocalPath:     sf.LocalPath,
			RemoteID:      sf.RemoteID,
			LocalModTime:  localModTime,
			RemoteModTime: remoteModTime,
			LocalHash:     sf.LocalHash,
			RemoteHash:    sf.RemoteHash,
		})
		// Mark as conflict in the store so the Issues view picks it up.
		_ = e.store.UpdateStatus(e.ctx, e.pair.ID, sf.LocalPath, models.SyncStatusConflict)
		return nil

	default: // auto (last-write-wins)
		// If the local copy is unchanged since our last sync, just pull.
		if curHash, err := e.scanner.ComputeHash(localPath); err == nil && curHash == sf.LocalHash {
			return e.downloadFile(sf)
		}

		// Both sides diverged — apply last-write-wins by modification time.
		localModTime := sf.ModTime
		if info, err := os.Stat(longPath(localPath)); err == nil {
			localModTime = info.ModTime()
		}
		var remoteModTime time.Time
		if sf.RemoteID != "" {
			if remoteFile, err := e.driveClient.GetFile(e.ctx, sf.RemoteID); err == nil {
				remoteModTime = remoteFile.ModTime
			}
		}

		// Local strictly newer → push.
		if localModTime.After(remoteModTime) {
			return e.uploadFile(sf)
		}

		// Remote newer (or tie) → remote wins with local backup.
		e.backupLocalConflictCopy(localPath)
		return e.downloadFile(sf)
	}
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
			if e.appCfg.Schedule.QuietHoursEnabled && IsQuietHours(e.appCfg.Schedule.QuietHoursStart, e.appCfg.Schedule.QuietHoursEnd) {
				continue
			}
			if e.appCfg.Schedule.PauseOnMetered && IsMeteredNetwork() {
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
			// Immediate scan triggered by SyncNow(). A manual request always does a
			// full reconcile rather than the change-gated fast path, so "Sync Now"
			// is guaranteed to reconcile even if the changes feed reports nothing.
			if e.State() == StatePaused {
				continue
			}

			if err := e.reconcileRemote(); err != nil {
				e.sendError(fmt.Errorf("sync: sync-now poll: %w", err))
			}
		}
	}
}

// pollOnce performs one remote poll cycle. It uses the Drive changes feed as a
// cheap gate: once a baseline page token exists, it asks Drive only "did
// anything relevant change since last time?" and skips the full tree
// enumeration entirely when nothing did. A full reconcile runs on the first
// poll (to establish the baseline), whenever a relevant change is seen, and as a
// safe fallback if the changes feed errors or its token has expired.
func (e *Engine) pollOnce() error {
	if e.changePageToken == "" {
		// No baseline yet: capture "now" so subsequent polls are incremental, then
		// do a full reconcile to catch anything that changed before the token.
		if token, err := e.driveClient.GetStartPageToken(e.ctx); err == nil {
			e.changePageToken = token
		}
		return e.reconcileRemote()
	}

	relevant, newToken, err := e.pollChanges()
	if err != nil {
		// Feed failed or token expired (410). Drop the token so the next poll
		// re-establishes a baseline, and reconcile fully now to stay correct.
		e.changePageToken = ""
		return e.reconcileRemote()
	}
	e.changePageToken = newToken
	if !relevant {
		return nil // nothing in our tree changed — skip the full enumeration
	}
	return e.reconcileRemote()
}

// pollChanges drains the account-wide Drive changes feed from the held page
// token, reporting whether any change touches this pair's tree and returning the
// new token to hold for next time. A change is relevant when it touches a file
// gcrypt already tracks for this pair, or a file whose parent is one of the
// pair's known (cached) folders — which includes newly created files/subfolders
// under the sync root.
func (e *Engine) pollChanges() (relevant bool, newToken string, err error) {
	folderIDs := e.knownFolderIDs()

	tracked := make(map[string]struct{})
	if files, lerr := e.store.ListAll(e.ctx, e.pair.ID); lerr == nil {
		for _, sf := range files {
			if sf.RemoteID != "" {
				tracked[sf.RemoteID] = struct{}{}
			}
		}
	}

	token := e.changePageToken
	for {
		changes, next, newStart, lerr := e.driveClient.ListChanges(e.ctx, token)
		if lerr != nil {
			return false, e.changePageToken, lerr
		}
		for _, ch := range changes {
			if _, ok := tracked[ch.FileID]; ok {
				relevant = true
				continue
			}
			for _, p := range ch.Parents {
				if _, ok := folderIDs[p]; ok {
					relevant = true
					break
				}
			}
		}
		if next != "" {
			token = next
			continue
		}
		return relevant, newStart, nil
	}
}

// knownFolderIDs returns the set of Drive folder IDs that make up this pair's
// tree: the pair's root folder plus every subfolder discovered (and cached)
// during reconciliation.
func (e *Engine) knownFolderIDs() map[string]struct{} {
	e.folderMu.Lock()
	ids := make(map[string]struct{}, len(e.folderCache)+1)
	for _, id := range e.folderCache {
		ids[id] = struct{}{}
	}
	e.folderMu.Unlock()
	ids[e.pair.DriveFolderID] = struct{}{}
	return ids
}

// reconcileRemote performs a full remote poll cycle: it enumerates the pair's
// Drive tree and generates download/conflict/delete operations as needed.
func (e *Engine) reconcileRemote() error {
	// Enumerate all remote files with their decrypted local paths (flat or
	// hierarchical layout, transparently), plus every remote folder discovered.
	remoteFiles, remoteDirs, err := e.collectRemoteFiles()
	if err != nil {
		return fmt.Errorf("list remote files: %w", err)
	}

	// Mirror remote folders locally only when this direction pulls to local.
	if e.allowsDownload() {
		e.ensureLocalDirs(remoteDirs)
	}

	// Build a set of remote IDs currently tracked in the store.
	trackedFiles, err := e.store.ListAll(e.ctx, e.pair.ID)
	if err != nil {
		return fmt.Errorf("list store files: %w", err)
	}

	remoteByID := make(map[string]*models.SyncFile, len(trackedFiles))
	for _, sf := range trackedFiles {
		if sf.RemoteID != "" {
			remoteByID[sf.RemoteID] = sf
		}
	}

	// Nothing to do remote→local for upload-only / mirror directions.
	if !e.allowsDownload() && !e.allowsLocalDelete() {
		return nil
	}

	if e.allowsDownload() {
		// Detect remote files that would collide on a case-insensitive / Unicode-
		// normalising local filesystem (Windows, macOS): two distinct remote names
		// mapping to the same local path. Downloading both would silently clobber
		// one, so keep the first and skip the rest, reporting each collision.
		collisionSkip := e.detectCollisions(remoteFiles)
		twoWay := e.pair.EffectiveDirection() == config.SyncDirTwoWay

		for _, rf := range remoteFiles {
			if _, skip := collisionSkip[rf.ID]; skip {
				continue
			}
			// Honour the ignore rules on the remote side too. An older build (or a
			// pre-ignore config) may have uploaded now-ignored trees such as
			// node_modules/ or .git/ to Drive; never pull them back down. Tracked
			// ignored files are trashed by the local scan's deletion pass; untracked
			// remote orphans have no store record to drive that, so trash them here
			// when the direction permits remote deletion (recoverable from Drive
			// trash). Without this, two-way sync would re-download the junk the
			// upload side now skips — an endless restart-time churn.
			if e.scanner.ShouldIgnore(filepath.Join(e.pair.LocalDir, rf.LocalPath)) {
				if _, isTracked := remoteByID[rf.ID]; !isTracked && e.allowsRemoteDelete() {
					e.enqueueWork(newSyncOperation(&models.SyncFile{
						SyncRootID: e.pair.ID,
						LocalPath:  rf.LocalPath,
						RemoteID:   rf.ID,
					}, models.OpTypeDeleteRemote, e.maxRetries))
				}
				continue
			}
			tracked, isTracked := remoteByID[rf.ID]
			if !isTracked {
				// New remote file. In on-demand (online-only) mode, record it as a
				// tracked placeholder instead of downloading — it can be fetched
				// later via "make available offline".
				sf := &models.SyncFile{
					SyncRootID: e.pair.ID,
					LocalPath:  rf.LocalPath,
					RemoteID:   rf.ID,
					RemoteHash: rf.RemoteHash,
					Size:       rf.Size,
					ModTime:    rf.ModTime,
				}
				if e.pair.OnlineOnly {
					sf.SyncStatus = models.SyncStatusOnlineOnly
					if perr := e.store.PutSyncFile(e.ctx, sf); perr != nil {
						e.sendError(fmt.Errorf("sync: record online-only file %s: %w", sf.LocalPath, perr))
					}
					continue
				}
				e.enqueueWork(newSyncOperation(sf, models.OpTypeDownload, e.maxRetries))
				continue
			}

			// Already tracked: detect a remote modification by comparing the Drive
			// content hash against what we recorded at the last sync.
			if rf.RemoteHash != "" && tracked.RemoteHash != "" && rf.RemoteHash != tracked.RemoteHash {
				// An online-only placeholder isn't on disk; just refresh its recorded
				// hash so we don't re-detect the change every poll.
				if tracked.SyncStatus == models.SyncStatusOnlineOnly {
					tracked.RemoteHash = rf.RemoteHash
					tracked.Size = rf.Size
					tracked.ModTime = rf.ModTime
					if perr := e.store.PutSyncFile(e.ctx, tracked); perr != nil {
						e.sendError(fmt.Errorf("sync: update online-only file %s: %w", tracked.LocalPath, perr))
					}
					continue
				}
				sf := &models.SyncFile{
					SyncRootID: e.pair.ID,
					LocalPath:  tracked.LocalPath,
					RemoteID:   rf.ID,
					RemoteHash: rf.RemoteHash,
					LocalHash:  tracked.LocalHash,
					Size:       rf.Size,
					ModTime:    rf.ModTime,
				}
				// In two-way mode a remote edit may clash with a local edit → run
				// conflict resolution (honours the pair's ConflictPolicy). In
				// download-only mode the remote always wins → straight download.
				if twoWay {
					e.enqueueWork(newSyncOperation(sf, models.OpTypeConflict, e.maxRetries))
				} else {
					e.enqueueWork(newSyncOperation(sf, models.OpTypeDownload, e.maxRetries))
				}
			}
		}
	}

	// For each store file no longer present remotely → delete locally, if this
	// direction propagates remote deletions. A successful remote enumeration is
	// authoritative (collectRemoteFiles aborts the reconcile on any API error),
	// so a bulk remote delete propagates in full.
	if e.allowsLocalDelete() {
		remoteIDLookup := make(map[string]struct{}, len(remoteFiles))
		for _, rf := range remoteFiles {
			remoteIDLookup[rf.ID] = struct{}{}
		}
		for _, sf := range trackedFiles {
			if sf.RemoteID == "" {
				continue
			}
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
func (e *Engine) collectRemoteFiles() (files []remoteFileInfo, dirs []string, err error) {
	if err := e.collectRemoteHierarchical(e.pair.DriveFolderID, "", &files, &dirs); err != nil {
		return nil, nil, err
	}
	return files, dirs, nil
}

// collectRemoteHierarchical recursively walks the encrypted Drive folder tree
// rooted at folderID, which maps to the plaintext relative directory relDir
// ("" = the sync root). It appends decrypted file entries to acc, every
// discovered subfolder's relative path to dirs, and caches the IDs of folders
// it discovers so later uploads can reuse them.
func (e *Engine) collectRemoteHierarchical(folderID, relDir string, acc *[]remoteFileInfo, dirs *[]string) error {
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
				*dirs = append(*dirs, childRel)
				if err := e.collectRemoteHierarchical(rf.ID, childRel, acc, dirs); err != nil {
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

// ensureLocalDirs creates the local directories corresponding to the given
// remote folder relative paths (slash-separated), so empty remote folders are
// mirrored locally. Only the folders discovered in the current reconcile are
// passed in, so a folder deleted remotely is not resurrected.
func (e *Engine) ensureLocalDirs(dirs []string) {
	for _, rel := range dirs {
		local := filepath.Join(e.pair.LocalDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(longPath(local), 0750); err != nil {
			e.sendError(fmt.Errorf("sync: create local folder %s: %w", rel, err))
		}
	}
}

// detectCollisions finds remote files whose local paths would collide on a
// case-insensitive / Unicode-normalising filesystem (Windows, macOS). It returns
// the set of remote IDs to skip — the first file seen for each colliding key is
// kept, the rest are skipped — and reports each collision so the user can rename
// one side. Without this, the second download would silently overwrite the first.
func (e *Engine) detectCollisions(files []remoteFileInfo) map[string]struct{} {
	seen := make(map[string]remoteFileInfo, len(files))
	skip := make(map[string]struct{})
	for _, rf := range files {
		key := collisionKey(rf.LocalPath)
		if first, ok := seen[key]; ok {
			skip[rf.ID] = struct{}{}
			e.sendError(fmt.Errorf("sync: filename collision on this filesystem between %q and %q — keeping the first and skipping the second; rename one in Drive to sync both",
				first.LocalPath, rf.LocalPath))
			continue
		}
		seen[key] = rf
	}
	return skip
}

// collisionKey case-folds and Unicode-normalises (NFC) a local path so names
// differing only by case or normalisation map to the same key, matching how
// Windows and macOS treat them.
func collisionKey(localPath string) string {
	return norm.NFC.String(strings.ToLower(filepath.ToSlash(localPath)))
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

	// Fast path: already resolved/cached.
	e.folderMu.Lock()
	cached, ok := e.folderCache[slashDir]
	e.folderMu.Unlock()
	if ok {
		return cached, nil
	}

	// Resolve (creating as needed) under singleflight keyed by the directory
	// path. Concurrent uploads into the same new directory share one resolution
	// — so no duplicate Drive folders are created — while uploads into different
	// directories resolve in parallel. The parent is resolved recursively (also
	// via singleflight), so only the single missing component is created here.
	v, err, _ := e.folderGroup.Do(slashDir, func() (interface{}, error) {
		e.folderMu.Lock()
		id, ok := e.folderCache[slashDir]
		e.folderMu.Unlock()
		if ok {
			return id, nil
		}

		parentID, err := e.resolveRemoteDir(path.Dir(slashDir))
		if err != nil {
			return "", err
		}
		comp := path.Base(slashDir)
		encName, err := e.encName(comp)
		if err != nil {
			return "", fmt.Errorf("encrypt dir component %q: %w", comp, err)
		}
		childID, err := e.driveClient.EnsureFolderUnder(e.ctx, parentID, encName)
		if err != nil {
			return "", fmt.Errorf("ensure remote folder %q: %w", slashDir, err)
		}
		e.folderMu.Lock()
		e.folderCache[slashDir] = childID
		e.folderMu.Unlock()
		return childID, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// cacheRemoteDir records the Drive folder ID for a plaintext relative directory
// (slash-separated) discovered while walking the remote tree.
func (e *Engine) cacheRemoteDir(relDir, folderID string) {
	e.folderMu.Lock()
	e.folderCache[relDir] = folderID
	e.folderMu.Unlock()
}

// uncacheRemoteDir drops a plaintext relative directory from the folder cache,
// used after its remote folder has been deleted.
func (e *Engine) uncacheRemoteDir(slashDir string) {
	e.folderMu.Lock()
	delete(e.folderCache, slashDir)
	e.folderMu.Unlock()
}

// pruneEmptyRemoteDirs removes now-empty encrypted folders on Drive, walking up
// from relDir toward the sync root, so deletions don't leave empty folders
// behind. It is best-effort: any error simply stops the walk (a leftover empty
// folder is harmless and a later deletion will retry). The pair's root Drive
// folder is never removed.
func (e *Engine) pruneEmptyRemoteDirs(relDir string) {
	slashDir := path.Clean(filepath.ToSlash(relDir))
	for slashDir != "." && slashDir != "/" && slashDir != "" {
		id, ok := e.lookupRemoteDirID(slashDir)
		if !ok || id == e.pair.DriveFolderID {
			return
		}
		empty, err := e.driveFolderIsEmpty(id)
		if err != nil || !empty {
			// On error, or while the folder still has contents, stop: ancestors
			// can't be empty either.
			return
		}
		if err := e.driveClient.DeleteFile(e.ctx, id); err != nil {
			slog.Warn("prune empty remote folder failed", "dir", slashDir, "error", err)
			return
		}
		e.uncacheRemoteDir(slashDir)
		slashDir = path.Dir(slashDir)
	}
}

// driveFolderIsEmpty reports whether the given Drive folder has no non-trashed
// children.
func (e *Engine) driveFolderIsEmpty(folderID string) (bool, error) {
	files, _, err := e.driveClient.ListFiles(e.ctx, folderID, "")
	if err != nil {
		return false, err
	}
	return len(files) == 0, nil
}

// lookupRemoteDirID resolves the Drive folder ID for a plaintext relative
// directory WITHOUT creating any folders (unlike resolveRemoteDir). It returns
// false if the folder does not exist remotely. Discovered IDs are cached.
func (e *Engine) lookupRemoteDirID(slashDir string) (string, bool) {
	slashDir = path.Clean(slashDir)
	if slashDir == "." || slashDir == "/" || slashDir == "" {
		return e.pair.DriveFolderID, true
	}

	e.folderMu.Lock()
	cached, ok := e.folderCache[slashDir]
	e.folderMu.Unlock()
	if ok {
		return cached, true
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
		e.folderMu.Lock()
		id, ok := e.folderCache[cum]
		e.folderMu.Unlock()
		if ok {
			parentID = id
			continue
		}
		encName, err := e.encName(comp)
		if err != nil {
			return "", false
		}
		childID, found := e.findChildFolderID(parentID, encName)
		if !found {
			return "", false
		}
		e.cacheRemoteDir(cum, childID)
		parentID = childID
	}
	return parentID, true
}

// findChildFolderID returns the ID of the immediate child folder of parentID
// whose (encrypted) name equals encName, or false if there is no such folder.
func (e *Engine) findChildFolderID(parentID, encName string) (string, bool) {
	pageToken := ""
	for {
		files, next, err := e.driveClient.ListFiles(e.ctx, parentID, pageToken)
		if err != nil {
			return "", false
		}
		for _, f := range files {
			if f.IsFolder() && f.Name == encName {
				return f.ID, true
			}
		}
		if next == "" {
			return "", false
		}
		pageToken = next
	}
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
