package service

import (
	"fmt"
	"os"
	"os/exec"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/arumes31/gcrypt/internal/config"
	syncpkg "github.com/arumes31/gcrypt/internal/sync"
)

// ---------------------------------------------------------------------------
// Settings tab
// ---------------------------------------------------------------------------

// option pairs a human label with its underlying value for a Select widget.
type intOption struct {
	label string
	value int
}

var (
	intervalOptions = []intOption{
		{"10 seconds", 10}, {"30 seconds", 30}, {"1 minute", 60},
		{"5 minutes", 300}, {"15 minutes", 900}, {"30 minutes", 1800},
	}
	maxSizeOptions = []intOption{
		{"Unlimited", 0}, {"50 MB", 50}, {"100 MB", 100},
		{"500 MB", 500}, {"1 GB", 1024}, {"2 GB", 2048},
	}
	bandwidthOptions = []intOption{
		{"Unlimited", 0}, {"128 KB/s", 128}, {"256 KB/s", 256},
		{"512 KB/s", 512}, {"1 MB/s", 1024}, {"5 MB/s", 5120},
	}
	logLevelOptions = []intOption{
		{"Debug", 0}, {"Info", 1}, {"Warn", 2}, {"Error", 3},
	}
	logLevelNames = map[int]string{0: "debug", 1: "info", 2: "warn", 3: "error"}

	directionLabels  = []string{"Two-way", "Upload only", "Download only", "Mirror (backup)"}
	directionByLabel = map[string]config.SyncDirection{
		"Two-way":         config.SyncDirTwoWay,
		"Upload only":     config.SyncDirUploadOnly,
		"Download only":   config.SyncDirDownloadOnly,
		"Mirror (backup)": config.SyncDirMirror,
	}
	labelByDirection = map[config.SyncDirection]string{
		config.SyncDirTwoWay:       "Two-way",
		config.SyncDirUploadOnly:   "Upload only",
		config.SyncDirDownloadOnly: "Download only",
		config.SyncDirMirror:       "Mirror (backup)",
	}

	conflictLabels  = []string{"Newest wins", "Keep mine", "Keep theirs", "Keep both", "Ask me"}
	conflictByLabel = map[string]config.ConflictPolicy{
		"Newest wins": config.ConflictPolicyAuto,
		"Keep mine":   config.ConflictPolicyKeepLocal,
		"Keep theirs": config.ConflictPolicyKeepRemote,
		"Keep both":   config.ConflictPolicyKeepBoth,
		"Ask me":      config.ConflictPolicyManual,
	}
	labelByConflict = map[config.ConflictPolicy]string{
		config.ConflictPolicyAuto:       "Newest wins",
		config.ConflictPolicyKeepLocal:  "Keep mine",
		config.ConflictPolicyKeepRemote: "Keep theirs",
		config.ConflictPolicyKeepBoth:   "Keep both",
		config.ConflictPolicyManual:     "Ask me",
	}
)

func optionLabels(opts []intOption) []string {
	out := make([]string, len(opts))
	for i, o := range opts {
		out[i] = o.label
	}
	return out
}

// labelForValue returns the label whose value is closest to v (exact match
// preferred), defaulting to the first option.
func labelForValue(opts []intOption, v int) string {
	for _, o := range opts {
		if o.value == v {
			return o.label
		}
	}
	return opts[0].label
}

func valueForLabel(opts []intOption, label string) int {
	for _, o := range opts {
		if o.label == label {
			return o.value
		}
	}
	return opts[0].value
}

