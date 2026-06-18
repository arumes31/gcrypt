package sync

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/arumes31/gcrypt/internal/appstate"
	"github.com/arumes31/gcrypt/internal/config"
	"github.com/arumes31/gcrypt/internal/drive"
)

// ---------------------------------------------------------------------------
// Logger interface (avoids import cycle with service package)
// ---------------------------------------------------------------------------

// Logger defines the minimal logging interface needed by the SyncManager.
// The *service.Logger type satisfies this interface.
type Logger interface {
	Info(msg string, fields ...map[string]interface{})
	Error(msg string, fields ...map[string]interface{})
	Warn(msg string, fields ...map[string]interface{})
}

// ---------------------------------------------------------------------------
// Aggregated types
// ---------------------------------------------------------------------------

// PairStatus holds the status of a single sync pair.
type PairStatus struct {
	ID       string
	State    SyncState
	Stats    SyncStats
	Activity SyncActivity
}

// AggregatedState represents the combined state of all sync pairs.
type AggregatedState struct {
	OverallState SyncState
	PairStatuses []PairStatus
}

// ---------------------------------------------------------------------------
// SyncManager
// ---------------------------------------------------------------------------

// SyncManager manages multiple Engine instances, one per SyncPair.
// It provides aggregated state, error forwarding, and per-pair lifecycle
// control.
type SyncManager struct {
	mu          sync.RWMutex
	engines     map[string]*Engine // pair ID → engine
	cfg         *config.Config
	store       *drive.Store
	driveClient *drive.Client
	masterKey   []byte
	logger      Logger

	// Aggregated state
	stateChangeCh chan AggregatedState
	errorCh       chan error
	stopCh        chan struct{}
	stopOnce      sync.Once // guards close(stopCh) so StopAll is idempotent

	// Track per-engine error forwarding cancel functions so we can clean up
	// when an engine is removed.
	engineCancels map[string]context.CancelFunc

	// OnError is a channel that aggregates errors from all engines.
	// Errors received on each engine's OnError channel are forwarded here
	// so the tray or controller can monitor a single channel for
	// asynchronous errors from any engine.
	OnError chan error

	// OnStateChange is an optional callback invoked when any engine's
	// appstate-level state transitions. This allows the tray/controller to
	// react to lifecycle transitions (Scanning → Syncing → Idle, etc.)
	// without watching individual engine channels.
	OnStateChange func(oldState, newState appstate.State)
}

// NewSyncManager creates a new SyncManager.
func NewSyncManager(cfg *config.Config, store *drive.Store, driveClient *drive.Client, masterKey []byte, logger Logger) *SyncManager {
	return &SyncManager{
		engines:       make(map[string]*Engine),
		cfg:           cfg,
		store:         store,
		driveClient:   driveClient,
		masterKey:     masterKey,
		logger:        logger,
		stateChangeCh: make(chan AggregatedState, 16),
		errorCh:       make(chan error, 64),
		stopCh:        make(chan struct{}),
		engineCancels: make(map[string]context.CancelFunc),
		OnError:       make(chan error, 64),
	}
}

// StartAll starts engines for all enabled sync pairs.
func (m *SyncManager) StartAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.cfg.SyncPairs {
		pair := &m.cfg.SyncPairs[i]
		if !pair.Enabled {
			if m.logger != nil {
				m.logger.Info("skipping disabled sync pair", map[string]interface{}{
					"pair_id": pair.ID,
				})
			}
			continue
		}
		if err := m.addPairLocked(pair, false); err != nil {
			return fmt.Errorf("syncmanager: start pair %s: %w", pair.ID, err)
		}
	}

	// Start the aggregation goroutine.
	go m.aggregateLoop()

	return nil
}

// StartAllAsync starts engines for all enabled sync pairs using async startup.
// Unlike StartAll, this method calls engine.StartAsync() instead of
// engine.Start(), so the initial scan runs in a background goroutine and the
// method returns immediately. This is the preferred method for tray-first
// startup where the UI must appear before the initial scan completes.
func (m *SyncManager) StartAllAsync() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.cfg.SyncPairs {
		pair := &m.cfg.SyncPairs[i]
		if !pair.Enabled {
			if m.logger != nil {
				m.logger.Info("skipping disabled sync pair", map[string]interface{}{
					"pair_id": pair.ID,
				})
			}
			continue
		}
		if err := m.addPairLocked(pair, true); err != nil {
			return fmt.Errorf("syncmanager: start pair %s: %w", pair.ID, err)
		}
	}

	// Start the aggregation goroutine.
	go m.aggregateLoop()

	return nil
}

