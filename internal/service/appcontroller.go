package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/arumes31/gcrypt/internal/appstate"
	"github.com/arumes31/gcrypt/internal/config"
	"github.com/arumes31/gcrypt/internal/crypto"
	"github.com/arumes31/gcrypt/internal/drive"
	"github.com/arumes31/gcrypt/internal/models"
	syncpkg "github.com/arumes31/gcrypt/internal/sync"

	"golang.org/x/oauth2"
)

// ---------------------------------------------------------------------------
// AppController — Central Orchestrator
// ---------------------------------------------------------------------------

// AppController owns the application lifecycle state machine and coordinates
// between the tray UI, authentication, and sync components. It replaces the
// blocking sequential startup in cmd/gcrypt/main.go with a non-blocking,
// state-driven approach where the tray appears immediately and the user
// provides credentials through the tray UI.
//
// State transitions follow the state machine defined in docs/STARTUP_REDESIGN.md:
//
//	NotConfigured → NeedsPassphrase  (setup wizard completed)
//	NeedsPassphrase → Connecting     (passphrase entered and verified)
//	NeedsPassphrase → NeedsPassphrase (wrong passphrase — retry)
//	Connecting → NeedsOAuth          (no stored token or token expired)
//	Connecting → Scanning             (token loaded successfully)
//	NeedsOAuth → Scanning             (OAuth flow completed)
//	NeedsOAuth → NeedsPassphrase      (OAuth cancelled or failed critically)
//	Scanning → Syncing                (initial scan done, changes detected)
//	Scanning → Idle                   (initial scan done, no changes)
//	Idle → Syncing                    (file change detected)
//	Idle → Scanning                   (SyncNow triggered)
//	Idle → Disconnected               (network error)
//	Syncing → Idle                    (all operations complete)
//	Syncing → Error                   (max retries exceeded)
//	Error → NeedsPassphrase           (auth error — re-auth required)
//	Error → Scanning                  (user clicks Retry)
//	Error → Idle                      (transient error resolved)
//	Disconnected → Scanning           (network restored + re-auth if needed)
//	Disconnected → NeedsOAuth         (token refresh failed)
//	Idle → NeedsPassphrase            (user locks app / signs out)
type AppController struct {
	mu          sync.Mutex
	state       appstate.State
	stateCh     chan appstate.State // broadcasts state changes to subscribers
	cfg         *config.Config
	logger      *Logger
	manager     *syncpkg.SyncManager // may be nil until sync is ready
	driveClient *drive.Client        // may be nil until auth is done
	masterKey   []byte               // may be nil until passphrase is entered
	tokenStore  *drive.Store         // may be nil
	salt        []byte               // loaded/generated during HandlePassphrase

	// Callbacks that the tray registers to update its UI
	OnStateChange func(oldState, newState appstate.State)

	// Error channel for async errors from sync engines
	asyncErrCh chan error

	// shutdownCh is closed during Shutdown to signal background goroutines.
	shutdownCh chan struct{}

	// oauthCfg holds the OAuth2 client credentials, resolved from environment
	// variables or config. It is populated lazily when OAuth is needed.
	oauthCfg drive.OAuthConfig
}

// ---------------------------------------------------------------------------
// Constructor
// ---------------------------------------------------------------------------

// NewAppController creates a new AppController and determines the initial
// lifecycle state based on the provided configuration.
//
// Initial state logic:
//   - If cfg is nil or ValidateForStartup() returns ErrNotConfigured → NotConfigured
//   - If cfg is nil or ValidateForStartup() returns ErrMissingFields → NotConfigured
//   - If config has no stored OAuth token → NeedsOAuth
//   - If config has no passphrase hash → NeedsPassphrase
//   - Otherwise → Idle (will transition to Scanning when sync starts)
func NewAppController(cfg *config.Config, logger *Logger) *AppController {
	ac := &AppController{
		state:      appstate.NotConfigured, // default, overridden below
		stateCh:    make(chan appstate.State, 16),
		cfg:        cfg,
		logger:     logger,
		asyncErrCh: make(chan error, 64),
		shutdownCh: make(chan struct{}),
	}

	// Resolve OAuth credentials from environment variables.
	ac.oauthCfg = drive.OAuthConfig{
		ClientID:     os.Getenv("GCRYPT_OAUTH_CLIENT_ID"),
		ClientSecret: os.Getenv("GCRYPT_OAUTH_CLIENT_SECRET"),
	}

	ac.setState(deriveStartupState(cfg))
	return ac
}

