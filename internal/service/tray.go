// Package service provides Windows-specific integration for gcrypt,
// including system tray, autostart, and logging functionality.
package service

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/daniel/gcrypt/internal/appstate"
	"github.com/daniel/gcrypt/internal/config"
	syncpkg "github.com/daniel/gcrypt/internal/sync"
	"github.com/getlantern/systray"
)

// ---------------------------------------------------------------------------
// Embedded icon data
// ---------------------------------------------------------------------------

// iconGreenICO is a minimal 16×16 green-circle ICO file encoded as base64.
var iconGreenICO = mustDecodeBase64("AAABAAEAEBAAAAEAIABoBAAAFgAAACgAAAAQAAAAIAAAAAEAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AAAAAAAAAAAAAAAAAAAAAAAAAAAAyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AAAAAAAAAAAAAAAAAyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wAAAAAAAAAAAMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AAAAAAAAAAADIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AAAAAAAAAAAAyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wAAAAAAAAAAAMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AAAAAAAAAAADIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AAAAAAAAAAAAAAAAAMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AAAAAAAAAAAAAAAAAAAAAAAAAAAAyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAADIAP8AyAD/AMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAMgA/wDIAP8AyAD/AMgA/wDIAP8AyAD/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==")

// iconBlueICO is a minimal 16×16 blue-circle ICO file encoded as base64.
var iconBlueICO = mustDecodeBase64("AAABAAEAEBAAAAEAIABoBAAAFgAAACgAAAAQAAAAIAAAAAEAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD/ZAD//2QA//9kAP//ZAD//2QA//9kAP8AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD/ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA/wAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD/ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD/AAAAAAAAAAAAAAAAAAAAAAAAAAD/ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP8AAAAAAAAAAAAAAAD/ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA/wAAAAAAAAAA/2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP8AAAAAAAAAAP9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD/AAAAAAAAAAD/ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA/wAAAAAAAAAA/2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP8AAAAAAAAAAP9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD/AAAAAAAAAAAAAAAA/2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD/AAAAAAAAAAAAAAAAAAAAAAAAAAD/ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAP9kAP//ZAD//2QA//9kAP//ZAD//2QA//9kAP//ZAD/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA/2QA//9kAP//ZAD//2QA//9kAP//ZAD/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==")

// iconRedICO is a minimal 16×16 red-circle ICO file encoded as base64.
var iconRedICO = mustDecodeBase64("AAABAAEAEBAAAAEAIABoBAAAFgAAACgAAAAQAAAAIAAAAAEAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAANz/AADc/wAA3P8AANz/AADc/wAA3P8AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AAAAAAAAAAAAAAAAAAAAAAAAAAAAANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AAAAAAAAAAAAAAAAAANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAAAAAAAAAAAADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AAAAAAAAAAAAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AAAAAAAAAAAAANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAAAAAAAAAAAADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AAAAAAAAAAAAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AAAAAAAAAAAAAAAAAADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AAAAAAAAAAAAAAAAAAAAAAAAAAAAANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA3P8AANz/AADc/wAA3P8AANz/AADc/wAA3P8AANz/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAADc/wAA3P8AANz/AADc/wAA3P8AANz/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==")

// iconGrayICO is a minimal 16×16 gray-circle ICO file encoded as base64.
var iconGrayICO = mustDecodeBase64("AAABAAEAEBAAAAEAIABoBAAAFgAAACgAAAAQAAAAIAAAAAEAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACAgID/gICA/4CAgP+AgID/gICA/4CAgP8AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACAgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/wAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACAgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/AAAAAAAAAAAAAAAAAAAAAAAAAACAgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP8AAAAAAAAAAAAAAACAgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/wAAAAAAAAAAgICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP8AAAAAAAAAAICAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/AAAAAAAAAACAgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/wAAAAAAAAAAgICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP8AAAAAAAAAAICAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/AAAAAAAAAAAAAAAAgICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/AAAAAAAAAAAAAAAAAAAAAAAAAACAgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAICAgP+AgID/gICA/4CAgP+AgID/gICA/4CAgP+AgID/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAgICA/4CAgP+AgID/gICA/4CAgP+AgID/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==")

// mustDecodeBase64 decodes a base64 string or panics.
func mustDecodeBase64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(fmt.Sprintf("service: decode base64 icon: %v", err))
	}
	return b
}

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

// maxPairSlots is the maximum number of sync pair submenus that can be
// displayed. Since systray does not support removing menu items, we
// pre-allocate slots and show/hide them as pairs are added/removed.
const maxPairSlots = 10

// ---------------------------------------------------------------------------
// Radio group helper
// ---------------------------------------------------------------------------

// radioGroup manages a set of mutually exclusive checkable menu items.
// Since systray does not have native radio items, this helper enforces
// the radio behaviour: clicking one item unchecks all others in the group.
type radioGroup struct {
	items  []*systray.MenuItem
	values []interface{} // Corresponding value for each item
	cur    int           // Index of the currently selected item
	apply  func(value interface{})
}

// newRadioGroup creates a radio group from pre-created checkable menu items.
// The item at initialIdx is checked; all others are unchecked.
func newRadioGroup(items []*systray.MenuItem, values []interface{}, initialIdx int, apply func(interface{})) *radioGroup {
	rg := &radioGroup{
		items:  items,
		values: values,
		cur:    initialIdx,
		apply:  apply,
	}
	// Set initial check state.
	for i, item := range items {
		if i == initialIdx {
			item.Check()
		} else {
			item.Uncheck()
		}
	}
	return rg
}

// selectItem checks the item at idx and unchecks all others, then applies
// the setting.
func (rg *radioGroup) selectItem(idx int) {
	for i, item := range rg.items {
		if i == idx {
			item.Check()
		} else {
			item.Uncheck()
		}
	}
	rg.cur = idx
	if rg.apply != nil {
		rg.apply(rg.values[idx])
	}
}

// ---------------------------------------------------------------------------
// Pair slot data
// ---------------------------------------------------------------------------

// pairSlot holds the menu items for a single sync pair within the
// "Sync Pairs" submenu. Slots are pre-allocated and shown/hidden as
// pairs are added or removed.
type pairSlot struct {
	pairID        string
	header        *systray.MenuItem // Status display (disabled, shows name + state)
	pause         *systray.MenuItem // Pause or Resume
	syncNow       *systray.MenuItem
	openDir       *systray.MenuItem
	sep           *systray.MenuItem // Separator before Remove
	remove        *systray.MenuItem
	removeConfirm *systray.MenuItem // "Confirm Remove?" — shown after clicking Remove
	removeTimer   *time.Timer

	// Additional menu items for Phase 1 features
	selectedFolders *systray.MenuItem
	forwardOnly     *systray.MenuItem
}

// ---------------------------------------------------------------------------
// TrayApp
// ---------------------------------------------------------------------------

