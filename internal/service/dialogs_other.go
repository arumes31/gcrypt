//go:build !windows

package service

// Native dialogs are Windows-only. These stubs let the package compile on other
// platforms; the tray-driven setup flow that uses them is Windows-only in
// practice.

func messageBox(title, text string, flags uint32) int {
	return mbIDOK
}

func pickFolder(title string) (string, bool) {
	return "", false
}

func promptText(title, label, initial string, password bool) (string, bool) {
	return "", false
}
