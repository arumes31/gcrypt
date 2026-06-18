package drive

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/arumes31/gcrypt/internal/models"
	"github.com/google/uuid"

	_ "modernc.org/sqlite"
)

// Store wraps a SQLite database that persists sync metadata mapping local
// file paths to their remote Google Drive counterparts.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// schemaV2 is the DDL used to initialise the database with the V2 schema
// that supports multiple sync roots. It mirrors db/schema.sql so that the
// drive package can create the schema independently when opened with NewStore.
const schemaV2 = `
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER PRIMARY KEY,
    applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS sync_roots (
    id TEXT PRIMARY KEY,
    local_dir TEXT NOT NULL,
    drive_folder_id TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS sync_map (
    sync_root_id TEXT NOT NULL,
    local_path TEXT NOT NULL,
    remote_id TEXT,
    local_hash TEXT,
    remote_hash TEXT,
    version INTEGER DEFAULT 1,
    encrypted_dek BLOB,
    dek_nonce BLOB,
    content_nonce BLOB,
    size INTEGER,
    mod_time DATETIME,
    sync_status TEXT DEFAULT 'pending',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (sync_root_id, local_path),
    FOREIGN KEY (sync_root_id) REFERENCES sync_roots(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_sync_map_remote_id ON sync_map(remote_id);
CREATE INDEX IF NOT EXISTS idx_sync_map_status ON sync_map(sync_status);
CREATE INDEX IF NOT EXISTS idx_sync_map_root_status ON sync_map(sync_root_id, sync_status);
`

// NewStore opens (or creates) a SQLite database at dbPath and ensures the
// V2 schema is applied. If the database contains a V1 schema (single-key
// sync_map without sync_root_id), it is automatically migrated.
//
// defaultRootID is used as the sync root ID during V1→V2 migration. If empty,
// a new UUID is generated. For fresh databases the parameter is unused.
func NewStore(dbPath string, defaultRootID string) (*Store, error) {
	db, err := openStore(dbPath)
	if err != nil {
		// The database could not be opened or failed its integrity check. The
		// sync map is a rebuildable cache (re-derived from Drive + local files on
		// the next scan), so rather than failing to launch we quarantine the bad
		// file and start fresh. The dedup-on-upload and remote-reconcile logic
		// then re-establishes all mappings without duplicating cloud data.
		slog.Warn("store: database unusable; quarantining and rebuilding",
			"path", dbPath, "error", err.Error())
		if qerr := quarantineDB(dbPath); qerr != nil {
			return nil, fmt.Errorf("store: quarantine unusable db: %w", qerr)
		}
		db, err = openStore(dbPath)
		if err != nil {
			return nil, fmt.Errorf("store: open fresh db after rebuild: %w", err)
		}
	}

	// Apply V2 schema (CREATE IF NOT EXISTS is idempotent for new databases).
	if _, err := db.Exec(schemaV2); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: failed to apply schema: %w", err)
	}

	// Check if migration from V1 is needed.
	var needsMigration bool
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='sync_roots'").Scan(&count)
	if err == nil && count == 0 {
		// sync_roots doesn't exist — check if old sync_map exists (V1 schema).
		err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='sync_map'").Scan(&count)
		if err == nil && count > 0 {
			needsMigration = true
		}
	}

	if needsMigration {
		if defaultRootID == "" {
			defaultRootID = uuid.New().String()
		}
		if err := migrateV1ToV2(db, defaultRootID); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: migrating database: %w", err)
		}
	}

	return &Store{db: db}, nil
}

