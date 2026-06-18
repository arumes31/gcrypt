package main

import "golang.org/x/sys/windows"

// singleInstanceMutex is the session-local named mutex that guards against a
// second gcrypt running in the same Windows session (two copies would fight
// over the same sync pairs). The handle is intentionally held for the whole
// process lifetime and released automatically by the OS on exit.
const singleInstanceMutex = `gcrypt-single-instance-9c1f`

var singleInstanceHandle windows.Handle

// acquireSingleInstance reports whether this process may run. It returns false
// when another gcrypt instance already holds the named mutex. If the guard
// can't be created for any other reason it fails open (returns true) so a
// permissions quirk never blocks the app from starting.
func acquireSingleInstance() bool {
	name, err := windows.UTF16PtrFromString(singleInstanceMutex)
	if err != nil {
		return true
	}
	h, err := windows.CreateMutex(nil, false, name)
	if err == windows.ERROR_ALREADY_EXISTS {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
		return false
	}
	if h == 0 {
		return true // couldn't create the guard — allow startup
	}
	singleInstanceHandle = h
	return true
}
