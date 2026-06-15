-- gcrypt database schema V2
-- Supports multiple sync roots

-- Schema version tracking
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER PRIMARY KEY,
    applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Sync roots: one row per configured sync pair
CREATE TABLE IF NOT EXISTS sync_roots (
    id TEXT PRIMARY KEY,                -- UUID v4, matches config.SyncPair.ID
    local_dir TEXT NOT NULL,            -- Local directory path
    drive_folder_id TEXT NOT NULL,      -- Google Drive folder ID
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Sync map: one row per tracked file
-- Composite primary key: (sync_root_id, local_path) to allow same relative path in different roots
CREATE TABLE IF NOT EXISTS sync_map (
    sync_root_id TEXT NOT NULL,         -- FK to sync_roots.id
    local_path TEXT NOT NULL,           -- Relative path within sync dir
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

-- Indexes
CREATE INDEX IF NOT EXISTS idx_sync_map_remote_id ON sync_map(remote_id);
CREATE INDEX IF NOT EXISTS idx_sync_map_status ON sync_map(sync_status);
CREATE INDEX IF NOT EXISTS idx_sync_map_root_status ON sync_map(sync_root_id, sync_status);

-- Insert schema version
INSERT INTO schema_version (version) VALUES (2);
