//go:build windows

package service

import (
	"fmt"
	"syscall"
	"unsafe"
)

// This file wraps the Windows Data Protection API (DPAPI) — CryptProtectData /
// CryptUnprotectData — which encrypts data tied to the current Windows user
// account. It is used to store the passphrase-derived master key so the app can
// auto-unlock on startup without re-prompting. The protected blob cannot be
// decrypted by another user or on another machine.

var (
	crypt32DLL    = syscall.NewLazyDLL("crypt32.dll")
	kernel32DLL   = syscall.NewLazyDLL("kernel32.dll")
	procProtect   = crypt32DLL.NewProc("CryptProtectData")
	procUnprotect = crypt32DLL.NewProc("CryptUnprotectData")
	procLocalFree = kernel32DLL.NewProc("LocalFree")
)

// cryptProtectUIForbidden disables any UI prompt; the call fails instead of
// blocking if user interaction would be required.
const cryptProtectUIForbidden = 0x1

// dataBlob mirrors the Win32 DATA_BLOB structure.
type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(d []byte) dataBlob {
	if len(d) == 0 {
		return dataBlob{}
	}
	return dataBlob{cbData: uint32(len(d)), pbData: &d[0]} // #nosec G115 -- len(d) is a small in-memory secret blob, far below uint32 max
}

// bytes copies the blob's contents into a Go-managed slice.
func (b dataBlob) bytes() []byte {
	if b.cbData == 0 || b.pbData == nil {
		return nil
	}
	out := make([]byte, b.cbData)
	copy(out, unsafe.Slice(b.pbData, b.cbData)) // #nosec G103 -- required to copy the DPAPI-allocated blob into Go memory
	return out
}

// protectData encrypts plaintext with DPAPI bound to the current user.
func protectData(plaintext []byte) ([]byte, error) {
	in := newBlob(plaintext)
	var out dataBlob

	ret, _, err := procProtect.Call(
		uintptr(unsafe.Pointer(&in)), // #nosec G103 -- required Win32 DPAPI syscall pointer marshalling
		0,                            // szDataDescr
		0,                            // pOptionalEntropy
		0,                            // pvReserved
		0,                            // pPromptStruct
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)), // #nosec G103 -- required Win32 DPAPI syscall pointer marshalling
	)
	if ret == 0 {
		return nil, fmt.Errorf("service: CryptProtectData failed: %w", err)
	}
	defer func() { _, _, _ = procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData))) }() // #nosec G103 -- required Win32 syscall pointer marshalling

	return out.bytes(), nil
}

// unprotectData reverses protectData. It fails if the blob was produced by a
// different user or machine.
func unprotectData(ciphertext []byte) ([]byte, error) {
	in := newBlob(ciphertext)
	var out dataBlob

	ret, _, err := procUnprotect.Call(
		uintptr(unsafe.Pointer(&in)), // #nosec G103 -- required Win32 DPAPI syscall pointer marshalling
		0,                            // ppszDataDescr
		0,                            // pOptionalEntropy
		0,                            // pvReserved
		0,                            // pPromptStruct
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)), // #nosec G103 -- required Win32 DPAPI syscall pointer marshalling
	)
	if ret == 0 {
		return nil, fmt.Errorf("service: CryptUnprotectData failed: %w", err)
	}
	defer func() { _, _, _ = procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData))) }() // #nosec G103 -- required Win32 syscall pointer marshalling

	return out.bytes(), nil
}
