package service

import (
	_ "embed"
	"fmt"
	"image/color"
	"os"
	"os/exec"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/arumes31/gcrypt/internal/appstate"
	syncpkg "github.com/arumes31/gcrypt/internal/sync"
)

// ---------------------------------------------------------------------------
// Embedded tray / window icons (PNG so Fyne can render them on any platform)
// ---------------------------------------------------------------------------

//go:embed assets/logo.png
var pngLogo []byte

//go:embed assets/warning.png
var pngWarning []byte

//go:embed assets/syncing.png
var pngSyncing []byte

//go:embed assets/synced.png
var pngSynced []byte

//go:embed assets/error.png
var pngError []byte

var (
	// resLogo is the brand logo / favicon used for the app and window icons.
	resLogo = fyne.NewStaticResource("logo.png", pngLogo)
	// State icons used for the tray (and as the window icon while active).
	resWarning = fyne.NewStaticResource("warning.png", pngWarning)
	resSyncing = fyne.NewStaticResource("syncing.png", pngSyncing)
	resSynced  = fyne.NewStaticResource("synced.png", pngSynced)
	resError   = fyne.NewStaticResource("error.png", pngError)
)

// Status dot colours.
var (
	colGreen = color.NRGBA{R: 0x2e, G: 0xcc, B: 0x71, A: 0xff}
	colBlue  = color.NRGBA{R: 0x34, G: 0x98, B: 0xdb, A: 0xff}
	colRed   = color.NRGBA{R: 0xe7, G: 0x4c, B: 0x3c, A: 0xff}
	colGray  = color.NRGBA{R: 0x95, G: 0xa5, B: 0xa6, A: 0xff}
	colAmber = color.NRGBA{R: 0xf3, G: 0x9c, B: 0x12, A: 0xff}
)

// ---------------------------------------------------------------------------
// FyneApp
// ---------------------------------------------------------------------------

// FyneApp is the Fyne-based GUI for gcrypt. It owns the system tray icon/menu
// and a Nextcloud-style flyout window (status header + activity feed + folder
// and settings tabs). It reads application state from the AppController and the
// SyncManager, mirroring what the legacy systray TrayApp exposed.
type FyneApp struct {
	ctrl   *AppController
	logger *Logger

	app fyne.App
	win fyne.Window

	mu       sync.Mutex
	events   []syncpkg.ActivityEvent
	isPaused bool

	// Header widgets.
	statusDot   *canvas.Circle
	statusLabel *widget.Label
	summary     *widget.Label // sync folder path
	metrics     *widget.Label // cumulative counts + transfer rate
	liveLabel   *widget.Label // current in-flight file(s) + pending

	// Live transfer-rate tracking (byte deltas between refreshes).
	lastBytesUp   int64
	lastBytesDown int64
	lastRateTime  time.Time
	rateUp        float64 // bytes/sec, smoothed
	rateDown      float64

	// lastIconState avoids re-decoding/re-setting the tray icon every refresh;
	// the icon is only swapped when the high-level state actually changes.
	lastIconState    appstate.State
	lastIconStateSet bool

	// State-driven call-to-action (Run Setup / Unlock / Sign in).
	cta *widget.Button

	// Activity feed.
	activityList *widget.List
	emptyHint    *widget.Label

	// Folders tab content (rebuilt on change).
	pairsContainer *fyne.Container

	// Footer.
	pauseBtn *widget.Button
}

// NewFyneApp constructs the Fyne GUI bound to the given controller.
func NewFyneApp(ctrl *AppController, logger *Logger) *FyneApp {
	return &FyneApp{ctrl: ctrl, logger: logger}
}