// deriveStartupState determines the initial lifecycle state from the config.
// The app is only considered configured (NeedsPassphrase) when it is valid for
// startup, has a passphrase hash, AND has usable OAuth credentials. Otherwise —
// including old/incomplete configs that predate the stored OAuth fields — it is
// NotConfigured, so the tray offers "Run Setup".
func deriveStartupState(cfg *config.Config) appstate.State {
	if cfg == nil {
		return appstate.NotConfigured
	}
	if err := cfg.ValidateForStartup(); err != nil {
		return appstate.NotConfigured
	}
	if cfg.PassphraseHash() == "" {
		return appstate.NotConfigured
	}
	if !hasOAuthCredentials(cfg) {
		return appstate.NotConfigured
	}
	return appstate.NeedsPassphrase
}

// hasOAuthCredentials reports whether usable OAuth client credentials are
// available, either persisted in the config or supplied via environment
// variables.
func hasOAuthCredentials(cfg *config.Config) bool {
	if cfg.OAuthClientID() != "" && cfg.OAuthClientSecretEnc() != "" {
		return true
	}
	if os.Getenv("GCRYPT_OAUTH_CLIENT_ID") != "" && os.Getenv("GCRYPT_OAUTH_CLIENT_SECRET") != "" {
		return true
	}
	return false
}

// ReloadConfig re-reads the configuration from disk (e.g. after the tray-driven
// setup flow writes it), swaps it into the controller, and recomputes the
// lifecycle state. Intended to be called after setup completes so the tray
// transitions out of NotConfigured.
func (ac *AppController) ReloadConfig() error {
	cfg, err := config.Load(config.ConfigPath())
	if err != nil {
		return fmt.Errorf("service: reloading config: %w", err)
	}
	ac.mu.Lock()
	ac.cfg = cfg
	ac.mu.Unlock()
	ac.applyRateLimits()
	ac.setState(deriveStartupState(cfg))
	return nil
}

// ---------------------------------------------------------------------------
// State Access
// ---------------------------------------------------------------------------

// State returns the current application lifecycle state (thread-safe).
func (ac *AppController) State() appstate.State {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return ac.state
}

// StateChan returns a read-only channel that receives state change
// notifications. The channel is buffered (cap 16) and sends are non-blocking;
// if the channel is full, older notifications may be dropped.
func (ac *AppController) StateChan() <-chan appstate.State {
	return ac.stateCh
}

// setState updates the controller's state, sends a notification on the state
// channel, and invokes the OnStateChange callback. It is a private method
// that must be called with ac.mu NOT held (it acquires the lock itself).
func (ac *AppController) setState(newState appstate.State) {
	ac.mu.Lock()
	oldState := ac.state
	ac.state = newState
	ac.mu.Unlock()

	// Non-blocking send on the state channel.
	select {
	case ac.stateCh <- newState:
	default:
		// Channel full — drop the notification to avoid blocking.
	}

	// Invoke the callback if registered.
	if ac.OnStateChange != nil {
		ac.OnStateChange(oldState, newState)
	}

	if ac.logger != nil {
		ac.logger.Info("AppController state change", map[string]interface{}{
			"old_state": oldState.String(),
			"new_state": newState.String(),
		})
	}
}

// ---------------------------------------------------------------------------
// Passphrase Handling
// ---------------------------------------------------------------------------

