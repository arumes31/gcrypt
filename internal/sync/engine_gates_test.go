package sync

import (
	"testing"

	"github.com/arumes31/gcrypt/internal/config"
)

// TestDirectionGates locks in the per-direction permission table that the
// reconcile/scan logic relies on. A regression here would silently change which
// operations a sync mode performs (e.g. a mirror starting to delete remotely).
func TestDirectionGates(t *testing.T) {
	type want struct{ up, down, remoteDel, localDel bool }
	cases := []struct {
		name string
		pair config.SyncPair
		want want
	}{
		{"two-way (default)", config.SyncPair{}, want{true, true, true, true}},
		{"two-way explicit", config.SyncPair{SyncDirection: config.SyncDirTwoWay}, want{true, true, true, true}},
		{"upload-only", config.SyncPair{SyncDirection: config.SyncDirUploadOnly}, want{true, false, true, false}},
		{"download-only", config.SyncPair{SyncDirection: config.SyncDirDownloadOnly}, want{false, true, false, true}},
		{"mirror (backup)", config.SyncPair{SyncDirection: config.SyncDirMirror}, want{true, false, false, false}},
		{"legacy ForwardOnly", config.SyncPair{ForwardOnly: true}, want{true, false, true, false}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := c.pair
			e := &Engine{pair: &p}
			if got := e.allowsUpload(); got != c.want.up {
				t.Errorf("allowsUpload = %v, want %v", got, c.want.up)
			}
			if got := e.allowsDownload(); got != c.want.down {
				t.Errorf("allowsDownload = %v, want %v", got, c.want.down)
			}
			if got := e.allowsRemoteDelete(); got != c.want.remoteDel {
				t.Errorf("allowsRemoteDelete = %v, want %v", got, c.want.remoteDel)
			}
			if got := e.allowsLocalDelete(); got != c.want.localDel {
				t.Errorf("allowsLocalDelete = %v, want %v", got, c.want.localDel)
			}
		})
	}
}
