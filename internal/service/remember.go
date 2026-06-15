package service

import (
	"fmt"
	"os"
	"path/filepath"
)

// remember.go implements the "remember passphrase / auto-unlock" store. The
// passphrase-derived master key is protected with the platform DPAPI wrapper
// (Windows-only; see dpapi_windows.go) and written to disk so the app can
// unlock on startup without prompting. The on-disk blob is bound to the current
// Windows user and is useless on another account or machine.

// rememberedKeyPath returns the path of the DPAPI-protected master-key file.
func rememberedKeyPath() string {
	return filepath.Join(appDataDir(), "gcrypt", "unlock.bin")
}

// saveRememberedKey DPAPI-protects the master key and writes it to disk.
func saveRememberedKey(masterKey []byte) error {
	protected, err := protectData(masterKey)
	if err != nil {
		return err
	}
	path := rememberedKeyPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("service: creating remember dir: %w", err)
	}
	if err := os.WriteFile(path, protected, 0600); err != nil {
		return fmt.Errorf("service: writing remembered key: %w", err)
	}
	return nil
}

// loadRememberedKey reads and DPAPI-unprotects the stored master key.
func loadRememberedKey() ([]byte, error) {
	data, err := os.ReadFile(rememberedKeyPath())
	if err != nil {
		return nil, err
	}
	return unprotectData(data)
}

// clearRememberedKey deletes the stored key. It is not an error if no key
// exists.
func clearRememberedKey() error {
	if err := os.Remove(rememberedKeyPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("service: removing remembered key: %w", err)
	}
	return nil
}

// rememberedKeyExists reports whether a remembered key file is present.
func rememberedKeyExists() bool {
	_, err := os.Stat(rememberedKeyPath())
	return err == nil
}