// TrayApp manages the Windows system tray icon and context menu for gcrypt.
// It reads the application lifecycle state from AppController and updates
// the icon, tooltip, and menu items accordingly.
type TrayApp struct {
	ctrl   *AppController
	logger *Logger
	mu     sync.Mutex

	// --- State-driven menu items (shown/hidden based on appstate) ---

	// mStatus is always visible, disabled, shows current state as text.
	mStatus *systray.MenuItem

	// mRunSetup is visible when state is NotConfigured.
	mRunSetup *systray.MenuItem

	// mEnterPassphrase is visible when state is NeedsPassphrase.
	mEnterPassphrase *systray.MenuItem

	// mSignIn is visible when state is NeedsOAuth.
	mSignIn *systray.MenuItem

	// mOpenSyncFolder is visible when state is Idle, Syncing, or Scanning.
	mOpenSyncFolder *systray.MenuItem

	// mStats shows sync statistics (visible when sync is active).
	mStats *systray.MenuItem

	// mActivity shows live sync activity (current file + pending count).
	mActivity *systray.MenuItem

	// mPauseAll pauses/resumes all sync pairs (visible when sync is active).
	mPauseAll *systray.MenuItem

	// mSyncAll triggers immediate sync for all pairs (visible when sync is active).
	mSyncAll *systray.MenuItem

	// mViewLog opens the log file in Notepad (always visible).
	mViewLog *systray.MenuItem

	// mQuit is always visible.
	mQuit *systray.MenuItem

	// --- Sync Pairs submenu (visible when sync is active) ---

	// mSyncPairs is the parent menu item for the sync pairs submenu.
	mSyncPairs *systray.MenuItem

	// Pre-allocated pair slots (up to maxPairSlots)
	pairSlots [maxPairSlots]*pairSlot

	// "No sync pairs" placeholder (shown when 0 pairs)
	mNoPairs *systray.MenuItem

	// "Add Sync Pair" item
	mAddPair *systray.MenuItem

	// --- Settings submenu (visible when sync is active) ---

	// mSettings is the parent menu item for the settings submenu.
	mSettings *systray.MenuItem

	// Settings submenu items
	mAutoStart      *systray.MenuItem
	mStartMinimized *systray.MenuItem
	mRememberPass   *systray.MenuItem

	// Radio groups for settings
	syncIntervalGroup  *radioGroup
	maxFileSizeGroup   *radioGroup
	uploadLimitGroup   *radioGroup
	downloadLimitGroup *radioGroup
	logLevelGroup      *radioGroup
	logSizeGroup       *radioGroup
	logBackupsGroup    *radioGroup

	// State tracking
	isPaused bool
}

// NewTrayApp creates a new TrayApp that reads state from the given
// AppController. The logger may be nil.
func NewTrayApp(ctrl *AppController, logger *Logger) *TrayApp {
	return &TrayApp{
		ctrl:   ctrl,
		logger: logger,
	}
}

// Run starts the systray application and blocks until the user quits.
func (t *TrayApp) Run() {
	if t.logger != nil {
		t.logger.Info("systray.Run() called", nil)
	} else {
		fmt.Fprintf(os.Stderr, "gcrypt: systray.Run() called\n")
	}
	systray.Run(t.onReady, t.onExit)
}

// ---------------------------------------------------------------------------
// Menu construction (onReady)
// ---------------------------------------------------------------------------

// onReady is the systray callback invoked when the tray is initialised.
func (t *TrayApp) onReady() {
	if t.logger != nil {
		t.logger.Info("systray onReady callback started", map[string]interface{}{
			"icon_size": len(iconGreenICO),
		})
	} else {
		fmt.Fprintf(os.Stderr, "gcrypt: systray onReady callback started, icon size=%d bytes\n", len(iconGreenICO))
	}

	// --- Build all menu items (pre-allocate, then show/hide based on state) ---

	// Status item — always visible, always disabled (display only).
	t.mStatus = systray.AddMenuItem("📊 Status: —", "Current application state")
	t.mStatus.Disable()

	// Stats item — visible when sync is active.
	t.mStats = systray.AddMenuItem("📁 Files synced: 0", "Sync statistics")
	t.mStats.Disable()
	t.mStats.Hide()

	// Activity item — live current-file + pending count (visible when sync is active).
	t.mActivity = systray.AddMenuItem("✓ Up to date", "Current sync activity")
	t.mActivity.Disable()
	t.mActivity.Hide()

	systray.AddSeparator()

	// Enter Passphrase — visible when state is NeedsPassphrase.
	// Run Setup — visible when state is NotConfigured.
	t.mRunSetup = systray.AddMenuItem("🔧 Run Setup...", "Configure gcrypt (sync folder, passphrase, Google account)")
	t.mRunSetup.Hide()

	t.mEnterPassphrase = systray.AddMenuItem("🔑 Enter Passphrase", "Enter your encryption passphrase to unlock")
	t.mEnterPassphrase.Hide()

	// Sign In — visible when state is NeedsOAuth.
	t.mSignIn = systray.AddMenuItem("🔐 Sign In with Google", "Authorize gcrypt to access your Google Drive")
	t.mSignIn.Hide()

	// Open Sync Folder — visible when state is Idle, Syncing, or Scanning.
	t.mOpenSyncFolder = systray.AddMenuItem("📂 Open Sync Folder", "Open the local sync directory in Explorer")
	t.mOpenSyncFolder.Hide()

	systray.AddSeparator()

	// --- Sync Pairs submenu (visible when sync is active) ---
	t.buildSyncPairsSubmenu()
	t.mSyncPairs.Hide()

	systray.AddSeparator()

	// Pause All / Sync All Now — visible when sync is active.
	t.mPauseAll = systray.AddMenuItem("⏸️ Pause All", "Pause or resume all sync pairs")
	t.mSyncAll = systray.AddMenuItem("🔄 Sync All Now", "Trigger an immediate sync for all pairs")
	t.mPauseAll.Hide()
	t.mSyncAll.Hide()

	systray.AddSeparator()

	// --- Settings submenu (visible when sync is active) ---
	t.buildSettingsSubmenu()
	t.mSettings.Hide()

	systray.AddSeparator()

	// View Log — always visible.
	t.mViewLog = systray.AddMenuItem("📋 View Log", "Open the log file in Notepad")

	systray.AddSeparator()

	// Quit — always visible.
	t.mQuit = systray.AddMenuItem("❌ Quit", "Exit gcrypt")

	// --- Start click handlers --------------------------------------------

	// Enter Passphrase
	go func() {
		for range t.mEnterPassphrase.ClickedCh {
			t.handleEnterPassphrase()
		}
	}()

	// Sign In
	go func() {
		for range t.mSignIn.ClickedCh {
			t.handleSignIn()
		}
	}()

	// Open Sync Folder
	go func() {
		for range t.mOpenSyncFolder.ClickedCh {
			t.handleOpenSyncFolder()
		}
	}()

	// Pause All / Resume All
	go t.handlePauseAllLoop()

	// Sync All Now
	go func() {
		for range t.mSyncAll.ClickedCh {
			t.handleSyncAllNow()
		}
	}()

	// View Log
	go func() {
		for range t.mViewLog.ClickedCh {
			t.handleViewLog()
		}
	}()

	// Quit
	go func() {
		for range t.mQuit.ClickedCh {
			t.handleQuit()
		}
	}()

	// Start per-pair click handlers
	for _, ps := range t.pairSlots {
		if ps != nil {
			t.startPairHandlers(ps)
		}
	}

	// Start settings checkbox handlers
	go t.handleAutoStartLoop()
	go t.handleStartMinimizedLoop()
	go t.handleRememberPassphraseLoop()

	// Add Sync Pair handler
	go t.handleAddPairLoop()
	go t.handleRunSetupLoop()

	// --- Register for state changes from AppController -------------------

	t.ctrl.OnStateChange = func(oldState, newState appstate.State) {
		// State change callbacks may come from any goroutine.
		// systray menu operations must be called from the main goroutine on
		// some platforms, but getlantern/systray dispatches them safely on
		// Windows. We update the UI directly here.
		t.updateUIForState(newState)
	}

	// --- Background updaters ----------------------------------------------

	// Periodic status refresh every 2 seconds (updates stats, per-pair state).
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			t.updateSyncDetails()
		}
	}()

	// React to aggregated state changes from the SyncManager (if running).
	go func() {
		manager := t.ctrl.Manager()
		if manager == nil {
			return
		}
		ch := manager.StateChanges()
		if ch == nil {
			return
		}
		for range ch {
			t.updateSyncDetails()
		}
	}()

	// Log manager errors to stderr.
	go func() {
		manager := t.ctrl.Manager()
		if manager == nil {
			return
		}
		ch := manager.Errors()
		if ch == nil {
			return
		}
		for err := range ch {
			fmt.Fprintf(os.Stderr, "gcrypt engine error: %v\n", err)
		}
	}()

	// --- Set initial UI state based on AppController state ----------------

	initialState := t.ctrl.State()
	t.updateUIForState(initialState)

	// Attempt a silent auto-unlock if the user previously chose to remember
	// their passphrase. Run async so the tray stays responsive; state changes
	// propagate to the UI via the registered OnStateChange callback.
	go func() {
		if t.ctrl.TryAutoUnlock() && t.logger != nil {
			t.logger.Info("Auto-unlock attempted at startup")
		}
	}()
}

