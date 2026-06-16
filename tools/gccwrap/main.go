// Command gccwrap is a thin wrapper around the system gcc that strips the
// clang-only "-Qunused-arguments" flag before forwarding to the real compiler.
//
// Why this exists: Go 1.26.x's cgo driver probes the C compiler and, on this
// machine, incorrectly decides the MinGW gcc 16.x supports clang's
// "-Qunused-arguments" flag. gcc 16 rejects it, breaking every cgo build
// (runtime/cgo fails to compile). Fyne requires cgo (GLFW/OpenGL) on Windows,
// so we route CC through this wrapper.
//
// Usage: set CC to the built gccwrap.exe. The wrapper locates the real gcc via
// PATH (it must not be named gcc.exe itself, to avoid recursion).
package main

import (
	"os"
	"os/exec"
)

func main() {
	// Drop the offending flag; forward everything else verbatim.
	args := make([]string, 0, len(os.Args)-1)
	for _, a := range os.Args[1:] {
		if a == "-Qunused-arguments" {
			continue
		}
		args = append(args, a)
	}

	// Resolve the real gcc. REAL_CC overrides; otherwise the first gcc on PATH.
	real := os.Getenv("REAL_CC")
	if real == "" {
		p, err := exec.LookPath("gcc")
		if err != nil {
			os.Stderr.WriteString("gccwrap: cannot find real gcc on PATH: " + err.Error() + "\n")
			os.Exit(1)
		}
		real = p
	}

	cmd := exec.Command(real, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		os.Stderr.WriteString("gccwrap: " + err.Error() + "\n")
		os.Exit(1)
	}
}
