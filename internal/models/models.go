// Package models defines the core data structures used across gcrypt packages.
package models

import "time"

// SyncStatus represents the synchronization state of a file.
type SyncStatus string

const (
	SyncStatusPending    SyncStatus = "pending"
	SyncStatusUploading  SyncStatus = "uploading"
	SyncStatusDownloaded SyncStatus = "downloaded"
	SyncStatusSynced     SyncStatus = "synced"
	SyncStatusConflict   SyncStatus = "conflict"
	SyncStatusError      SyncStatus = "error"
	// SyncStatusOnlineOnly marks a remote file that is tracked but intentionally
	// not downloaded (on-demand / online-only). It has no local content.
	SyncStatusOnlineOnly SyncStatus = "online_only"
)

// ChangeOp represents the type of file system change event.
type ChangeOp string

const (
	ChangeOpCreate ChangeOp = "create"
	ChangeOpModify ChangeOp = "modify"
	ChangeOpDelete ChangeOp = "delete"
	ChangeOpRename ChangeOp = "rename"
)

// OpType represents the type of sync operation.
type OpType string

const (
	OpTypeUpload       OpType = "upload"
	OpTypeDownload     OpType = "download"
	OpTypeDeleteRemote OpType = "delete_remote"
	OpTypeDeleteLocal  OpType = "delete_local"
	OpTypeConflict     OpType = "conflict"
)

// SyncFile represents the mapping between a local file and its remote encrypted counterpart.
type SyncFile struct {
	SyncRootID   string     `yaml:"sync_root_id" json:"sync_root_id"` // ID of the sync pair this file belongs to
	LocalPath    string     `yaml:"local_path" json:"local_path"`
	RemoteID     string     `yaml:"remote_id" json:"remote_id"`
	LocalHash    string     `yaml:"local_hash" json:"local_hash"`
	RemoteHash   string     `yaml:"remote_hash" json:"remote_hash"`
	Version      int        `yaml:"version" json:"version"`
	EncryptedDEK []byte     `yaml:"encrypted_dek" json:"encrypted_dek"`
	DEKNonce     []byte     `yaml:"dek_nonce" json:"dek_nonce"`
	ContentNonce []byte     `yaml:"content_nonce" json:"content_nonce"`
	Size         int64      `yaml:"size" json:"size"`
	ModTime      time.Time  `yaml:"mod_time" json:"mod_time"`
	SyncStatus   SyncStatus `yaml:"sync_status" json:"sync_status"`
}

// SyncRoot represents a configured sync pair in the database.
type SyncRoot struct {
	ID            string // UUID, matches config.SyncPair.ID
	LocalDir      string // Local directory path
	DriveFolderID string // Google Drive folder ID
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ChangeEvent represents a file system change detected by the watcher.
type ChangeEvent struct {
	Path      string    `yaml:"path" json:"path"`
	Op        ChangeOp  `yaml:"op" json:"op"`
	OldPath   string    `yaml:"old_path,omitempty" json:"old_path,omitempty"`
	Timestamp time.Time `yaml:"timestamp" json:"timestamp"`
}

// SyncOperation represents a queued sync operation to be processed by the sync engine.
type SyncOperation struct {
	ID          string    `yaml:"id" json:"id"`
	File        *SyncFile `yaml:"file" json:"file"`
	OpType      OpType    `yaml:"op_type" json:"op_type"`
	Priority    int       `yaml:"priority" json:"priority"`
	Attempts    int       `yaml:"attempts" json:"attempts"`
	MaxAttempts int       `yaml:"max_attempts" json:"max_attempts"`
	LastError   string    `yaml:"last_error,omitempty" json:"last_error,omitempty"`
	CreatedAt   time.Time `yaml:"created_at" json:"created_at"`
}

// Encrypted file format constants.
const (
	// Magic is the 6-byte magic number identifying gcrypt encrypted files.
	Magic = "GCRYPT"

	// CurrentStreamVersion is the encrypted format version. Every value (file
	// content as well as small blobs like the OAuth token and client secret) is
	// written with the chunked stream format, whose per-chunk AAD binds a
	// "final" marker so the chunk count is authenticated and a truncated or
	// extended ciphertext fails to decrypt.
	CurrentStreamVersion uint16 = 2

	// HeaderSize is the total size of the encrypted file header in bytes.
	// 6 (magic) + 2 (version) + 48 (encrypted DEK) + 12 (DEK nonce) + 12 (content nonce) = 80
	HeaderSize = 80
)

// EncryptedFileHeader is the header prepended to every encrypted file uploaded to Google Drive.
type EncryptedFileHeader struct {
	Magic        [6]byte
	Version      uint16
	EncryptedDEK [48]byte
	DEKNonce     [12]byte
	ContentNonce [12]byte
}
