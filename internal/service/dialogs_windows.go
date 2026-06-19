//go:build windows

package service

import (
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

// dialogs_windows.go provides small native Win32 dialogs used by the tray-driven
// setup flow: a message box, a folder picker (SHBrowseForFolder), and a modal
// single-line text-input dialog (custom window + EDIT control). These replace
// the old CLI --setup wizard so all configuration happens through the tray.

var (
	user32Lazy  = syscall.NewLazyDLL("user32.dll")
	shell32Lazy = syscall.NewLazyDLL("shell32.dll")
	gdi32Lazy   = syscall.NewLazyDLL("gdi32.dll")
	ole32Lazy   = syscall.NewLazyDLL("ole32.dll")

	pRegisterClassExDlg = user32Lazy.NewProc("RegisterClassExW")
	pCreateWindowExDlg  = user32Lazy.NewProc("CreateWindowExW")
	pDefWindowProcDlg   = user32Lazy.NewProc("DefWindowProcW")
	pDestroyWindowDlg   = user32Lazy.NewProc("DestroyWindow")
	pGetMessageDlg      = user32Lazy.NewProc("GetMessageW")
	pTranslateMsgDlg    = user32Lazy.NewProc("TranslateMessage")
	pDispatchMsgDlg     = user32Lazy.NewProc("DispatchMessageW")
	pPostQuitDlg        = user32Lazy.NewProc("PostQuitMessage")
	pPostMessageDlg     = user32Lazy.NewProc("PostMessageW")
	pSendMessageDlg     = user32Lazy.NewProc("SendMessageW")
	pGetWindowTextDlg   = user32Lazy.NewProc("GetWindowTextW")
	pGetWindowTextLen   = user32Lazy.NewProc("GetWindowTextLengthW")
	pShowWindowDlg      = user32Lazy.NewProc("ShowWindow")
	pSetForegroundDlg   = user32Lazy.NewProc("SetForegroundWindow")
	pMessageBoxDlg      = user32Lazy.NewProc("MessageBoxW")
	pLoadCursorDlg      = user32Lazy.NewProc("LoadCursorW")
	pIsDialogMessageDlg = user32Lazy.NewProc("IsDialogMessageW")

	pSHBrowseForFolder  = shell32Lazy.NewProc("SHBrowseForFolderW")
	pSHGetPathFromIDLst = shell32Lazy.NewProc("SHGetPathFromIDListW")

	pGetStockObjectDlg = gdi32Lazy.NewProc("GetStockObject")
	pCoTaskMemFreeDlg  = ole32Lazy.NewProc("CoTaskMemFree")
)

// Win32 constants used by the dialogs.
const (
	wsOverlapped  = 0x00000000
	wsCaption     = 0x00C00000
	wsSysMenu     = 0x00080000
	wsVisible     = 0x10000000
	wsChild       = 0x40000000
	wsTabStop     = 0x00010000
	wsBorder      = 0x00800000
	wsExTopmost   = 0x00000008
	wsExDlgFrame  = 0x00000001
	wsExClcontrol = 0x00010000

	esAutoHScroll = 0x0080
	esPassword    = 0x0020
	ssLeft        = 0x00000000
	bsDefPush     = 0x00000001
	bsPush        = 0x00000000

	wmCommand       = 0x0111
	wmClose         = 0x0010
	wmDestroy       = 0x0002
	wmSetFont       = 0x0030
	swShow          = 5
	cwUseDefault    = 0x80000000
	idcArrow        = 32512
	defaultGUIFont  = 17
	colorWindowBrsh = 6 // COLOR_WINDOW (5) + 1

	idOK     = 1
	idCancel = 2
	idEdit   = 100
)

// wndClassExDlg mirrors the Win32 WNDCLASSEXW structure.
type wndClassExDlg struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

// msgDlg mirrors the Win32 MSG structure.
type msgDlg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	ptX     int32
	ptY     int32
}

// browseInfo mirrors the Win32 BROWSEINFOW structure.
type browseInfo struct {
	hwndOwner      uintptr
	pidlRoot       uintptr
	pszDisplayName *uint16
	lpszTitle      *uint16
	ulFlags        uint32
	lpfn           uintptr
	lParam         uintptr
	iImage         int32
}

