// Package main is the entry point for the gcrypt encrypted Google Drive sync client.
//
// The startup flow is tray-first: the system tray icon appears immediately
// with a state-driven UI, and all user interactions — first-time setup,
// passphrase entry, and OAuth authorization — happen through the tray via
// native dialogs. No blocking operations run before the tray is visible.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/arumes31/gcrypt/internal/config"
	"github.com/arumes31/gcrypt/internal/service"
)

// Version is the application version, embedded at build time.
const Version = "0.1.0"

var (
	flagConfig  string
	flagSyncDir string
)

func init() {
	flag.StringVar(&flagConfig, "config", "", "Path to config file (default: %APPDATA%/gcrypt/config.yaml)")
	flag.StringVar(&flagSyncDir, "syncdir", "", "Override sync directory from config")
}

func main() {
	// Hide the stray console window for double-click launches (no-op when run
	// from a terminal or when built with -H=windowsgui).
	hideConsoleIfOwned()

	flag.Parse()

	// --- Tray-first startup sequence --------------------------------------

	// 1. Load config (soft validation — doesn't require auth or passphrase).
	//    If the config file doesn't exist, that's fine: the AppController
	//    starts in NotConfigured state and the tray shows "Run Setup".
	cfgPath := flagConfig
	if cfgPath == "" {
		cfgPath = config.ConfigPath()
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		if !errors.Is(err, config.ErrNotConfigured) && !os.IsNotExist(err) {
			// Config file exists but is malformed — this is a real error.
			fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
			os.Exit(1)
		}
		// Config file doesn't exist — cfg is nil, AppController will
		// set state to NotConfigured.
		cfg = nil
	}

	// Override sync directory if provided via flag.
	if flagSyncDir != "" && cfg != nil {
		cfg.SetSyncDir(flagSyncDir)
	}

	// 2. Create logger. Use the default log path if config is nil.
	logPath := ""
	if cfg != nil {
		logPath = cfg.LogPath()
	}
	if logPath == "" {
		logPath = defaultLogPath()
	}

	logger, err := service.NewLogger(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating logger: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = logger.Close() }()

	// Apply persisted logging settings (level + rotation) so the configured
	// values take effect from startup, not just after a runtime change.
	if cfg != nil {
		if cfg.App.LogLevel != "" {
			logger.SetLevel(cfg.App.LogLevel)
		}
		if cfg.App.LogMaxSize > 0 {
			logger.SetMaxSize(int64(cfg.App.LogMaxSize) * 1024 * 1024)
		}
		if cfg.App.LogMaxBackups >= 1 {
			logger.SetMaxBackups(cfg.App.LogMaxBackups)
		}
	}

	logger.Info("gcrypt starting", map[string]interface{}{
		"version": Version,
	})

	// 3. Create AppController (determines initial state from config).
	ctrl := service.NewAppController(cfg, logger)

	logger.Info("AppController created", map[string]interface{}{
		"initial_state": ctrl.State().String(),
	})

	// 4. Register autostart (if configured).
	if cfg != nil && cfg.AutoStart() {
		if err := service.EnableAutoStart(); err != nil {
			logger.Warn("failed to enable autostart", map[string]interface{}{
				"error": err.Error(),
			})
		}
	}

	// 5. Start the tray — this is the blocking main loop. All further user
	//    interaction (setup, passphrase, sign-in) is driven from the tray.
	trayApp := service.NewTrayApp(ctrl, logger)
	trayApp.Run()

	// After the tray quits, perform final cleanup.
	ctrl.Shutdown()

	logger.Info("gcrypt shutdown complete")
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

// defaultLogPath returns the default log file path when no config is available.
func defaultLogPath() string {
	return filepath.Join(appDataDir(), "gcrypt", "gcrypt.log")
}
