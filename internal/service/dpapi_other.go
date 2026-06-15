//go:build !windows

package service

import "fmt"

// DPAPI is Windows-only. On other platforms these stubs report that the
// remember-passphrase feature is unavailable, so callers degrade to prompting.

func protectData(plaintext []byte) ([]byte, error) {
	return nil, fmt.Errorf("service: DPAPI not supported on this platform")
}

func unprotectData(ciphertext []byte) ([]byte, error) {
	return nil, fmt.Errorf("service: DPAPI not supported on this platform")
}