// HandlePassphrase is called when the user clicks "Enter Passphrase" in the
// tray. It shows the native passphrase dialog, derives the master key, and
// transitions the state accordingly.
//
// Flow:
//  1. Call PromptPassphrase(0) to show the native dialog
//  2. Load or generate the salt
//  3. Derive the master key using crypto.DeriveMasterKey
//  4. Verify the passphrase hash against the stored hash
//  5. If correct: store the master key, transition to Connecting
//  6. If wrong: return error, stay in NeedsPassphrase
//  7. Try to load an existing OAuth token; if found → Scanning, else → NeedsOAuth
func (ac *AppController) HandlePassphrase() error {
	// Show the native passphrase dialog.
	passphrase, err := PromptPassphrase(0)
	if err != nil {
		// User cancelled or dialog failed — stay in current state.
		if ac.logger != nil {
			ac.logger.Warn("Passphrase dialog cancelled or failed", map[string]interface{}{
				"error": err.Error(),
			})
		}
		return fmt.Errorf("service: passphrase dialog: %w", err)
	}
	if len(passphrase) == 0 {
		return fmt.Errorf("service: passphrase cannot be empty")
	}

	// Load or generate salt.
	appDir := appDataDir()
	saltPath := filepath.Join(appDir, "gcrypt", "salt.bin")

	var salt []byte
	saltData, err := os.ReadFile(saltPath) // #nosec G304 -- saltPath is the app's own salt.bin under %APPDATA%, not user input
	if err == nil {
		salt = saltData
		if len(salt) != 16 {
			return fmt.Errorf("service: salt file has wrong size (%d bytes, expected 16)", len(salt))
		}
	} else if os.IsNotExist(err) {
		salt, err = crypto.GenerateSalt()
		if err != nil {
			return fmt.Errorf("service: generating salt: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(saltPath), 0750); err != nil {
			return fmt.Errorf("service: creating salt directory: %w", err)
		}

		if err := os.WriteFile(saltPath, salt, 0600); err != nil {
			return fmt.Errorf("service: saving salt: %w", err)
		}
	} else {
		return fmt.Errorf("service: reading salt file: %w", err)
	}

	// Derive master key.
	masterKey, err := crypto.DeriveMasterKey(passphrase, salt)
	if err != nil {
		return fmt.Errorf("service: deriving master key: %w", err)
	}

	// Verify passphrase against stored hash.
	computedHash := crypto.HashPassphrase(passphrase, salt)
	if ac.cfg.PassphraseHash() != "" && computedHash != ac.cfg.PassphraseHash() {
		crypto.WipeBytes(masterKey)
		return fmt.Errorf("service: incorrect passphrase")
	}

	// If "remember passphrase" is enabled, persist the DPAPI-protected key so
	// future launches can auto-unlock without prompting. A failure here is
	// non-fatal: the user is still unlocked for this session.
	if ac.cfg != nil && ac.cfg.RememberPassphrase() {
		if err := saveRememberedKey(masterKey); err != nil && ac.logger != nil {
			ac.logger.Warn("failed to remember passphrase for auto-unlock", map[string]interface{}{
				"error": err.Error(),
			})
		}
	}

	return ac.activateMasterKey(masterKey, salt)
}

// activateMasterKey stores a verified master key on the controller, locks it in
// memory, transitions to Connecting, and either loads the existing OAuth token
// and starts syncing or transitions to NeedsOAuth. It is the shared tail of
// HandlePassphrase (after deriving/verifying) and TryAutoUnlock. It takes
// ownership of masterKey.
func (ac *AppController) activateMasterKey(masterKey, salt []byte) error {
	// Lock master key memory to prevent swapping to disk.
	if err := crypto.LockMemory(masterKey); err != nil {
		if ac.logger != nil {
			ac.logger.Warn("VirtualLock failed for master key", map[string]interface{}{
				"error": err.Error(),
			})
		}
	}

	// Store the master key and salt on the controller.
	ac.mu.Lock()
	if ac.masterKey != nil {
		crypto.WipeBytes(ac.masterKey)
		_ = crypto.UnlockMemory(ac.masterKey)
	}
	ac.masterKey = masterKey
	ac.salt = salt
	ac.mu.Unlock()

	if ac.logger != nil {
		ac.logger.Info("Passphrase verified, master key derived")
	}

	// Transition to Connecting.
	ac.setState(appstate.Connecting)

	// Try to load an existing OAuth token.
	tokenPath := drive.TokenPath()
	token, err := drive.LoadToken(tokenPath, masterKey)
	if err != nil {
		if ac.logger != nil {
			ac.logger.Info("OAuth token not found or invalid, transitioning to NeedsOAuth", map[string]interface{}{
				"error": err.Error(),
			})
		}
		// No valid token — user needs to sign in.
		ac.setState(appstate.NeedsOAuth)
		return nil
	}

	// Token loaded successfully — create the Drive client and start sync.
	if err := ac.createDriveClientAndSync(token); err != nil {
		if ac.logger != nil {
			ac.logger.Error("Failed to create Drive client after loading token", map[string]interface{}{
				"error": err.Error(),
			})
		}
		// If it's a network error, go to Disconnected; otherwise Error.
		if isNetworkError(err) {
			ac.setState(appstate.Disconnected)
		} else {
			ac.setState(appstate.Error)
		}
		return err
	}

	return nil
}

// TryAutoUnlock attempts a silent unlock using a previously remembered master
// key (DPAPI-protected). It is a no-op returning false unless the app is in
// NeedsPassphrase state, remember-passphrase is enabled, and a stored key
// exists and decrypts for the current Windows user. Intended to be called once
// at startup.
func (ac *AppController) TryAutoUnlock() bool {
	if ac.State() != appstate.NeedsPassphrase {
		return false
	}
	if ac.cfg == nil || !ac.cfg.RememberPassphrase() || !rememberedKeyExists() {
		return false
	}

	masterKey, err := loadRememberedKey()
	if err != nil {
		if ac.logger != nil {
			ac.logger.Warn("auto-unlock failed to load remembered key", map[string]interface{}{
				"error": err.Error(),
			})
		}
		return false
	}

	// Load the salt (kept alongside the key; needed for later crypto ops).
	saltPath := filepath.Join(appDataDir(), "gcrypt", "salt.bin")
	salt, err := os.ReadFile(saltPath) // #nosec G304 -- saltPath is the app's own salt.bin under %APPDATA%, not user input
	if err != nil {
		if ac.logger != nil {
			ac.logger.Warn("auto-unlock failed to read salt", map[string]interface{}{
				"error": err.Error(),
			})
		}
		crypto.WipeBytes(masterKey)
		return false
	}

	if ac.logger != nil {
		ac.logger.Info("Auto-unlocking with remembered passphrase")
	}

	// activateMasterKey sets an appropriate state on failure; either way the
	// auto-unlock attempt did fire, so report true.
	if err := ac.activateMasterKey(masterKey, salt); err != nil && ac.logger != nil {
		ac.logger.Warn("auto-unlock activation failed", map[string]interface{}{
			"error": err.Error(),
		})
	}
	return true
}

// SetRememberPassphrase enables or disables auto-unlock and persists the
// setting. When enabling, the current master key (if the app is already
// unlocked) is DPAPI-protected and stored immediately; otherwise it is stored
// on the next successful unlock. When disabling, any stored key is deleted.
func (ac *AppController) SetRememberPassphrase(enabled bool) error {
	if ac.cfg == nil {
		return fmt.Errorf("service: no config loaded")
	}

	ac.cfg.SetRememberPassphrase(enabled)
	if err := config.Save(config.ConfigPath(), ac.cfg); err != nil {
		return fmt.Errorf("service: saving config: %w", err)
	}

	if !enabled {
		return clearRememberedKey()
	}

	ac.mu.Lock()
	masterKey := ac.masterKey
	ac.mu.Unlock()
	if len(masterKey) > 0 {
		return saveRememberedKey(masterKey)
	}
	return nil
}

// applyRateLimits pushes the configured upload/download bandwidth limits into
// the global Drive throttle. Safe to call repeatedly.
func (ac *AppController) applyRateLimits() {
	if ac.cfg == nil {
		return
	}
	drive.SetUploadLimitKBps(ac.cfg.RateLimitUpKBps())
	drive.SetDownloadLimitKBps(ac.cfg.RateLimitDownKBps())
}

// SetRateLimits updates the upload/download bandwidth limits (KB/s, 0 =
// unlimited), persists them, and applies them immediately to in-flight and
// future transfers.
func (ac *AppController) SetRateLimits(upKBps, downKBps int) error {
	if ac.cfg == nil {
		return fmt.Errorf("service: no config loaded")
	}
	ac.cfg.SetRateLimits(upKBps, downKBps)
	if err := config.Save(config.ConfigPath(), ac.cfg); err != nil {
		return fmt.Errorf("service: saving config: %w", err)
	}
	ac.applyRateLimits()
	return nil
}

// ---------------------------------------------------------------------------
// OAuth Handling
// ---------------------------------------------------------------------------

// HandleOAuth is called when the user clicks "Sign In" in the tray.
// It transitions to Connecting, opens the browser for OAuth, stores the
// token, creates the Drive client, and transitions to the next state.
//
// Flow:
//  1. Transition to Connecting
//  2. Resolve OAuth credentials (env vars or prompt)
//  3. Call drive.GetTokenFromWebBrowser() to open browser for OAuth
//  4. Save the encrypted token
//  5. Create the Drive client
//  6. Transition to Idle (ready for sync) or NeedsPassphrase (if no master key)
func (ac *AppController) HandleOAuth() error {
	// Transition to Connecting.
	ac.setState(appstate.Connecting)

	// We need the master key to save the encrypted token.
	ac.mu.Lock()
	masterKey := ac.masterKey
	ac.mu.Unlock()

	if masterKey == nil {
		// Can't do OAuth without a master key — need passphrase first.
		if ac.logger != nil {
			ac.logger.Warn("OAuth requested but no master key available, transitioning to NeedsPassphrase")
		}
		ac.setState(appstate.NeedsPassphrase)
		return fmt.Errorf("service: passphrase required before OAuth")
	}

	// Resolve OAuth credentials.
	if err := ac.resolveOAuthCredentials(); err != nil {
		if ac.logger != nil {
			ac.logger.Error("OAuth credential resolution failed", map[string]interface{}{
				"error": err.Error(),
			})
		}
		ac.setState(appstate.NeedsOAuth)
		return fmt.Errorf("service: resolving OAuth credentials: %w", err)
	}

	// Create the oauth2.Config.
	oauth2Config, err := drive.NewOAuthConfig(ac.oauthCfg)
	if err != nil {
		if ac.logger != nil {
			ac.logger.Error("OAuth config creation failed", map[string]interface{}{
				"error": err.Error(),
			})
		}
		ac.setState(appstate.NeedsOAuth)
		return fmt.Errorf("service: creating OAuth config: %w", err)
	}

	// Perform the browser-based OAuth flow.
	if ac.logger != nil {
		ac.logger.Info("Starting OAuth browser authorization flow")
	}

	token, err := drive.GetTokenFromWebBrowser(oauth2Config)
	if err != nil {
		if ac.logger != nil {
			ac.logger.Error("OAuth authorization failed", map[string]interface{}{
				"error": err.Error(),
			})
		}
		// Check if it's a timeout (user didn't complete the flow).
		if strings.Contains(err.Error(), "timed out") {
			ac.setState(appstate.NeedsOAuth)
			return fmt.Errorf("service: OAuth flow timed out: %w", err)
		}
		ac.setState(appstate.NeedsOAuth)
		return fmt.Errorf("service: OAuth authorization: %w", err)
	}

	// Save the encrypted token.
	tokenPath := drive.TokenPath()
	if err := drive.SaveToken(tokenPath, token, masterKey); err != nil {
		ac.setState(appstate.Error)
		return fmt.Errorf("service: saving OAuth token: %w", err)
	}

	if ac.logger != nil {
		ac.logger.Info("OAuth token saved successfully")
	}

	// Create the Drive client and start sync.
	if err := ac.createDriveClientAndSync(token); err != nil {
		if ac.logger != nil {
			ac.logger.Error("Failed to create Drive client after OAuth", map[string]interface{}{
				"error": err.Error(),
			})
		}
		if isNetworkError(err) {
			ac.setState(appstate.Disconnected)
		} else {
			ac.setState(appstate.Error)
		}
		return err
	}

	return nil
}

// ---------------------------------------------------------------------------
// Sync Management
// ---------------------------------------------------------------------------

// StartSync creates the SyncManager, wires up error and state change handlers,
// and starts all sync engines asynchronously. It transitions to Scanning and
// listens for engine state changes to transition Scanning → Syncing → Idle.
//
// This method should be called after both passphrase and OAuth are complete.
func (ac *AppController) StartSync() error {
	ac.mu.Lock()
	cfg := ac.cfg
	driveClient := ac.driveClient
	masterKey := ac.masterKey
	logger := ac.logger
	// Tear down any previous sync session before starting a new one so we don't
	// leak the old manager's goroutines or the old metadata-store DB handle.
	// StartSync can run more than once per process — e.g. re-entering the
	// passphrase after an auth error, or re-running the OAuth flow.
	prevManager := ac.manager
	prevStore := ac.tokenStore
	ac.manager = nil
	ac.tokenStore = nil
	ac.mu.Unlock()

	if prevManager != nil {
		prevManager.StopAll()
	}
	if prevStore != nil {
		_ = prevStore.Close()
	}

	if cfg == nil {
		return fmt.Errorf("service: config is required to start sync")
	}
	if driveClient == nil {
		return fmt.Errorf("service: drive client is required to start sync")
	}
	if masterKey == nil {
		return fmt.Errorf("service: master key is required to start sync")
	}

	// Open the metadata store.
	appDir := appDataDir()
	dbPath := filepath.Join(appDir, "gcrypt", "gcrypt.db")

	// Use the first pair's ID for store initialization (backward compat).
	var defaultPairID string
	if len(cfg.SyncPairs) > 0 {
		defaultPairID = cfg.SyncPairs[0].ID
	}

	store, err := drive.NewStore(dbPath, defaultPairID)
	if err != nil {
		return fmt.Errorf("service: opening metadata store: %w", err)
	}

	ac.mu.Lock()
	ac.tokenStore = store
	ac.mu.Unlock()

	// Ensure the sync root exists in the database for each configured pair.
	for _, pair := range cfg.SyncPairs {
		root := &models.SyncRoot{
			ID:            pair.ID,
			LocalDir:      pair.LocalDir,
			DriveFolderID: pair.DriveFolderID,
		}
		if err := store.UpsertSyncRoot(context.Background(), root); err != nil {
			return fmt.Errorf("service: upsert sync root %s: %w", pair.ID, err)
		}
	}

	// Apply the configured bandwidth limits before any transfers begin.
	ac.applyRateLimits()

	// Create the SyncManager.
	manager := syncpkg.NewSyncManager(cfg, store, driveClient, masterKey, logger)

	// Wire up the manager's OnError channel to forward async errors.
	manager.OnError = make(chan error, 64)

	// Wire up the manager's OnStateChange callback to translate engine-level
	// state changes into AppController-level state transitions.
	manager.OnStateChange = func(oldState, newState appstate.State) {
		if ac.logger != nil {
			ac.logger.Info("SyncManager state change", map[string]interface{}{
				"old_state": oldState.String(),
				"new_state": newState.String(),
			})
		}

		// Map engine-level appstate changes to AppController state transitions.
		currentACState := ac.State()
		switch newState {
		case appstate.Scanning:
			if currentACState == appstate.Idle || currentACState == appstate.Syncing {
				ac.setState(appstate.Scanning)
			}
		case appstate.Syncing:
			if currentACState == appstate.Scanning || currentACState == appstate.Idle {
				ac.setState(appstate.Syncing)
			}
		case appstate.Idle:
			if currentACState == appstate.Scanning || currentACState == appstate.Syncing {
				ac.setState(appstate.Idle)
			}
		case appstate.Error:
			if currentACState == appstate.Scanning || currentACState == appstate.Syncing {
				ac.setState(appstate.Error)
			}
		case appstate.Disconnected:
			if currentACState != appstate.Disconnected {
				ac.setState(appstate.Disconnected)
			}
		}
	}

	ac.mu.Lock()
	ac.manager = manager
	ac.mu.Unlock()

	// Start the async error listener goroutine.
	go ac.listenAsyncErrors()

	// Transition to Scanning before starting engines.
	ac.setState(appstate.Scanning)

	// Start all sync engines asynchronously.
	if err := manager.StartAllAsync(); err != nil {
		if ac.logger != nil {
			ac.logger.Error("Failed to start sync manager", map[string]interface{}{
				"error": err.Error(),
			})
		}
		ac.setState(appstate.Error)
		return fmt.Errorf("service: starting sync manager: %w", err)
	}

	if ac.logger != nil {
		ac.logger.Info("Sync manager started, all engines running async")
	}

	return nil
}

// ---------------------------------------------------------------------------
// Error Handling
// ---------------------------------------------------------------------------

// HandleError processes an asynchronous error received from the sync engines
// or other background operations. It logs the error and transitions the
// controller to the appropriate state based on the error type.
//
//   - Network errors → Disconnected
//   - Auth errors → NeedsOAuth
//   - Critical errors → Error
func (ac *AppController) HandleError(err error) {
	if err == nil {
		return
	}

	if ac.logger != nil {
		ac.logger.Error("Async error received", map[string]interface{}{
			"error": err.Error(),
		})
	}

	// Classify the error and transition state.
	if isNetworkError(err) {
		currentState := ac.State()
		if currentState != appstate.Disconnected && currentState != appstate.NeedsPassphrase && currentState != appstate.NeedsOAuth {
			ac.setState(appstate.Disconnected)
		}
		return
	}

	if isAuthError(err) {
		currentState := ac.State()
		if currentState != appstate.NeedsOAuth && currentState != appstate.NeedsPassphrase {
			ac.setState(appstate.NeedsOAuth)
		}
		return
	}

	// For other errors, transition to Error state if we're in a sync-related state.
	currentState := ac.State()
	if currentState == appstate.Scanning || currentState == appstate.Syncing || currentState == appstate.Idle {
		ac.setState(appstate.Error)
	}
}

// ---------------------------------------------------------------------------
// Shutdown
// ---------------------------------------------------------------------------

// Shutdown performs a clean shutdown of the AppController: stops the sync
// manager if running, closes the metadata store, wipes the master key from
// memory, and closes channels.
func (ac *AppController) Shutdown() {
	if ac.logger != nil {
		ac.logger.Info("AppController shutting down")
	}

	// Signal background goroutines to stop.
	select {
	case <-ac.shutdownCh:
		// Already closed.
	default:
		close(ac.shutdownCh)
	}

	// Stop the sync manager if running.
	ac.mu.Lock()
	manager := ac.manager
	store := ac.tokenStore
	ac.mu.Unlock()

	if manager != nil {
		manager.StopAll()
		if ac.logger != nil {
			ac.logger.Info("Sync manager stopped")
		}
	}

	// Close the metadata store.
	if store != nil {
		if err := store.Close(); err != nil && ac.logger != nil {
			ac.logger.Warn("Error closing metadata store", map[string]interface{}{
				"error": err.Error(),
			})
		}
	}

	// Wipe and unlock the master key from memory.
	ac.mu.Lock()
	if ac.masterKey != nil {
		_ = crypto.UnlockMemory(ac.masterKey)
		crypto.WipeBytes(ac.masterKey)
		ac.masterKey = nil
	}
	ac.mu.Unlock()

	if ac.logger != nil {
		ac.logger.Info("AppController shutdown complete")
	}
}

// ---------------------------------------------------------------------------
// Accessors for Tray Integration
// ---------------------------------------------------------------------------

// Manager returns the SyncManager, which may be nil if sync has not been
// started yet. The tray should check for nil before calling any methods.
func (ac *AppController) Manager() *syncpkg.SyncManager {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return ac.manager
}

// Config returns the application configuration.
func (ac *AppController) Config() *config.Config {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return ac.cfg
}

// SetConfig replaces the controller's config reference. This is used by
// the tray after the setup wizard completes and the config is reloaded.
func (ac *AppController) SetConfig(cfg *config.Config) {
	ac.mu.Lock()
	ac.cfg = cfg
	ac.mu.Unlock()
}

// DriveClient returns the Drive client, which may be nil if auth has not
// been completed yet.
func (ac *AppController) DriveClient() *drive.Client {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return ac.driveClient
}

// MasterKey returns a copy of the master key, or nil if the passphrase has
// not been entered yet. The caller should not modify the returned slice.
func (ac *AppController) MasterKey() []byte {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if ac.masterKey == nil {
		return nil
	}
	// Return a copy to prevent external mutation.
	cp := make([]byte, len(ac.masterKey))
	copy(cp, ac.masterKey)
	return cp
}

// ---------------------------------------------------------------------------
// Internal Helpers
// ---------------------------------------------------------------------------

// createDriveClientAndSync creates the Drive client from the given token and
// starts the sync engines. This is the common path after either loading an
// existing token or completing a new OAuth flow.
func (ac *AppController) createDriveClientAndSync(token *oauth2.Token) error {
	// Resolve OAuth credentials if not already done.
	if err := ac.resolveOAuthCredentials(); err != nil {
		return fmt.Errorf("resolving OAuth credentials: %w", err)
	}

	// Create the Drive client with a 30-second timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	driveClient, err := drive.NewClient(ctx, ac.oauthCfg, token, ac.cfg.DriveFolderID())
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("creating Drive client: operation timed out after 30 seconds (check network connectivity and OAuth credentials)")
		}
		return fmt.Errorf("creating Drive client: %w", err)
	}

	ac.mu.Lock()
	ac.driveClient = driveClient
	ac.mu.Unlock()

	// Ensure remote folder exists.
	if ac.cfg.DriveFolderID() == "" {
		folderCtx, folderCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer folderCancel()

		folderID, err := driveClient.EnsureFolder(folderCtx, "gcrypt")
		if err != nil {
			if folderCtx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("ensuring remote folder: operation timed out after 30 seconds (check network connectivity)")
			}
			return fmt.Errorf("ensuring remote folder: %w", err)
		}
		ac.cfg.SetDriveFolderID(folderID)

		// Save updated config with the folder ID.
		cfgPath := config.ConfigPath()
		if err := config.Save(cfgPath, ac.cfg); err != nil && ac.logger != nil {
			ac.logger.Warn("Failed to save config with folder ID", map[string]interface{}{
				"error": err.Error(),
			})
		}
	}

	// Start sync.
	if err := ac.StartSync(); err != nil {
		return fmt.Errorf("starting sync: %w", err)
	}

	return nil
}

