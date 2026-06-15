package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/daniel/gcrypt/internal/config"
	"github.com/daniel/gcrypt/internal/crypto"
	"github.com/daniel/gcrypt/internal/drive"
	syncpkg "github.com/daniel/gcrypt/internal/sync"
)

// errSetupCancelled indicates the user dismissed a setup dialog.
var errSetupCancelled = fmt.Errorf("service: setup cancelled by user")

// RunSetup runs the interactive, native-dialog-driven first-time setup (the
// in-tray replacement for the old CLI --setup wizard). It collects the sync
// folder, passphrase, Google OAuth credentials, and Drive folder, writes the
// configuration and encrypted secrets, then reloads the config into the
// controller so the tray leaves the NotConfigured state.
//
// It must be called from a background goroutine: the dialogs and the OAuth
// browser flow block.
func RunSetup(ctrl *AppController, logger *Logger) error {
	// --- Step 1: sync directory ------------------------------------------
	syncDir, ok := pickFolder("Select the local folder to sync with Google Drive")
	if !ok || strings.TrimSpace(syncDir) == "" {
		// Fall back to a sensible default if the picker was dismissed.
		syncDir = filepath.Join(os.Getenv("USERPROFILE"), "gcrypt")
	}
	if err := os.MkdirAll(syncDir, 0750); err != nil {
		messageBox("gcrypt setup", fmt.Sprintf("Could not create sync folder:\n%v", err), mbOK|mbIconError)
		return fmt.Errorf("service: creating sync dir: %w", err)
	}

	// --- Step 2: passphrase (set + confirm) ------------------------------
	var passphrase string
	for {
		p1, ok := promptText("gcrypt setup — Passphrase",
			"Create an encryption passphrase.\nThere is NO recovery if you lose it.", "", true)
		if !ok {
			return errSetupCancelled
		}
		if p1 == "" {
			messageBox("gcrypt setup", "Passphrase cannot be empty.", mbOK|mbIconWarning)
			continue
		}
		p2, ok := promptText("gcrypt setup — Confirm passphrase",
			"Re-enter your passphrase to confirm.", "", true)
		if !ok {
			return errSetupCancelled
		}
		if p1 != p2 {
			messageBox("gcrypt setup", "Passphrases do not match. Try again.", mbOK|mbIconWarning)
			continue
		}
		passphrase = p1
		break
	}

	// Salt + master key derivation.
	salt, err := crypto.GenerateSalt()
	if err != nil {
		return fmt.Errorf("service: generating salt: %w", err)
	}
	saltPath := filepath.Join(appDataDir(), "gcrypt", "salt.bin")
	if err := os.MkdirAll(filepath.Dir(saltPath), 0750); err != nil {
		return fmt.Errorf("service: creating salt dir: %w", err)
	}

	if err := os.WriteFile(saltPath, salt, 0600); err != nil {
		return fmt.Errorf("service: saving salt: %w", err)
	}

	masterKey, err := crypto.DeriveMasterKey(passphrase, salt)
	if err != nil {
		return fmt.Errorf("service: deriving master key: %w", err)
	}
	defer crypto.WipeBytes(masterKey)
	passphraseHash := crypto.HashPassphrase(passphrase, salt)

	// --- Step 3: Google OAuth client credentials -------------------------
	clientID, ok := promptText("gcrypt setup — Google OAuth",
		"Enter your Google OAuth Client ID.", "", false)
	if !ok || strings.TrimSpace(clientID) == "" {
		return errSetupCancelled
	}
	clientSecret, ok := promptText("gcrypt setup — Google OAuth",
		"Enter your Google OAuth Client Secret.", "", true)
	if !ok || strings.TrimSpace(clientSecret) == "" {
		return errSetupCancelled
	}
	clientID = strings.TrimSpace(clientID)
	clientSecret = strings.TrimSpace(clientSecret)

	oauthCfg := drive.OAuthConfig{ClientID: clientID, ClientSecret: clientSecret}
	oauth2Config, err := drive.NewOAuthConfig(oauthCfg)
	if err != nil {
		return fmt.Errorf("service: creating OAuth config: %w", err)
	}

	// --- Step 4: OAuth browser flow --------------------------------------
	messageBox("gcrypt setup",
		"A browser window will open to authorize gcrypt with Google.\nComplete sign-in, then return here.",
		mbOK|mbIconInfo)
	token, err := drive.GetTokenFromWebBrowser(oauth2Config)
	if err != nil {
		messageBox("gcrypt setup", fmt.Sprintf("Authorization failed:\n%v", err), mbOK|mbIconError)
		return fmt.Errorf("service: OAuth: %w", err)
	}

	// Persist the encrypted token and encrypted client secret.
	if err := drive.SaveToken(drive.TokenPath(), token, masterKey); err != nil {
		return fmt.Errorf("service: saving token: %w", err)
	}
	encSecret, err := drive.EncryptClientSecret(clientSecret, masterKey)
	if err != nil {
		return fmt.Errorf("service: encrypting client secret: %w", err)
	}

	// --- Step 5: Drive folder --------------------------------------------
	folderName, ok := promptText("gcrypt setup — Drive folder",
		"Name of the Google Drive folder to sync into.", "gcrypt", false)
	if !ok || strings.TrimSpace(folderName) == "" {
		folderName = "gcrypt"
	}
	driveClient, err := drive.NewClient(context.TODO(), oauthCfg, token, "root")
	if err != nil {
		return fmt.Errorf("service: creating Drive client: %w", err)
	}
	folderID, err := driveClient.EnsureFolder(context.TODO(), strings.TrimSpace(folderName))
	if err != nil {
		messageBox("gcrypt setup", fmt.Sprintf("Could not create Drive folder:\n%v", err), mbOK|mbIconError)
		return fmt.Errorf("service: ensure folder: %w", err)
	}

	// --- Build + save config ---------------------------------------------
	cfg := config.DefaultConfig()
	cfg.Encryption.PassphraseHash = passphraseHash
	cfg.SetOAuthClientID(clientID)
	cfg.SetOAuthClientSecretEnc(encSecret)
	cfg.App.LogPath = filepath.Join(appDataDir(), "gcrypt", "gcrypt.log")
	cfg.App.AutoStart = true
	cfg.AddSyncPair(syncDir, folderID, syncpkg.DefaultIgnorePatterns(), 30)

	if err := config.Save(config.ConfigPath(), cfg); err != nil {
		return fmt.Errorf("service: saving config: %w", err)
	}

	// --- Autostart preference --------------------------------------------
	if messageBox("gcrypt setup", "Start gcrypt automatically when you sign in to Windows?",
		mbYesNo|mbIconInfo) == mbIDYes {
		if err := EnableAutoStart(); err != nil && logger != nil {
			logger.Warn("enable autostart failed", map[string]interface{}{"error": err.Error()})
		}
		cfg.SetAutoStart(true)
	} else {
		cfg.SetAutoStart(false)
	}
	_ = config.Save(config.ConfigPath(), cfg)

	// Reload into the controller so the tray leaves NotConfigured.
	if err := ctrl.ReloadConfig(); err != nil {
		return fmt.Errorf("service: reload after setup: %w", err)
	}

	messageBox("gcrypt setup",
		"Setup complete! Click the tray icon and enter your passphrase to unlock.",
		mbOK|mbIconInfo)
	if logger != nil {
		logger.Info("Tray-driven setup completed")
	}
	return nil
}