// onExit is the systray callback invoked on shutdown.
func (t *TrayApp) onExit() {
	t.ctrl.Shutdown()
}

// ---------------------------------------------------------------------------
// State-driven UI updates
// ---------------------------------------------------------------------------

// updateUIForState updates the tray icon, tooltip, and menu item visibility
// based on the given application lifecycle state.
func (t *TrayApp) updateUIForState(state appstate.State) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Update icon.
	icon := iconForState(state)
	systray.SetIcon(icon)

	// Update tooltip.
	tooltip := tooltipForState(state)
	systray.SetTooltip(tooltip)

	// Update status text.
	statusText := statusTextForState(state)
	t.mStatus.SetTitle(statusText)

	// Show/hide state-driven menu items.
	switch state {
	case appstate.NotConfigured:
		t.mEnterPassphrase.Hide()
		t.mSignIn.Hide()
		t.mOpenSyncFolder.Hide()
		t.mStats.Hide()
		t.mPauseAll.Hide()
		t.mSyncAll.Hide()
		t.mSyncPairs.Hide()
		t.mSettings.Hide()

	case appstate.NeedsPassphrase:
		t.mEnterPassphrase.Show()
		t.mSignIn.Hide()
		t.mOpenSyncFolder.Hide()
		t.mStats.Hide()
		t.mPauseAll.Hide()
		t.mSyncAll.Hide()
		t.mSyncPairs.Hide()
		t.mSettings.Hide()

	case appstate.NeedsOAuth:
		t.mEnterPassphrase.Hide()
		t.mSignIn.Show()
		t.mOpenSyncFolder.Hide()
		t.mStats.Hide()
		t.mPauseAll.Hide()
		t.mSyncAll.Hide()
		t.mSyncPairs.Hide()
		t.mSettings.Hide()

	case appstate.Connecting:
		t.mEnterPassphrase.Hide()
		t.mSignIn.Hide()
		t.mOpenSyncFolder.Hide()
		t.mStats.Hide()
		t.mPauseAll.Hide()
		t.mSyncAll.Hide()
		t.mSyncPairs.Hide()
		t.mSettings.Hide()

	case appstate.Scanning:
		t.mEnterPassphrase.Hide()
		t.mSignIn.Hide()
		t.mOpenSyncFolder.Show()
		t.mStats.Show()
		t.mPauseAll.Show()
		t.mSyncAll.Hide()
		t.mSyncPairs.Show()
		t.mSettings.Show()

	case appstate.Syncing:
		t.mEnterPassphrase.Hide()
		t.mSignIn.Hide()
		t.mOpenSyncFolder.Show()
		t.mStats.Show()
		t.mPauseAll.Show()
		t.mSyncAll.Show()
		t.mSyncPairs.Show()
		t.mSettings.Show()

	case appstate.Idle:
		t.mEnterPassphrase.Hide()
		t.mSignIn.Hide()
		t.mOpenSyncFolder.Show()
		t.mStats.Show()
		t.mPauseAll.Show()
		t.mSyncAll.Show()
		t.mSyncPairs.Show()
		t.mSettings.Show()

	case appstate.Error:
		t.mEnterPassphrase.Hide()
		t.mSignIn.Hide()
		t.mOpenSyncFolder.Hide()
		t.mStats.Hide()
		t.mPauseAll.Hide()
		t.mSyncAll.Hide()
		t.mSyncPairs.Hide()
		t.mSettings.Hide()

	case appstate.Disconnected:
		t.mEnterPassphrase.Hide()
		t.mSignIn.Hide()
		t.mOpenSyncFolder.Hide()
		t.mStats.Hide()
		t.mPauseAll.Hide()
		t.mSyncAll.Hide()
		t.mSyncPairs.Hide()
		t.mSettings.Hide()
	}

	// The live activity item mirrors mStats: visible only in active sync states.
	switch state {
	case appstate.Scanning, appstate.Syncing, appstate.Idle:
		t.mActivity.Show()
	default:
		t.mActivity.Hide()
	}

	// "Run Setup" is offered only when the app is not yet configured.
	if state == appstate.NotConfigured {
		t.mRunSetup.Show()
	} else {
		t.mRunSetup.Hide()
	}
}

// iconForState returns the appropriate icon data for the given state.
func iconForState(state appstate.State) []byte {
	switch state {
	case appstate.NotConfigured, appstate.NeedsPassphrase:
		return iconGrayICO
	case appstate.Connecting, appstate.Scanning, appstate.Syncing:
		return iconBlueICO
	case appstate.Idle:
		return iconGreenICO
	case appstate.NeedsOAuth, appstate.Error, appstate.Disconnected:
		return iconRedICO
	default:
		return iconGrayICO
	}
}

// tooltipForState returns the tray tooltip text for the given state.
func tooltipForState(state appstate.State) string {
	switch state {
	case appstate.NotConfigured:
		return "gcrypt — Not Configured"
	case appstate.NeedsPassphrase:
		return "gcrypt — Enter Passphrase"
	case appstate.Connecting:
		return "gcrypt — Connecting..."
	case appstate.NeedsOAuth:
		return "gcrypt — Sign In Required"
	case appstate.Scanning:
		return "gcrypt — Scanning..."
	case appstate.Syncing:
		return "gcrypt — Syncing"
	case appstate.Idle:
		return "gcrypt — Connected"
	case appstate.Error:
		return "gcrypt — Error"
	case appstate.Disconnected:
		return "gcrypt — Disconnected"
	default:
		return "gcrypt"
	}
}

// statusTextForState returns the status menu item text for the given state.
func statusTextForState(state appstate.State) string {
	switch state {
	case appstate.NotConfigured:
		return "📊 Status: Not Configured"
	case appstate.NeedsPassphrase:
		return "📊 Status: Locked"
	case appstate.Connecting:
		return "📊 Status: Connecting..."
	case appstate.NeedsOAuth:
		return "📊 Status: Sign In Required"
	case appstate.Scanning:
		return "📊 Status: Scanning..."
	case appstate.Syncing:
		return "📊 Status: Syncing..."
	case appstate.Idle:
		return "📊 Status: Idle"
	case appstate.Error:
		return "📊 Status: Error"
	case appstate.Disconnected:
		return "📊 Status: Disconnected"
	default:
		return fmt.Sprintf("📊 Status: %s", state.String())
	}
}

// ---------------------------------------------------------------------------
// Sync detail updates (stats, per-pair state)
// ---------------------------------------------------------------------------