// StopAll stops all running engines and signals the aggregation goroutine.
func (m *SyncManager) StopAll() {
	// Close stopCh to broadcast shutdown to the aggregation goroutine and every
	// per-engine async-error forwarder (a single send would wake only one of
	// them, leaking the rest). Guarded so StopAll can be called more than once.
	m.stopOnce.Do(func() { close(m.stopCh) })

	m.mu.Lock()
	defer m.mu.Unlock()

	for id, engine := range m.engines {
		if err := engine.Stop(); err != nil && m.logger != nil {
			m.logger.Error("error stopping engine", map[string]interface{}{
				"pair_id": id,
				"error":   err.Error(),
			})
		}

		// Cancel per-engine forwarding goroutines.
		if cancel, ok := m.engineCancels[id]; ok {
			cancel()
			delete(m.engineCancels, id)
		}
	}

	m.engines = make(map[string]*Engine)
}

// AddPair creates and starts an engine for a new sync pair.
func (m *SyncManager) AddPair(pair *config.SyncPair) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Reject a pair that overlaps an already-running one (nested/identical local
	// folder or the same Drive folder) — that causes double-syncing and delete
	// races.
	if pair != nil {
		for _, eng := range m.engines {
			if eng.pair.ID == pair.ID {
				continue
			}
			if config.PairsOverlap(pair, eng.pair) {
				return fmt.Errorf("syncmanager: sync folder %q overlaps existing pair %q", pair.LocalDir, eng.pair.LocalDir)
			}
		}
	}
	// Start asynchronously: engine.Start() runs the full initial scan+enqueue
	// inline, which would hold m.mu (and, via the caller, the tray lock) for the
	// whole scan and freeze the UI. StartAsync returns immediately and scans in
	// the background, matching StartAllAsync.
	return m.addPairLocked(pair, true)
}

// addPairLocked is the internal implementation that must be called with m.mu held.
// The async parameter controls whether the engine is started synchronously
// (Start) or asynchronously (StartAsync).
func (m *SyncManager) addPairLocked(pair *config.SyncPair, async bool) error {
	if pair == nil {
		return fmt.Errorf("syncmanager: pair must not be nil")
	}

	if _, exists := m.engines[pair.ID]; exists {
		return fmt.Errorf("syncmanager: engine already exists for pair %s", pair.ID)
	}

	engine, err := NewEngine(pair, &m.cfg.App, m.driveClient, m.store, m.masterKey)
	if err != nil {
		return fmt.Errorf("syncmanager: create engine: %w", err)
	}

	// Wire up the engine's OnError channel to forward to the manager's
	// OnError channel so the tray/controller can monitor a single channel.
	go m.forwardAsyncErrors(pair.ID, engine)

	// Wire up the engine's OnStateChange callback to invoke the manager's
	// OnStateChange callback.
	if m.OnStateChange != nil {
		engine.OnStateChange = m.OnStateChange
	}

	if async {
		if err := engine.StartAsync(); err != nil {
			return fmt.Errorf("syncmanager: start engine async: %w", err)
		}
	} else {
		if err := engine.Start(); err != nil {
			return fmt.Errorf("syncmanager: start engine: %w", err)
		}
	}

	m.engines[pair.ID] = engine

	// Start per-engine error forwarding.
	ctx, cancel := context.WithCancel(context.Background())
	m.engineCancels[pair.ID] = cancel
	go m.forwardEngineErrors(ctx, pair.ID, engine)

	if m.logger != nil {
		m.logger.Info("started sync engine for pair", map[string]interface{}{
			"pair_id": pair.ID,
			"async":   async,
		})
	}

	return nil
}