// resolveOAuthCredentials populates ac.oauthCfg from environment variables or
// the persisted config if not already set. Returns an error if credentials
// cannot be found.
func (ac *AppController) resolveOAuthCredentials() error {
	if ac.oauthCfg.ClientID != "" && ac.oauthCfg.ClientSecret != "" {
		return nil // Already resolved.
	}

	// Try environment variables first (they override the persisted config).
	clientID := os.Getenv("GCRYPT_OAUTH_CLIENT_ID")
	clientSecret := os.Getenv("GCRYPT_OAUTH_CLIENT_SECRET")

	if clientID != "" && clientSecret != "" {
		ac.oauthCfg = drive.OAuthConfig{
			ClientID:     clientID,
			ClientSecret: clientSecret,
		}
		return nil
	}

	// Fall back to the credentials persisted in the config during setup. The
	// client secret is stored encrypted, so we need the master key (available
	// after the passphrase has been entered) to decrypt it.
	if ac.cfg != nil && ac.cfg.OAuthClientID() != "" && ac.cfg.OAuthClientSecretEnc() != "" {
		ac.mu.Lock()
		masterKey := ac.masterKey
		ac.mu.Unlock()
		if len(masterKey) == 0 {
			return fmt.Errorf("service: passphrase required to decrypt OAuth client secret")
		}
		clientSecret, err := drive.DecryptClientSecret(ac.cfg.OAuthClientSecretEnc(), masterKey)
		if err == nil {
			ac.oauthCfg = drive.OAuthConfig{
				ClientID:     ac.cfg.OAuthClientID(),
				ClientSecret: clientSecret,
			}
			return nil
		}
		// The stored secret could not be decrypted. This happens for configs
		// written by older gcrypt versions (the encrypted format was bumped to v2
		// and old v1 blobs are not readable). Rather than dead-ending the sign-in
		// button, prompt the user to re-enter the secret and re-save it in the
		// current format, keeping the existing passphrase/master key intact.
		if ac.logger != nil {
			ac.logger.Warn("stored OAuth client secret could not be decrypted; prompting to re-enter", map[string]interface{}{
				"error": err.Error(),
			})
		}
		return ac.promptAndPersistOAuthCredentials()
	}

	// No usable stored credentials — prompt the user for them.
	return ac.promptAndPersistOAuthCredentials()
}