// Run builds the UI and blocks on the Fyne event loop until the app quits.
// It must be called on the main goroutine.
func (f *FyneApp) Run() {
	f.app = app.NewWithID("com.arumes31.gcrypt")
	f.app.SetIcon(resLogo)

	f.win = f.app.NewWindow("gcrypt")
	f.win.SetIcon(resLogo)
	f.win.Resize(fyne.NewSize(400, 560))
	f.win.SetContent(f.buildContent())
	// Closing the window hides it; the app keeps living in the tray.
	f.win.SetCloseIntercept(func() { f.win.Hide() })

	f.installTray()

	// React to controller lifecycle transitions.
	f.ctrl.OnStateChange = func(_, _ appstate.State) {
		fyne.Do(func() { f.refresh() })
	}

	// Periodic refresh of stats / activity / per-pair state.
	go f.refreshLoop()

	// Initial paint.
	f.refresh()

	// Show the window on first run so the user can act on setup/unlock/sign-in;
	// once configured and idle we still show it (acts as the main panel).
	f.win.Show()

	// Attempt a silent auto-unlock if the user opted to remember the passphrase.
	go func() {
		if f.ctrl.TryAutoUnlock() && f.logger != nil {
			f.logger.Info("Auto-unlock attempted at startup")
		}
	}()

	f.app.Run()
	f.ctrl.Shutdown()
}

// ---------------------------------------------------------------------------
// Window content
// ---------------------------------------------------------------------------

// buildContent assembles the full window: header, CTA, and the tabbed body.
func (f *FyneApp) buildContent() fyne.CanvasObject {
	// --- Header: status dot + label, plus an Open-folder shortcut. ---
	f.statusDot = canvas.NewCircle(colGray)
	// GridWrap pins the circle to a fixed cell size (Container has no SetMinSize).
	dotWrap := container.NewGridWrap(fyne.NewSize(16, 16), f.statusDot)

	f.statusLabel = widget.NewLabelWithStyle("Starting…", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	f.summary = widget.NewLabel("")
	f.summary.Wrapping = fyne.TextWrapWord
	f.metrics = widget.NewLabel("")
	f.liveLabel = widget.NewLabel("")
	f.liveLabel.Wrapping = fyne.TextWrapWord
	f.liveLabel.Importance = widget.LowImportance

	// Small brand logo on the left of the header.
	logoImg := canvas.NewImageFromResource(resLogo)
	logoImg.FillMode = canvas.ImageFillContain
	logoWrap := container.NewGridWrap(fyne.NewSize(40, 40), logoImg)

	textCol := container.NewVBox(
		widget.NewLabelWithStyle("gcrypt", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		container.NewHBox(dotWrap, f.statusLabel),
		f.summary,
		f.metrics,
		f.liveLabel,
	)
	header := container.NewBorder(nil, nil, logoWrap, nil, textCol)

	// --- State-driven CTA button (hidden unless an action is needed). ---
	f.cta = widget.NewButton("", nil)
	f.cta.Importance = widget.HighImportance
	f.cta.Hide()

	// --- Tabs: Activity / Folders / Settings. ---
	f.activityList = f.buildActivityList()
	f.emptyHint = widget.NewLabel("No recent activity yet.")
	f.emptyHint.Alignment = fyne.TextAlignCenter
	activityTab := container.NewStack(f.activityList, container.NewCenter(f.emptyHint))

	f.pairsContainer = container.NewVBox()
	foldersTab := container.NewVScroll(f.pairsContainer)

	tabs := container.NewAppTabs(
		container.NewTabItemWithIcon("Activity", theme.HistoryIcon(), activityTab),
		container.NewTabItemWithIcon("Folders", theme.FolderIcon(), foldersTab),
		container.NewTabItemWithIcon("Settings", theme.SettingsIcon(), f.buildSettingsTab()),
	)
	tabs.OnSelected = func(ti *container.TabItem) {
		if ti.Text == "Folders" {
			f.refreshPairs()
		}
	}

	// --- Footer toolbar. ---
	f.pauseBtn = widget.NewButtonWithIcon("Pause All", theme.MediaPauseIcon(), func() { f.togglePauseAll() })
	footer := container.NewGridWithColumns(3,
		widget.NewButtonWithIcon("Open", theme.FolderOpenIcon(), func() { f.openSyncFolder() }),
		widget.NewButtonWithIcon("Sync Now", theme.ViewRefreshIcon(), func() { f.syncAllNow() }),
		f.pauseBtn,
	)

	top := container.NewVBox(header, f.cta, widget.NewSeparator())
	return container.NewBorder(top, footer, nil, nil, tabs)
}

// ---------------------------------------------------------------------------
// System tray
// ---------------------------------------------------------------------------

// installTray wires the Fyne system-tray icon and menu (desktop driver only).
func (f *FyneApp) installTray() {
	desk, ok := f.app.(desktop.App)
	if !ok {
		if f.logger != nil {
			f.logger.Warn("Fyne driver has no system tray support")
		}
		return
	}
	desk.SetSystemTrayIcon(fyneIconForState(f.ctrl.State()))
	desk.SetSystemTrayMenu(f.buildTrayMenu())
}

// buildTrayMenu constructs the (small, Nextcloud-style) tray menu.
func (f *FyneApp) buildTrayMenu() *fyne.Menu {
	pauseLabel := "Pause All"
	if f.isPaused {
		pauseLabel = "Resume All"
	}
	return fyne.NewMenu("gcrypt",
		fyne.NewMenuItem("Open gcrypt", func() { f.showWindow() }),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Sync All Now", func() { f.syncAllNow() }),
		fyne.NewMenuItem(pauseLabel, func() { f.togglePauseAll() }),
		fyne.NewMenuItem("Open Sync Folder", func() { f.openSyncFolder() }),
		fyne.NewMenuItem("View Log", func() { f.viewLog() }),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Quit", func() { f.quit() }),
	)
}

// refreshTrayMenu rebuilds the tray menu (e.g. to flip Pause/Resume).
func (f *FyneApp) refreshTrayMenu() {
	if desk, ok := f.app.(desktop.App); ok {
		desk.SetSystemTrayMenu(f.buildTrayMenu())
	}
}

func (f *FyneApp) showWindow() {
	fyne.Do(func() {
		f.win.Show()
		f.win.RequestFocus()
		f.refresh()
	})
}

// ---------------------------------------------------------------------------
// Refresh
// ---------------------------------------------------------------------------

// refreshLoop periodically repaints the dynamic parts of the UI.
func (f *FyneApp) refreshLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		fyne.Do(func() { f.refresh() })
	}
}

