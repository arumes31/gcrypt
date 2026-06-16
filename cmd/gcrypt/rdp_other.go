//go:build !windows

package main

// isRemoteSession always reports false on non-Windows platforms.
func isRemoteSession() bool { return false }

// enableSoftwareOpenGL is a no-op on non-Windows platforms.
func enableSoftwareOpenGL() {}
