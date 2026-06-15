//go:build !windows

package service

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

// PromptPassphrase prompts the user for their encryption passphrase via the
// terminal. On non-Windows platforms, this is the only implementation; on
// Windows, the native CredUI dialog is preferred with this as a fallback.
func PromptPassphrase(parentHWND uintptr) (string, error) {
	fmt.Print("Enter passphrase: ")
	passBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("service: read passphrase from terminal: %w", err)
	}
	return string(passBytes), nil
}