// refresh updates the header, CTA, activity feed and tray icon from current
// controller/manager state. Must run on the Fyne goroutine.
func (f *FyneApp) refresh() {
	state := f.ctrl.State()

	// Status dot + label.
	col, statusText := dotColorAndText(state)
	f.statusDot.FillColor = col
	f.statusDot.Refresh()
	f.statusLabel.SetText(statusText)

	// The tray icon tracks the high-level state; the app/window keep the brand
	// logo. Only swap the tray icon when the state actually changes (the icons
	// are 256px, so re-decoding every 2s would be wasteful).
	if !f.lastIconStateSet || f.lastIconState != state {
		f.lastIconState = state
		f.lastIconStateSet = true
		if desk, ok := f.app.(desktop.App); ok {
			desk.SetSystemTrayIcon(fyneIconForState(state))
		}
	}

	// Summary line + activity feed (only meaningful once syncing).
	f.refreshSummary()
	f.refreshActivity()

	// State-driven CTA.
	switch state {
	case appstate.NotConfigured:
		f.setCTA("Run Setup", func() { f.runSetup() })
	case appstate.NeedsPassphrase:
		f.setCTA("Unlock", func() { go func() { _ = f.ctrl.HandlePassphrase() }() })
	case appstate.NeedsOAuth:
		f.setCTA("Sign in with Google", func() {
			go func() {
				if err := f.ctrl.HandleOAuth(); err != nil {
					fyne.Do(func() { dialog.ShowError(err, f.win) })
				}
			}()
		})
	default:
		f.cta.Hide()
	}
}

func (f *FyneApp) setCTA(label string, fn func()) {
	f.cta.SetText(label)
	f.cta.OnTapped = fn
	f.cta.Show()
}

