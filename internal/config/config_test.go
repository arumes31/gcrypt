package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// TestDefaultConfig — Verify default config has sensible V2 values
// ---------------------------------------------------------------------------

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Version != 2 {
		t.Errorf("default Version = %d, want 2", cfg.Version)
	}
	if cfg.SyncPairs == nil {
		t.Error("default SyncPairs should not be nil")
	}
	if len(cfg.SyncPairs) != 0 {
		t.Errorf("default SyncPairs should be empty slice, got %d items", len(cfg.SyncPairs))
	}
	if cfg.App.LogLevel != "info" {
		t.Errorf("default LogLevel = %q, want %q", cfg.App.LogLevel, "info")
	}
	if cfg.App.LogPath == "" {
		t.Error("default LogPath should not be empty")
	}
	if cfg.App.AutoStart != true {
		t.Error("default AutoStart should be true")
	}
	if cfg.App.LogMaxSize != 10 {
		t.Errorf("default LogMaxSize = %d, want 10", cfg.App.LogMaxSize)
	}
	if cfg.App.LogMaxBackups != 3 {
		t.Errorf("default LogMaxBackups = %d, want 3", cfg.App.LogMaxBackups)
	}
	if cfg.App.MaxFileSize != 0 {
		t.Errorf("default MaxFileSize = %d, want 0", cfg.App.MaxFileSize)
	}
	if cfg.Encryption.PassphraseHash != "" {
		t.Error("default PassphraseHash should be empty")
	}
}

// ---------------------------------------------------------------------------
// TestDefaultIgnorePatterns — Verify the helper returns expected patterns
// ---------------------------------------------------------------------------

