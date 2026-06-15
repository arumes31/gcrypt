//go:build windows

package service

import "fmt"

// PromptPassphrase displays a native password dialog prompting the user for
// their encryption passphrase and returns the entered text.
//
// It uses the app's own native input dialog (promptText, see dialogs_windows.go)
// — the same one used by the setup flow — which reads the edit control directly
// via GetWindowText.
//
// parentHWND is currently unused (the dialog has no parent); kept for API
// compatibility with the non-Windows implementation.
func PromptPassphrase(parentHWND uintptr) (string, error) {
	passphrase, ok := promptText("gcrypt", "Enter your gcrypt encryption passphrase:", "", true)
	if !ok {
		return "", fmt.Errorf("service: passphrase entry cancelled")
	}
	return passphrase, nil
}
