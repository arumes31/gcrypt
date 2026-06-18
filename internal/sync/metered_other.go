//go:build !windows

package sync

// IsMeteredNetwork is a no-op on non-Windows platforms. Returns false so sync
// always proceeds without metered-network gating.
func IsMeteredNetwork() bool { return false }