func TestDefaultIgnorePatterns(t *testing.T) {
	patterns := DefaultIgnorePatterns()
	set := make(map[string]bool, len(patterns))
	for _, p := range patterns {
		set[p] = true
	}
	// Both the transient-artifact patterns and the common build/dependency trees
	// must be present (order-independent, so adding more later won't break this).
	for _, want := range []string{
		"~$*", "*~", ".~lock.*#", "*.lock", "*.tmp", "*.swp", ".DS_Store", "Thumbs.db", "desktop.ini",
		"vendor", "dist", "build", "target", "__pycache__", ".venv",
	} {
		if !set[want] {
			t.Errorf("DefaultIgnorePatterns missing %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// TestV2RoundTrip — Save a V2 config, load it back, verify all fields match
// ---------------------------------------------------------------------------

func TestV2RoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	cfg := DefaultConfig()
	cfg.Encryption.PassphraseHash = "argon2id$test$hash"
	cfg.App.LogLevel = "debug"
	cfg.App.MaxFileSize = 1024 * 1024 * 50 // 50 MiB

	pair := cfg.AddSyncPair("/tmp/gcrypt", "folder123", []string{"*.tmp", "~$*"}, 60)

	if err := Save(configPath, cfg); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Fatalf("config file was not created at %s", configPath)
	}

	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if loaded.Version != cfg.Version {
		t.Errorf("Version mismatch: got %d, want %d", loaded.Version, cfg.Version)
	}
	if len(loaded.SyncPairs) != 1 {
		t.Fatalf("SyncPairs length = %d, want 1", len(loaded.SyncPairs))
	}

	lp := loaded.SyncPairs[0]
	if lp.ID != pair.ID {
		t.Errorf("SyncPair ID mismatch: got %q, want %q", lp.ID, pair.ID)
	}
	if lp.LocalDir != "/tmp/gcrypt" {
		t.Errorf("LocalDir mismatch: got %q, want %q", lp.LocalDir, "/tmp/gcrypt")
	}
	if lp.DriveFolderID != "folder123" {
		t.Errorf("DriveFolderID mismatch: got %q, want %q", lp.DriveFolderID, "folder123")
	}
	if lp.Enabled != true {
		t.Error("SyncPair Enabled should be true")
	}
	if lp.SyncInterval != 60 {
		t.Errorf("SyncInterval mismatch: got %d, want 60", lp.SyncInterval)
	}
	if len(lp.IgnorePatterns) != 2 {
		t.Fatalf("IgnorePatterns length = %d, want 2", len(lp.IgnorePatterns))
	}
	if lp.IgnorePatterns[0] != "*.tmp" || lp.IgnorePatterns[1] != "~$*" {
		t.Errorf("IgnorePatterns mismatch: got %v", lp.IgnorePatterns)
	}

	if loaded.Encryption.PassphraseHash != "argon2id$test$hash" {
		t.Errorf("PassphraseHash mismatch: got %q", loaded.Encryption.PassphraseHash)
	}
	if loaded.App.LogLevel != "debug" {
		t.Errorf("LogLevel mismatch: got %q, want %q", loaded.App.LogLevel, "debug")
	}
	if loaded.App.MaxFileSize != 1024*1024*50 {
		t.Errorf("MaxFileSize mismatch: got %d, want %d", loaded.App.MaxFileSize, 1024*1024*50)
	}
}

// ---------------------------------------------------------------------------
// TestV1Migration — Create a V1 YAML file, load it, verify V2 structure
// ---------------------------------------------------------------------------

func TestV1Migration(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	backupPath := configPath + ".v1.bak"

	// Write a V1 config file (no version key).
	v1 := v1Config{ // #nosec G101 -- test fixture; PassphraseHash is a dummy, not a real credential
		SyncDir:        "/home/user/GcryptDrive",
		DriveFolderID:  "1aBcDeFgHiJkLmNoPqRsTuVwXyZ",
		PassphraseHash: "argon2id$v=19$m=262144,t=4,p=4$hash",
		IgnorePatterns: []string{"*.tmp", "~$*"},
		SyncInterval:   45,
		LogPath:        "/var/log/gcrypt.log",
		AutoStart:      true,
	}
	data, err := yaml.Marshal(&v1)
	if err != nil {
		t.Fatalf("marshal V1 config: %v", err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatalf("write V1 config: %v", err)
	}

	// Load should detect V1 and migrate.
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Verify V2 structure.
	if cfg.Version != 2 {
		t.Errorf("Version = %d, want 2 after migration", cfg.Version)
	}
	if len(cfg.SyncPairs) != 1 {
		t.Fatalf("expected 1 SyncPair after migration, got %d", len(cfg.SyncPairs))
	}

	pair := cfg.SyncPairs[0]
	if pair.ID == "" {
		t.Error("migrated SyncPair should have auto-generated ID")
	}
	if pair.LocalDir != "/home/user/GcryptDrive" {
		t.Errorf("LocalDir = %q, want %q", pair.LocalDir, "/home/user/GcryptDrive")
	}
	if pair.DriveFolderID != "1aBcDeFgHiJkLmNoPqRsTuVwXyZ" {
		t.Errorf("DriveFolderID = %q, want %q", pair.DriveFolderID, "1aBcDeFgHiJkLmNoPqRsTuVwXyZ")
	}
	if pair.Enabled != true {
		t.Error("migrated SyncPair should be enabled")
	}
	if pair.SyncInterval != 45 {
		t.Errorf("SyncInterval = %d, want 45", pair.SyncInterval)
	}
	if len(pair.IgnorePatterns) != 2 || pair.IgnorePatterns[0] != "*.tmp" {
		t.Errorf("IgnorePatterns = %v, want [*.tmp ~$*]", pair.IgnorePatterns)
	}

	if cfg.Encryption.PassphraseHash != "argon2id$v=19$m=262144,t=4,p=4$hash" {
		t.Errorf("PassphraseHash = %q, mismatch", cfg.Encryption.PassphraseHash)
	}
	if cfg.App.LogPath != "/var/log/gcrypt.log" {
		t.Errorf("LogPath = %q, want %q", cfg.App.LogPath, "/var/log/gcrypt.log")
	}
	if cfg.App.AutoStart != true {
		t.Error("AutoStart should be true after migration")
	}

	// Verify backup was created.
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		t.Error("V1 backup file was not created")
	}

	// Verify the file on disk is now V2.
	raw, err := os.ReadFile(configPath) // #nosec G304 -- configPath is a test temp file
	if err != nil {
		t.Fatalf("read migrated config: %v", err)
	}
	var rawMap map[string]interface{}
	if err := yaml.Unmarshal(raw, &rawMap); err != nil {
		t.Fatalf("unmarshal migrated config: %v", err)
	}
	if ver, _ := rawMap["version"].(int); ver != 2 {
		t.Errorf("on-disk version = %v, want 2", rawMap["version"])
	}
}

// ---------------------------------------------------------------------------
// TestV1MigrationWithVersion1 — V1 file that explicitly has version: 1
// ---------------------------------------------------------------------------

func TestV1MigrationWithVersion1(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	v1YAML := `version: 1
sync_dir: /test/dir
drive_folder_id: abc123
passphrase_hash: somehash
sync_interval: 30
log_path: /test/log.log
auto_start: false
`
	if err := os.WriteFile(configPath, []byte(v1YAML), 0600); err != nil {
		t.Fatalf("write V1 config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Version != 2 {
		t.Errorf("Version = %d, want 2", cfg.Version)
	}
	if len(cfg.SyncPairs) != 1 {
		t.Fatalf("expected 1 SyncPair, got %d", len(cfg.SyncPairs))
	}
	if cfg.SyncPairs[0].LocalDir != "/test/dir" {
		t.Errorf("LocalDir = %q, want %q", cfg.SyncPairs[0].LocalDir, "/test/dir")
	}
	if cfg.App.AutoStart != false {
		t.Error("AutoStart should be false after migration")
	}
}

// ---------------------------------------------------------------------------
// TestDefaultsAppliedOnLoad — Verify defaults fill in zero-value fields
// ---------------------------------------------------------------------------

func TestDefaultsAppliedOnLoad(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Minimal V2 config — many fields intentionally omitted.
	minimalYAML := `version: 2
sync_pairs:
  - id: "test-id-1"
    local_dir: "/tmp/gcrypt"
    drive_folder_id: "folder123"
    enabled: true
encryption:
  passphrase_hash: "somehash"
`
	if err := os.WriteFile(configPath, []byte(minimalYAML), 0600); err != nil {
		t.Fatalf("write minimal config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// App defaults should be applied.
	if cfg.App.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want default %q", cfg.App.LogLevel, "info")
	}
	if cfg.App.LogPath == "" {
		t.Error("LogPath should have default value")
	}
	if cfg.App.LogMaxSize != 10 {
		t.Errorf("LogMaxSize = %d, want default 10", cfg.App.LogMaxSize)
	}
	if cfg.App.LogMaxBackups != 3 {
		t.Errorf("LogMaxBackups = %d, want default 3", cfg.App.LogMaxBackups)
	}
}

// ---------------------------------------------------------------------------
// TestUUIDAutoGeneration — SyncPairs without IDs get auto-generated UUIDs
// ---------------------------------------------------------------------------

func TestUUIDAutoGeneration(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// V2 config with a pair missing an ID.
	yamlContent := `version: 2
sync_pairs:
  - local_dir: "/tmp/gcrypt"
    drive_folder_id: "folder123"
    enabled: true
encryption:
  passphrase_hash: "somehash"
`
	if err := os.WriteFile(configPath, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if len(cfg.SyncPairs) != 1 {
		t.Fatalf("expected 1 SyncPair, got %d", len(cfg.SyncPairs))
	}
	if cfg.SyncPairs[0].ID == "" {
		t.Error("SyncPair ID should be auto-generated when missing")
	}
	// Verify it looks like a UUID (format: 8-4-4-4-12)
	id := cfg.SyncPairs[0].ID
	if len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Errorf("auto-generated ID %q doesn't look like a UUID", id)
	}
}

// ---------------------------------------------------------------------------
// TestValidate — Test that Validate() catches various invalid configs
// ---------------------------------------------------------------------------

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr string // substring to match in error message
	}{
		{
			name: "wrong version",
			cfg: &Config{
				Version: 1,
				SyncPairs: []SyncPair{{
					ID: "id1", LocalDir: "/tmp", DriveFolderID: "f1", SyncInterval: 30,
				}},
				Encryption: EncryptionConfig{PassphraseHash: "hash"},
				App:        AppConfig{LogLevel: "info", LogMaxSize: 10, LogMaxBackups: 3},
			},
			wantErr: "version must be 2",
		},
		{
			name: "no sync pairs",
			cfg: &Config{
				Version:    2,
				SyncPairs:  []SyncPair{},
				Encryption: EncryptionConfig{PassphraseHash: "hash"},
				App:        AppConfig{LogLevel: "info", LogMaxSize: 10, LogMaxBackups: 3},
			},
			wantErr: "at least one sync_pair",
		},
		{
			name: "missing local_dir",
			cfg: &Config{
				Version: 2,
				SyncPairs: []SyncPair{{
					ID: "id1", LocalDir: "", DriveFolderID: "f1", SyncInterval: 30,
				}},
				Encryption: EncryptionConfig{PassphraseHash: "hash"},
				App:        AppConfig{LogLevel: "info", LogMaxSize: 10, LogMaxBackups: 3},
			},
			wantErr: "local_dir is required",
		},
		{
			name: "missing drive_folder_id",
			cfg: &Config{
				Version: 2,
				SyncPairs: []SyncPair{{
					ID: "id1", LocalDir: "/tmp", DriveFolderID: "", SyncInterval: 30,
				}},
				Encryption: EncryptionConfig{PassphraseHash: "hash"},
				App:        AppConfig{LogLevel: "info", LogMaxSize: 10, LogMaxBackups: 3},
			},
			wantErr: "drive_folder_id is required",
		},
		{
			name: "missing pair id",
			cfg: &Config{
				Version: 2,
				SyncPairs: []SyncPair{{
					ID: "", LocalDir: "/tmp", DriveFolderID: "f1", SyncInterval: 30,
				}},
				Encryption: EncryptionConfig{PassphraseHash: "hash"},
				App:        AppConfig{LogLevel: "info", LogMaxSize: 10, LogMaxBackups: 3},
			},
			wantErr: "id is required",
		},
		{
			name: "sync_interval too low",
			cfg: &Config{
				Version: 2,
				SyncPairs: []SyncPair{{
					ID: "id1", LocalDir: "/tmp", DriveFolderID: "f1", SyncInterval: 2,
				}},
				Encryption: EncryptionConfig{PassphraseHash: "hash"},
				App:        AppConfig{LogLevel: "info", LogMaxSize: 10, LogMaxBackups: 3},
			},
			wantErr: "sync_interval must be at least 5",
		},
		{
			name: "sync_interval zero uses default 30",
			cfg: &Config{
				Version: 2,
				SyncPairs: []SyncPair{{
					ID: "id1", LocalDir: "/tmp", DriveFolderID: "f1", SyncInterval: 0,
				}},
				Encryption: EncryptionConfig{PassphraseHash: "hash"},
				App:        AppConfig{LogLevel: "info", LogMaxSize: 10, LogMaxBackups: 3},
			},
			wantErr: "", // valid: EffectiveSyncInterval returns 30
		},
		{
			name: "missing passphrase_hash",
			cfg: &Config{
				Version: 2,
				SyncPairs: []SyncPair{{
					ID: "id1", LocalDir: "/tmp", DriveFolderID: "f1", SyncInterval: 30,
				}},
				Encryption: EncryptionConfig{PassphraseHash: ""},
				App:        AppConfig{LogLevel: "info", LogMaxSize: 10, LogMaxBackups: 3},
			},
			wantErr: "passphrase_hash is required",
		},
		{
			name: "invalid log_level",
			cfg: &Config{
				Version: 2,
				SyncPairs: []SyncPair{{
					ID: "id1", LocalDir: "/tmp", DriveFolderID: "f1", SyncInterval: 30,
				}},
				Encryption: EncryptionConfig{PassphraseHash: "hash"},
				App:        AppConfig{LogLevel: "verbose", LogMaxSize: 10, LogMaxBackups: 3},
			},
			wantErr: "log_level must be one of",
		},
		{
			name: "log_max_size too low",
			cfg: &Config{
				Version: 2,
				SyncPairs: []SyncPair{{
					ID: "id1", LocalDir: "/tmp", DriveFolderID: "f1", SyncInterval: 30,
				}},
				Encryption: EncryptionConfig{PassphraseHash: "hash"},
				App:        AppConfig{LogLevel: "info", LogMaxSize: 0, LogMaxBackups: 3},
			},
			wantErr: "log_max_size must be at least 1",
		},
		{
			name: "log_max_backups negative",
			cfg: &Config{
				Version: 2,
				SyncPairs: []SyncPair{{
					ID: "id1", LocalDir: "/tmp", DriveFolderID: "f1", SyncInterval: 30,
				}},
				Encryption: EncryptionConfig{PassphraseHash: "hash"},
				App:        AppConfig{LogLevel: "info", LogMaxSize: 10, LogMaxBackups: -1},
			},
			wantErr: "log_max_backups must be >= 0",
		},
		{
			name: "valid config",
			cfg: &Config{
				Version: 2,
				SyncPairs: []SyncPair{{
					ID: "id1", LocalDir: "/tmp", DriveFolderID: "f1", SyncInterval: 30,
				}},
				Encryption: EncryptionConfig{PassphraseHash: "hash"},
				App:        AppConfig{LogLevel: "info", LogMaxSize: 10, LogMaxBackups: 3},
			},
			wantErr: "",
		},
		{
			name: "overlapping local dirs",
			cfg: &Config{
				Version: 2,
				SyncPairs: []SyncPair{
					{ID: "id1", LocalDir: "/data/docs", DriveFolderID: "f1", SyncInterval: 30},
					{ID: "id2", LocalDir: "/data/docs/sub", DriveFolderID: "f2", SyncInterval: 30},
				},
				Encryption: EncryptionConfig{PassphraseHash: "hash"},
				App:        AppConfig{LogLevel: "info", LogMaxSize: 10, LogMaxBackups: 3},
			},
			wantErr: "overlap",
		},
		{
			name: "duplicate drive folder",
			cfg: &Config{
				Version: 2,
				SyncPairs: []SyncPair{
					{ID: "id1", LocalDir: "/dir1", DriveFolderID: "sameFolder", SyncInterval: 30},
					{ID: "id2", LocalDir: "/dir2", DriveFolderID: "sameFolder", SyncInterval: 30},
				},
				Encryption: EncryptionConfig{PassphraseHash: "hash"},
				App:        AppConfig{LogLevel: "info", LogMaxSize: 10, LogMaxBackups: 3},
			},
			wantErr: "same Drive folder",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.wantErr)
				} else if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("expected error containing %q, got %q", tt.wantErr, err.Error())
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestAddSyncPair — Verify AddSyncPair generates UUID and appends correctly
// ---------------------------------------------------------------------------

func TestAddSyncPair(t *testing.T) {
	cfg := DefaultConfig()

	p1 := cfg.AddSyncPair("/dir1", "folder1", []string{"*.tmp"}, 60)
	if p1.ID == "" {
		t.Error("AddSyncPair should generate a UUID")
	}
	if p1.LocalDir != "/dir1" {
		t.Errorf("LocalDir = %q, want %q", p1.LocalDir, "/dir1")
	}
	if p1.DriveFolderID != "folder1" {
		t.Errorf("DriveFolderID = %q, want %q", p1.DriveFolderID, "folder1")
	}
	if p1.Enabled != true {
		t.Error("new SyncPair should be enabled")
	}
	if p1.SyncInterval != 60 {
		t.Errorf("SyncInterval = %d, want 60", p1.SyncInterval)
	}
	if len(p1.IgnorePatterns) != 1 || p1.IgnorePatterns[0] != "*.tmp" {
		t.Errorf("IgnorePatterns = %v, want [*.tmp]", p1.IgnorePatterns)
	}

	p2 := cfg.AddSyncPair("/dir2", "folder2", nil, 0)
	if p2.ID == "" {
		t.Error("second AddSyncPair should generate a UUID")
	}
	if p1.ID == p2.ID {
		t.Error("two SyncPairs should have different UUIDs")
	}
	if len(cfg.SyncPairs) != 2 {
		t.Errorf("expected 2 SyncPairs, got %d", len(cfg.SyncPairs))
	}
}

// ---------------------------------------------------------------------------
// TestRemoveSyncPair — Verify RemoveSyncPair removes by ID
// ---------------------------------------------------------------------------

func TestRemoveSyncPair(t *testing.T) {
	cfg := DefaultConfig()
	p1 := cfg.AddSyncPair("/dir1", "folder1", nil, 30)
	p2ID := p1.ID // capture before potential slice reallocation

	_ = cfg.AddSyncPair("/dir2", "folder2", nil, 30)

	if len(cfg.SyncPairs) != 2 {
		t.Fatalf("expected 2 SyncPairs, got %d", len(cfg.SyncPairs))
	}

	// Remove first pair.
	if ok := cfg.RemoveSyncPair(p2ID); !ok {
		t.Error("RemoveSyncPair should return true for existing ID")
	}
	if len(cfg.SyncPairs) != 1 {
		t.Errorf("expected 1 SyncPair after removal, got %d", len(cfg.SyncPairs))
	}
	if cfg.SyncPairs[0].LocalDir != "/dir2" {
		t.Errorf("remaining pair LocalDir = %q, want %q", cfg.SyncPairs[0].LocalDir, "/dir2")
	}

	// Remove non-existent ID.
	if ok := cfg.RemoveSyncPair("nonexistent"); ok {
		t.Error("RemoveSyncPair should return false for non-existent ID")
	}
	if len(cfg.SyncPairs) != 1 {
		t.Errorf("expected 1 SyncPair after failed removal, got %d", len(cfg.SyncPairs))
	}
}

// ---------------------------------------------------------------------------
// TestGetSyncPair — Verify GetSyncPair returns correct pair or nil
// ---------------------------------------------------------------------------

func TestGetSyncPair(t *testing.T) {
	cfg := DefaultConfig()
	p1 := cfg.AddSyncPair("/dir1", "folder1", nil, 30)

	found := cfg.GetSyncPair(p1.ID)
	if found == nil {
		t.Fatal("GetSyncPair should return the pair")
	}
	if found.LocalDir != "/dir1" {
		t.Errorf("LocalDir = %q, want %q", found.LocalDir, "/dir1")
	}

	missing := cfg.GetSyncPair("nonexistent")
	if missing != nil {
		t.Error("GetSyncPair should return nil for non-existent ID")
	}
}

// ---------------------------------------------------------------------------
// TestEffectiveSyncInterval — Verify fallback to default (30)
// ---------------------------------------------------------------------------

func TestEffectiveSyncInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval int
		want     int
	}{
		{"positive value", 60, 60},
		{"zero uses default", 0, 30},
		{"negative uses default", -1, 30},
		{"minimum valid", 5, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &SyncPair{SyncInterval: tt.interval}
			got := p.EffectiveSyncInterval()
			if got != tt.want {
				t.Errorf("EffectiveSyncInterval() = %d, want %d", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestEffectiveIgnorePatterns — Verify fallback to defaults
// ---------------------------------------------------------------------------

func TestEffectiveIgnorePatterns(t *testing.T) {
	// Pair with custom patterns.
	p := &SyncPair{IgnorePatterns: []string{"*.custom"}}
	patterns := p.EffectiveIgnorePatterns()
	if len(patterns) != 1 || patterns[0] != "*.custom" {
		t.Errorf("custom patterns = %v, want [*.custom]", patterns)
	}

	// Pair with empty patterns — should get defaults.
	p2 := &SyncPair{IgnorePatterns: nil}
	patterns2 := p2.EffectiveIgnorePatterns()
	defaults := DefaultIgnorePatterns()
	if len(patterns2) != len(defaults) {
		t.Errorf("nil patterns length = %d, want %d", len(patterns2), len(defaults))
	}

	p3 := &SyncPair{IgnorePatterns: []string{}}
	patterns3 := p3.EffectiveIgnorePatterns()
	if len(patterns3) != len(defaults) {
		t.Errorf("empty patterns length = %d, want %d", len(patterns3), len(defaults))
	}
}

// ---------------------------------------------------------------------------
// TestConfigPath — Verify ConfigPath returns a non-empty path
// ---------------------------------------------------------------------------

func TestConfigPath(t *testing.T) {
	p := ConfigPath()
	if p == "" {
		t.Error("ConfigPath should not return empty string")
	}
	if !strings.HasSuffix(p, "config.yaml") {
		t.Errorf("ConfigPath = %q, should end with config.yaml", p)
	}
}

// ---------------------------------------------------------------------------
// TestSaveCreatesDirectories — Verify Save creates parent directories
// ---------------------------------------------------------------------------

func TestSaveCreatesDirectories(t *testing.T) {
	tmpDir := t.TempDir()
	nestedPath := filepath.Join(tmpDir, "a", "b", "c", "config.yaml")

	cfg := DefaultConfig()
	if err := Save(nestedPath, cfg); err != nil {
		t.Fatalf("Save with nested dirs failed: %v", err)
	}
	if _, err := os.Stat(nestedPath); os.IsNotExist(err) {
		t.Error("config file was not created in nested directory")
	}
}
