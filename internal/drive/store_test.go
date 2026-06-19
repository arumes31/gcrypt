package drive

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/arumes31/gcrypt/internal/models"
)

// TestUpsertSyncRootPreservesSyncMap is a regression test for the bug where
// UpsertSyncRoot used INSERT OR REPLACE. SQLite implements REPLACE as
// DELETE-then-INSERT on a PK conflict, and sync_map references sync_roots with
// ON DELETE CASCADE — so re-upserting an existing root wiped every sync_map row
// for that root. Because the app upserts each pair's root on every startup, the
// whole sync map was erased on each launch and all files re-uploaded. Upserting
// the same root must leave its sync_map rows intact.
func TestUpsertSyncRootPreservesSyncMap(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := NewStore(dbPath, "")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	const rootID = "root-1"

	root := &models.SyncRoot{ID: rootID, LocalDir: `C:\data`, DriveFolderID: "drive-1"}
	if err := store.UpsertSyncRoot(ctx, root); err != nil {
		t.Fatalf("UpsertSyncRoot (initial): %v", err)
	}

	// Record several tracked files under that root.
	for _, p := range []string{`a.txt`, `sub\b.txt`, `sub\c.txt`} {
		sf := &models.SyncFile{
			SyncRootID: rootID,
			LocalPath:  p,
			RemoteID:   "rid-" + p,
			LocalHash:  "hash-" + p,
			SyncStatus: models.SyncStatusSynced,
			Version:    1,
		}
		if err := store.PutSyncFile(ctx, sf); err != nil {
			t.Fatalf("PutSyncFile %q: %v", p, err)
		}
	}

	before, err := store.ListAll(ctx, rootID)
	if err != nil {
		t.Fatalf("ListAll (before): %v", err)
	}
	if len(before) != 3 {
		t.Fatalf("expected 3 rows before re-upsert, got %d", len(before))
	}

	// Re-upsert the same root, exactly as the app does on every startup.
	root.DriveFolderID = "drive-1-updated"
	if err := store.UpsertSyncRoot(ctx, root); err != nil {
		t.Fatalf("UpsertSyncRoot (re-upsert): %v", err)
	}

	after, err := store.ListAll(ctx, rootID)
	if err != nil {
		t.Fatalf("ListAll (after): %v", err)
	}
	if len(after) != 3 {
		t.Fatalf("sync_map rows were wiped by re-upsert: expected 3, got %d", len(after))
	}

	// The root's mutable fields should have been updated in place.
	got, err := store.GetSyncRoot(ctx, rootID)
	if err != nil {
		t.Fatalf("GetSyncRoot: %v", err)
	}
	if got.DriveFolderID != "drive-1-updated" {
		t.Fatalf("expected drive_folder_id updated to %q, got %q", "drive-1-updated", got.DriveFolderID)
	}
}
