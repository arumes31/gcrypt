//go:build windows

package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"
)

// This file makes the Fyne GUI work in environments without usable hardware
// OpenGL — most importantly Remote Desktop sessions, but also VMs, headless
// hosts and machines with broken GPU drivers.
//
// Background: Fyne renders through GLFW/OpenGL. gcrypt.exe imports opengl32.dll
// at load time, so the OpenGL implementation is chosen when the process starts,
// before any of our code runs. The only way to substitute a software renderer
// is therefore to have a software opengl32.dll (Mesa3D "llvmpipe") sitting next
// to the executable *at launch*. We can't swap it mid-process, so when we detect
// inadequate OpenGL we stage Mesa next to the exe and re-launch.
//
// Layout the user provides:
//   <dir>\gcrypt.exe
//   <dir>\mesa\opengl32.dll   (+ any Mesa support DLLs)
//
// Behaviour:
//   - opengl32.dll already next to gcrypt.exe  -> already running on Mesa; do nothing.
//   - hardware OpenGL works                    -> use it; do nothing.
//   - hardware OpenGL inadequate + mesa\ staged -> copy mesa\* beside exe, re-exec.
//   - hardware OpenGL inadequate + no mesa\     -> log an actionable message and continue
//     (the window will fail to appear, but the reason is now in the log).
//
// Everything here is failure-safe: if detection is uncertain we assume hardware
// OpenGL is fine, so machines with a working GPU are never affected.

// isRemoteSession reports whether the process runs inside a Remote Desktop (RDP)
// session, which never exposes hardware OpenGL.
func isRemoteSession() bool {
	user32 := syscall.NewLazyDLL("user32.dll")
	getSystemMetrics := user32.NewProc("GetSystemMetrics")
	const smRemoteSession = 0x1000 // SM_REMOTESESSION
	r, _, _ := getSystemMetrics.Call(uintptr(smRemoteSession))
	return r != 0
}

// ensureWorkingOpenGL switches the process to bundled software OpenGL when the
// available OpenGL is inadequate. It returns true if it re-launched the process
// (in which case the caller must exit immediately and let the child take over).
func ensureWorkingOpenGL(log func(string, ...map[string]interface{})) (reexeced bool) {
	if log == nil {
		log = func(string, ...map[string]interface{}) {}
	}

	exe, err := os.Executable()
	if err != nil {
		return false
	}
	exeDir := filepath.Dir(exe)

	// Already running on a bundled opengl32.dll next to the exe? Then software GL
	// is active (or the user deliberately placed one). We still must force the
	// llvmpipe (pure software) driver before Fyne initialises — without it Mesa
	// may pick a hardware-backed gallium driver (e.g. d3d12) that is unavailable
	// over RDP and the window creation crashes.
	if fileExists(filepath.Join(exeDir, "opengl32.dll")) {
		forceSoftwareGLEnv()
		return false
	}

	// Decide whether the current (hardware/system) OpenGL is good enough. RDP is a
	// definitive "no"; otherwise best-effort probe. Probe uncertainty => assume OK.
	inadequate := false
	switch {
	case isRemoteSession():
		inadequate = true
		log("Remote Desktop session detected; hardware OpenGL is unavailable")
	default:
		if ok, known := probeHardwareOpenGL(); known && !ok {
			inadequate = true
			log("Hardware OpenGL appears inadequate for the GUI")
		}
	}
	if !inadequate {
		return false
	}

	// Need software OpenGL. It only takes effect if a Mesa opengl32.dll is staged.
	mesaDir := filepath.Join(exeDir, "mesa")
	if !fileExists(filepath.Join(mesaDir, "opengl32.dll")) {
		log("Software OpenGL is required but no bundled Mesa was found; the window cannot open", map[string]interface{}{
			"expected": filepath.Join(mesaDir, "opengl32.dll"),
			"hint":     "download a Mesa3D (llvmpipe) Windows build and place opengl32.dll in the mesa\\ folder next to gcrypt.exe",
		})
		return false
	}

	// Force llvmpipe (pure software) and a modern reported GL version so Fyne's
	// backend initialises cleanly once Mesa is loaded. These are inherited by the
	// re-launched child via its environment.
	forceSoftwareGLEnv()

	if err := stageMesaBesideExe(mesaDir, exeDir); err != nil {
		log("Failed to stage software OpenGL next to the executable", map[string]interface{}{
			"error": err.Error(),
			"hint":  "ensure the gcrypt.exe folder is writable, or copy mesa\\*.dll next to gcrypt.exe manually",
		})
		return false
	}

	// Re-launch: the fresh process will load the just-staged Mesa opengl32.dll at
	// startup and the GUI will render in software.
	cmd := exec.CommandContext(context.Background(), exe, os.Args[1:]...) // #nosec G204 G702 -- re-launches this same executable with its own args
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Start(); err != nil {
		log("Failed to re-launch with software OpenGL", map[string]interface{}{"error": err.Error()})
		return false
	}
	log("Re-launched with bundled software OpenGL (Mesa llvmpipe) for the GUI")
	return true
}

