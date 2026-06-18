package service

import (
	"fmt"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/arumes31/gcrypt/internal/config"
	syncpkg "github.com/arumes31/gcrypt/internal/sync"
)

// ---------------------------------------------------------------------------
// Issues tab — failed transfers and resolved conflicts
// ---------------------------------------------------------------------------

// conflictHistoryLimit bounds how many recent conflicts the Issues tab lists.
const conflictHistoryLimit = 20

// buildIssuesTab assembles the Issues tab. Its body is rebuilt on demand by
// refreshIssues (the contents are dynamic), so here we only create the scroll
// container the body lives in.
func (f *FyneApp) buildIssuesTab() fyne.CanvasObject {
	f.issuesContainer = container.NewVBox()
	return container.NewVScroll(f.issuesContainer)
}

// refreshIssues rebuilds the Issues tab from the current errored-file list and
// recent conflict events. Must run on the Fyne goroutine.
func (f *FyneApp) refreshIssues() {
	if f.issuesContainer == nil {
		return
	}
	f.issuesContainer.RemoveAll()

	manager := f.ctrl.Manager()
	var errored []syncpkg.ErroredFile
	var pending []syncpkg.ConflictItem
	var conflicts []syncpkg.ActivityEvent
	if manager != nil {
		errored = manager.ListErrored()
		pending = manager.PendingConflicts()
		for _, ev := range manager.RecentActivity(0) {
			if ev.Kind == syncpkg.ActivityConflict {
				conflicts = append(conflicts, ev)
				if len(conflicts) >= conflictHistoryLimit {
					break
				}
			}
		}
	}

	if len(errored) == 0 && len(pending) == 0 && len(conflicts) == 0 {
		f.issuesContainer.Add(container.NewCenter(
			widget.NewLabel("No issues — everything synced cleanly."),
		))
		f.issuesContainer.Refresh()
		return
	}

	// --- Conflicts awaiting your decision (manual policy). ---
	if len(pending) > 0 {
		f.issuesContainer.Add(widget.NewLabelWithStyle(
			fmt.Sprintf("Conflicts needing a decision (%d)", len(pending)),
			fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
		for _, c := range pending {
			f.issuesContainer.Add(f.buildConflictCard(c))
		}
		f.issuesContainer.Add(widget.NewSeparator())
	}

	// --- Failed transfers section. ---
	f.issuesContainer.Add(widget.NewLabelWithStyle(
		fmt.Sprintf("Failed transfers (%d)", len(errored)),
		fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))

	if len(errored) == 0 {
		hint := widget.NewLabel("None.")
		hint.Importance = widget.LowImportance
		f.issuesContainer.Add(hint)
	} else {
		retry := widget.NewButtonWithIcon("Retry all failed", theme.ViewRefreshIcon(), func() {
			go func() {
				if mgr := f.ctrl.Manager(); mgr != nil {
					if n := mgr.RetryFailed(); n > 0 && f.logger != nil {
						f.logger.Info("retrying failed transfers", map[string]interface{}{"count": n})
					}
				}
				fyne.Do(func() { f.refreshIssues() })
			}()
		})
		retry.Importance = widget.HighImportance
		f.issuesContainer.Add(retry)

		for _, ef := range errored {
			row := widget.NewLabel(fmt.Sprintf("⚠  %s  ·  %s", ef.LocalPath, f.pairName(ef.PairID)))
			row.Wrapping = fyne.TextWrapWord
			f.issuesContainer.Add(row)
		}
	}

	f.issuesContainer.Add(widget.NewSeparator())

	// --- Recent conflicts section. ---
	f.issuesContainer.Add(widget.NewLabelWithStyle(
		fmt.Sprintf("Recent conflicts (%d)", len(conflicts)),
		fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))

	if len(conflicts) == 0 {
		hint := widget.NewLabel("None.")
		hint.Importance = widget.LowImportance
		f.issuesContainer.Add(hint)
	} else {
		note := widget.NewLabel("When both sides changed, the local copy is kept as a \".conflict\" file next to the original.")
		note.Wrapping = fyne.TextWrapWord
		note.Importance = widget.LowImportance
		f.issuesContainer.Add(note)

		for _, ev := range conflicts {
			name := ev.Name
			if name == "" {
				name = "(unknown)"
			}
			row := widget.NewLabel(fmt.Sprintf("⚠  %s  ·  %s", name, humanizeSince(ev.Time)))
			row.Wrapping = fyne.TextWrapWord
			f.issuesContainer.Add(row)
		}
	}

	f.issuesContainer.Refresh()
}

// buildConflictCard renders one pending conflict with Keep-local / Keep-remote /
// Keep-both resolution buttons. Choosing an action resolves it on the engine and
// refreshes the view.
func (f *FyneApp) buildConflictCard(c syncpkg.ConflictItem) fyne.CanvasObject {
	title := widget.NewLabelWithStyle("⚠  "+c.LocalPath, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	title.Wrapping = fyne.TextWrapWord

	sub := widget.NewLabel(fmt.Sprintf("%s   ·   yours: %s   ·   theirs: %s",
		f.pairName(c.PairID), humanizeSince(c.LocalModTime), humanizeSince(c.RemoteModTime)))
	sub.Importance = widget.LowImportance
	sub.Wrapping = fyne.TextWrapWord

	resolve := func(action config.ConflictPolicy) {
		go func() {
			if mgr := f.ctrl.Manager(); mgr != nil {
				if err := mgr.ResolveConflict(c.PairID, c.LocalPath, action); err != nil && f.logger != nil {
					f.logger.Warn("resolve conflict failed", map[string]interface{}{"error": err.Error()})
				}
			}
			fyne.Do(func() { f.refreshIssues() })
		}()
	}

	keepLocal := widget.NewButton("Keep mine", func() { resolve(config.ConflictPolicyKeepLocal) })
	keepRemote := widget.NewButton("Keep theirs", func() { resolve(config.ConflictPolicyKeepRemote) })
	keepBoth := widget.NewButton("Keep both", func() { resolve(config.ConflictPolicyKeepBoth) })
	buttons := container.NewGridWithColumns(3, keepLocal, keepRemote, keepBoth)

	return container.NewVBox(title, sub, buttons)
}

// pairName resolves a sync pair's display name from its ID, falling back to a
// short ID when the pair is no longer configured.
func (f *FyneApp) pairName(pairID string) string {
	if cfg := f.ctrl.Config(); cfg != nil {
		if p := cfg.GetSyncPair(pairID); p != nil {
			return pairDisplayName(p)
		}
	}
	if len(pairID) > 8 {
		return pairID[:8]
	}
	return pairID
}
