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
	"context"
	"os"
	"os/exec"
	"strings"
)

// sameFile reports whether two paths refer to the same file on disk. It falls
// back to a case-insensitive string compare if either path cannot be stat'd.
func sameFile(a, b string) bool {
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	if aerr == nil && berr == nil {
		return os.SameFile(ai, bi)
	}
	return strings.EqualFold(a, b)
}

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
			_, _ = os.Stderr.WriteString("gccwrap: cannot find real gcc on PATH: " + err.Error() + "\n")
			os.Exit(1)
		}
		// Guard against recursion: if the wrapper was named gcc.exe and placed on
		// PATH, LookPath("gcc") could resolve back to this binary. Refuse rather
		// than fork-bomb.
		if self, serr := os.Executable(); serr == nil && sameFile(self, p) {
			_, _ = os.Stderr.WriteString("gccwrap: refusing to invoke self (resolved gcc is this wrapper at " + p + "); set REAL_CC to the real gcc\n")
			os.Exit(1)
		}
		real = p
	}

	cmd := exec.CommandContext(context.Background(), real, args...) // #nosec G204 G702 -- this wrapper exists precisely to forward args to the real gcc (REAL_CC/PATH)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		_, _ = os.Stderr.WriteString("gccwrap: " + err.Error() + "\n")
		os.Exit(1)
	}
}