// refreshSummary updates the folder line, cumulative counters + live transfer
// rate, and the in-flight ("currently syncing") line.
func (f *FyneApp) refreshSummary() {
	cfg := f.ctrl.Config()
	dir := ""
	if cfg != nil {
		dir = cfg.SyncDir()
	}
	f.summary.SetText(dir)

	manager := f.ctrl.Manager()
	if manager == nil {
		f.metrics.SetText("")
		f.liveLabel.SetText("")
		return
	}

	agg := manager.GetAggregatedState()
	var (
		upFiles, downFiles    int64
		bytesUp, bytesDown    int64
		pending, active       int
		current               []string
	)
	for _, ps := range agg.PairStatuses {
		upFiles += ps.Stats.FilesUploaded
		downFiles += ps.Stats.FilesDownloaded
		bytesUp += ps.Stats.BytesUploaded
		bytesDown += ps.Stats.BytesDownloaded
		pending += ps.Activity.Pending
		active += ps.Activity.Active
		current = append(current, ps.Activity.Current...)
	}

	// Compute a smoothed transfer rate from byte deltas since the last refresh.
	f.updateRates(bytesUp, bytesDown)

	metrics := fmt.Sprintf("↑ %d files (%s)   ·   ↓ %d files (%s)",
		upFiles, humanBytes(bytesUp), downFiles, humanBytes(bytesDown))
	if f.rateUp >= 1 || f.rateDown >= 1 {
		metrics += fmt.Sprintf("\n↑ %s   ↓ %s", humanRate(f.rateUp), humanRate(f.rateDown))
	}
	f.metrics.SetText(metrics)

	// Live in-flight line (current file(s) + pending backlog).
	switch {
	case active > 0 && len(current) > 0:
		first := current[0]
		if len(current) > 1 {
			f.liveLabel.SetText(fmt.Sprintf("⚡ %s  (+%d more)   ·   %d pending", first, len(current)-1, pending))
		} else {
			f.liveLabel.SetText(fmt.Sprintf("⚡ %s   ·   %d pending", first, pending))
		}
	case pending > 0:
		f.liveLabel.SetText(fmt.Sprintf("⏳ %d pending", pending))
	default:
		f.liveLabel.SetText("✓ Up to date")
	}
}

// updateRates recomputes the smoothed up/down byte rate from the change in
// cumulative bytes since the previous call.
func (f *FyneApp) updateRates(bytesUp, bytesDown int64) {
	now := time.Now()
	if !f.lastRateTime.IsZero() {
		dt := now.Sub(f.lastRateTime).Seconds()
		if dt > 0 {
			du := float64(bytesUp-f.lastBytesUp) / dt
			dd := float64(bytesDown-f.lastBytesDown) / dt
			if du < 0 {
				du = 0
			}
			if dd < 0 {
				dd = 0
			}
			// Exponential smoothing so the figure isn't jumpy; decays to ~0
			// quickly once a transfer stops.
			const alpha = 0.4
			f.rateUp = alpha*du + (1-alpha)*f.rateUp
			f.rateDown = alpha*dd + (1-alpha)*f.rateDown
			if f.rateUp < 1 {
				f.rateUp = 0
			}
			if f.rateDown < 1 {
				f.rateDown = 0
			}
		}
	}
	f.lastBytesUp = bytesUp
	f.lastBytesDown = bytesDown
	f.lastRateTime = now
}

// humanBytes formats a byte count as B/KB/MB/GB.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// humanRate formats a bytes/sec rate as KB/s, MB/s, etc.
func humanRate(bps float64) string {
	switch {
	case bps >= 1024*1024:
		return fmt.Sprintf("%.1f MB/s", bps/(1024*1024))
	case bps >= 1024:
		return fmt.Sprintf("%.0f KB/s", bps/1024)
	default:
		return fmt.Sprintf("%.0f B/s", bps)
	}
}

// ---------------------------------------------------------------------------
// Footer / menu actions
// ---------------------------------------------------------------------------

func (f *FyneApp) syncAllNow() {
	manager := f.ctrl.Manager()
	if manager == nil {
		return
	}
	for _, ps := range manager.ListPairs() {
		_ = manager.SyncNow(ps.ID)
	}
}

