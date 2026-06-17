// Package config handles loading, saving, and validating gcrypt configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Sentinel errors for tray-first startup validation
// ---------------------------------------------------------------------------

// ErrNotConfigured is returned by ValidateForStartup when no config file
// exists at the expected path.
var ErrNotConfigured = errors.New("config: no configuration file found")

// ErrMissingFields is returned by ValidateForStartup when the config file
// exists but is missing required fields. The error message lists which
// fields are absent.
type ErrMissingFields struct {
	Fields []string
}

// Error implements the error interface for ErrMissingFields.
func (e *ErrMissingFields) Error() string {
	return fmt.Sprintf("config: missing required fields: %s", strings.Join(e.Fields, ", "))
}

// ---------------------------------------------------------------------------
// V1 Config (used only during migration)
// ---------------------------------------------------------------------------

// v1Config is the legacy flat configuration format.
type v1Config struct {
	SyncDir        string   `yaml:"sync_dir"`
	DriveFolderID  string   `yaml:"drive_folder_id"`
	PassphraseHash string   `yaml:"passphrase_hash"`
	IgnorePatterns []string `yaml:"ignore_patterns"`
	SyncInterval   int      `yaml:"sync_interval"`
	LogPath        string   `yaml:"log_path"`
	AutoStart      bool     `yaml:"auto_start"`
}

// ---------------------------------------------------------------------------
// V2 Config types
// ---------------------------------------------------------------------------

// SyncPair represents one local↔remote sync relationship.
type SyncPair struct {
	ID              string   `yaml:"id"`
	LocalDir        string   `yaml:"local_dir"`
	DriveFolderID   string   `yaml:"drive_folder_id"`
	Enabled         bool     `yaml:"enabled"`
	IgnorePatterns  []string `yaml:"ignore_patterns"`
	SyncInterval    int      `yaml:"sync_interval"`
	SelectedFolders []string `yaml:"selected_folders"`
	ForwardOnly     bool     `yaml:"forward_only"`
}

// EncryptionConfig holds encryption-related settings.
type EncryptionConfig struct {
	PassphraseHash string `yaml:"passphrase_hash"`
}

// OAuthConfig holds the Google OAuth client credentials. The client ID is not
// sensitive and is stored in plaintext. The client secret is encrypted with the
// passphrase-derived master key (base64-encoded ciphertext), mirroring how the
// OAuth token is protected at rest, so it can only be read after the user
// unlocks with their passphrase.
type OAuthConfig struct {
	ClientID        string `yaml:"client_id"`
	ClientSecretEnc string `yaml:"client_secret_enc"`
}

// AppConfig holds application-level settings.
type AppConfig struct {
	AutoStart          bool   `yaml:"auto_start"`
	RememberPassphrase bool   `yaml:"remember_passphrase"`
	LogLevel           string `yaml:"log_level"`
	LogPath            string `yaml:"log_path"`
	LogMaxSize         int    `yaml:"log_max_size"`
	LogMaxBackups      int    `yaml:"log_max_backups"`
	MaxFileSize        int64  `yaml:"max_file_size"`
	RateLimitUpKBps    int    `yaml:"rate_limit_up_kbps"`   // 0 = unlimited
	RateLimitDownKBps  int    `yaml:"rate_limit_down_kbps"` // 0 = unlimited
	UploadWorkers      int    `yaml:"upload_workers"`       // 0 = use default; concurrent uploads per pair
}

// Config is the top-level V2 configuration.
type Config struct {
	Version    int              `yaml:"version"`
	SyncPairs  []SyncPair       `yaml:"sync_pairs"`
	Encryption EncryptionConfig `yaml:"encryption"`
	OAuth      OAuthConfig      `yaml:"oauth"`
	App        AppConfig        `yaml:"app"`
}

// ---------------------------------------------------------------------------
// Default values
// ---------------------------------------------------------------------------

// DefaultIgnorePatterns returns the default ignore patterns list.
func DefaultIgnorePatterns() []string {
	return []string{"~$*", "*.tmp", "*.swp", ".DS_Store", "Thumbs.db", "desktop.ini"}
}

// DefaultConfig returns a Config populated with sensible defaults.
func DefaultConfig() *Config {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		appData = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
	}
	gcryptDir := filepath.Join(appData, "gcrypt")

	return &Config{
		Version:   2,
		SyncPairs: []SyncPair{},
		Encryption: EncryptionConfig{
			PassphraseHash: "",
		},
		App: AppConfig{
			AutoStart:      true,
			LogLevel:       "info",
			LogPath:        filepath.Join(gcryptDir, "gcrypt.log"),
			LogMaxSize:     10,
			LogMaxBackups:  3,
			MaxFileSize:    0,
		},
	}
}