// messageBox shows a native MessageBox. flags are standard MB_* values.
func messageBox(title, text string, flags uint32) int {
	t, _ := syscall.UTF16PtrFromString(title)
	m, _ := syscall.UTF16PtrFromString(text)
	r, _, _ := pMessageBoxDlg.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), uintptr(flags)) // #nosec G103 -- required Win32 syscall pointer marshalling
	return int(r)
}

// pickFolder shows the native folder browser and returns the chosen path. The
// second return is false if the user cancelled.
func pickFolder(title string) (string, bool) {
	const bifReturnOnlyFSDirs = 0x00000001
	const bifNewDialogStyle = 0x00000040

	titlePtr, _ := syscall.UTF16PtrFromString(title)
	displayName := make([]uint16, 260)

	bi := browseInfo{
		lpszTitle: titlePtr,
		ulFlags:   bifReturnOnlyFSDirs | bifNewDialogStyle,
		pszDisplayName: func() *uint16 {
			if len(displayName) > 0 {
				return &displayName[0]
			}
			return nil
		}(),
	}

	pidl, _, _ := pSHBrowseForFolder.Call(uintptr(unsafe.Pointer(&bi))) // #nosec G103 -- required Win32 syscall pointer marshalling
	if pidl == 0 {
		return "", false
	}
	defer func() { _, _, _ = pCoTaskMemFreeDlg.Call(pidl) }()

	pathBuf := make([]uint16, 260)
	ret, _, _ := pSHGetPathFromIDLst.Call(pidl, uintptr(unsafe.Pointer(&pathBuf[0]))) // #nosec G103 -- required Win32 syscall pointer marshalling
	if ret == 0 {
		return "", false
	}
	return syscall.UTF16ToString(pathBuf), true
}

// ---------------------------------------------------------------------------
// Modal text-input dialog
// ---------------------------------------------------------------------------

// dlgMu serializes text dialogs so the package-global state below is safe.
var dlgMu sync.Mutex

type textDialogState struct {
	editHwnd uintptr
	result   string
	ok       bool
}

var curTextDlg *textDialogState

// textDlgClassRegistered ensures the window class is registered only once.
var (
	textDlgClassOnce sync.Once
	textDlgClassName *uint16
	textDlgProcPtr   uintptr
)

func registerTextDlgClass() {
	textDlgClassOnce.Do(func() {
		textDlgClassName, _ = syscall.UTF16PtrFromString("gcryptTextDialog")
		textDlgProcPtr = syscall.NewCallback(textDlgProc)
		cursor, _, _ := pLoadCursorDlg.Call(0, uintptr(idcArrow))
		wc := wndClassExDlg{
			style:         0,
			lpfnWndProc:   textDlgProcPtr,
			hCursor:       cursor,
			hbrBackground: colorWindowBrsh,
			lpszClassName: textDlgClassName,
		}
		wc.cbSize = uint32(unsafe.Sizeof(wc))
		_, _, _ = pRegisterClassExDlg.Call(uintptr(unsafe.Pointer(&wc))) // #nosec G103 -- required Win32 syscall pointer marshalling
	})
}

func textDlgProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmCommand:
		id := wParam & 0xFFFF
		switch id {
		case idOK:
			if curTextDlg != nil {
				curTextDlg.result = getControlText(curTextDlg.editHwnd)
				curTextDlg.ok = true
			}
			_, _, _ = pPostMessageDlg.Call(hwnd, wmClose, 0, 0)

			return 0
		case idCancel:
			_, _, _ = pPostMessageDlg.Call(hwnd, wmClose, 0, 0)

			return 0
		}
	case wmClose:
		_, _, _ = pDestroyWindowDlg.Call(hwnd)
		return 0
	case wmDestroy:
		_, _, _ = pPostQuitDlg.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProcDlg.Call(hwnd, msg, wParam, lParam)
	return r
}

func getControlText(hwnd uintptr) string {
	n, _, _ := pGetWindowTextLen.Call(hwnd)
	length := int(n)
	if length <= 0 {
		return ""
	}
	buf := make([]uint16, length+1)
	_, _, _ = pGetWindowTextDlg.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(length+1)) // #nosec G103 -- required Win32 syscall pointer marshalling
	return syscall.UTF16ToString(buf)
}

