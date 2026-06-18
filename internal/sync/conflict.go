package sync

import (
	"sync"
	"time"
)

// ConflictItem represents an unresolved conflict queued for manual resolution.
type ConflictItem struct {
	PairID        string
	LocalPath     string
	RemoteID      string
	LocalModTime  time.Time
	RemoteModTime time.Time
	LocalHash     string
	RemoteHash    string
}

// conflictQueue is a thread-safe list of unresolved conflicts.
type conflictQueue struct {
	mu    sync.Mutex
	items []ConflictItem
}

// Add appends a conflict to the queue, replacing any existing entry for the
// same (PairID, LocalPath) pair.
func (q *conflictQueue) Add(item ConflictItem) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, existing := range q.items {
		if existing.PairID == item.PairID && existing.LocalPath == item.LocalPath {
			q.items[i] = item
			return
		}
	}
	q.items = append(q.items, item)
}

// Remove removes the conflict for the given pair + local path and returns
// true if found.
func (q *conflictQueue) Remove(pairID, localPath string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, item := range q.items {
		if item.PairID == pairID && item.LocalPath == localPath {
			q.items = append(q.items[:i], q.items[i+1:]...)
			return true
		}
	}
	return false
}

// List returns a snapshot of all pending conflicts.
func (q *conflictQueue) List() []ConflictItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]ConflictItem, len(q.items))
	copy(out, q.items)
	return out
}

// Len returns the number of pending conflicts.
func (q *conflictQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}