// ---------------------------------------------------------------------------
// Helper methods on Config
// ---------------------------------------------------------------------------

// AddSyncPair adds a new sync pair with auto-generated UUID.
func (c *Config) AddSyncPair(localDir, driveFolderID string, ignorePatterns []string, syncInterval int) *SyncPair {
	pair := SyncPair{
		ID:             uuid.New().String(),
		LocalDir:       localDir,
		DriveFolderID:  driveFolderID,
		Enabled:        true,
		IgnorePatterns: ignorePatterns,
		SyncInterval:   syncInterval,
	}
	c.SyncPairs = append(c.SyncPairs, pair)
	return &c.SyncPairs[len(c.SyncPairs)-1]
}

// RemoveSyncPair removes a sync pair by ID, returns true if found.
func (c *Config) RemoveSyncPair(id string) bool {
	for i, pair := range c.SyncPairs {
		if pair.ID == id {
			c.SyncPairs = slices.Delete(c.SyncPairs, i, i+1)
			return true
		}
	}
	return false
}

// GetSyncPair returns a sync pair by ID, or nil.
func (c *Config) GetSyncPair(id string) *SyncPair {
	for i := range c.SyncPairs {
		if c.SyncPairs[i].ID == id {
			return &c.SyncPairs[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helper methods on SyncPair
// ---------------------------------------------------------------------------

// EffectiveSyncInterval returns the pair's interval if > 0, otherwise the default (30).
func (p *SyncPair) EffectiveSyncInterval() int {
	if p.SyncInterval > 0 {
		return p.SyncInterval
	}
	return 30
}

// EffectiveIgnorePatterns returns the pair's patterns if non-empty, otherwise the defaults.
func (p *SyncPair) EffectiveIgnorePatterns() []string {
	if len(p.IgnorePatterns) > 0 {
		return p.IgnorePatterns
	}
	return DefaultIgnorePatterns()
}

// ---------------------------------------------------------------------------
// V1 backward-compatible accessors
// These methods allow existing code that references the old flat struct fields
// to continue compiling. They delegate to the first SyncPair and nested V2
// fields. They will be removed once all callers are updated to the V2 API.
// ---------------------------------------------------------------------------

// SyncDir returns the first SyncPair's LocalDir, or "" if no pairs exist.
func (c *Config) SyncDir() string {
	if len(c.SyncPairs) > 0 {
		return c.SyncPairs[0].LocalDir
	}
	return ""
}

// SetSyncDir sets the first SyncPair's LocalDir, creating a pair if needed.
func (c *Config) SetSyncDir(v string) {
	if len(c.SyncPairs) == 0 {
		c.AddSyncPair(v, "", nil, 0)
		return
	}
	c.SyncPairs[0].LocalDir = v
}

// DriveFolderID returns the first SyncPair's DriveFolderID, or "" if no pairs exist.
func (c *Config) DriveFolderID() string {
	if len(c.SyncPairs) > 0 {
		return c.SyncPairs[0].DriveFolderID
	}
	return ""
}

// SetDriveFolderID sets the first SyncPair's DriveFolderID.
func (c *Config) SetDriveFolderID(v string) {
	if len(c.SyncPairs) > 0 {
		c.SyncPairs[0].DriveFolderID = v
	}
}

// IgnorePatterns returns the first SyncPair's EffectiveIgnorePatterns, or defaults.
func (c *Config) IgnorePatterns() []string {
	if len(c.SyncPairs) > 0 {
		return c.SyncPairs[0].EffectiveIgnorePatterns()
	}
	return DefaultIgnorePatterns()
}

// SetIgnorePatterns sets the first SyncPair's IgnorePatterns.
func (c *Config) SetIgnorePatterns(v []string) {
	if len(c.SyncPairs) > 0 {
		c.SyncPairs[0].IgnorePatterns = v
	}
}

// SyncInterval returns the first SyncPair's EffectiveSyncInterval, or the default 30.
func (c *Config) SyncInterval() int {
	if len(c.SyncPairs) > 0 {
		return c.SyncPairs[0].EffectiveSyncInterval()
	}
	return 30
}

// SetSyncInterval sets the first SyncPair's SyncInterval.
func (c *Config) SetSyncInterval(v int) {
	if len(c.SyncPairs) > 0 {
		c.SyncPairs[0].SyncInterval = v
	}
}

// PassphraseHash returns the Encryption.PassphraseHash.
func (c *Config) PassphraseHash() string {
	return c.Encryption.PassphraseHash
}

// SetPassphraseHash sets the Encryption.PassphraseHash.
func (c *Config) SetPassphraseHash(v string) {
	c.Encryption.PassphraseHash = v
}

// OAuthClientID returns the OAuth.ClientID.
func (c *Config) OAuthClientID() string {
	return c.OAuth.ClientID
}

// SetOAuthClientID sets the OAuth.ClientID.
func (c *Config) SetOAuthClientID(v string) {
	c.OAuth.ClientID = v
}

// OAuthClientSecretEnc returns the encrypted (base64) OAuth client secret.
func (c *Config) OAuthClientSecretEnc() string {
	return c.OAuth.ClientSecretEnc
}

// SetOAuthClientSecretEnc sets the encrypted (base64) OAuth client secret.
func (c *Config) SetOAuthClientSecretEnc(v string) {
	c.OAuth.ClientSecretEnc = v
}

// RememberPassphrase reports whether auto-unlock is enabled.
func (c *Config) RememberPassphrase() bool {
	return c.App.RememberPassphrase
}

// SetRememberPassphrase enables or disables auto-unlock.
func (c *Config) SetRememberPassphrase(v bool) {
	c.App.RememberPassphrase = v
}

// RateLimitUpKBps returns the upload bandwidth limit in KB/s (0 = unlimited).
func (c *Config) RateLimitUpKBps() int {
	return c.App.RateLimitUpKBps
}

// RateLimitDownKBps returns the download bandwidth limit in KB/s (0 = unlimited).
func (c *Config) RateLimitDownKBps() int {
	return c.App.RateLimitDownKBps
}

// SetRateLimits sets the upload and download bandwidth limits in KB/s.
func (c *Config) SetRateLimits(upKBps, downKBps int) {
	c.App.RateLimitUpKBps = upKBps
	c.App.RateLimitDownKBps = downKBps
}

// LogPath returns the App.LogPath.
func (c *Config) LogPath() string {
	return c.App.LogPath
}

// SetLogPath sets the App.LogPath.
func (c *Config) SetLogPath(v string) {
	c.App.LogPath = v
}

// AutoStart returns the App.AutoStart value.
func (c *Config) AutoStart() bool {
	return c.App.AutoStart
}

// SetAutoStart sets the App.AutoStart value.
func (c *Config) SetAutoStart(v bool) {
	c.App.AutoStart = v
}

// ---------------------------------------------------------------------------
// ConfigPath
// ---------------------------------------------------------------------------

// ConfigPath returns the default path for the configuration file on Windows.
func ConfigPath() string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		appData = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
	}
	return filepath.Join(appData, "gcrypt", "config.yaml")
}

// ---------------------------------------------------------------------------
// Load (with V1→V2 migration)
// ---------------------------------------------------------------------------

// Load reads the configuration from a YAML file at the given path.
// If the file contains a V1 format (no "version" key or version=1), it is
// automatically migrated to V2 and saved back.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Unmarshal into a raw map to check the version key.
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	version, _ := raw["version"].(int)

	if version < 2 {
		// V1 migration path
		return migrateV1(path, data)
	}

	// V2 — unmarshal directly
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	applyDefaults(cfg)
	ensurePairIDs(cfg)

	return cfg, nil
}

// migrateV1 converts a V1 config file to V2, creates a backup, and saves.
func migrateV1(path string, data []byte) (*Config, error) {
	var v1 v1Config
	if err := yaml.Unmarshal(data, &v1); err != nil {
		return nil, fmt.Errorf("parsing V1 config: %w", err)
	}

	// Create backup of the original V1 file.
	backupPath := path + ".v1.bak"
	if err := os.WriteFile(backupPath, data, 0600); err != nil {
		return nil, fmt.Errorf("creating V1 backup: %w", err)
	}

	// Map V1 fields to V2 structure.
	cfg := DefaultConfig()
	cfg.Version = 2

	cfg.Encryption.PassphraseHash = v1.PassphraseHash
	cfg.App.LogPath = v1.LogPath
	cfg.App.AutoStart = v1.AutoStart

	// Build the first SyncPair from V1 fields.
	pair := SyncPair{
		ID:             uuid.New().String(),
		LocalDir:       v1.SyncDir,
		DriveFolderID:  v1.DriveFolderID,
		Enabled:        true,
		IgnorePatterns: v1.IgnorePatterns,
		SyncInterval:   v1.SyncInterval,
	}
	cfg.SyncPairs = []SyncPair{pair}

	// Save the migrated config back to the original path.
	if err := Save(path, cfg); err != nil {
		return nil, fmt.Errorf("saving migrated config: %w", err)
	}

	return cfg, nil
}

// applyDefaults fills in zero-value fields with sensible defaults.
func applyDefaults(cfg *Config) {
	if cfg.Version == 0 {
		cfg.Version = 2
	}

	if cfg.SyncPairs == nil {
		cfg.SyncPairs = []SyncPair{}
	}

	def := DefaultConfig()

	if cfg.App.LogLevel == "" {
		cfg.App.LogLevel = def.App.LogLevel
	}
	if cfg.App.LogPath == "" {
		cfg.App.LogPath = def.App.LogPath
	}
	if cfg.App.LogMaxSize == 0 {
		cfg.App.LogMaxSize = def.App.LogMaxSize
	}
	if cfg.App.LogMaxBackups == 0 {
		cfg.App.LogMaxBackups = def.App.LogMaxBackups
	}

	// Apply SyncPair-level defaults.
	for range cfg.SyncPairs {
		// Enabled is false by default in Go; the AddSyncPair method always
		// sets Enabled=true explicitly. We respect the deserialized value.
	}
}

// ensurePairIDs generates UUIDs for any SyncPair that has an empty ID.
func ensurePairIDs(cfg *Config) {
	for i := range cfg.SyncPairs {
		if cfg.SyncPairs[i].ID == "" {
			cfg.SyncPairs[i].ID = uuid.New().String()
		}
	}
}

// ---------------------------------------------------------------------------
// Save
// ---------------------------------------------------------------------------

// Save writes the configuration to a YAML file at the given path.
// It creates the parent directories if they do not exist.
func Save(path string, cfg *Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("writing config file: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Validate
// ---------------------------------------------------------------------------

// Validate checks that the configuration values are valid and returns an error
// describing the first issue found.
func (c *Config) Validate() error {
	if c.Version != 2 {
		return fmt.Errorf("config version must be 2, got %d", c.Version)
	}

	if len(c.SyncPairs) == 0 {
		return errors.New("at least one sync_pair is required")
	}

	for i, pair := range c.SyncPairs {
		if pair.ID == "" {
			return fmt.Errorf("sync_pairs[%d]: id is required", i)
		}
		if pair.LocalDir == "" {
			return fmt.Errorf("sync_pairs[%d]: local_dir is required", i)
		}
		if pair.DriveFolderID == "" {
			return fmt.Errorf("sync_pairs[%d]: drive_folder_id is required", i)
		}
		if pair.EffectiveSyncInterval() < 5 {
			return fmt.Errorf("sync_pairs[%d]: sync_interval must be at least 5 seconds", i)
		}
	}

	if c.Encryption.PassphraseHash == "" {
		return errors.New("encryption.passphrase_hash is required; run setup to configure")
	}

	validLogLevels := []string{"debug", "info", "warn", "error"}
	if !slices.Contains(validLogLevels, c.App.LogLevel) {
		return fmt.Errorf("app.log_level must be one of %s, got %q",
			strings.Join(validLogLevels, ", "), c.App.LogLevel)
	}

	if c.App.LogMaxSize < 1 {
		return errors.New("app.log_max_size must be at least 1 MiB")
	}

	if c.App.LogMaxBackups < 0 {
		return errors.New("app.log_max_backups must be >= 0")
	}

	return nil
}

// ---------------------------------------------------------------------------
// Soft validation for tray-first startup
// ---------------------------------------------------------------------------

// ValidateForStartup performs a "soft" validation suitable for tray-first
// startup. Unlike Validate, it does NOT require a passphrase hash or OAuth
// token — those can be obtained later via the tray UI. It checks only that
// the config has enough information to show the tray icon (i.e. at least one
// sync pair with a local_dir and drive_folder_id).
//
// Returns:
//   - ErrNotConfigured       if the config file does not exist (use ValidateForStartupPath)
//   - *ErrMissingFields      if the config exists but required fields are absent
//   - nil                    if the config has enough information to show the tray
func (c *Config) ValidateForStartup() error {
	var missing []string

	if len(c.SyncPairs) == 0 {
		missing = append(missing, "sync_pairs")
	} else {
		for i, pair := range c.SyncPairs {
			if pair.LocalDir == "" {
				missing = append(missing, fmt.Sprintf("sync_pairs[%d].local_dir", i))
			}
			if pair.DriveFolderID == "" {
				missing = append(missing, fmt.Sprintf("sync_pairs[%d].drive_folder_id", i))
			}
		}
	}

	if len(missing) > 0 {
		return &ErrMissingFields{Fields: missing}
	}

	return nil
}

// ValidateForStartupPath is a convenience function that checks whether a
// config file exists at the given path and, if so, loads it and calls
// ValidateForStartup. It returns ErrNotConfigured if the file does not exist.
func ValidateForStartupPath(path string) (*Config, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, ErrNotConfigured
	}

	cfg, err := Load(path)
	if err != nil {
		return nil, fmt.Errorf("config: failed to load: %w", err)
	}

	if err := cfg.ValidateForStartup(); err != nil {
		return nil, err
	}

	return cfg, nil
}