func createControl(class, text string, style uint32, x, y, w, h int, parent uintptr, id int, font uintptr) uintptr {
	classPtr, _ := syscall.UTF16PtrFromString(class)
	textPtr, _ := syscall.UTF16PtrFromString(text)
	hwnd, _, _ := pCreateWindowExDlg.Call(
		0,
		uintptr(unsafe.Pointer(classPtr)), // #nosec G103 -- required Win32 syscall pointer marshalling
		uintptr(unsafe.Pointer(textPtr)),  // #nosec G103 -- required Win32 syscall pointer marshalling
		uintptr(style),
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		parent,
		uintptr(id),
		0,
		0,
	)
	if font != 0 {
		_, _, _ = pSendMessageDlg.Call(hwnd, wmSetFont, font, 1)
	}
	return hwnd
}

// promptText shows a modal single-line input dialog. When password is true the
// entered text is masked. The second return is false if the user cancelled.
func promptText(title, label, initial string, password bool) (string, bool) {
	dlgMu.Lock()
	defer dlgMu.Unlock()

	// Win32 windows have thread affinity; keep the dialog on one OS thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	registerTextDlgClass()

	font, _, _ := pGetStockObjectDlg.Call(uintptr(defaultGUIFont))

	titlePtr, _ := syscall.UTF16PtrFromString(title)
	const width, height = 420, 170

	hwnd, _, _ := pCreateWindowExDlg.Call(
		wsExTopmost|wsExDlgFrame,
		uintptr(unsafe.Pointer(textDlgClassName)), // #nosec G103 -- required Win32 syscall pointer marshalling
		uintptr(unsafe.Pointer(titlePtr)),         // #nosec G103 -- required Win32 syscall pointer marshalling
		wsOverlapped|wsCaption|wsSysMenu,
		cwUseDefault, cwUseDefault, width, height,
		0, 0, 0, 0,
	)
	if hwnd == 0 {
		return "", false
	}

	state := &textDialogState{}
	curTextDlg = state
	defer func() { curTextDlg = nil }()

	// Label.
	createControl("STATIC", label, wsChild|wsVisible|ssLeft, 15, 15, width-40, 40, hwnd, 0, font)

	// Edit control.
	editStyle := uint32(wsChild | wsVisible | wsBorder | wsTabStop | esAutoHScroll)
	if password {
		editStyle |= esPassword
	}
	edit := createControl("EDIT", initial, editStyle, 15, 60, width-45, 24, hwnd, idEdit, font)
	state.editHwnd = edit

	// OK / Cancel buttons.
	createControl("BUTTON", "OK", wsChild|wsVisible|wsTabStop|bsDefPush, width-200, 100, 85, 28, hwnd, idOK, font)
	createControl("BUTTON", "Cancel", wsChild|wsVisible|wsTabStop|bsPush, width-105, 100, 85, 28, hwnd, idCancel, font)

	_, _, _ = pShowWindowDlg.Call(hwnd, swShow)
	_, _, _ = pSetForegroundDlg.Call(hwnd)

	// Modal message loop.
	var msg msgDlg
	for {
		r, _, _ := pGetMessageDlg.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0) // #nosec G103 -- required Win32 syscall pointer marshalling
		if int32(r) <= 0 {                                                     // #nosec G115 -- GetMessage's documented return is -1/0/>0; int32 is the correct contract
			break
		}
		// Let the dialog manager handle Tab/Enter/Esc navigation.
		handled, _, _ := pIsDialogMessageDlg.Call(hwnd, uintptr(unsafe.Pointer(&msg))) // #nosec G103 -- required Win32 syscall pointer marshalling
		if handled == 0 {
			_, _, _ = pTranslateMsgDlg.Call(uintptr(unsafe.Pointer(&msg))) // #nosec G103 -- required Win32 syscall pointer marshalling
			_, _, _ = pDispatchMsgDlg.Call(uintptr(unsafe.Pointer(&msg)))  // #nosec G103 -- required Win32 syscall pointer marshalling
		}
	}

	return state.result, state.ok
}