// stageMesaBesideExe copies every file from the mesa\ folder next to the
// executable so the next launch loads opengl32.dll (and its dependencies) from
// the application directory.
func stageMesaBesideExe(mesaDir, exeDir string) error {
	entries, err := os.ReadDir(mesaDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := copyFile(filepath.Join(mesaDir, e.Name()), filepath.Join(exeDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) // #nosec G304 -- src is a bundled Mesa DLL path under the app dir, not user input
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst) // #nosec G304 -- dst is beside the executable, an app-controlled path
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func setEnvIfUnset(key, value string) {
	if os.Getenv(key) == "" {
		_ = os.Setenv(key, value)
	}
}

// forceSoftwareGLEnv selects Mesa's llvmpipe (pure CPU) driver and advertises a
// modern GL version, so the bundled Mesa renders in software rather than trying
// an unavailable hardware-backed gallium driver. User-set values are preserved.
func forceSoftwareGLEnv() {
	setEnvIfUnset("GALLIUM_DRIVER", "llvmpipe")
	setEnvIfUnset("MESA_GL_VERSION_OVERRIDE", "3.3")
	setEnvIfUnset("MESA_GLSL_VERSION_OVERRIDE", "330")
}

// ---------------------------------------------------------------------------
// Best-effort in-process WGL probe
// ---------------------------------------------------------------------------

// pixelFormatDescriptor mirrors the Win32 PIXELFORMATDESCRIPTOR struct.
type pixelFormatDescriptor struct {
	nSize           uint16
	nVersion        uint16
	dwFlags         uint32
	iPixelType      byte
	cColorBits      byte
	cRedBits        byte
	cRedShift       byte
	cGreenBits      byte
	cGreenShift     byte
	cBlueBits       byte
	cBlueShift      byte
	cAlphaBits      byte
	cAlphaShift     byte
	cAccumBits      byte
	cAccumRedBits   byte
	cAccumGreenBits byte
	cAccumBlueBits  byte
	cAccumAlphaBits byte
	cDepthBits      byte
	cStencilBits    byte
	cAuxBuffers     byte
	iLayerType      byte
	bReserved       byte
	dwLayerMask     uint32
	dwVisibleMask   uint32
	dwDamageMask    uint32
}

// probeHardwareOpenGL inspects the OpenGL-capable pixel format the system offers
// for a throwaway window and reports whether it is hardware-accelerated. It
// returns (ok, known): ok is true when an accelerated (non-generic) format is
// available; known is false when the probe could not determine anything (in
// which case callers must assume hardware OpenGL is fine, to avoid disrupting
// working machines). Using the pixel-format flags avoids creating a real GL
// context and reading foreign-pointer strings.
func probeHardwareOpenGL() (ok bool, known bool) {
	// Never let a probe failure crash startup; treat panics as "unknown".
	defer func() {
		if r := recover(); r != nil {
			ok, known = false, false
		}
	}()

	user32 := syscall.NewLazyDLL("user32.dll")
	gdi32 := syscall.NewLazyDLL("gdi32.dll")

	createWindowExW := user32.NewProc("CreateWindowExW")
	destroyWindow := user32.NewProc("DestroyWindow")
	getDC := user32.NewProc("GetDC")
	releaseDC := user32.NewProc("ReleaseDC")

	choosePixelFormat := gdi32.NewProc("ChoosePixelFormat")
	describePixelFormat := gdi32.NewProc("DescribePixelFormat")

	classStatic, _ := syscall.UTF16PtrFromString("STATIC")
	// A predefined STATIC control window owns a DC we can set a pixel format on,
	// avoiding the need to register our own window class.
	const wsOverlapped = 0x00000000
	hwnd, _, _ := createWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(classStatic)), // #nosec G103 -- required Win32 syscall pointer marshalling
		0,
		wsOverlapped,
		0, 0, 1, 1,
		0, 0, 0, 0,
	)
	if hwnd == 0 {
		return false, false
	}
	defer func() { _, _, _ = destroyWindow.Call(hwnd) }()

	hdc, _, _ := getDC.Call(hwnd)
	if hdc == 0 {
		return false, false
	}
	defer func() { _, _, _ = releaseDC.Call(hwnd, hdc) }()

	const (
		pfdDrawToWindow = 0x00000004
		pfdSupportGL    = 0x00000020
		pfdDoubleBuffer = 0x00000001
	)
	pfd := pixelFormatDescriptor{
		nSize:      uint16(unsafe.Sizeof(pixelFormatDescriptor{})),
		nVersion:   1,
		dwFlags:    pfdDrawToWindow | pfdSupportGL | pfdDoubleBuffer,
		iPixelType: 0, // PFD_TYPE_RGBA
		cColorBits: 32,
		cDepthBits: 24,
		iLayerType: 0, // PFD_MAIN_PLANE
	}
	pf, _, _ := choosePixelFormat.Call(hdc, uintptr(unsafe.Pointer(&pfd))) // #nosec G103 -- required Win32 syscall pointer marshalling
	if pf == 0 {
		// No OpenGL-capable pixel format at all -> definitely inadequate.
		return false, true
	}

	// Describe the chosen format. The PFD_GENERIC_FORMAT flag (without
	// PFD_GENERIC_ACCELERATED) marks Microsoft's generic GDI software renderer,
	// which only supports OpenGL 1.1 — exactly what RDP exposes and far too old
	// for Fyne. A hardware ICD reports neither flag.
	var desc pixelFormatDescriptor
	r, _, _ := describePixelFormat.Call(hdc, pf, unsafe.Sizeof(desc), uintptr(unsafe.Pointer(&desc))) // #nosec G103 -- required Win32 syscall pointer marshalling
	if r == 0 {
		return false, false // couldn't determine
	}
	const (
		pfdGenericFormat      = 0x00000040
		pfdGenericAccelerated = 0x00001000
	)
	generic := desc.dwFlags&pfdGenericFormat != 0
	accelerated := desc.dwFlags&pfdGenericAccelerated != 0
	hardwareOK := !generic || accelerated
	return hardwareOK, true
}
