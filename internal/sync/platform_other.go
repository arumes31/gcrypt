//go:build !windows

package sync

// longPath is a no-op off Windows: other platforms have no MAX_PATH limit.
func longPath(p string) string { return p }

// availableDiskBytes reports "effectively unlimited" off Windows, so the
// pre-download space check never blocks on platforms where it isn't wired up.
func availableDiskBytes(string) (uint64, error) { return ^uint64(0), nil }