// updateSyncDetails refreshes the sync statistics and per-pair status labels.
// This is called periodically and on SyncManager state changes. It only
// updates details when the SyncManager is running; the high-level
// icon/tooltip/menu visibility is handled by updateUIForState.
func (t *TrayApp) updateSyncDetails() {
	t.mu.Lock()
	defer t.mu.Unlock()

	manager := t.ctrl.Manager()
	if manager == nil {
		return
	}

	aggState := manager.GetAggregatedState()
	state := aggState.OverallState

	// Aggregate stats across all pairs.
	var totalStats syncpkg.SyncStats
	for _, ps := range aggState.PairStatuses {
		totalStats.FilesUploaded += ps.Stats.FilesUploaded
		totalStats.FilesDownloaded += ps.Stats.FilesDownloaded
		totalStats.FilesDeleted += ps.Stats.FilesDeleted
		totalStats.BytesUploaded += ps.Stats.BytesUploaded
		totalStats.BytesDownloaded += ps.Stats.BytesDownloaded
		totalStats.Errors += ps.Stats.Errors
	}

	// Update stats display.
	statsText := fmt.Sprintf("📁 Files synced: %d | ↑%d ↓%d",
		totalStats.FilesUploaded+totalStats.FilesDownloaded,
		totalStats.FilesUploaded,
		totalStats.FilesDownloaded,
	)
	t.mStats.SetTitle(statsText)

	// Update live activity display (current file + pending count).
	var totalPending, totalActive int
	var current []string
	for _, ps := range aggState.PairStatuses {
		totalPending += ps.Activity.Pending
		totalActive += ps.Activity.Active
		current = append(current, ps.Activity.Current...)
	}
	var activityText string
	switch {
	case totalActive > 0 && len(current) > 0:
		if len(current) > 1 {
			activityText = fmt.Sprintf("⚡ %s (+%d more) · %d pending", current[0], len(current)-1, totalPending)
		} else {
			activityText = fmt.Sprintf("⚡ %s · %d pending", current[0], totalPending)
		}
	case totalPending > 0:
		activityText = fmt.Sprintf("⏳ %d pending", totalPending)
	default:
		activityText = "✓ Up to date"
	}
	t.mActivity.SetTitle(activityText)

	// Update Pause All / Resume All text.
	if t.isPaused {
		t.mPauseAll.SetTitle("▶️ Resume All")
		t.mPauseAll.SetTooltip("Resume all sync pairs")
	} else {
		t.mPauseAll.SetTitle("⏸️ Pause All")
		t.mPauseAll.SetTooltip("Pause all sync pairs")
	}

	// Update per-pair status labels.
	cfg := t.ctrl.Config()
	for _, ps := range aggState.PairStatuses {
		for _, slot := range t.pairSlots {
			if slot != nil && slot.pairID == ps.ID {
				icon := stateIconFromSyncState(ps.State)
				label := stateLabelFromSyncState(ps.State)
				pairName := pairDisplayNameFromConfig(ps.ID, cfg)
				slot.header.SetTitle(fmt.Sprintf("%s %s (%s)", icon, pairName, label))

				// Update Pause/Resume text for this pair.
				if ps.State == syncpkg.StatePaused {
					slot.pause.SetTitle("Resume")
				} else {
					slot.pause.SetTitle("Pause")
				}
				break
			}
		}
	}

	// Also update the tray icon based on the sync-level state, but only if
	// the AppController is in a sync-active state. The AppController's state
	// machine handles the high-level transitions; this provides finer-grained
	// icon updates (e.g. paused → yellow icon).
	acState := t.ctrl.State()
	switch acState {
	case appstate.Scanning, appstate.Syncing, appstate.Idle:
		// Override with sync-level icon if we're in an active sync state.
		switch state {
		case syncpkg.StateIdle:
			systray.SetIcon(iconGreenICO)
		case syncpkg.StateScanning, syncpkg.StateSyncing:
			systray.SetIcon(iconBlueICO)
		case syncpkg.StatePaused:
			systray.SetIcon(iconGrayICO)
		case syncpkg.StateError, syncpkg.StateDisconnected:
			systray.SetIcon(iconRedICO)
		}
	}
}

// ---------------------------------------------------------------------------
// Click handlers — state-driven items
// ---------------------------------------------------------------------------

// handleEnterPassphrase is called when the user clicks "Enter Passphrase".
// It delegates to the AppController which shows the native dialog and
// handles key derivation.
func (t *TrayApp) handleEnterPassphrase() {
	if t.logger != nil {
		t.logger.Info("User clicked Enter Passphrase", nil)
	}

	go func() {
		if err := t.ctrl.HandlePassphrase(); err != nil {
			if t.logger != nil {
				t.logger.Warn("HandlePassphrase failed", map[string]interface{}{
					"error": err.Error(),
				})
			}
		}
	}()
}

// handleSignIn is called when the user clicks "Sign In with Google".
// It delegates to the AppController which opens the browser for OAuth.
func (t *TrayApp) handleSignIn() {
	if t.logger != nil {
		t.logger.Info("User clicked Sign In with Google", nil)
	}

	go func() {
		if err := t.ctrl.HandleOAuth(); err != nil {
			if t.logger != nil {
				t.logger.Warn("HandleOAuth failed", map[string]interface{}{
					"error": err.Error(),
				})
			}
		}
	}()
}

// handleOpenSyncFolder opens the first sync pair's local directory in
// Windows Explorer.
func (t *TrayApp) handleOpenSyncFolder() {
	cfg := t.ctrl.Config()
	dir := cfg.SyncDir()
	if dir == "" {
		return
	}
	if err := exec.Command("explorer", dir).Start(); err != nil {
		fmt.Fprintf(os.Stderr, "service: open sync folder: %v\n", err)
	}
}

// ---------------------------------------------------------------------------
// Click handlers — top-level items
// ---------------------------------------------------------------------------

// handlePauseAllLoop handles the Pause All / Resume All toggle.
func (t *TrayApp) handlePauseAllLoop() {
	for range t.mPauseAll.ClickedCh {
		t.mu.Lock()
		manager := t.ctrl.Manager()
		if manager == nil {
			t.mu.Unlock()
			continue
		}

		if t.isPaused {
			// Resume all pairs.
			for _, ps := range manager.ListPairs() {
				_ = manager.ResumePair(ps.ID)
			}
			t.isPaused = false
		} else {
			// Pause all pairs.
			for _, ps := range manager.ListPairs() {
				_ = manager.PausePair(ps.ID)
			}
			t.isPaused = true
		}
		t.mu.Unlock()
		t.updateSyncDetails()
	}
}

// handleSyncAllNow triggers an immediate sync scan for all pairs.
func (t *TrayApp) handleSyncAllNow() {
	manager := t.ctrl.Manager()
	if manager == nil {
		return
	}
	for _, ps := range manager.ListPairs() {
		_ = manager.SyncNow(ps.ID)
	}
}

// handleViewLog opens the log file in Notepad.
func (t *TrayApp) handleViewLog() {
	logPath := ""
	if t.logger != nil {
		logPath = t.logger.Path()
	}
	if logPath == "" {
		cfg := t.ctrl.Config()
		if cfg != nil {
			logPath = cfg.LogPath()
		}
	}
	if logPath == "" {
		return
	}
	if err := exec.Command("notepad", logPath).Start(); err != nil {
		fmt.Fprintf(os.Stderr, "service: view log: %v\n", err)
	}
}

// handleQuit performs a graceful shutdown of the application.
func (t *TrayApp) handleQuit() {
	t.ctrl.Shutdown()
	systray.Quit()
}

// ---------------------------------------------------------------------------
// Click handlers — per-pair items
// ---------------------------------------------------------------------------

// handlePairPauseResume toggles pause/resume for a single sync pair.
func (t *TrayApp) handlePairPauseResume(pairID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	manager := t.ctrl.Manager()
	if manager == nil {
		return
	}

	for _, ps := range manager.ListPairs() {
		if ps.ID == pairID {
			if ps.State == syncpkg.StatePaused {
				_ = manager.ResumePair(pairID)
			} else {
				_ = manager.PausePair(pairID)
			}
			break
		}
	}
	t.updateSyncDetails()
}

