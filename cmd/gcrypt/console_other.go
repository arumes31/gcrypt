//go:build !windows

package main

// hideConsoleIfOwned is a no-op on non-Windows platforms.
func hideConsoleIfOwned() {}
