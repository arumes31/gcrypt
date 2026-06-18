//go:build !windows

package main

// acquireSingleInstance is a no-op off Windows (the single-instance guard is
// implemented with a Windows named mutex).
func acquireSingleInstance() bool { return true }
