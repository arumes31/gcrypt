package service

import (
	"fmt"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	syncpkg "github.com/arumes31/gcrypt/internal/sync"
)

// activityFeedLimit bounds how many events the panel shows.
const activityFeedLimit = 50

// buildActivityList creates the scrolling activity feed widget. Each row shows
// a kind glyph, the file name, a "verb · relative-time" subtitle, and is bound
// to f.events (most-recent-first).
func (f *FyneApp) buildActivityList() *widget.List {
	return widget.NewList(
		func() int {
			f.mu.Lock()
			defer f.mu.Unlock()
			return len(f.events)
		},
		func() fyne.CanvasObject {
			glyph := widget.NewLabel("•")
			title := widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
			sub := widget.NewLabel("")
			sub.Importance = widget.LowImportance
			return container.NewBorder(nil, nil, glyph, nil,
				container.NewVBox(title, sub),
			)
		},
		func(id widget.ListItemID, o fyne.CanvasObject) {
			f.mu.Lock()
			if id < 0 || id >= len(f.events) {
				f.mu.Unlock()
				return
			}
			ev := f.events[id]
			f.mu.Unlock()

			border := o.(*fyne.Container)
			glyph := border.Objects[1].(*widget.Label)
			vbox := border.Objects[0].(*fyne.Container)
			title := vbox.Objects[0].(*widget.Label)
			sub := vbox.Objects[1].(*widget.Label)

			glyph.SetText(activityGlyph(ev.Kind))
			name := ev.Name
			if name == "" {
				name = "(unknown)"
			}
			title.SetText(name)
			sub.SetText(fmt.Sprintf("%s · %s", activityVerb(ev.Kind), humanizeSince(ev.Time)))
		},
	)
}

// refreshActivity pulls the latest events from the manager into f.events and
// repaints the list. Must run on the Fyne goroutine.
func (f *FyneApp) refreshActivity() {
	var events []syncpkg.ActivityEvent
	if manager := f.ctrl.Manager(); manager != nil {
		events = manager.RecentActivity(activityFeedLimit)
	}

	// Apply the Activity-tab search filter (matches file name or relative path).
	if f.activityFilter != "" {
		filtered := make([]syncpkg.ActivityEvent, 0, len(events))
		for _, ev := range events {
			if strings.Contains(strings.ToLower(ev.Name), f.activityFilter) ||
				strings.Contains(strings.ToLower(ev.Path), f.activityFilter) {
				filtered = append(filtered, ev)
			}
		}
		events = filtered
	}

	f.mu.Lock()
	f.events = events
	n := len(events)
	f.mu.Unlock()

	if n == 0 {
		if f.activityFilter != "" {
			f.emptyHint.SetText("No activity matches your search.")
		} else {
			f.emptyHint.SetText("Nothing yet — changes will show up here as they sync.")
		}
		f.emptyHint.Show()
	} else {
		f.emptyHint.Hide()
	}

	// Only repaint when the feed actually changed (count or newest timestamp).
	// Refreshing unconditionally every 2s fights the user's scroll position and
	// wastes redraws.
	var top time.Time
	if n > 0 {
		top = events[0].Time
	}
	if n == f.lastActivityCount && top.Equal(f.lastActivityTop) {
		return
	}
	f.lastActivityCount = n
	f.lastActivityTop = top
	f.activityList.Refresh()
}

// activityGlyph returns a short symbol for an activity kind.
func activityGlyph(k syncpkg.ActivityKind) string {
	switch k {
	case syncpkg.ActivityUpload:
		return "↑"
	case syncpkg.ActivityDownload:
		return "↓"
	case syncpkg.ActivityDelete:
		return "🗑"
	case syncpkg.ActivityConflict:
		return "⚠"
	default:
		return "•"
	}
}

// activityVerb returns a Nextcloud-style human verb for an activity kind.
func activityVerb(k syncpkg.ActivityKind) string {
	switch k {
	case syncpkg.ActivityUpload:
		return "You changed"
	case syncpkg.ActivityDownload:
		return "Downloaded"
	case syncpkg.ActivityDelete:
		return "Deleted"
	case syncpkg.ActivityConflict:
		return "Conflict resolved"
	default:
		return "Synced"
	}
}

// humanizeSince renders a compact relative time like "now", "5min", "2h", "3d".
func humanizeSince(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 45*time.Second:
		return "now"
	case d < 90*time.Minute:
		m := int(d.Minutes())
		if m < 1 {
			m = 1
		}
		return fmt.Sprintf("%dmin", m)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