// RemovePair stops and removes an engine by pair ID.
func (m *SyncManager) RemovePair(pairID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	engine, exists := m.engines[pairID]
	if !exists {
		return fmt.Errorf("syncmanager: no engine for pair %s", pairID)
	}

	if err := engine.Stop(); err != nil {
		return fmt.Errorf("syncmanager: stop engine %s: %w", pairID, err)
	}

	// Cancel per-engine forwarding goroutines.
	if cancel, ok := m.engineCancels[pairID]; ok {
		cancel()
		delete(m.engineCancels, pairID)
	}

	delete(m.engines, pairID)

	if m.logger != nil {
		m.logger.Info("removed sync engine for pair", map[string]interface{}{
			"pair_id": pairID,
		})
	}

	return nil
}

// PausePair pauses a specific engine.
func (m *SyncManager) PausePair(pairID string) error {
	m.mu.RLock()
	engine, exists := m.engines[pairID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("syncmanager: no engine for pair %s", pairID)
	}

	engine.Pause()
	return nil
}

// ResumePair resumes a specific engine.
func (m *SyncManager) ResumePair(pairID string) error {
	m.mu.RLock()
	engine, exists := m.engines[pairID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("syncmanager: no engine for pair %s", pairID)
	}

	engine.Resume()
	return nil
}

// SyncNow triggers immediate sync for a specific pair.
func (m *SyncManager) SyncNow(pairID string) error {
	m.mu.RLock()
	engine, exists := m.engines[pairID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("syncmanager: no engine for pair %s", pairID)
	}

	engine.SyncNow()
	return nil
}

// GetEngine returns an engine by pair ID (for direct access).
func (m *SyncManager) GetEngine(pairID string) (*Engine, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	engine, ok := m.engines[pairID]
	return engine, ok
}

// GetAggregatedState returns the combined state of all engines.
func (m *SyncManager) GetAggregatedState() AggregatedState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := AggregatedState{
		PairStatuses: make([]PairStatus, 0, len(m.engines)),
	}

	for id, engine := range m.engines {
		ps := PairStatus{
			ID:       id,
			State:    engine.State(),
			Stats:    engine.Stats(),
			Activity: engine.Activity(),
		}
		result.PairStatuses = append(result.PairStatuses, ps)
	}

	result.OverallState = computeOverallState(result.PairStatuses)
	return result
}

// RecentActivity returns up to limit most-recent-first completed events merged
// across all engines. A limit <= 0 returns all retained events.
func (m *SyncManager) RecentActivity(limit int) []ActivityEvent {
	m.mu.RLock()
	merged := make([]ActivityEvent, 0, len(m.engines)*8)
	for _, engine := range m.engines {
		merged = append(merged, engine.RecentEvents(0)...)
	}
	m.mu.RUnlock()

	// Most-recent-first across all pairs.
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Time.After(merged[j].Time)
	})
	if limit > 0 && limit < len(merged) {
		merged = merged[:limit]
	}
	return merged
}

// ListErrored returns the errored files across all managed pairs, for the GUI
// Issues view.
func (m *SyncManager) ListErrored() []ErroredFile {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var out []ErroredFile
	for _, engine := range m.engines {
		out = append(out, engine.ListErrored()...)
	}
	return out
}

// RetryFailed re-enqueues all errored files across every pair for another
// attempt and returns the total number of operations re-enqueued. The engines
// are snapshotted under the lock and retried without it held, since RetryFailed
// can block on queue backpressure.
func (m *SyncManager) RetryFailed() int {
	m.mu.RLock()
	engines := make([]*Engine, 0, len(m.engines))
	for _, engine := range m.engines {
		engines = append(engines, engine)
	}
	m.mu.RUnlock()

	n := 0
	for _, engine := range engines {
		n += engine.RetryFailed()
	}
	return n
}

// PendingConflicts returns all manually-queued conflicts across every pair.
func (m *SyncManager) PendingConflicts() []ConflictItem {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var out []ConflictItem
	for _, engine := range m.engines {
		out = append(out, engine.PendingConflicts()...)
	}
	return out
}

// ResolveConflict resolves a manual conflict on the correct engine.
func (m *SyncManager) ResolveConflict(pairID, localPath string, action config.ConflictPolicy) error {
	m.mu.RLock()
	engine, exists := m.engines[pairID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("syncmanager: no engine for pair %s", pairID)
	}
	return engine.ResolveConflictAction(localPath, action)
}