// migrateV1ToV2 migrates a V1 database (single PK on local_path) to the V2
// schema (composite PK on sync_root_id, local_path). The migration runs in a
// single transaction so it is atomic.
func migrateV1ToV2(db *sql.DB, defaultRootID string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op if committed

	// 1. Create schema_version and sync_roots tables (idempotent).
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS schema_version (
			version INTEGER PRIMARY KEY,
			applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS sync_roots (
			id TEXT PRIMARY KEY,
			local_dir TEXT NOT NULL,
			drive_folder_id TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`); err != nil {
		return fmt.Errorf("create schema_version/sync_roots: %w", err)
	}

	// 2. Create new sync_map_v2 table with composite PK.
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS sync_map_v2 (
			sync_root_id TEXT NOT NULL,
			local_path TEXT NOT NULL,
			remote_id TEXT,
			local_hash TEXT,
			remote_hash TEXT,
			version INTEGER DEFAULT 1,
			encrypted_dek BLOB,
			dek_nonce BLOB,
			content_nonce BLOB,
			size INTEGER,
			mod_time DATETIME,
			sync_status TEXT DEFAULT 'pending',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (sync_root_id, local_path),
			FOREIGN KEY (sync_root_id) REFERENCES sync_roots(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("create sync_map_v2: %w", err)
	}

	// 3. Copy all existing rows from sync_map to sync_map_v2, adding the default sync_root_id.
	if _, err := tx.Exec(`
		INSERT INTO sync_map_v2
			(sync_root_id, local_path, remote_id, local_hash, remote_hash,
			 version, encrypted_dek, dek_nonce, content_nonce,
			 size, mod_time, sync_status, created_at, updated_at)
		SELECT ?, local_path, remote_id, local_hash, remote_hash,
			 version, encrypted_dek, dek_nonce, content_nonce,
			 size, mod_time, sync_status, created_at, updated_at
		FROM sync_map;
	`, defaultRootID); err != nil {
		return fmt.Errorf("copy rows to sync_map_v2: %w", err)
	}

	// 4. Drop old sync_map table.
	if _, err := tx.Exec(`DROP TABLE sync_map`); err != nil {
		return fmt.Errorf("drop old sync_map: %w", err)
	}

	// 5. Rename sync_map_v2 to sync_map.
	if _, err := tx.Exec(`ALTER TABLE sync_map_v2 RENAME TO sync_map`); err != nil {
		return fmt.Errorf("rename sync_map_v2: %w", err)
	}

	// 6. Recreate indexes.
	if _, err := tx.Exec(`
		CREATE INDEX IF NOT EXISTS idx_sync_map_remote_id ON sync_map(remote_id);
		CREATE INDEX IF NOT EXISTS idx_sync_map_status ON sync_map(sync_status);
		CREATE INDEX IF NOT EXISTS idx_sync_map_root_status ON sync_map(sync_root_id, sync_status);
	`); err != nil {
		return fmt.Errorf("create indexes: %w", err)
	}

	// 7. Insert the default sync root into sync_roots.
	//    We don't know the local_dir or drive_folder_id from the DB alone,
	//    so we insert placeholder values. The caller should update them
	//    via UpsertSyncRoot after migration.
	if _, err := tx.Exec(`
		INSERT OR IGNORE INTO sync_roots (id, local_dir, drive_folder_id)
		VALUES (?, '', '');
	`, defaultRootID); err != nil {
		return fmt.Errorf("insert default sync root: %w", err)
	}

	// 8. Insert schema version 2.
	if _, err := tx.Exec(`
		INSERT OR IGNORE INTO schema_version (version) VALUES (2);
	`); err != nil {
		return fmt.Errorf("insert schema version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}

	return nil
}

// openStore opens the SQLite database at dbPath, applies the connection
// pragmas, and verifies the file with PRAGMA quick_check. It returns an error if
// the database cannot be opened or fails the integrity check (the caller may
// then quarantine the file and retry with a fresh one). A new/empty database
// passes the check.
func openStore(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}
	// quick_check is far cheaper than integrity_check and still catches the
	// structural corruption that would otherwise crash queries at runtime.
	var res string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&res); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("integrity check: %w", err)
	}
	if res != "ok" {
		_ = db.Close()
		return nil, fmt.Errorf("integrity check failed: %s", res)
	}
	return db, nil
}

// quarantineDB renames a corrupt database aside (with a timestamped suffix),
// along with its -wal/-shm sidecars, so a fresh database can take its place. If
// the main file can't be renamed (e.g. locked) it is removed instead.
func quarantineDB(dbPath string) error {
	suffix := ".corrupt-" + time.Now().Format("20060102-150405")
	if _, err := os.Stat(dbPath); err == nil {
		if err := os.Rename(dbPath, dbPath+suffix); err != nil {
			if rmErr := os.Remove(dbPath); rmErr != nil {
				return err
			}
		}
	}
	// Sidecars are best-effort; a leftover -wal/-shm for a fresh db is harmless.
	for _, side := range []string{"-wal", "-shm"} {
		_ = os.Rename(dbPath+side, dbPath+side+suffix)
	}
	return nil
}

// Close closes the underlying SQLite database connection.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: failed to close database: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// SyncFile CRUD
// ---------------------------------------------------------------------------

// GetSyncFile looks up sync metadata by sync root ID and local file path.
// Returns sql.ErrNoRows if no matching row exists.
func (s *Store) GetSyncFile(syncRootID, localPath string) (*models.SyncFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	const query = `
	SELECT sync_root_id, local_path, remote_id, local_hash, remote_hash,
	       version, encrypted_dek, dek_nonce, content_nonce,
	       size, mod_time, sync_status
	FROM sync_map
	WHERE sync_root_id = ? AND local_path = ?
	`

	row := s.db.QueryRow(query, syncRootID, localPath)
	sf, err := scanSyncFile(row)
	if err != nil {
		return nil, fmt.Errorf("store: get sync file by path: %w", err)
	}
	return sf, nil
}

// GetSyncFileByRemoteID looks up sync metadata by sync root ID and the remote
// Google Drive file ID. Returns sql.ErrNoRows if no matching row exists.
func (s *Store) GetSyncFileByRemoteID(syncRootID, remoteID string) (*models.SyncFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	const query = `
	SELECT sync_root_id, local_path, remote_id, local_hash, remote_hash,
	       version, encrypted_dek, dek_nonce, content_nonce,
	       size, mod_time, sync_status
	FROM sync_map
	WHERE sync_root_id = ? AND remote_id = ?
	`

	row := s.db.QueryRow(query, syncRootID, remoteID)
	sf, err := scanSyncFile(row)
	if err != nil {
		return nil, fmt.Errorf("store: get sync file by remote ID: %w", err)
	}
	return sf, nil
}

// PutSyncFile inserts or replaces a sync metadata record. If a row with the
// same (sync_root_id, local_path) already exists it is fully replaced.
func (s *Store) PutSyncFile(sf *models.SyncFile) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	const query = `
	INSERT OR REPLACE INTO sync_map
		(sync_root_id, local_path, remote_id, local_hash, remote_hash,
		 version, encrypted_dek, dek_nonce, content_nonce,
		 size, mod_time, sync_status, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	`

	_, err := s.db.Exec(query,
		sf.SyncRootID,
		sf.LocalPath,
		sf.RemoteID,
		sf.LocalHash,
		sf.RemoteHash,
		sf.Version,
		sf.EncryptedDEK,
		sf.DEKNonce,
		sf.ContentNonce,
		sf.Size,
		sf.ModTime,
		string(sf.SyncStatus),
	)
	if err != nil {
		return fmt.Errorf("store: put sync file: %w", err)
	}

	return nil
}

// DeleteSyncFile removes the sync metadata row for the given sync root and
// local path.
func (s *Store) DeleteSyncFile(syncRootID, localPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	const query = `DELETE FROM sync_map WHERE sync_root_id = ? AND local_path = ?`

	// Deleting a row that does not exist is treated as success: a local file
	// that was never tracked (e.g. an ignored or never-synced file) can still
	// generate a delete operation, and that should be a no-op rather than an
	// error that retries forever.
	if _, err := s.db.Exec(query, syncRootID, localPath); err != nil {
		return fmt.Errorf("store: delete sync file: %w", err)
	}

	return nil
}

// ListPending returns all tracked files in the given sync root whose
// sync_status is not "synced".
func (s *Store) ListPending(syncRootID string) ([]*models.SyncFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	const query = `
	SELECT sync_root_id, local_path, remote_id, local_hash, remote_hash,
	       version, encrypted_dek, dek_nonce, content_nonce,
	       size, mod_time, sync_status
	FROM sync_map
	WHERE sync_root_id = ? AND sync_status != 'synced'
	`

	rows, err := s.db.Query(query, syncRootID)
	if err != nil {
		return nil, fmt.Errorf("store: list pending: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanSyncFiles(rows)
}

// ListAll returns all tracked files in the given sync root.
func (s *Store) ListAll(syncRootID string) ([]*models.SyncFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	const query = `
	SELECT sync_root_id, local_path, remote_id, local_hash, remote_hash,
	       version, encrypted_dek, dek_nonce, content_nonce,
	       size, mod_time, sync_status
	FROM sync_map
	WHERE sync_root_id = ?
	`

	rows, err := s.db.Query(query, syncRootID)
	if err != nil {
		return nil, fmt.Errorf("store: list all: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanSyncFiles(rows)
}

// UpdateStatus changes the sync_status column for the given sync root and
// local path.
func (s *Store) UpdateStatus(syncRootID, localPath string, status models.SyncStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	const query = `
	UPDATE sync_map
	SET sync_status = ?, updated_at = CURRENT_TIMESTAMP
	WHERE sync_root_id = ? AND local_path = ?
	`

	result, err := s.db.Exec(query, string(status), syncRootID, localPath)
	if err != nil {
		return fmt.Errorf("store: update status: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("store: update status: no row with sync_root_id %q local_path %q", syncRootID, localPath)
	}

	return nil
}

// UpdateRemoteInfo updates the remote metadata fields after a successful
// upload: the remote file ID, the remote hash, and the file size.
func (s *Store) UpdateRemoteInfo(syncRootID, localPath, remoteID, remoteHash string, size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	const query = `
	UPDATE sync_map
	SET remote_id = ?, remote_hash = ?, size = ?, updated_at = CURRENT_TIMESTAMP
	WHERE sync_root_id = ? AND local_path = ?
	`

	result, err := s.db.Exec(query, remoteID, remoteHash, size, syncRootID, localPath)
	if err != nil {
		return fmt.Errorf("store: update remote info: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("store: update remote info: no row with sync_root_id %q local_path %q", syncRootID, localPath)
	}

	return nil
}

// ---------------------------------------------------------------------------
// SyncRoot CRUD
// ---------------------------------------------------------------------------

// UpsertSyncRoot inserts a new sync root or updates an existing one (matched
// by ID).
func (s *Store) UpsertSyncRoot(root *models.SyncRoot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	const query = `
	INSERT OR REPLACE INTO sync_roots (id, local_dir, drive_folder_id, updated_at)
	VALUES (?, ?, ?, CURRENT_TIMESTAMP)
	`

	_, err := s.db.Exec(query, root.ID, root.LocalDir, root.DriveFolderID)
	if err != nil {
		return fmt.Errorf("store: upsert sync root: %w", err)
	}

	return nil
}

// GetSyncRoot retrieves a sync root by ID. Returns sql.ErrNoRows if not found.
func (s *Store) GetSyncRoot(id string) (*models.SyncRoot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	const query = `
	SELECT id, local_dir, drive_folder_id, created_at, updated_at
	FROM sync_roots
	WHERE id = ?
	`

	row := s.db.QueryRow(query, id)
	root := &models.SyncRoot{}
	err := row.Scan(&root.ID, &root.LocalDir, &root.DriveFolderID, &root.CreatedAt, &root.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("store: get sync root: %w", err)
	}
	return root, nil
}

// ListSyncRoots returns all configured sync roots.
func (s *Store) ListSyncRoots() ([]*models.SyncRoot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	const query = `
	SELECT id, local_dir, drive_folder_id, created_at, updated_at
	FROM sync_roots
	`

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("store: list sync roots: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var roots []*models.SyncRoot
	for rows.Next() {
		root := &models.SyncRoot{}
		if err := rows.Scan(&root.ID, &root.LocalDir, &root.DriveFolderID, &root.CreatedAt, &root.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scanning sync root row: %w", err)
		}
		roots = append(roots, root)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating sync roots: %w", err)
	}

	return roots, nil
}

// DeleteSyncRoot removes a sync root by ID. Associated sync_map rows are
// automatically deleted via the ON DELETE CASCADE foreign key constraint.
func (s *Store) DeleteSyncRoot(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	const query = `DELETE FROM sync_roots WHERE id = ?`

	result, err := s.db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("store: delete sync root: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("store: delete sync root: no row with id %q", id)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// scanSyncFile scans a single row from a *sql.Row into a SyncFile.
func scanSyncFile(row *sql.Row) (*models.SyncFile, error) {
	sf := &models.SyncFile{}
	var status string

	err := row.Scan(
		&sf.SyncRootID,
		&sf.LocalPath,
		&sf.RemoteID,
		&sf.LocalHash,
		&sf.RemoteHash,
		&sf.Version,
		&sf.EncryptedDEK,
		&sf.DEKNonce,
		&sf.ContentNonce,
		&sf.Size,
		&sf.ModTime,
		&status,
	)
	if err != nil {
		return nil, err
	}

	sf.SyncStatus = models.SyncStatus(status)
	return sf, nil
}

// scanSyncFiles iterates over a *sql.Rows result set and returns all SyncFiles.
func scanSyncFiles(rows *sql.Rows) ([]*models.SyncFile, error) {
	var files []*models.SyncFile

	for rows.Next() {
		sf := &models.SyncFile{}
		var status string

		err := rows.Scan(
			&sf.SyncRootID,
			&sf.LocalPath,
			&sf.RemoteID,
			&sf.LocalHash,
			&sf.RemoteHash,
			&sf.Version,
			&sf.EncryptedDEK,
			&sf.DEKNonce,
			&sf.ContentNonce,
			&sf.Size,
			&sf.ModTime,
			&status,
		)
		if err != nil {
			return nil, fmt.Errorf("store: scanning row: %w", err)
		}

		sf.SyncStatus = models.SyncStatus(status)
		files = append(files, sf)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: rows iteration: %w", err)
	}

	return files, nil
}
