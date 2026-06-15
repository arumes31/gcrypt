package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/arumes31/gcrypt/internal/config"
	"github.com/arumes31/gcrypt/internal/crypto"
	"github.com/arumes31/gcrypt/internal/drive"
	syncpkg "github.com/arumes31/gcrypt/internal/sync"
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
	// --- Step 0: new sync vs. connect to an existing one -----------------
	// An encrypted sync is bound to a master key derived from the passphrase
	// AND a random salt. The salt lives only on the machine that created the
	// sync, so a second PC must import it (and the Drive/OAuth identity) to
	// derive the same key — otherwise nothing in the cloud can be decrypted.
	if messageBox("gcrypt setup",
		"Is this the FIRST computer you are setting up gcrypt on?\n\n"+
			"• Yes — create a new encrypted sync.\n"+
			"• No — connect this PC to an existing gcrypt sync from another computer.",
		mbYesNo|mbIconInfo) == mbIDNo {
		return runConnectExistingSetup(ctrl, logger)
	}

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

// runConnectExistingSetup configures this PC to join an encrypted sync that was
// originally created on another computer. It imports the encryption salt and the
// Drive/OAuth identity from that machine's exported config.yaml + salt.bin so the
// same master key can be re-derived here, then performs a fresh OAuth sign-in
// (tokens are per-device) and points a local folder at the existing Drive folder.
//
// It must be called from a background goroutine: the dialogs and the OAuth
// browser flow block.
func runConnectExistingSetup(ctrl *AppController, logger *Logger) error {
	// --- Step 1: locate the exported identity files ----------------------
	messageBox("gcrypt setup — Connect to existing sync",
		"On the other PC, copy these two files from its  %APPDATA%\\gcrypt  folder to this PC\n"+
			"(for example via a USB stick or a temporary folder):\n\n"+
			"    • config.yaml\n    • salt.bin\n\n"+
			"Then select the folder where you placed them.",
		mbOK|mbIconInfo)

	srcDir, ok := pickFolder("Select the folder containing the copied config.yaml and salt.bin")
	if !ok || strings.TrimSpace(srcDir) == "" {
		return errSetupCancelled
	}

	// Read and validate the exported salt.
	importedSalt, err := os.ReadFile(filepath.Join(srcDir, "salt.bin"))
	if err != nil {
		messageBox("gcrypt setup",
			"Could not read salt.bin from the selected folder.\nMake sure you copied it from the other PC.",
			mbOK|mbIconError)
		return fmt.Errorf("service: reading imported salt: %w", err)
	}
	if len(importedSalt) != 16 {
		messageBox("gcrypt setup", "The salt.bin file is invalid (wrong size).", mbOK|mbIconError)
		return fmt.Errorf("service: imported salt has wrong size (%d bytes)", len(importedSalt))
	}

	// Read the exported config for the Drive folder, OAuth credentials, and the
	// passphrase verification hash.
	importedCfg, err := config.Load(filepath.Join(srcDir, "config.yaml"))
	if err != nil {
		messageBox("gcrypt setup", "Could not read config.yaml from the selected folder.", mbOK|mbIconError)
		return fmt.Errorf("service: reading imported config: %w", err)
	}
	driveFolderID := importedCfg.DriveFolderID()
	clientID := importedCfg.OAuthClientID()
	encSecret := importedCfg.OAuthClientSecretEnc()
	passphraseHash := importedCfg.PassphraseHash()
	if driveFolderID == "" || clientID == "" || encSecret == "" || passphraseHash == "" {
		messageBox("gcrypt setup",
			"The imported config.yaml is missing required fields\n(Drive folder, OAuth credentials, or passphrase hash).",
			mbOK|mbIconError)
		return fmt.Errorf("service: imported config missing required fields")
	}

	// --- Step 2: passphrase (must match the original) --------------------
	passphrase, ok := promptText("gcrypt setup — Passphrase",
		"Enter the SAME encryption passphrase you use on the other PC.", "", true)
	if !ok {
		return errSetupCancelled
	}
	if passphrase == "" {
		return fmt.Errorf("service: passphrase cannot be empty")
	}

	masterKey, err := crypto.DeriveMasterKey(passphrase, importedSalt)
	if err != nil {
		return fmt.Errorf("service: deriving master key: %w", err)
	}
	defer crypto.WipeBytes(masterKey)

	if crypto.HashPassphrase(passphrase, importedSalt) != passphraseHash {
		messageBox("gcrypt setup",
			"Incorrect passphrase for the imported identity.\nIt must match the passphrase from the other PC.",
			mbOK|mbIconError)
		return fmt.Errorf("service: incorrect passphrase")
	}

	// Recover the OAuth client secret from the imported (encrypted) config.
	clientSecret, err := drive.DecryptClientSecret(encSecret, masterKey)
	if err != nil {
		return fmt.Errorf("service: decrypting imported client secret: %w", err)
	}

	// --- Step 3: persist the imported salt locally -----------------------
	saltPath := filepath.Join(appDataDir(), "gcrypt", "salt.bin")
	if err := os.MkdirAll(filepath.Dir(saltPath), 0750); err != nil {
		return fmt.Errorf("service: creating salt dir: %w", err)
	}
	if err := os.WriteFile(saltPath, importedSalt, 0600); err != nil {
		return fmt.Errorf("service: saving salt: %w", err)
	}

	// --- Step 4: OAuth sign-in on this device ----------------------------
	oauthCfg := drive.OAuthConfig{ClientID: clientID, ClientSecret: clientSecret}
	oauth2Config, err := drive.NewOAuthConfig(oauthCfg)
	if err != nil {
		return fmt.Errorf("service: creating OAuth config: %w", err)
	}
	messageBox("gcrypt setup",
		"A browser window will open to authorize gcrypt with Google on THIS PC.\n"+
			"Sign in with the SAME Google account used on the other PC, then return here.",
		mbOK|mbIconInfo)
	token, err := drive.GetTokenFromWebBrowser(oauth2Config)
	if err != nil {
		messageBox("gcrypt setup", fmt.Sprintf("Authorization failed:\n%v", err), mbOK|mbIconError)
		return fmt.Errorf("service: OAuth: %w", err)
	}
	if err := drive.SaveToken(drive.TokenPath(), token, masterKey); err != nil {
		return fmt.Errorf("service: saving token: %w", err)
	}

	// --- Step 5: local sync folder on this PC ----------------------------
	syncDir, ok := pickFolder("Select the local folder on THIS PC to sync (it will be filled from the cloud)")
	if !ok || strings.TrimSpace(syncDir) == "" {
		syncDir = filepath.Join(os.Getenv("USERPROFILE"), "gcrypt")
	}
	if err := os.MkdirAll(syncDir, 0750); err != nil {
		messageBox("gcrypt setup", fmt.Sprintf("Could not create sync folder:\n%v", err), mbOK|mbIconError)
		return fmt.Errorf("service: creating sync dir: %w", err)
	}

	// --- Build + save config ---------------------------------------------
	// Reuse the imported encryption identity (passphrase hash + OAuth client)
	// and the existing Drive folder; only the local directory is machine-local.
	cfg := config.DefaultConfig()
	cfg.Encryption.PassphraseHash = passphraseHash
	cfg.SetOAuthClientID(clientID)
	cfg.SetOAuthClientSecretEnc(encSecret)
	cfg.App.LogPath = filepath.Join(appDataDir(), "gcrypt", "gcrypt.log")
	cfg.App.AutoStart = true
	cfg.AddSyncPair(syncDir, driveFolderID, syncpkg.DefaultIgnorePatterns(), 30)

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
		"Connected! Click the tray icon and enter your passphrase to unlock.\n"+
			"Your encrypted files will download from Google Drive into the selected folder.",
		mbOK|mbIconInfo)
	if logger != nil {
		logger.Info("Tray-driven connect-existing setup completed")
	}
	return nil
}