// OnlineOnlyCount returns the number of online-only placeholder files for a pair.
func (m *SyncManager) OnlineOnlyCount(pairID string) int {
	m.mu.RLock()
	engine, ok := m.engines[pairID]
	m.mu.RUnlock()
	if !ok {
		return 0
	}
	return engine.OnlineOnlyCount()
}

// MakeAvailableOffline downloads all online-only placeholders for a pair,
// returning the number queued.
func (m *SyncManager) MakeAvailableOffline(pairID string) int {
	m.mu.RLock()
	engine, ok := m.engines[pairID]
	m.mu.RUnlock()
	if !ok {
		return 0
	}
	return engine.MakeAvailableOffline()
}

// StateChanges returns a channel that emits aggregated state changes.
func (m *SyncManager) StateChanges() <-chan AggregatedState {
	return m.stateChangeCh
}

// Errors returns a channel that forwards errors from all engines.
func (m *SyncManager) Errors() <-chan error {
	return m.errorCh
}

// ListPairs returns info about all managed sync pairs.
func (m *SyncManager) ListPairs() []PairStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]PairStatus, 0, len(m.engines))
	for id, engine := range m.engines {
		ps := PairStatus{
			ID:       id,
			State:    engine.State(),
			Stats:    engine.Stats(),
			Activity: engine.Activity(),
		}
		result = append(result, ps)
	}
	return result
}

// ---------------------------------------------------------------------------
// Internal goroutines
// ---------------------------------------------------------------------------

// aggregateLoop periodically computes and emits the aggregated state.
func (m *SyncManager) aggregateLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			state := m.GetAggregatedState()
			select {
			case m.stateChangeCh <- state:
			default:
				// Channel full — drop to avoid blocking.
			}
		}
	}
}

// forwardEngineErrors reads errors from a single engine and forwards them
// to the manager's error channel. It exits when the provided context is
// cancelled or the engine's error channel is closed.
func (m *SyncManager) forwardEngineErrors(ctx context.Context, pairID string, engine *Engine) {
	errCh := engine.Errors()
	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-errCh:
			if !ok {
				return
			}
			wrapped := fmt.Errorf("pair %s: %w", pairID, err)
			select {
			case m.errorCh <- wrapped:
			default:
				// Channel full — drop to avoid blocking.
			}
		}
	}
}

// forwardAsyncErrors reads errors from a single engine's OnError channel and
// forwards them to the manager's OnError channel. This goroutine runs for the
// lifetime of the engine. Unlike forwardEngineErrors (which is cancelled via
// context when an engine is removed), this goroutine exits when the engine's
// OnError channel is closed or the manager's stopCh is signalled.
func (m *SyncManager) forwardAsyncErrors(pairID string, engine *Engine) {
	for {
		select {
		case <-m.stopCh:
			return
		case err, ok := <-engine.OnError:
			if !ok {
				return
			}
			wrapped := fmt.Errorf("pair %s: %w", pairID, err)
			select {
			case m.OnError <- wrapped:
			default:
				// Channel full — drop to avoid blocking.
			}
		}
	}
}

// ---------------------------------------------------------------------------
// State aggregation helpers
// ---------------------------------------------------------------------------

// computeOverallState determines the overall state from a set of pair statuses.
// Priority: Syncing > Error > Scanning > Disconnected > Paused > Idle
func computeOverallState(statuses []PairStatus) SyncState {
	if len(statuses) == 0 {
		return StateIdle
	}

	// Check for highest-priority states first.
	hasError := false
	hasScanning := false
	hasDisconnected := false
	allPaused := true

	for _, ps := range statuses {
		switch ps.State {
		case StateSyncing:
			return StateSyncing
		case StateError:
			hasError = true
		case StateScanning:
			hasScanning = true
		case StateDisconnected:
			hasDisconnected = true
		case StatePaused:
			// Still potentially all paused
		default:
			allPaused = false
		}
	}

	if hasError {
		return StateError
	}
	if hasScanning {
		return StateScanning
	}
	if hasDisconnected {
		return StateDisconnected
	}
	if allPaused && len(statuses) > 0 {
		return StatePaused
	}

	return StateIdle
}
