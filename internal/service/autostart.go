package service

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows/registry"
)

// autostartRegPath is the Windows registry key for user-level startup programs.
const autostartRegPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// autostartValueName is the registry value name for the gcrypt entry.
const autostartValueName = "gcrypt"

// EnableAutoStart registers the current executable to run on Windows boot
// by writing an entry to the current user's Run registry key.
func EnableAutoStart() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("service: get executable path: %w", err)
	}

	key, _, err := registry.CreateKey(registry.CURRENT_USER, autostartRegPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("service: open registry key: %w", err)
	}
	defer func() { _ = key.Close() }()

	// Quote the path so Windows parses it correctly when the executable lives in
	// a directory containing spaces (e.g. "C:\Program Files\..."). An unquoted
	// path with spaces can be misinterpreted by the Run-key launcher.
	if err := key.SetStringValue(autostartValueName, `"`+exePath+`"`); err != nil {
		return fmt.Errorf("service: set registry value: %w", err)
	}

	return nil
}

// DisableAutoStart removes the gcrypt entry from the Windows startup registry
// key so it no longer starts automatically on boot.
func DisableAutoStart() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, autostartRegPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("service: open registry key: %w", err)
	}
	defer func() { _ = key.Close() }()

	if err := key.DeleteValue(autostartValueName); err != nil {
		return fmt.Errorf("service: delete registry value: %w", err)
	}

	return nil
}

// IsAutoStartEnabled checks whether gcrypt is registered to start on Windows
// boot. Returns true if the registry entry exists, false if it does not.
func IsAutoStartEnabled() (bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, autostartRegPath, registry.QUERY_VALUE)
	if err != nil {
		return false, fmt.Errorf("service: open registry key: %w", err)
	}
	defer func() { _ = key.Close() }()

	_, _, err = key.GetStringValue(autostartValueName)
	if err != nil {
		if err == registry.ErrNotExist {
			return false, nil
		}
		return false, fmt.Errorf("service: read registry value: %w", err)
	}

	return true, nil
}
