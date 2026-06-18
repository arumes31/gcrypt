//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// hideConsoleIfOwned hides the console window when gcrypt owns it (i.e. it was
// launched by double-clicking, which spawns a dedicated console). It deliberately
// does NOT hide the console when gcrypt was started from an existing terminal —
// in that case the console is shared with the parent shell and hiding it would
// hide the user's own window.
//
// This makes the tray app run without a stray black console window even when the
// binary is compiled as a console application. For a production build, also link
// with `-ldflags -H=windowsgui` so no console is ever created.
func hideConsoleIfOwned() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	user32 := syscall.NewLazyDLL("user32.dll")
	getConsoleWindow := kernel32.NewProc("GetConsoleWindow")
	getConsoleProcessList := kernel32.NewProc("GetConsoleProcessList")
	showWindow := user32.NewProc("ShowWindow")

	hwnd, _, _ := getConsoleWindow.Call()
	if hwnd == 0 {
		// No console attached (e.g. built with -H=windowsgui).
		return
	}

	// If only one process is attached to this console, it's ours alone and safe
	// to hide. More than one means we share a parent terminal — leave it.
	var pids [2]uint32
	n, _, _ := getConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids))) // #nosec G103 -- required Win32 syscall pointer marshalling
	if int(n) == 1 {
		const swHide = 0
		_, _, _ = showWindow.Call(hwnd, swHide)
	}
}