// handleOpenPairFolder opens the sync pair's local directory in Explorer.
func (t *TrayApp) handleOpenPairFolder(pairID string) {
	cfg := t.ctrl.Config()
	if cfg == nil {
		return
	}
	pair := cfg.GetSyncPair(pairID)
	if pair == nil {
		return
	}
	if err := exec.Command("explorer", pair.LocalDir).Start(); err != nil {
		fmt.Fprintf(os.Stderr, "service: open pair folder: %v\n", err)
	}
}

// handlePairRemoveFirstClick is the first click on "Remove". It shows the
// "Confirm Remove?" item and starts a 5-second timer to reset.
func (t *TrayApp) handlePairRemoveFirstClick(pairID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	slot := t.findSlot(pairID)
	if slot == nil {
		return
	}

	// Hide the original Remove item and show the confirmation item.
	slot.remove.Hide()
	slot.removeConfirm.Show()

	// Cancel any existing timer.
	if slot.removeTimer != nil {
		slot.removeTimer.Stop()
	}

	// After 5 seconds, reset back to the normal Remove item.
	pairIDCopy := pairID // Capture for closure.
	slot.removeTimer = time.AfterFunc(5*time.Second, func() {
		t.mu.Lock()
		defer t.mu.Unlock()

		if s := t.findSlot(pairIDCopy); s != nil {
			s.removeConfirm.Hide()
			s.remove.Show()
			s.removeTimer = nil
		}
	})
}

// handlePairRemoveConfirm is the second click on "Confirm Remove?". It
// actually removes the sync pair.
func (t *TrayApp) handlePairRemoveConfirm(pairID string) {
	t.mu.Lock()
	// Stop the timer.
	if slot := t.findSlot(pairID); slot != nil && slot.removeTimer != nil {
		slot.removeTimer.Stop()
		slot.removeTimer = nil
	}
	t.mu.Unlock()

	manager := t.ctrl.Manager()

	// Remove the pair from the manager.
	if manager != nil {
		_ = manager.RemovePair(pairID)
	}

	// Remove from config and save.
	cfg := t.ctrl.Config()
	if cfg != nil {
		cfg.RemoveSyncPair(pairID)
		config.Save(config.ConfigPath(), cfg)
	}

	// Refresh the pair slots to reflect the removal.
	t.mu.Lock()
	t.refreshPairSlots()
	t.mu.Unlock()
}

// handleSelectedFolders opens a dialog to select which folders to sync.
// TODO: implement folder selection UI.
func (t *TrayApp) handleSelectedFolders(pairID string) {
	fmt.Fprintf(os.Stderr, "service: handleSelectedFolders not yet implemented for pair %s\n", pairID)
}

// handleForwardOnly toggles forward-only mode for a sync pair.
// TODO: implement forward-only toggle in manager/config.
func (t *TrayApp) handleForwardOnly(pairID string) {
	fmt.Fprintf(os.Stderr, "service: handleForwardOnly not yet implemented for pair %s\n", pairID)
}

