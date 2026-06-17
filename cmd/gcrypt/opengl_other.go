//go:build !windows

package main

// isRemoteSession always reports false on non-Windows platforms.
func isRemoteSession() bool { return false }

// ensureWorkingOpenGL is a no-op on non-Windows platforms (the RDP / software
// OpenGL handling is Windows-specific).
func ensureWorkingOpenGL(func(string, ...map[string]interface{})) bool { return false }
