package sync

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// IsQuietHours reports whether the current time falls within the configured
// quiet window. The window is defined by start and end strings in "HH:MM"
// 24-hour format. Handles midnight-crossing ranges (e.g. "23:00"–"06:00").
// Returns false if either string is empty or unparseable.
func IsQuietHours(start, end string) bool {
	if start == "" || end == "" {
		return false
	}
	sh, sm, ok1 := parseHHMM(start)
	eh, em, ok2 := parseHHMM(end)
	if !ok1 || !ok2 {
		return false
	}

	now := time.Now()
	nowMin := now.Hour()*60 + now.Minute()
	startMin := sh*60 + sm
	endMin := eh*60 + em

	if startMin <= endMin {
		// Same-day window, e.g. 01:00–06:00.
		return nowMin >= startMin && nowMin < endMin
	}
	// Midnight-crossing window, e.g. 23:00–06:00.
	return nowMin >= startMin || nowMin < endMin
}

// parseHHMM parses "HH:MM" into (hour, minute, ok).
func parseHHMM(s string) (int, int, bool) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || h < 0 || h > 23 {
		return 0, 0, false
	}
	m, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// ValidateScheduleTime validates a "HH:MM" string, returning a descriptive
// error if it is malformed.
func ValidateScheduleTime(s string) error {
	if s == "" {
		return nil
	}
	_, _, ok := parseHHMM(s)
	if !ok {
		return fmt.Errorf("invalid time %q: expected HH:MM (24-hour)", s)
	}
	return nil
}