// promptAndPersistOAuthCredentials asks the user for their Google OAuth client
// ID and secret via native dialogs, sets them on the controller, and (re)saves
// them to the config encrypted with the current master key. It is used when no
// usable credentials are present — including when an older config's encrypted
// secret can no longer be decrypted — so the user can sign in without having to
// re-run full setup (which would create a new sync identity).
func (ac *AppController) promptAndPersistOAuthCredentials() error {
	ac.mu.Lock()
	masterKey := ac.masterKey
	ac.mu.Unlock()
	if len(masterKey) == 0 {
		return fmt.Errorf("service: passphrase required before entering OAuth credentials")
	}

	prefillID := ""
	if ac.cfg != nil {
		prefillID = ac.cfg.OAuthClientID()
	}

	clientID, ok := promptText("gcrypt — Google sign-in",
		"Enter your Google OAuth Client ID.", prefillID, false)
	if !ok || strings.TrimSpace(clientID) == "" {
		return fmt.Errorf("service: Google OAuth client ID is required to sign in")
	}
	clientSecret, ok := promptText("gcrypt — Google sign-in",
		"Enter your Google OAuth Client Secret.", "", true)
	if !ok || strings.TrimSpace(clientSecret) == "" {
		return fmt.Errorf("service: Google OAuth client secret is required to sign in")
	}
	clientID = strings.TrimSpace(clientID)
	clientSecret = strings.TrimSpace(clientSecret)

	ac.oauthCfg = drive.OAuthConfig{ClientID: clientID, ClientSecret: clientSecret}

	// Persist the credentials (re-encrypted in the current v2 format) so the user
	// only has to enter them once.
	if ac.cfg != nil {
		encSecret, err := drive.EncryptClientSecret(clientSecret, masterKey)
		if err != nil {
			if ac.logger != nil {
				ac.logger.Warn("failed to encrypt refreshed OAuth client secret", map[string]interface{}{"error": err.Error()})
			}
		} else {
			ac.cfg.SetOAuthClientID(clientID)
			ac.cfg.SetOAuthClientSecretEnc(encSecret)
			if err := config.Save(config.ConfigPath(), ac.cfg); err != nil && ac.logger != nil {
				ac.logger.Warn("failed to persist refreshed OAuth credentials", map[string]interface{}{"error": err.Error()})
			}
		}
	}
	return nil
}

