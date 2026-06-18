package service

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/arumes31/gcrypt/internal/config"
)

// backupFiles maps backup file names to their on-disk source paths. These two
// files are exactly what runConnectExistingSetup imports to recover/join an
// encrypted sync on another machine: the config (Drive folder, OAuth client,
// passphrase verifier) and the salt used to derive the master key from the
// passphrase. Note: the master key itself is never written to disk — recovery
// still requires the passphrase.
func backupFiles() map[string]string {
	return map[string]string{
		"config.yaml": config.ConfigPath(),
		"salt.bin":    filepath.Join(appDataDir(), "gcrypt", "salt.bin"),
	}
}

// ExportBackup copies the recovery files into destDir, returning the names
// copied. It is the deliberate, user-initiated counterpart to the
// connect-to-existing import flow, giving users a disaster-recovery copy of the
// identity needed (together with their passphrase) to restore access.
func ExportBackup(destDir string) ([]string, error) {
	if destDir == "" {
		return nil, fmt.Errorf("service: no backup destination selected")
	}
	if err := os.MkdirAll(destDir, 0700); err != nil {
		return nil, fmt.Errorf("service: creating backup dir: %w", err)
	}

	var copied []string
	for name, src := range backupFiles() {
		if _, err := os.Stat(src); err != nil {
			return copied, fmt.Errorf("service: %s is not available to back up: %w", name, err)
		}
		if err := copyFileSecure(src, filepath.Join(destDir, name)); err != nil {
			return copied, fmt.Errorf("service: copying %s: %w", name, err)
		}
		copied = append(copied, name)
	}
	return copied, nil
}

// copyFileSecure copies src to dst, creating dst with owner-only permissions.
func copyFileSecure(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst) // don't leave a partial/corrupt backup behind
		return err
	}
	return out.Close()
}
