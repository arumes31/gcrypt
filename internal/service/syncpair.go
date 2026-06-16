package service

import (
	"path/filepath"

	"github.com/arumes31/gcrypt/internal/config"
	syncpkg "github.com/arumes31/gcrypt/internal/sync"
)

// Shared sync-pair presentation helpers used by the GUI.

// pairDisplayName returns a human-readable name for a sync pair, using the
// directory base name when no explicit name is set.
func pairDisplayName(pair *config.SyncPair) string {
	if pair.LocalDir != "" {
		return filepath.Base(pair.LocalDir)
	}
	return pair.ID[:8]
}

// stateLabelForPair returns the human-readable status label for a sync pair as
// reported by the manager.
func stateLabelForPair(pairID string, manager *syncpkg.SyncManager) string {
	if manager == nil {
		return "Unknown"
	}
	for _, ps := range manager.ListPairs() {
		if ps.ID == pairID {
			return stateLabelFromSyncState(ps.State)
		}
	}
	return "Unknown"
}

// isPairPaused reports whether the pair is currently paused.
func isPairPaused(pairID string, manager *syncpkg.SyncManager) bool {
	if manager == nil {
		return false
	}
	for _, ps := range manager.ListPairs() {
		if ps.ID == pairID {
			return ps.State == syncpkg.StatePaused
		}
	}
	return false
}

// stateLabelFromSyncState maps a SyncState to a human-readable label.
func stateLabelFromSyncState(state syncpkg.SyncState) string {
	switch state {
	case syncpkg.StateIdle:
		return "Idle"
	case syncpkg.StateScanning:
		return "Scanning"
	case syncpkg.StateSyncing:
		return "Syncing"
	case syncpkg.StatePaused:
		return "Paused"
	case syncpkg.StateError:
		return "Error"
	case syncpkg.StateDisconnected:
		return "Disconnected"
	default:
		return string(state)
	}
}