// listenAsyncErrors reads errors from the SyncManager's OnError channel and
// forwards them to HandleError. This goroutine runs for the lifetime of the
// controller.
func (ac *AppController) listenAsyncErrors() {
	ac.mu.Lock()
	manager := ac.manager
	ac.mu.Unlock()

	if manager == nil || manager.OnError == nil {
		return
	}

	for {
		select {
		case <-ac.shutdownCh:
			return
		case err, ok := <-manager.OnError:
			if !ok {
				return
			}
			ac.HandleError(err)
		}
	}
}

// isNetworkError returns true if the error indicates a network connectivity
// problem (DNS failure, connection refused, timeout, etc.).
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}

	// Check for net.OpError (DNS, connection refused, etc.).
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	// Check for context deadline exceeded (timeout).
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// Check for common network error strings.
	msg := err.Error()
	if strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "network is unreachable") ||
		strings.Contains(msg, "temporary failure in name resolution") {
		return true
	}

	return false
}

// isAuthError returns true if the error indicates an authentication failure
// (expired token, invalid credentials, etc.).
func isAuthError(err error) bool {
	if err == nil {
		return false
	}

	msg := err.Error()
	// Check for common auth error patterns from the Google Drive API.
	if strings.Contains(msg, "401") ||
		strings.Contains(msg, "Unauthorized") ||
		strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "Token has been expired") ||
		strings.Contains(msg, "Access Token expired") {
		return true
	}

	return false
}

// appDataDir returns the Windows APPDATA directory, falling back to
// USERPROFILE\AppData\Roaming if APPDATA is not set.
func appDataDir() string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		appData = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
	}
	return appData
}
