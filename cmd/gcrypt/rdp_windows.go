//go:build windows

package main

import (
	"os"
	"syscall"
)

// isRemoteSession reports whether the current process is running inside a
// Remote Desktop (RDP) session.
//
// Why this matters: an RDP session exposes only Microsoft's Basic Display
// adapter, which provides no hardware OpenGL (OpenGL 1.1 at best). Fyne renders
// through GLFW/OpenGL, so without a software OpenGL implementation its window
// creation fails with "WGL: The driver does not appear to support OpenGL" and
// the flyout window silently never appears (the tray, which needs no OpenGL,
// still works).
func isRemoteSession() bool {
	user32 := syscall.NewLazyDLL("user32.dll")
	getSystemMetrics := user32.NewProc("GetSystemMetrics")
	const smRemoteSession = 0x1000 // SM_REMOTESESSION
	r, _, _ := getSystemMetrics.Call(uintptr(smRemoteSession))
	return r != 0
}

// enableSoftwareOpenGL points the process at a bundled Mesa3D (llvmpipe)
// software OpenGL renderer so the GUI works without a GPU (RDP, VMs, headless).
// It takes effect only when a Mesa opengl32.dll sits next to gcrypt.exe; a real
// hardware driver ignores these variables. Existing values are left untouched so
// an explicit user override always wins.
func enableSoftwareOpenGL() {
	setEnvIfUnset("GALLIUM_DRIVER", "llvmpipe")
	// Mesa can otherwise advertise a conservative GL version; ask for a modern
	// core profile so Fyne's GL backend initialises cleanly.
	setEnvIfUnset("MESA_GL_VERSION_OVERRIDE", "3.3")
	setEnvIfUnset("MESA_GLSL_VERSION_OVERRIDE", "330")
}

func setEnvIfUnset(key, value string) {
	if os.Getenv(key) == "" {
		_ = os.Setenv(key, value)
	}
}
