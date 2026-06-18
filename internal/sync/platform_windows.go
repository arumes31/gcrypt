package sync

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// longPath returns a form of p that is safe to pass to Windows file APIs even
// when it exceeds the legacy MAX_PATH (260-char) limit. Paths at/over 248 chars
// are rewritten to the extended-length "\\?\" form (or "\\?\UNC\" for UNC
// paths), which lifts the limit. Shorter paths are returned unchanged so the
// common case is a no-op and ordinary path math elsewhere is unaffected.
func longPath(p string) string {
	if p == "" || strings.HasPrefix(p, `\\?\`) {
		return p
	}
	// Resolve to an absolute path first: a short relative path can still expand
	// to a long absolute path under a deep working directory, and only the
	// resolved length determines whether MAX_PATH is exceeded.
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if len(abs) < 248 {
		return p
	}
	if strings.HasPrefix(abs, `\\`) {
		// UNC: \\server\share\... -> \\?\UNC\server\share\...
		return `\\?\UNC` + abs[1:]
	}
	return `\\?\` + abs
}

// availableDiskBytes returns the number of bytes available to the caller on the
// volume that contains path (which should be an existing directory).
func availableDiskBytes(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(longPath(path))
	if err != nil {
		return 0, err
	}
	var freeAvail, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeAvail, &total, &totalFree); err != nil {
		return 0, err
	}
	return freeAvail, nil
}