// buildSettingsTab builds the Settings form. Changes apply immediately and are
// persisted, mirroring the legacy tray settings submenu.
func (f *FyneApp) buildSettingsTab() fyne.CanvasObject {
	cfg := f.ctrl.Config()
	if cfg == nil {
		return container.NewCenter(widget.NewLabel("No configuration loaded.\nRun setup first."))
	}

	autoStart := widget.NewCheck("Start gcrypt on Windows boot", func(on bool) {
		c := f.ctrl.Config()
		if c == nil {
			return
		}
		c.App.AutoStart = on
		if on {
			if err := EnableAutoStart(); err != nil {
				fmt.Fprintf(os.Stderr, "service: enable autostart: %v\n", err)
			}
		} else {
			if err := DisableAutoStart(); err != nil {
				fmt.Fprintf(os.Stderr, "service: disable autostart: %v\n", err)
			}
		}
		_ = config.Save(config.ConfigPath(), c)
	})
	autoStart.SetChecked(cfg.App.AutoStart)

	rememberPass := widget.NewCheck("Remember passphrase (DPAPI auto-unlock)", func(on bool) {
		if err := f.ctrl.SetRememberPassphrase(on); err != nil && f.logger != nil {
			f.logger.Warn("SetRememberPassphrase failed", map[string]interface{}{"error": err.Error()})
		}
	})
	rememberPass.SetChecked(cfg.App.RememberPassphrase)

	maxSize := widget.NewSelect(optionLabels(maxSizeOptions), func(label string) {
		mib := valueForLabel(maxSizeOptions, label)
		c := f.ctrl.Config()
		if c == nil {
			return
		}
		c.App.MaxFileSize = int64(mib) * 1024 * 1024
		_ = config.Save(config.ConfigPath(), c)
	})
	maxSize.SetSelected(labelForValue(maxSizeOptions, int(cfg.App.MaxFileSize/(1024*1024))))

	upLimit := widget.NewSelect(optionLabels(bandwidthOptions), func(label string) {
		kbps := valueForLabel(bandwidthOptions, label)
		c := f.ctrl.Config()
		down := 0
		if c != nil {
			down = c.App.RateLimitDownKBps
		}
		if err := f.ctrl.SetRateLimits(kbps, down); err != nil && f.logger != nil {
			f.logger.Warn("SetRateLimits failed", map[string]interface{}{"error": err.Error()})
		}
	})
	upLimit.SetSelected(labelForValue(bandwidthOptions, cfg.App.RateLimitUpKBps))

	downLimit := widget.NewSelect(optionLabels(bandwidthOptions), func(label string) {
		kbps := valueForLabel(bandwidthOptions, label)
		c := f.ctrl.Config()
		up := 0
		if c != nil {
			up = c.App.RateLimitUpKBps
		}
		if err := f.ctrl.SetRateLimits(up, kbps); err != nil && f.logger != nil {
			f.logger.Warn("SetRateLimits failed", map[string]interface{}{"error": err.Error()})
		}
	})
	downLimit.SetSelected(labelForValue(bandwidthOptions, cfg.App.RateLimitDownKBps))

	logLevel := widget.NewSelect(optionLabels(logLevelOptions), func(label string) {
		v := valueForLabel(logLevelOptions, label)
		name := logLevelNames[v]
		c := f.ctrl.Config()
		if c == nil {
			return
		}
		c.App.LogLevel = name
		if f.logger != nil {
			f.logger.SetLevel(name)
		}
		_ = config.Save(config.ConfigPath(), c)
	})
	logLevel.SetSelected(labelForValue(logLevelOptions, severityOf(cfg.App.LogLevel)))

	form := widget.NewForm(
		widget.NewFormItem("Max file size", maxSize),
		widget.NewFormItem("Upload limit", upLimit),
		widget.NewFormItem("Download limit", downLimit),
		widget.NewFormItem("Log level", logLevel),
	)

	// Account section: signed-in user + Drive storage quota. These labels are
	// populated asynchronously by refreshAccountInfo (via a background poll), so
	// keep references on the FyneApp and kick off an immediate fetch.
	f.accountLabel = widget.NewLabel("Not signed in")
	f.quotaLabel = widget.NewLabel("")
	f.quotaLabel.Importance = widget.LowImportance
	f.quotaBar = widget.NewProgressBar()
	f.quotaBar.Hide()
	accountSection := container.NewVBox(
		widget.NewLabelWithStyle("Account", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		f.accountLabel,
		f.quotaBar,
		f.quotaLabel,
		widget.NewSeparator(),
	)
	go f.refreshAccountInfo()

	// Backup & recovery: export the identity files needed (with the passphrase)
	// to restore or connect this sync on another machine.
	exportNote := widget.NewLabel("Save a copy of config.yaml + salt.bin. With your passphrase, these let you restore access on another PC. Keep them somewhere safe.")
	exportNote.Wrapping = fyne.TextWrapWord
	exportNote.Importance = widget.LowImportance
	exportBtn := widget.NewButtonWithIcon("Export backup…", theme.DocumentSaveIcon(), func() {
		go f.exportBackup()
	})

	// Scheduling & network: pause syncing during quiet hours and/or on metered
	// connections. Changes are read live by the engines on their next poll tick.
	saveSchedule := func(mutate func(s *config.ScheduleConfig)) {
		c := f.ctrl.Config()
		if c == nil {
			return
		}
		mutate(&c.App.Schedule)
		_ = config.Save(config.ConfigPath(), c)
	}
	quietStart := widget.NewEntry()
	quietStart.SetPlaceHolder("22:00")
	quietStart.SetText(cfg.App.Schedule.QuietHoursStart)
	quietStart.OnChanged = func(v string) { saveSchedule(func(s *config.ScheduleConfig) { s.QuietHoursStart = v }) }
	quietEnd := widget.NewEntry()
	quietEnd.SetPlaceHolder("07:00")
	quietEnd.SetText(cfg.App.Schedule.QuietHoursEnd)
	quietEnd.OnChanged = func(v string) { saveSchedule(func(s *config.ScheduleConfig) { s.QuietHoursEnd = v }) }
	quietEnabled := widget.NewCheck("Pause syncing during quiet hours", func(on bool) {
		saveSchedule(func(s *config.ScheduleConfig) { s.QuietHoursEnabled = on })
	})
	quietEnabled.SetChecked(cfg.App.Schedule.QuietHoursEnabled)
	meteredCheck := widget.NewCheck("Pause on metered connections", func(on bool) {
		saveSchedule(func(s *config.ScheduleConfig) { s.PauseOnMetered = on })
	})
	meteredCheck.SetChecked(cfg.App.Schedule.PauseOnMetered)
	quietRow := container.NewBorder(nil, nil, widget.NewLabel("Quiet hours"), nil,
		container.NewGridWithColumns(3, quietStart, widget.NewLabel("to"), quietEnd))

	body := container.NewVBox(
		accountSection,
		widget.NewLabelWithStyle("General", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		autoStart,
		rememberPass,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("Sync & bandwidth", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		form,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("Scheduling & network", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		quietEnabled,
		quietRow,
		meteredCheck,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("Backup & recovery", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		exportNote,
		exportBtn,
	)
	return container.NewVScroll(body)
}

// exportBackup prompts for a destination folder and writes the recovery files
// there. It blocks on the folder picker, so it must run on a background
// goroutine; UI updates are marshalled back onto the Fyne goroutine.
func (f *FyneApp) exportBackup() {
	dir, ok := pickFolder("Choose a folder to save the gcrypt backup into")
	if !ok || dir == "" {
		return
	}
	copied, err := ExportBackup(dir)
	fyne.Do(func() {
		if err != nil {
			dialog.ShowError(err, f.win)
			return
		}
		dialog.ShowInformation("Backup exported",
			fmt.Sprintf("Saved %d file(s) to:\n%s\n\nKeep these safe — together with your passphrase they grant access to your encrypted data.",
				len(copied), dir),
			f.win)
	})
}

// currentSyncInterval returns the sync interval of the first configured pair,
// or a sensible default.
func (f *FyneApp) currentSyncInterval() int {
	cfg := f.ctrl.Config()
	if cfg != nil {
		for i := range cfg.SyncPairs {
			if cfg.SyncPairs[i].SyncInterval > 0 {
				return cfg.SyncPairs[i].SyncInterval
			}
		}
	}
	return 60
}

// ---------------------------------------------------------------------------
// Folders (sync pairs) tab
// ---------------------------------------------------------------------------

// pairWidgets holds the live, in-place-updated widgets for a single Folders-tab
// card so per-folder state/stats track the sync engine without rebuilding the
// whole tab on every refresh tick.
type pairWidgets struct {
	localDir string
	sub      *widget.Label       // "<state> · <path>"
	metrics  *widget.Label       // ↑/↓ counts for this pair
	progress *widget.ProgressBar // this pair's in-flight progress
}

// refreshPairs rebuilds the Folders tab to reflect the current config/manager.
// Must run on the Fyne goroutine.
func (f *FyneApp) refreshPairs() {
	if f.pairsContainer == nil {
		return
	}
	f.pairsContainer.RemoveAll()
	f.pairWidgets = make(map[string]*pairWidgets)

	addBtn := widget.NewButtonWithIcon("Add Sync Folder…", theme.ContentAddIcon(), func() { f.runSetup() })
	addBtn.Importance = widget.HighImportance
	f.pairsContainer.Add(addBtn)

	cfg := f.ctrl.Config()
	if cfg == nil || len(cfg.SyncPairs) == 0 {
		f.pairsContainer.Add(widget.NewLabel("No sync folders configured."))
		f.pairsContainer.Refresh()
		return
	}

	for i := range cfg.SyncPairs {
		pair := &cfg.SyncPairs[i]
		f.pairsContainer.Add(f.buildPairCard(pair.ID))
	}
	f.pairsContainer.Refresh()
}

// buildPairCard builds a single sync-folder card with its controls.
func (f *FyneApp) buildPairCard(pairID string) fyne.CanvasObject {
	cfg := f.ctrl.Config()
	manager := f.ctrl.Manager()

	pair := cfg.GetSyncPair(pairID)
	if pair == nil {
		return widget.NewLabel("(removed)")
	}
	name := pairDisplayName(pair)
	stateLabel := stateLabelForPair(pairID, manager)
	paused := isPairPaused(pairID, manager)

	title := widget.NewLabelWithStyle(name, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	sub := widget.NewLabel(fmt.Sprintf("%s · %s", stateLabel, pair.LocalDir))
	sub.Importance = widget.LowImportance
	sub.Wrapping = fyne.TextWrapWord

	// Live per-folder stats + progress, updated in place by updatePairWidgets.
	metrics := widget.NewLabel("")
	metrics.Importance = widget.LowImportance
	pairProgress := widget.NewProgressBar()
	pairProgress.Hide()
	f.pairWidgets[pairID] = &pairWidgets{
		localDir: pair.LocalDir,
		sub:      sub,
		metrics:  metrics,
		progress: pairProgress,
	}

	// Per-folder sync interval (replaces the old global selector that silently
	// overwrote every pair). Applies and persists immediately for this pair.
	interval := widget.NewSelect(optionLabels(intervalOptions), func(label string) {
		secs := valueForLabel(intervalOptions, label)
		c := f.ctrl.Config()
		if c == nil {
			return
		}
		if p := c.GetSyncPair(pairID); p != nil {
			p.SyncInterval = secs
			_ = config.Save(config.ConfigPath(), c)
		}
	})
	intervalVal := pair.SyncInterval
	if intervalVal <= 0 {
		intervalVal = f.currentSyncInterval()
	}
	interval.SetSelected(labelForValue(intervalOptions, intervalVal))
	intervalRow := container.NewBorder(nil, nil, widget.NewLabel("Sync every"), nil, interval)

	// Direction (two-way / upload-only / download-only / mirror).
	directionSel := widget.NewSelect(directionLabels, func(label string) {
		c := f.ctrl.Config()
		if c == nil {
			return
		}
		if p := c.GetSyncPair(pairID); p != nil {
			p.SyncDirection = directionByLabel[label]
			p.ForwardOnly = false // superseded by SyncDirection
			_ = config.Save(config.ConfigPath(), c)
		}
	})
	directionSel.SetSelected(labelByDirection[pair.EffectiveDirection()])
	directionRow := container.NewBorder(nil, nil, widget.NewLabel("Direction"), nil, directionSel)

	// Conflict resolution policy.
	conflictSel := widget.NewSelect(conflictLabels, func(label string) {
		c := f.ctrl.Config()
		if c == nil {
			return
		}
		if p := c.GetSyncPair(pairID); p != nil {
			p.ConflictPolicy = conflictByLabel[label]
			_ = config.Save(config.ConfigPath(), c)
		}
	})
	conflictSel.SetSelected(labelByConflict[pair.EffectiveConflictPolicy()])
	conflictRow := container.NewBorder(nil, nil, widget.NewLabel("On conflict"), nil, conflictSel)

	// On-demand (online-only) toggle.
	onlineOnly := widget.NewCheck("On-demand: don't auto-download new files", func(on bool) {
		c := f.ctrl.Config()
		if c == nil {
			return
		}
		if p := c.GetSyncPair(pairID); p != nil {
			p.OnlineOnly = on
			_ = config.Save(config.ConfigPath(), c)
		}
	})
	onlineOnly.SetChecked(pair.OnlineOnly)

	// "Download online-only files" button, shown only when there are some.
	onDemandRow := container.NewVBox(onlineOnly)
	if manager != nil {
		if n := manager.OnlineOnlyCount(pairID); n > 0 {
			availBtn := widget.NewButtonWithIcon(
				fmt.Sprintf("Download %d online-only file(s)", n), theme.DownloadIcon(), func() {
					go func() {
						if mgr := f.ctrl.Manager(); mgr != nil {
							mgr.MakeAvailableOffline(pairID)
						}
						fyne.Do(func() { f.refreshPairs() })
					}()
				})
			onDemandRow.Add(availBtn)
		}
	}

	pauseLabel := "Pause"
	pauseIcon := theme.MediaPauseIcon()
	if paused {
		pauseLabel = "Resume"
		pauseIcon = theme.MediaPlayIcon()
	}
	pauseBtn := widget.NewButtonWithIcon(pauseLabel, pauseIcon, func() {
		if manager == nil {
			return
		}
		if isPairPaused(pairID, manager) {
			_ = manager.ResumePair(pairID)
		} else {
			_ = manager.PausePair(pairID)
		}
		fyne.Do(func() { f.refreshPairs() })
	})
	syncBtn := widget.NewButtonWithIcon("Sync", theme.ViewRefreshIcon(), func() {
		if manager != nil {
			_ = manager.SyncNow(pairID)
		}
	})
	openBtn := widget.NewButtonWithIcon("Open", theme.FolderOpenIcon(), func() {
		if p := cfg.GetSyncPair(pairID); p != nil {
			if err := exec.Command("explorer", p.LocalDir).Start(); err != nil {
				fmt.Fprintf(os.Stderr, "service: open pair folder: %v\n", err)
			}
		}
	})
	removeBtn := widget.NewButtonWithIcon("Remove", theme.DeleteIcon(), func() {
		dialog.ShowConfirm("Remove sync folder",
			fmt.Sprintf("Stop syncing %q?\n\nThis only removes it from gcrypt; your local and cloud files are kept.", name),
			func(ok bool) {
				if !ok {
					return
				}
				f.removePair(pairID)
			}, f.win)
	})
	removeBtn.Importance = widget.DangerImportance

	controls := container.NewGridWithColumns(4, pauseBtn, syncBtn, openBtn, removeBtn)
	card := container.NewVBox(title, sub, metrics, pairProgress,
		intervalRow, directionRow, conflictRow, onDemandRow, controls, widget.NewSeparator())

	// Paint the live widgets once now so the card isn't blank until the next tick.
	if manager != nil {
		for _, ps := range manager.ListPairs() {
			if ps.ID == pairID {
				f.applyPairWidgets(pairID, ps)
				break
			}
		}
	}
	return card
}

// updatePairWidgets refreshes the live per-folder widgets from the aggregated
// state. Must run on the Fyne goroutine. It is a no-op when the Folders tab has
// not been built (no widgets registered).
func (f *FyneApp) updatePairWidgets(agg syncpkg.AggregatedState) {
	if len(f.pairWidgets) == 0 {
		return
	}
	for _, ps := range agg.PairStatuses {
		f.applyPairWidgets(ps.ID, ps)
	}
}

// applyPairWidgets writes a single pair's state/stats into its card widgets.
func (f *FyneApp) applyPairWidgets(pairID string, ps syncpkg.PairStatus) {
	pw := f.pairWidgets[pairID]
	if pw == nil {
		return
	}

	pw.sub.SetText(fmt.Sprintf("%s · %s", stateLabelFromSyncState(ps.State), pw.localDir))

	up, down := ps.Stats.FilesUploaded, ps.Stats.FilesDownloaded
	pw.metrics.SetText(fmt.Sprintf("↑ %d (%s)   ·   ↓ %d (%s)",
		up, humanBytes(ps.Stats.BytesUploaded), down, humanBytes(ps.Stats.BytesDownloaded)))

	done := up + down
	total := done + int64(ps.Activity.Active) + int64(ps.Activity.Pending)
	if total > 0 && (ps.Activity.Active > 0 || ps.Activity.Pending > 0) {
		pw.progress.SetValue(float64(done) / float64(total))
		pw.progress.Show()
	} else {
		pw.progress.Hide()
	}
}

func (f *FyneApp) removePair(pairID string) {
	if manager := f.ctrl.Manager(); manager != nil {
		_ = manager.RemovePair(pairID)
	}
	if cfg := f.ctrl.Config(); cfg != nil {
		cfg.RemoveSyncPair(pairID)
		_ = config.Save(config.ConfigPath(), cfg)
	}
	fyne.Do(func() {
		f.refreshPairs()
		f.refresh()
	})
}
