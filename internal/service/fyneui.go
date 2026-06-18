package service

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"strings"
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
	"github.com/arumes31/gcrypt/internal/drive"
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

	mu     sync.Mutex
	events []syncpkg.ActivityEvent

	// lastPaused mirrors the derived "all pairs paused" state so the tray menu
	// and footer button are only rebuilt when it actually flips.
	lastPaused    bool
	lastPausedSet bool

	// notifyState tracks the high-level state used for desktop notifications so
	// we only fire one toast per real transition (not on every 2s refresh).
	notifyState    appstate.State
	notifyStateSet bool

	// Activity-feed dedup: skip List.Refresh() when nothing changed (avoids
	// fighting the user's scroll position and pointless redraws).
	lastActivityCount int
	lastActivityTop   time.Time

	// Per-pair live widgets (Folders tab), keyed by pair ID. Rebuilt by
	// refreshPairs and updated in place by the refresh loop so per-folder stats
	// stay live without rebuilding the whole tab.
	pairWidgets map[string]*pairWidgets

	// Header widgets.
	statusDot   *canvas.Circle
	statusLabel *widget.Label
	summary     *widget.Label       // sync folder path
	metrics     *widget.Label       // cumulative counts + transfer rate
	liveLabel   *widget.Label       // current in-flight file(s) + pending
	progress    *widget.ProgressBar // overall sync progress (done / total)

	// Account / Drive-quota widgets (Settings tab; updated by a background poll).
	accountLabel *widget.Label
	quotaLabel   *widget.Label
	quotaBar     *widget.ProgressBar
	lastAcctTime time.Time // throttles the About() poll

	// Live transfer-rate tracking (byte deltas between refreshes).
	lastBytesUp   int64
	lastBytesDown int64
	lastRateTime  time.Time
	rateUp        float64 // bytes/sec, smoothed
	rateDown      float64

	// lastIconState avoids re-decoding/re-setting the tray icon every refresh;
	// the icon is only swapped when the high-level state (or the issue badge)
	// actually changes. See lastBadgeSet/lastHasIssues in refreshTrayStatus.
	lastIconState appstate.State

	// State-driven call-to-action (Run Setup / Unlock / Sign in).
	cta *widget.Button

	// Activity feed.
	activityList *widget.List
	emptyHint    *widget.Label

	// Live "now transferring" panel (top of Activity tab) + its last-rendered
	// signature, so it's only rebuilt when the in-flight set actually changes.
	liveTransfers *fyne.Container
	lastLiveSig   string

	// Search/filter text for the Activity and Folders tabs (lower-cased).
	activityFilter string
	folderFilter   string

	// Byte-based progress baseline: when a fresh batch of work begins we snapshot
	// the cumulative transferred bytes so the bar measures this batch from 0→100%
	// instead of including all prior sessions.
	batchActive    bool
	batchBaseBytes int64

	// Tray status header text + badge state, tracked so the tray menu/icon are
	// only rebuilt when the displayed status actually changes.
	trayStatus     string
	lastTrayStatus string
	lastHasIssues  bool
	lastBadgeSet   bool

	// Throttled issue count (failed transfers + pending conflicts), so the tray
	// status doesn't hit the store on every 2s tick.
	lastIssues     int
	lastIssuesTime time.Time

	// Folders tab content (rebuilt on change).
	pairsContainer *fyne.Container

	// Issues tab content (rebuilt on demand): failed transfers + conflicts.
	issuesContainer *fyne.Container

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
	f.app.SetIcon(tightIcon(resLogo))
	f.app.Settings().SetTheme(brandTheme{})

	f.win = f.app.NewWindow("gcrypt")
	f.win.SetIcon(tightIcon(resLogo))
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

	// Periodic refresh of the signed-in account + Drive storage quota.
	go f.accountLoop()

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
	logoImg := canvas.NewImageFromResource(tightIcon(resLogo))
	logoImg.FillMode = canvas.ImageFillContain
	logoWrap := container.NewGridWrap(fyne.NewSize(40, 40), logoImg)

	// Always-visible reminder of the product's core promise: everything is
	// encrypted on this PC before it ever reaches Drive.
	encLine := widget.NewLabel("🔒 End-to-end encrypted (AES-256-GCM)")
	encLine.Importance = widget.LowImportance

	textCol := container.NewVBox(
		widget.NewLabelWithStyle("gcrypt", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		container.NewHBox(dotWrap, f.statusLabel),
		encLine,
		f.summary,
		f.metrics,
		f.liveLabel,
	)
	header := container.NewBorder(nil, nil, logoWrap, nil, textCol)

	// --- Overall sync progress bar (hidden when idle / up to date). ---
	f.progress = widget.NewProgressBar()
	f.progress.Hide()

	// --- State-driven CTA button (hidden unless an action is needed). ---
	f.cta = widget.NewButton("", nil)
	f.cta.Importance = widget.HighImportance
	f.cta.Hide()

	// --- Tabs: Activity / Folders / Settings. ---
	f.activityList = f.buildActivityList()
	f.emptyHint = widget.NewLabel("No recent activity yet.")
	f.emptyHint.Alignment = fyne.TextAlignCenter

	// Live "now transferring" panel above the recent-activity feed; hidden when
	// nothing is in flight.
	f.liveTransfers = container.NewVBox()
	f.liveTransfers.Hide()

	activitySearch := widget.NewEntry()
	activitySearch.SetPlaceHolder("Search activity…")
	activitySearch.OnChanged = func(s string) {
		f.activityFilter = strings.ToLower(strings.TrimSpace(s))
		f.lastActivityCount = -1 // force a repaint even if the count is unchanged
		fyne.Do(func() { f.refreshActivity() })
	}
	activityTop := container.NewVBox(activitySearch, f.liveTransfers)
	activityTab := container.NewBorder(activityTop, nil, nil, nil,
		container.NewStack(f.activityList, container.NewCenter(f.emptyHint)))

	f.pairsContainer = container.NewVBox()
	folderSearch := widget.NewEntry()
	folderSearch.SetPlaceHolder("Filter folders…")
	folderSearch.OnChanged = func(s string) {
		f.folderFilter = strings.ToLower(strings.TrimSpace(s))
		fyne.Do(func() { f.refreshPairs() })
	}
	foldersTab := container.NewBorder(folderSearch, nil, nil, nil,
		container.NewVScroll(f.pairsContainer))

	tabs := container.NewAppTabs(
		container.NewTabItemWithIcon("Activity", theme.HistoryIcon(), activityTab),
		container.NewTabItemWithIcon("Folders", theme.FolderIcon(), foldersTab),
		container.NewTabItemWithIcon("Issues", theme.WarningIcon(), f.buildIssuesTab()),
		container.NewTabItemWithIcon("Settings", theme.SettingsIcon(), f.buildSettingsTab()),
	)
	tabs.OnSelected = func(ti *container.TabItem) {
		switch ti.Text {
		case "Folders":
			f.refreshPairs()
		case "Issues":
			f.refreshIssues()
		}
	}

	// --- Footer toolbar. ---
	f.pauseBtn = widget.NewButtonWithIcon("Pause All", theme.MediaPauseIcon(), func() { f.togglePauseAll() })
	footer := container.NewGridWithColumns(3,
		widget.NewButtonWithIcon("Open", theme.FolderOpenIcon(), func() { f.openSyncFolder() }),
		widget.NewButtonWithIcon("Sync Now", theme.ViewRefreshIcon(), func() { f.syncAllNow() }),
		f.pauseBtn,
	)

	top := container.NewVBox(header, f.progress, f.cta, widget.NewSeparator())
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

// buildTrayMenu constructs the (small, Nextcloud-style) tray menu. The first
// item is a disabled status header ("Up to date" / "Uploading…" / "2 issues")
// so the current state is visible at a glance without opening the window.
func (f *FyneApp) buildTrayMenu() *fyne.Menu {
	pauseLabel := "Pause All"
	if f.syncPaused() {
		pauseLabel = "Resume All"
	}
	statusText := f.trayStatus
	if statusText == "" {
		statusText = "gcrypt"
	}
	statusHeader := fyne.NewMenuItem(statusText, nil)
	statusHeader.Disabled = true
	return fyne.NewMenu("gcrypt",
		statusHeader,
		fyne.NewMenuItemSeparator(),
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

// issuesCount returns the number of attention-worthy problems (failed transfers
// + conflicts awaiting a decision), throttled so the underlying store reads run
// at most every few seconds rather than on every 2s refresh tick.
func (f *FyneApp) issuesCount() int {
	if !f.lastIssuesTime.IsZero() && time.Since(f.lastIssuesTime) < 8*time.Second {
		return f.lastIssues
	}
	n := 0
	if manager := f.ctrl.Manager(); manager != nil {
		n = len(manager.ListErrored()) + len(manager.PendingConflicts())
	}
	f.lastIssues = n
	f.lastIssuesTime = time.Now()
	return n
}

// refreshTrayStatus updates the tray menu's status header and the tray icon's
// "issues" badge. Both are only rebuilt when their displayed value actually
// changes, to avoid churning the native tray every refresh tick. Must run on the
// Fyne goroutine.
func (f *FyneApp) refreshTrayStatus(state appstate.State, busy bool) {
	issues := f.issuesCount()
	hasIssues := issues > 0

	var status string
	switch {
	case hasIssues:
		status = fmt.Sprintf("⚠ %d issue%s", issues, plural(issues))
	case f.syncPaused():
		status = "Paused"
	case busy:
		if f.rateDown > f.rateUp {
			status = "Downloading…"
		} else {
			status = "Uploading…"
		}
	default:
		_, txt := dotColorAndText(state)
		status = txt
	}
	f.trayStatus = status
	if status != f.lastTrayStatus {
		f.lastTrayStatus = status
		f.refreshTrayMenu()
	}

	// Tray icon (with badge), swapped only when the state or issue flag changes.
	if desk, ok := f.app.(desktop.App); ok {
		if !f.lastBadgeSet || f.lastIconState != state || f.lastHasIssues != hasIssues {
			f.lastBadgeSet = true
			f.lastIconState = state
			f.lastHasIssues = hasIssues
			desk.SetSystemTrayIcon(iconForState(state, hasIssues))
		}
	}
}

// plural returns "s" unless n == 1, for simple count labels.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// badgeCache memoises composited (badged) tray icons by base resource name so we
// don't re-decode/re-encode a PNG on every state change.
var badgeCache sync.Map // string -> fyne.Resource

// iconForState returns the tray icon for a state, overlaying a small red badge
// dot when there are unresolved issues so the tray flags problems at a glance.
func iconForState(state appstate.State, hasIssues bool) fyne.Resource {
	base := fyneIconForState(state)
	if !hasIssues {
		return base
	}
	if v, ok := badgeCache.Load(base.Name()); ok {
		return v.(fyne.Resource)
	}
	res := overlayBadge(base)
	badgeCache.Store(base.Name(), res)
	return res
}

// overlayBadge draws a filled red disc in the bottom-right of the base icon and
// returns it as a new PNG resource. On any decode/encode error it returns the
// base unchanged (a missing badge is cosmetic, never fatal).
func overlayBadge(base fyne.Resource) fyne.Resource {
	img, err := png.Decode(bytes.NewReader(base.Content()))
	if err != nil {
		return base
	}
	b := img.Bounds()
	rgba := image.NewRGBA(b)
	draw.Draw(rgba, b, img, b.Min, draw.Src)

	r := b.Dx() / 4
	cx := b.Max.X - r - b.Dx()/16
	cy := b.Max.Y - r - b.Dy()/16
	red := color.NRGBA{R: 0xe7, G: 0x4c, B: 0x3c, A: 0xff}
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= r*r && image.Pt(x, y).In(b) {
				rgba.Set(x, y, red)
			}
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, rgba); err != nil {
		return base
	}
	return fyne.NewStaticResource(base.Name()+"-badge", buf.Bytes())
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

// accountLoop periodically refreshes the signed-in account and Drive storage
// quota. Quota changes slowly, so it polls far less often than the stats loop
// and only when a Drive client is actually available.
func (f *FyneApp) accountLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	// Fetch once shortly after start, then on each tick.
	f.refreshAccountInfo()
	for range ticker.C {
		f.refreshAccountInfo()
	}
}

// refreshAccountInfo fetches the account email and Drive quota off the Fyne
// goroutine and pushes the result into the Settings-tab labels. It is a no-op
// until the Drive client exists (i.e. the user has signed in).
func (f *FyneApp) refreshAccountInfo() {
	client := f.ctrl.DriveClient()
	if client == nil {
		// No Drive client means auth genuinely hasn't happened yet.
		fyne.Do(func() {
			if f.accountLabel != nil {
				f.accountLabel.SetText("Not signed in")
			}
		})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	info, err := client.About(ctx)
	if err != nil {
		// A Drive client exists, so we ARE signed in — uploads/downloads work on
		// the drive.file scope. The "about" resource (email + storage quota) may
		// be unavailable or fail under that minimal scope, so don't mislabel this
		// as "Not signed in"; just show signed-in without the extra details.
		if f.logger != nil {
			f.logger.Warn("failed to fetch Drive account info", map[string]interface{}{"error": err.Error()})
		}
		fyne.Do(func() {
			if f.accountLabel != nil {
				f.accountLabel.SetText("Signed in")
			}
		})
		return
	}

	f.lastAcctTime = time.Now()
	fyne.Do(func() { f.applyAccountInfo(info) })
}

// applyAccountInfo updates the account/quota widgets. Must run on the Fyne
// goroutine. The widgets only exist once the Settings tab has been built.
func (f *FyneApp) applyAccountInfo(info *drive.AccountInfo) {
	if info == nil {
		return
	}
	if f.accountLabel != nil {
		who := info.Email
		if who == "" {
			who = info.DisplayName
		}
		if who == "" {
			who = "(unknown account)"
		}
		f.accountLabel.SetText(who)
	}
	if f.quotaLabel != nil {
		if info.QuotaLimit > 0 {
			pct := float64(info.QuotaUsed) / float64(info.QuotaLimit) * 100
			f.quotaLabel.SetText(fmt.Sprintf("%s of %s used (%.1f%%)",
				humanBytes(info.QuotaUsed), humanBytes(info.QuotaLimit), pct))
		} else {
			f.quotaLabel.SetText(fmt.Sprintf("%s used (unlimited)", humanBytes(info.QuotaUsed)))
		}
	}
	if f.quotaBar != nil {
		if info.QuotaLimit > 0 {
			f.quotaBar.SetValue(float64(info.QuotaUsed) / float64(info.QuotaLimit))
			f.quotaBar.Show()
		} else {
			f.quotaBar.Hide()
		}
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

	// The tray icon (with an "issues" badge) and the tray menu's status header
	// are managed by refreshTrayStatus, called from refreshSummary below, so they
	// can reflect issue count + live transfer state, not just the coarse state.

	// Keep the Pause/Resume controls in step with the real per-pair state
	// (a folder may have been paused/resumed from its own card).
	f.refreshPauseControls()

	// Desktop notifications on meaningful transitions (entered error, came back
	// up to date, sign-in required). Fired once per real change.
	f.maybeNotify(state)

	// Summary line + activity feed (only meaningful once syncing).
	f.refreshSummary(state)
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

// maybeNotify fires a desktop notification when the high-level state changes in
// a way worth surfacing while the window is hidden/unfocused. It only acts on a
// genuine transition (prev → state), so the 2s refresh loop won't spam toasts.
func (f *FyneApp) maybeNotify(state appstate.State) {
	prev := f.notifyState
	had := f.notifyStateSet
	f.notifyState = state
	f.notifyStateSet = true

	// Don't announce the very first observed state at startup.
	if !had || prev == state {
		return
	}

	switch state {
	case appstate.Error:
		msg := "A sync error occurred. Open gcrypt to see details."
		if le := f.lastSyncError(); le != "" {
			msg = le
		}
		f.notify("gcrypt — Sync error", msg)
	case appstate.NeedsOAuth:
		f.notify("gcrypt — Sign in required", "Reconnect your Google account to keep syncing.")
	case appstate.Idle:
		// Only celebrate reaching "up to date" after actual sync activity.
		switch prev {
		case appstate.Connecting, appstate.Scanning, appstate.Syncing:
			f.notify("gcrypt — Up to date", "All files are in sync.")
		}
	}
}

// notify sends a desktop notification, guarding against a nil app.
func (f *FyneApp) notify(title, content string) {
	if f.app == nil {
		return
	}
	f.app.SendNotification(fyne.NewNotification(title, content))
}

// lastSyncError returns the most recent per-pair error message, if any.
func (f *FyneApp) lastSyncError() string {
	manager := f.ctrl.Manager()
	if manager == nil {
		return ""
	}
	for _, ps := range manager.GetAggregatedState().PairStatuses {
		if ps.Stats.LastError != "" {
			return ps.Stats.LastError
		}
	}
	return ""
}

// refreshSummary updates the folder line, cumulative counters + live transfer
// rate, and the in-flight ("currently syncing") line. It also refines the header
// status to reflect what is actually happening (e.g. "Uploading…") rather than
// the coarse engine state, which stays "Scanning" for the whole streaming scan.
func (f *FyneApp) refreshSummary(state appstate.State) {
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
		f.refreshLiveTransfers(nil, 0, 0)
		f.refreshTrayStatus(state, false)
		return
	}

	agg := manager.GetAggregatedState()
	var (
		upFiles, downFiles int64
		bytesUp, bytesDown int64
		pending, active    int
		pendingBytes       int64
		current            []string
	)
	for _, ps := range agg.PairStatuses {
		upFiles += ps.Stats.FilesUploaded
		downFiles += ps.Stats.FilesDownloaded
		bytesUp += ps.Stats.BytesUploaded
		bytesDown += ps.Stats.BytesDownloaded
		pending += ps.Activity.Pending
		active += ps.Activity.Active
		pendingBytes += ps.Activity.PendingBytes
		current = append(current, ps.Activity.Current...)
	}
	busy := active > 0 || pending > 0

	// Update any live per-folder widgets on the Folders tab.
	f.updatePairWidgets(agg)

	// Compute a smoothed transfer rate from byte deltas since the last refresh.
	f.updateRates(bytesUp, bytesDown)

	// Refine the header status: while the engine is connecting/scanning/syncing,
	// what the user actually cares about is whether files are moving. The initial
	// scan of a large tree streams for a long time (state stays "Scanning") even
	// though uploads are already running, so show "Uploading…"/"Downloading…"
	// whenever there is in-flight or queued transfer work.
	if busy {
		switch state {
		case appstate.Connecting, appstate.Scanning, appstate.Syncing, appstate.Idle:
			if f.rateDown > f.rateUp {
				f.statusLabel.SetText("Downloading…")
			} else {
				f.statusLabel.SetText("Uploading…")
			}
		}
	}

	metrics := fmt.Sprintf("↑ %d files (%s)   ·   ↓ %d files (%s)",
		upFiles, humanBytes(bytesUp), downFiles, humanBytes(bytesDown))
	if f.rateUp >= 1 || f.rateDown >= 1 {
		metrics += fmt.Sprintf("\n↑ %s   ↓ %s", humanRate(f.rateUp), humanRate(f.rateDown))
	}
	f.metrics.SetText(metrics)

	// ETA from the bytes still to move and the smoothed combined transfer rate.
	rate := f.rateUp + f.rateDown
	etaSecs := 0.0
	if busy && pendingBytes > 0 && rate >= 1 {
		etaSecs = float64(pendingBytes) / rate
	}

	// Overall progress: byte-based for the current batch (so the bar doesn't jump
	// when a large file follows many tiny ones), measured from a baseline captured
	// when the batch began so it spans 0→100%. Falls back to a file-count ratio
	// for byte-less batches (e.g. pure deletes). Hidden when idle.
	if f.progress != nil {
		if busy {
			if !f.batchActive {
				f.batchActive = true
				f.batchBaseBytes = bytesUp + bytesDown
			}
			doneBatch := (bytesUp + bytesDown) - f.batchBaseBytes
			if doneBatch < 0 {
				doneBatch = 0
			}
			if denom := doneBatch + pendingBytes; denom > 0 {
				f.progress.SetValue(float64(doneBatch) / float64(denom))
			} else {
				doneFiles := upFiles + downFiles
				if totalFiles := doneFiles + int64(active+pending); totalFiles > 0 {
					f.progress.SetValue(float64(doneFiles) / float64(totalFiles))
				}
			}
			f.progress.Show()
		} else {
			f.batchActive = false
			f.progress.Hide()
		}
	}

	// Live in-flight line (current file(s) + pending backlog + ETA).
	etaSuffix := ""
	if etaSecs > 0 {
		etaSuffix = "   ·   ~" + humanETA(etaSecs) + " left"
	}
	switch {
	case active > 0 && len(current) > 0:
		first := current[0]
		if len(current) > 1 {
			f.liveLabel.SetText(fmt.Sprintf("⚡ %s  (+%d more)   ·   %d pending%s", first, len(current)-1, pending, etaSuffix))
		} else {
			f.liveLabel.SetText(fmt.Sprintf("⚡ %s   ·   %d pending%s", first, pending, etaSuffix))
		}
	case pending > 0:
		f.liveLabel.SetText(fmt.Sprintf("⏳ %d pending%s", pending, etaSuffix))
	default:
		f.liveLabel.SetText("✓ Up to date")
	}

	// Detailed "now transferring" panel + the tray status header/badge.
	f.refreshLiveTransfers(current, pending, etaSecs)
	f.refreshTrayStatus(state, busy)
}

// humanETA renders a remaining-time estimate compactly: "45s", "3m 20s",
// "1h 04m". Anything over a day is reported as ">1d" since the estimate is too
// rough to be meaningful at that range.
func humanETA(secs float64) string {
	if secs < 1 {
		return "0s"
	}
	d := time.Duration(secs) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return ">1d"
	}
}

// refreshLiveTransfers rebuilds the "now transferring" panel at the top of the
// Activity tab from the in-flight operation descriptions. It is hidden when
// nothing is moving, and only rebuilt when the displayed set actually changes
// (so it doesn't flicker every 2s). Must run on the Fyne goroutine.
func (f *FyneApp) refreshLiveTransfers(current []string, pending int, etaSecs float64) {
	if f.liveTransfers == nil {
		return
	}

	if len(current) == 0 && pending == 0 {
		if f.lastLiveSig != "" {
			f.lastLiveSig = ""
			f.liveTransfers.Hide()
		}
		return
	}

	const maxRows = 6
	etaTxt := ""
	if etaSecs > 0 {
		etaTxt = "~" + humanETA(etaSecs) + " left"
	}
	// Signature: changes only when the visible content would change.
	sig := fmt.Sprintf("%d|%d|%s|%v", len(current), pending, etaTxt, current)
	if sig == f.lastLiveSig {
		return
	}
	f.lastLiveSig = sig

	f.liveTransfers.RemoveAll()
	f.liveTransfers.Add(widget.NewLabelWithStyle(
		fmt.Sprintf("Transferring (%d)", len(current)),
		fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))

	shown := current
	if len(shown) > maxRows {
		shown = shown[:maxRows]
	}
	for _, desc := range shown {
		row := widget.NewLabel(desc)
		row.Wrapping = fyne.TextWrapOff
		f.liveTransfers.Add(row)
	}
	if len(current) > maxRows {
		more := widget.NewLabel(fmt.Sprintf("+%d more", len(current)-maxRows))
		more.Importance = widget.LowImportance
		f.liveTransfers.Add(more)
	}

	footer := fmt.Sprintf("%d pending", pending)
	if etaTxt != "" {
		footer += "   ·   " + etaTxt
	}
	footLabel := widget.NewLabel(footer)
	footLabel.Importance = widget.LowImportance
	f.liveTransfers.Add(footLabel)
	f.liveTransfers.Add(widget.NewSeparator())
	f.liveTransfers.Show()
	f.liveTransfers.Refresh()
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

// humanBytes formats a byte count using binary (1024-based) IEC units, so the
// labels (KiB/MiB/GiB/TiB) match the divisor actually used.
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
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// humanRate formats a bytes/sec rate using binary IEC units (KiB/s, MiB/s).
func humanRate(bps float64) string {
	switch {
	case bps >= 1024*1024:
		return fmt.Sprintf("%.1f MiB/s", bps/(1024*1024))
	case bps >= 1024:
		return fmt.Sprintf("%.0f KiB/s", bps/1024)
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

	// Derive the action from the real per-pair state rather than a local flag:
	// if everything is already paused, resume; otherwise (including a mixed
	// state where the user paused some folders by hand) pause all.
	pause := !f.syncPaused()
	for _, ps := range manager.ListPairs() {
		if pause {
			_ = manager.PausePair(ps.ID)
		} else {
			_ = manager.ResumePair(ps.ID)
		}
	}
	fyne.Do(func() {
		f.refreshPauseControls()
		f.refreshPairs()
	})
}

// syncPaused reports whether there is at least one managed pair and every pair
// is currently paused. It is the single source of truth for the Pause/Resume
// label in both the footer and the tray menu.
func (f *FyneApp) syncPaused() bool {
	manager := f.ctrl.Manager()
	if manager == nil {
		return false
	}
	pairs := manager.ListPairs()
	if len(pairs) == 0 {
		return false
	}
	for _, ps := range pairs {
		if ps.State != syncpkg.StatePaused {
			return false
		}
	}
	return true
}

// refreshPauseControls syncs the footer Pause/Resume button and (only when the
// derived state flips) the tray menu with the actual per-pair pause state.
func (f *FyneApp) refreshPauseControls() {
	paused := f.syncPaused()
	if f.lastPausedSet && f.lastPaused == paused {
		return // no change — avoid pointless widget/tray-menu refreshes
	}
	f.lastPaused = paused
	f.lastPausedSet = true

	if f.pauseBtn != nil {
		if paused {
			f.pauseBtn.SetText("Resume All")
			f.pauseBtn.SetIcon(theme.MediaPlayIcon())
		} else {
			f.pauseBtn.SetText("Pause All")
			f.pauseBtn.SetIcon(theme.MediaPauseIcon())
		}
	}
	f.refreshTrayMenu()
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
	if err := exec.CommandContext(context.Background(), "explorer", dir).Start(); err != nil { // #nosec G204 -- fixed command; dir is the app's configured sync folder
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
	if err := exec.CommandContext(context.Background(), "notepad", path).Start(); err != nil { // #nosec G204 -- fixed command; path is the app's own log file
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
	return tightIcon(rawIconForState(state))
}

func rawIconForState(state appstate.State) fyne.Resource {
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

// tightCache memoises cropped icons by base resource name.
var tightCache sync.Map // string -> fyne.Resource

// tightIcon crops the faint, near-empty margin around an icon's glyph so it
// fills the small space the OS gives it. The source PNGs center a ~126px glyph
// in a 256px canvas with a low-alpha halo filling the rest, which renders tiny
// in the system tray and header. Result is cached; errors return the original.
func tightIcon(res fyne.Resource) fyne.Resource {
	if res == nil {
		return res
	}
	if v, ok := tightCache.Load(res.Name()); ok {
		return v.(fyne.Resource)
	}
	out := cropToGlyph(res)
	tightCache.Store(res.Name(), out)
	return out
}

// cropToGlyph crops res to a square tightly bounding its opaque glyph (pixels
// with alpha ≥ 128), plus ~20% breathing room, so the visible mark fills the
// icon. On any decode/encode failure it returns res unchanged.
func cropToGlyph(res fyne.Resource) fyne.Resource {
	img, err := png.Decode(bytes.NewReader(res.Content()))
	if err != nil {
		return res
	}
	b := img.Bounds()
	const aThr = 128
	minX, minY, maxX, maxY := b.Max.X, b.Max.Y, b.Min.X, b.Min.Y
	found := false
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a>>8 >= aThr {
				found = true
				if x < minX {
					minX = x
				}
				if y < minY {
					minY = y
				}
				if x > maxX {
					maxX = x
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}
	if !found {
		return res
	}

	w, h := maxX-minX+1, maxY-minY+1
	cx, cy := (minX+maxX)/2, (minY+maxY)/2
	side := w
	if h > side {
		side = h
	}
	side += side / 5 // ~20% breathing room around the glyph
	half := side / 2
	x0, y0, x1, y1 := cx-half, cy-half, cx+half+1, cy+half+1
	if x0 < b.Min.X {
		x0 = b.Min.X
	}
	if y0 < b.Min.Y {
		y0 = b.Min.Y
	}
	if x1 > b.Max.X {
		x1 = b.Max.X
	}
	if y1 > b.Max.Y {
		y1 = b.Max.Y
	}

	rect := image.Rect(0, 0, x1-x0, y1-y0)
	dst := image.NewRGBA(rect)
	draw.Draw(dst, rect, img, image.Pt(x0, y0), draw.Src)

	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return res
	}
	return fyne.NewStaticResource(res.Name()+"-tight", buf.Bytes())
}