func (f *FyneApp) togglePauseAll() {
	manager := f.ctrl.Manager()
	if manager == nil {
		return
	}
	f.mu.Lock()
	pause := !f.isPaused
	f.isPaused = pause
	f.mu.Unlock()

	for _, ps := range manager.ListPairs() {
		if pause {
			_ = manager.PausePair(ps.ID)
		} else {
			_ = manager.ResumePair(ps.ID)
		}
	}
	fyne.Do(func() {
		if pause {
			f.pauseBtn.SetText("Resume All")
			f.pauseBtn.SetIcon(theme.MediaPlayIcon())
		} else {
			f.pauseBtn.SetText("Pause All")
			f.pauseBtn.SetIcon(theme.MediaPauseIcon())
		}
		f.refreshTrayMenu()
	})
}

func (f *FyneApp) openSyncFolder() {
	cfg := f.ctrl.Config()
	if cfg == nil {
		return
	}
	dir := cfg.SyncDir()
	if dir == "" {
		return
	}
	if err := exec.Command("explorer", dir).Start(); err != nil {
		fmt.Fprintf(os.Stderr, "service: open sync folder: %v\n", err)
	}
}

func (f *FyneApp) viewLog() {
	path := ""
	if f.logger != nil {
		path = f.logger.Path()
	}
	if path == "" {
		if cfg := f.ctrl.Config(); cfg != nil {
			path = cfg.LogPath()
		}
	}
	if path == "" {
		return
	}
	if err := exec.Command("notepad", path).Start(); err != nil {
		fmt.Fprintf(os.Stderr, "service: view log: %v\n", err)
	}
}

func (f *FyneApp) quit() {
	f.ctrl.Shutdown()
	f.app.Quit()
}

// runSetup launches the native setup flow, then starts any new pairs.
func (f *FyneApp) runSetup() {
	go func() {
		if err := RunSetup(f.ctrl, f.logger); err != nil {
			if f.logger != nil {
				f.logger.Warn("setup did not complete", map[string]interface{}{"error": err.Error()})
			}
			return
		}
		f.startNewPairs()
		fyne.Do(func() {
			f.refresh()
			f.refreshPairs()
		})
	}()
}

// startNewPairs adds engines for any enabled, not-yet-running pairs.
func (f *FyneApp) startNewPairs() {
	manager := f.ctrl.Manager()
	cfg := f.ctrl.Config()
	if manager == nil || cfg == nil {
		return
	}
	running := make(map[string]bool)
	for _, ps := range manager.ListPairs() {
		running[ps.ID] = true
	}
	for i := range cfg.SyncPairs {
		pair := &cfg.SyncPairs[i]
		if pair.Enabled && !running[pair.ID] {
			if err := manager.AddPair(pair); err != nil {
				fmt.Fprintf(os.Stderr, "service: start new pair %s: %v\n", pair.ID, err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// State → presentation helpers
// ---------------------------------------------------------------------------

func dotColorAndText(state appstate.State) (color.Color, string) {
	switch state {
	case appstate.NotConfigured:
		return colGray, "Not configured"
	case appstate.NeedsPassphrase:
		return colAmber, "Locked"
	case appstate.NeedsOAuth:
		return colRed, "Sign in required"
	case appstate.Connecting:
		return colBlue, "Connecting…"
	case appstate.Scanning:
		return colBlue, "Scanning…"
	case appstate.Syncing:
		return colBlue, "Syncing…"
	case appstate.Idle:
		return colGreen, "Up to date"
	case appstate.Error:
		return colRed, "Error"
	case appstate.Disconnected:
		return colRed, "Disconnected"
	default:
		return colGray, state.String()
	}
}

func fyneIconForState(state appstate.State) fyne.Resource {
	switch state {
	case appstate.NotConfigured, appstate.NeedsPassphrase, appstate.NeedsOAuth:
		// Needs user action (setup pending / login required) → warning.
		return resWarning
	case appstate.Connecting, appstate.Scanning, appstate.Syncing:
		return resSyncing
	case appstate.Idle:
		return resSynced
	case appstate.Error, appstate.Disconnected:
		return resError
	default:
		return resWarning
	}
}