// findSlot returns the pairSlot for the given pairID, or nil.
func (t *TrayApp) findSlot(pairID string) *pairSlot {
	for _, slot := range t.pairSlots {
		if slot != nil && slot.pairID == pairID {
			return slot
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Sync Pairs submenu construction
// ---------------------------------------------------------------------------

// buildSyncPairsSubmenu creates the "Sync Pairs" submenu with pre-allocated
// pair slots and the "Add Sync Pair" entry.
func (t *TrayApp) buildSyncPairsSubmenu() {
	t.mSyncPairs = systray.AddMenuItem("📂 Sync Pairs", "Configured sync pairs")

	// Placeholder for when there are no pairs.
	t.mNoPairs = t.mSyncPairs.AddSubMenuItem("(No sync pairs configured)", "No sync pairs")

	// Pre-allocate pair slots.
	for i := 0; i < maxPairSlots; i++ {
		slot := &pairSlot{
			header:          t.mSyncPairs.AddSubMenuItem("", ""),
			pause:           t.mSyncPairs.AddSubMenuItem("Pause", ""),
			syncNow:         t.mSyncPairs.AddSubMenuItem("Sync Now", ""),
			openDir:         t.mSyncPairs.AddSubMenuItem("Open Folder", ""),
			sep:             t.mSyncPairs.AddSubMenuItem("", ""), // acts as separator
			remove:          t.mSyncPairs.AddSubMenuItem("Remove", ""),
			removeConfirm:   t.mSyncPairs.AddSubMenuItem("⚠️ Confirm Remove?", "Click again to confirm removal"),
			selectedFolders: t.mSyncPairs.AddSubMenuItem("📁 Select Folders...", "Select which folders to sync"),
			forwardOnly:     t.mSyncPairs.AddSubMenuItem("📤 Forward Only", "Only upload files, never download"),
		}
		// Hide all slot items initially.
		slot.header.Disable()
		slot.header.Hide()
		slot.pause.Hide()
		slot.syncNow.Hide()
		slot.openDir.Hide()
		slot.sep.Hide()
		slot.remove.Hide()
		slot.removeConfirm.Hide()
		slot.selectedFolders.Hide()
		slot.forwardOnly.Hide()

		t.pairSlots[i] = slot
	}

	// Add Sync Pair item (always visible within the submenu).
	t.mSyncPairs.AddSubMenuItem("──────────", "").Disable() // visual separator
	t.mAddPair = t.mSyncPairs.AddSubMenuItem("➕ Add Sync Pair...", "Launch setup wizard to add a new sync pair")

	// Populate existing pairs into slots.
	t.refreshPairSlots()
}

// refreshPairSlots updates the pre-allocated pair slots to reflect the
// current config. It shows slots for existing pairs and hides unused ones.
func (t *TrayApp) refreshPairSlots() {
	cfg := t.ctrl.Config()
	if cfg == nil {
		// No config — hide all slots.
		t.mNoPairs.Show()
		t.mNoPairs.Disable()
		for i := 0; i < maxPairSlots; i++ {
			slot := t.pairSlots[i]
			slot.pairID = ""
			slot.header.Hide()
			slot.pause.Hide()
			slot.syncNow.Hide()
			slot.openDir.Hide()
			slot.sep.Hide()
			slot.remove.Hide()
			slot.removeConfirm.Hide()
			slot.selectedFolders.Hide()
			slot.forwardOnly.Hide()
			if slot.removeTimer != nil {
				slot.removeTimer.Stop()
				slot.removeTimer = nil
			}
		}
		return
	}

	pairs := cfg.SyncPairs
	manager := t.ctrl.Manager()

	// Show/hide the "no pairs" placeholder.
	if len(pairs) == 0 {
		t.mNoPairs.Show()
		t.mNoPairs.Disable()
	} else {
		t.mNoPairs.Hide()
	}

	// Populate slots.
	for i := 0; i < maxPairSlots; i++ {
		slot := t.pairSlots[i]
		if i < len(pairs) {
			pair := &pairs[i]
			slot.pairID = pair.ID

			// Determine current state for the header label.
			stateIcon := stateIconForPair(pair.ID, manager)
			pairName := pairDisplayName(pair)
			stateLabel := stateLabelForPair(pair.ID, manager)
			headerLabel := fmt.Sprintf("%s %s (%s)", stateIcon, pairName, stateLabel)

			slot.header.SetTitle(headerLabel)
			slot.header.SetTooltip(pair.LocalDir)
			slot.header.Disable() // Status display is always disabled.
			slot.header.Show()

			// Update Pause/Resume label.
			if isPairPaused(pair.ID, manager) {
				slot.pause.SetTitle("Resume")
			} else {
				slot.pause.SetTitle("Pause")
			}
			slot.pause.Show()

			slot.syncNow.Show()
			slot.openDir.Show()
			slot.sep.Show()
			slot.sep.Disable() // Separator-like item
			slot.remove.SetTitle("Remove")
			slot.remove.Show()
			slot.removeConfirm.Hide()
			slot.selectedFolders.Hide()
			slot.forwardOnly.Hide()

			// Cancel any pending remove timer.
			if slot.removeTimer != nil {
				slot.removeTimer.Stop()
				slot.removeTimer = nil
			}
		} else {
			// Hide unused slot.
			slot.pairID = ""
			slot.header.Hide()
			slot.pause.Hide()
			slot.syncNow.Hide()
			slot.openDir.Hide()
			slot.sep.Hide()
			slot.remove.Hide()
			slot.removeConfirm.Hide()
			slot.selectedFolders.Hide()
			slot.forwardOnly.Hide()

			if slot.removeTimer != nil {
				slot.removeTimer.Stop()
				slot.removeTimer = nil
			}
		}
	}
}

// startPairHandlers launches click handler goroutines for a pair slot.
func (t *TrayApp) startPairHandlers(slot *pairSlot) {
	go func() {
		for range slot.pause.ClickedCh {
			if slot.pairID == "" {
				return
			}
			t.handlePairPauseResume(slot.pairID)
		}
	}()

	go func() {
		for range slot.syncNow.ClickedCh {
			if slot.pairID == "" {
				return
			}
			manager := t.ctrl.Manager()
			if manager != nil {
				_ = manager.SyncNow(slot.pairID)
			}
		}
	}()

	go func() {
		for range slot.openDir.ClickedCh {
			if slot.pairID == "" {
				return
			}
			t.handleOpenPairFolder(slot.pairID)
		}
	}()

	go func() {
		for range slot.selectedFolders.ClickedCh {
			if slot.pairID == "" {
				return
			}
			t.handleSelectedFolders(slot.pairID)
		}
	}()

	go func() {
		for range slot.forwardOnly.ClickedCh {
			if slot.pairID == "" {
				return
			}
			t.handleForwardOnly(slot.pairID)
		}
	}()

	go func() {
		for range slot.remove.ClickedCh {
			if slot.pairID == "" {
				return
			}
			t.handlePairRemoveFirstClick(slot.pairID)
		}
	}()

	go func() {
		for range slot.removeConfirm.ClickedCh {
			if slot.pairID == "" {
				return
			}
			t.handlePairRemoveConfirm(slot.pairID)
		}
	}()
}

// ---------------------------------------------------------------------------
// Settings submenu construction
// ---------------------------------------------------------------------------

// buildSettingsSubmenu creates the "Settings" submenu with General, Sync, and
// Logging submenus containing checkable and radio-style items.
func (t *TrayApp) buildSettingsSubmenu() {
	t.mSettings = systray.AddMenuItem("⚙️ Settings", "Application settings")

	cfg := t.ctrl.Config()
	if cfg == nil {
		// No config available — add a placeholder and return.
		placeholder := t.mSettings.AddSubMenuItem("(No configuration loaded)", "")
		placeholder.Disable()
		return
	}

	// --- General ---------------------------------------------------------

	mGeneral := t.mSettings.AddSubMenuItem("🖥️ General", "General settings")

	t.mAutoStart = mGeneral.AddSubMenuItemCheckbox("Auto Start", "Start gcrypt on Windows boot", cfg.App.AutoStart)
	t.mStartMinimized = mGeneral.AddSubMenuItemCheckbox("Start Minimized", "Start gcrypt without showing a console window", cfg.App.StartMinimized)
	t.mRememberPass = mGeneral.AddSubMenuItemCheckbox("Remember Passphrase", "Securely store your passphrase (Windows DPAPI) to auto-unlock on startup", cfg.App.RememberPassphrase)

	// --- Sync ------------------------------------------------------------

	mSync := t.mSettings.AddSubMenuItem("🔄 Sync", "Sync settings")

	// Sync Interval radio items
	mInterval := mSync.AddSubMenuItem("Sync Interval", "Default sync interval")

	intervalSecs := []int{10, 30, 60, 300, 900, 1800}
	intervalLabels := []string{"10 seconds", "30 seconds", "1 minute", "5 minutes", "15 minutes", "30 minutes"}
	intervalItems := make([]*systray.MenuItem, len(intervalSecs))
	intervalValues := make([]interface{}, len(intervalSecs))
	currentInterval := t.currentSyncInterval()
	currentIntervalIdx := closestIndex(intervalSecs, currentInterval)
	for i, label := range intervalLabels {
		intervalItems[i] = mInterval.AddSubMenuItemCheckbox(label, label, i == currentIntervalIdx)
		intervalValues[i] = intervalSecs[i]
	}
	t.syncIntervalGroup = newRadioGroup(intervalItems, intervalValues, currentIntervalIdx, func(val interface{}) {
		t.handleSyncInterval(val.(int))
	})

	// Max File Size radio items
	mMaxSize := mSync.AddSubMenuItem("Max File Size", "Maximum file size to sync")

	maxSizeMiB := []int64{0, 50, 100, 500, 1024, 2048} // 0 = unlimited
	maxSizeLabels := []string{"Unlimited", "50 MB", "100 MB", "500 MB", "1 GB", "2 GB"}
	maxSizeItems := make([]*systray.MenuItem, len(maxSizeMiB))
	maxSizeValues := make([]interface{}, len(maxSizeMiB))
	currentMaxSize := cfg.App.MaxFileSize / (1024 * 1024) // Convert bytes to MiB
	currentMaxSizeIdx := closestIndexInt64(maxSizeMiB, currentMaxSize)
	for i, label := range maxSizeLabels {
		maxSizeItems[i] = mMaxSize.AddSubMenuItemCheckbox(label, label, i == currentMaxSizeIdx)
		maxSizeValues[i] = maxSizeMiB[i]
	}
	t.maxFileSizeGroup = newRadioGroup(maxSizeItems, maxSizeValues, currentMaxSizeIdx, func(val interface{}) {
		t.handleMaxFileSize(val.(int64))
	})

	// --- Bandwidth -------------------------------------------------------

	mBandwidth := t.mSettings.AddSubMenuItem("📶 Bandwidth", "Upload/download speed limits")

	bwKBps := []int{0, 128, 256, 512, 1024, 5120} // 0 = unlimited
	bwLabels := []string{"Unlimited", "128 KB/s", "256 KB/s", "512 KB/s", "1 MB/s", "5 MB/s"}

	// Upload limit
	mUpLimit := mBandwidth.AddSubMenuItem("Upload Limit", "Maximum upload speed")
	upItems := make([]*systray.MenuItem, len(bwKBps))
	upValues := make([]interface{}, len(bwKBps))
	currentUpIdx := closestIndex(bwKBps, cfg.App.RateLimitUpKBps)
	for i, label := range bwLabels {
		upItems[i] = mUpLimit.AddSubMenuItemCheckbox(label, label, i == currentUpIdx)
		upValues[i] = bwKBps[i]
	}
	t.uploadLimitGroup = newRadioGroup(upItems, upValues, currentUpIdx, func(val interface{}) {
		t.handleUploadLimit(val.(int))
	})

	// Download limit
	mDownLimit := mBandwidth.AddSubMenuItem("Download Limit", "Maximum download speed")
	downItems := make([]*systray.MenuItem, len(bwKBps))
	downValues := make([]interface{}, len(bwKBps))
	currentDownIdx := closestIndex(bwKBps, cfg.App.RateLimitDownKBps)
	for i, label := range bwLabels {
		downItems[i] = mDownLimit.AddSubMenuItemCheckbox(label, label, i == currentDownIdx)
		downValues[i] = bwKBps[i]
	}
	t.downloadLimitGroup = newRadioGroup(downItems, downValues, currentDownIdx, func(val interface{}) {
		t.handleDownloadLimit(val.(int))
	})

	// --- Logging ---------------------------------------------------------

	mLogging := t.mSettings.AddSubMenuItem("📝 Logging", "Logging settings")

	// Log Level radio items
	mLogLevel := mLogging.AddSubMenuItem("Log Level", "Minimum log level")

	logLevels := []string{"debug", "info", "warn", "error"}
	logLevelLabels := []string{"Debug", "Info", "Warn", "Error"}
	logLevelItems := make([]*systray.MenuItem, len(logLevels))
	logLevelValues := make([]interface{}, len(logLevels))
	currentLogLevelIdx := indexOfString(logLevels, cfg.App.LogLevel)
	for i, label := range logLevelLabels {
		logLevelItems[i] = mLogLevel.AddSubMenuItemCheckbox(label, label, i == currentLogLevelIdx)
		logLevelValues[i] = logLevels[i]
	}
	t.logLevelGroup = newRadioGroup(logLevelItems, logLevelValues, currentLogLevelIdx, func(val interface{}) {
		t.handleLogLevel(val.(string))
	})

	// Log Size radio items
	mLogSize := mLogging.AddSubMenuItem("Log Size", "Maximum log file size before rotation")

	logSizes := []int{5, 10, 25, 50} // MiB
	logSizeLabels := []string{"5 MB", "10 MB", "25 MB", "50 MB"}
	logSizeItems := make([]*systray.MenuItem, len(logSizes))
	logSizeValues := make([]interface{}, len(logSizes))
	currentLogSizeIdx := closestIndex(logSizes, cfg.App.LogMaxSize)
	for i, label := range logSizeLabels {
		logSizeItems[i] = mLogSize.AddSubMenuItemCheckbox(label, label, i == currentLogSizeIdx)
		logSizeValues[i] = logSizes[i]
	}
	t.logSizeGroup = newRadioGroup(logSizeItems, logSizeValues, currentLogSizeIdx, func(val interface{}) {
		t.handleLogMaxSize(val.(int))
	})

	// Log Backups radio items
	mLogBackups := mLogging.AddSubMenuItem("Log Backups", "Number of rotated log files to keep")

	logBackups := []int{1, 3, 5, 10}
	logBackupsLabels := []string{"1", "3", "5", "10"}
	logBackupsItems := make([]*systray.MenuItem, len(logBackups))
	logBackupsValues := make([]interface{}, len(logBackups))
	currentLogBackupsIdx := closestIndex(logBackups, cfg.App.LogMaxBackups)
	for i, label := range logBackupsLabels {
		logBackupsItems[i] = mLogBackups.AddSubMenuItemCheckbox(label, label, i == currentLogBackupsIdx)
		logBackupsValues[i] = logBackups[i]
	}
	t.logBackupsGroup = newRadioGroup(logBackupsItems, logBackupsValues, currentLogBackupsIdx, func(val interface{}) {
		t.handleLogMaxBackups(val.(int))
	})

	// Start radio group click handlers.
	t.startRadioGroupHandlers(t.syncIntervalGroup)
	t.startRadioGroupHandlers(t.maxFileSizeGroup)
	t.startRadioGroupHandlers(t.uploadLimitGroup)
	t.startRadioGroupHandlers(t.downloadLimitGroup)
	t.startRadioGroupHandlers(t.logLevelGroup)
	t.startRadioGroupHandlers(t.logSizeGroup)
	t.startRadioGroupHandlers(t.logBackupsGroup)
}

// startRadioGroupHandlers launches a goroutine for each item in a radio group
// that handles click events.
func (t *TrayApp) startRadioGroupHandlers(rg *radioGroup) {
	for i := range rg.items {
		idx := i
		go func() {
			for range rg.items[idx].ClickedCh {
				rg.selectItem(idx)
			}
		}()
	}
}

// ---------------------------------------------------------------------------
// Click handlers — Add Sync Pair
// ---------------------------------------------------------------------------

// handleAddPairLoop handles the "Add Sync Pair" click.
func (t *TrayApp) handleAddPairLoop() {
	for range t.mAddPair.ClickedCh {
		t.handleAddSyncPair()
	}
}

// handleRunSetupLoop handles clicks on the "Run Setup" item (NotConfigured).
func (t *TrayApp) handleRunSetupLoop() {
	for range t.mRunSetup.ClickedCh {
		t.runSetupFlow()
	}
}

// handleAddSyncPair runs the in-process, GUI-dialog-driven setup flow.
func (t *TrayApp) handleAddSyncPair() {
	t.runSetupFlow()
}

// runSetupFlow runs the native setup dialogs in a background goroutine (the
// dialogs and OAuth browser flow block), reloads config on success, and starts
// any newly configured sync pairs.
func (t *TrayApp) runSetupFlow() {
	go func() {
		if err := RunSetup(t.ctrl, t.logger); err != nil {
			if t.logger != nil {
				t.logger.Warn("setup did not complete", map[string]interface{}{"error": err.Error()})
			}
			return
		}

		// Start any newly configured pairs that aren't already running.
		t.mu.Lock()
		manager := t.ctrl.Manager()
		cfg := t.ctrl.Config()
		if manager != nil && cfg != nil {
			existing := manager.ListPairs()
			existingIDs := make(map[string]bool, len(existing))
			for _, ps := range existing {
				existingIDs[ps.ID] = true
			}
			for i := range cfg.SyncPairs {
				pair := &cfg.SyncPairs[i]
				if pair.Enabled && !existingIDs[pair.ID] {
					if err := manager.AddPair(pair); err != nil {
						fmt.Fprintf(os.Stderr, "service: start new pair %s: %v\n", pair.ID, err)
					}
				}
			}
		}
		t.refreshPairSlots()
		t.mu.Unlock()
	}()
}

// ---------------------------------------------------------------------------
// Click handlers — Settings
// ---------------------------------------------------------------------------

// handleAutoStartLoop handles the auto-start checkbox toggle.
func (t *TrayApp) handleAutoStartLoop() {
	for range t.mAutoStart.ClickedCh {
		t.mu.Lock()
		cfg := t.ctrl.Config()
		if cfg == nil {
			t.mu.Unlock()
			continue
		}
		cfg.App.AutoStart = !cfg.App.AutoStart
		if cfg.App.AutoStart {
			t.mAutoStart.Check()
			if err := EnableAutoStart(); err != nil {
				fmt.Fprintf(os.Stderr, "service: enable autostart: %v\n", err)
			}
		} else {
			t.mAutoStart.Uncheck()
			if err := DisableAutoStart(); err != nil {
				fmt.Fprintf(os.Stderr, "service: disable autostart: %v\n", err)
			}
		}
		config.Save(config.ConfigPath(), cfg)
		t.mu.Unlock()
	}
}

// handleRememberPassphraseLoop handles the "Remember Passphrase" checkbox
// toggle. Enabling stores the DPAPI-protected master key so the app can
// auto-unlock on startup; disabling deletes it. The persistence (config save +
// key store/clear) is handled by AppController.SetRememberPassphrase.
func (t *TrayApp) handleRememberPassphraseLoop() {
	for range t.mRememberPass.ClickedCh {
		t.mu.Lock()
		cfg := t.ctrl.Config()
		if cfg == nil {
			t.mu.Unlock()
			continue
		}
		enabled := !cfg.App.RememberPassphrase
		if enabled {
			t.mRememberPass.Check()
		} else {
			t.mRememberPass.Uncheck()
		}
		err := t.ctrl.SetRememberPassphrase(enabled)
		t.mu.Unlock()

		if err != nil {
			if t.logger != nil {
				t.logger.Warn("SetRememberPassphrase failed", map[string]interface{}{
					"error": err.Error(),
				})
			}
			// Revert the checkbox to reflect the unchanged state.
			t.mu.Lock()
			if enabled {
				t.mRememberPass.Uncheck()
			} else {
				t.mRememberPass.Check()
			}
			t.mu.Unlock()
		}
	}
}

// handleStartMinimizedLoop handles the start-minimized checkbox toggle.
func (t *TrayApp) handleStartMinimizedLoop() {
	for range t.mStartMinimized.ClickedCh {
		t.mu.Lock()
		cfg := t.ctrl.Config()
		if cfg == nil {
			t.mu.Unlock()
			continue
		}
		cfg.App.StartMinimized = !cfg.App.StartMinimized
		if cfg.App.StartMinimized {
			t.mStartMinimized.Check()
		} else {
			t.mStartMinimized.Uncheck()
		}
		config.Save(config.ConfigPath(), cfg)
		t.mu.Unlock()
	}
}

// handleSyncInterval updates the sync interval for all pairs.
func (t *TrayApp) handleSyncInterval(secs int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cfg := t.ctrl.Config()
	if cfg == nil {
		return
	}

	for i := range cfg.SyncPairs {
		cfg.SyncPairs[i].SyncInterval = secs
	}
	config.Save(config.ConfigPath(), cfg)
	// TODO: notify engines of interval change (could restart poller)
}

// handleMaxFileSize updates the max file size setting.
func (t *TrayApp) handleMaxFileSize(mib int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cfg := t.ctrl.Config()
	if cfg == nil {
		return
	}

	cfg.App.MaxFileSize = mib * 1024 * 1024 // MiB to bytes
	config.Save(config.ConfigPath(), cfg)
}

// handleUploadLimit updates the upload bandwidth limit (KB/s, 0 = unlimited)
// and applies it immediately.
func (t *TrayApp) handleUploadLimit(kbps int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cfg := t.ctrl.Config()
	down := 0
	if cfg != nil {
		down = cfg.App.RateLimitDownKBps
	}
	if err := t.ctrl.SetRateLimits(kbps, down); err != nil && t.logger != nil {
		t.logger.Warn("SetRateLimits failed", map[string]interface{}{"error": err.Error()})
	}
}

// handleDownloadLimit updates the download bandwidth limit (KB/s, 0 =
// unlimited) and applies it immediately.
func (t *TrayApp) handleDownloadLimit(kbps int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cfg := t.ctrl.Config()
	up := 0
	if cfg != nil {
		up = cfg.App.RateLimitUpKBps
	}
	if err := t.ctrl.SetRateLimits(up, kbps); err != nil && t.logger != nil {
		t.logger.Warn("SetRateLimits failed", map[string]interface{}{"error": err.Error()})
	}
}

// handleLogLevel updates the log level setting.
func (t *TrayApp) handleLogLevel(level string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cfg := t.ctrl.Config()
	if cfg == nil {
		return
	}

	cfg.App.LogLevel = level
	config.Save(config.ConfigPath(), cfg)
	// Note: runtime log level change would need a method on the logger.
	// This is a config-only change for now; it takes effect on restart.
}

// handleLogMaxSize updates the log max size setting and applies it at runtime.
func (t *TrayApp) handleLogMaxSize(mib int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cfg := t.ctrl.Config()
	if cfg == nil {
		return
	}

	cfg.App.LogMaxSize = mib
	if t.logger != nil {
		t.logger.SetMaxSize(int64(mib) * 1024 * 1024)
	}
	config.Save(config.ConfigPath(), cfg)
}

// handleLogMaxBackups updates the log backups setting and applies it at runtime.
func (t *TrayApp) handleLogMaxBackups(n int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cfg := t.ctrl.Config()
	if cfg == nil {
		return
	}

	cfg.App.LogMaxBackups = n
	if t.logger != nil {
		t.logger.SetMaxBackups(n)
	}
	config.Save(config.ConfigPath(), cfg)
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

// currentSyncInterval returns the effective sync interval from the config.
// It uses the first pair's interval if set, otherwise the default of 30.
func (t *TrayApp) currentSyncInterval() int {
	cfg := t.ctrl.Config()
	if cfg == nil {
		return 30
	}
	for _, pair := range cfg.SyncPairs {
		if pair.SyncInterval > 0 {
			return pair.SyncInterval
		}
	}
	return 30
}

// pairDisplayName returns a human-readable name for a sync pair. It uses the
// directory base name if no explicit name is set.
func pairDisplayName(pair *config.SyncPair) string {
	if pair.LocalDir != "" {
		return filepath.Base(pair.LocalDir)
	}
	return pair.ID[:8]
}

// pairDisplayNameFromConfig returns a display name for a pair by looking it
// up in the config by ID.
func pairDisplayNameFromConfig(pairID string, cfg *config.Config) string {
	if cfg == nil {
		return pairID[:8]
	}
	pair := cfg.GetSyncPair(pairID)
	if pair != nil {
		return pairDisplayName(pair)
	}
	return pairID[:8]
}

// stateIconForPair returns the emoji status icon for a sync pair based on
// its current state in the manager.
func stateIconForPair(pairID string, manager *syncpkg.SyncManager) string {
	if manager == nil {
		return "—"
	}
	for _, ps := range manager.ListPairs() {
		if ps.ID == pairID {
			return stateIconFromSyncState(ps.State)
		}
	}
	return "—"
}

// stateLabelForPair returns the human-readable status label for a sync pair.
func stateLabelForPair(pairID string, manager *syncpkg.SyncManager) string {
	if manager == nil {
		return "Unknown"
	}
	for _, ps := range manager.ListPairs() {
		if ps.ID == pairID {
			return stateLabelFromSyncState(ps.State)
		}
	}
	return "Unknown"
}

// isPairPaused returns true if the pair is currently paused.
func isPairPaused(pairID string, manager *syncpkg.SyncManager) bool {
	if manager == nil {
		return false
	}
	for _, ps := range manager.ListPairs() {
		if ps.ID == pairID {
			return ps.State == syncpkg.StatePaused
		}
	}
	return false
}

// stateIconFromSyncState maps a SyncState to an emoji prefix.
func stateIconFromSyncState(state syncpkg.SyncState) string {
	switch state {
	case syncpkg.StateIdle:
		return "🟢"
	case syncpkg.StateScanning, syncpkg.StateSyncing:
		return "🔵"
	case syncpkg.StatePaused:
		return "🟡"
	case syncpkg.StateError, syncpkg.StateDisconnected:
		return "🔴"
	default:
		return "⚪"
	}
}

// stateLabelFromSyncState maps a SyncState to a human-readable label.
func stateLabelFromSyncState(state syncpkg.SyncState) string {
	switch state {
	case syncpkg.StateIdle:
		return "Idle"
	case syncpkg.StateScanning:
		return "Scanning"
	case syncpkg.StateSyncing:
		return "Syncing"
	case syncpkg.StatePaused:
		return "Paused"
	case syncpkg.StateError:
		return "Error"
	case syncpkg.StateDisconnected:
		return "Disconnected"
	default:
		return string(state)
	}
}

// closestIndex returns the index in vals that is closest to target.
// Used for radio group initial selection.
func closestIndex(vals []int, target int) int {
	best := 0
	bestDiff := absInt(vals[0] - target)
	for i, v := range vals {
		diff := absInt(v - target)
		if diff < bestDiff {
			bestDiff = diff
			best = i
		}
	}
	return best
}

// closestIndexInt64 returns the index in vals that is closest to target.
func closestIndexInt64(vals []int64, target int64) int {
	best := 0
	bestDiff := absInt64(vals[0] - target)
	for i, v := range vals {
		diff := absInt64(v - target)
		if diff < bestDiff {
			bestDiff = diff
			best = i
		}
	}
	return best
}

// indexOfString returns the index of s in slice, or 0 if not found.
func indexOfString(slice []string, s string) int {
	for i, v := range slice {
		if v == s {
			return i
		}
	}
	return 0
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
