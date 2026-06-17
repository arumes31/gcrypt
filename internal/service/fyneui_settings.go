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

	interval := widget.NewSelect(optionLabels(intervalOptions), func(label string) {
		secs := valueForLabel(intervalOptions, label)
		c := f.ctrl.Config()
		if c == nil {
			return
		}
		for i := range c.SyncPairs {
			c.SyncPairs[i].SyncInterval = secs
		}
		_ = config.Save(config.ConfigPath(), c)
	})
	interval.SetSelected(labelForValue(intervalOptions, f.currentSyncInterval()))

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
		widget.NewFormItem("Sync interval", interval),
		widget.NewFormItem("Max file size", maxSize),
		widget.NewFormItem("Upload limit", upLimit),
		widget.NewFormItem("Download limit", downLimit),
		widget.NewFormItem("Log level", logLevel),
	)

	body := container.NewVBox(
		widget.NewLabelWithStyle("General", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		autoStart,
		rememberPass,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("Sync & bandwidth", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		form,
	)
	return container.NewVScroll(body)
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

// refreshPairs rebuilds the Folders tab to reflect the current config/manager.
// Must run on the Fyne goroutine.
func (f *FyneApp) refreshPairs() {
	if f.pairsContainer == nil {
		return
	}
	f.pairsContainer.RemoveAll()

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
	card := container.NewVBox(title, sub, controls, widget.NewSeparator())
	return card
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
