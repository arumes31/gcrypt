package sync

import (
	"log/slog"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// NLM_CONNECTION_COST flags.
const (
	nlmConnectionCostUnknown      = 0x0000
	nlmConnectionCostUnrestricted = 0x0001
	nlmConnectionCostFixed        = 0x0002
	nlmConnectionCostVariable     = 0x0004
	nlmConnectionCostOverDataLmt  = 0x0010000
	nlmConnectionCostRoaming      = 0x0020000
)

var (
	modOle32             = windows.NewLazySystemDLL("ole32.dll")
	procCoCreateInstance = modOle32.NewProc("CoCreateInstance")

	modNlmAPI = windows.NewLazySystemDLL("nlmapi.dll") //nolint:unused // referenced via CLSID
)

// CLSID_NetworkListManager {DCB00C01-570F-4A9B-8D69-199FDBA5723B}
var clsidNetworkListManager = windows.GUID{
	Data1: 0xDCB00C01,
	Data2: 0x570F,
	Data3: 0x4A9B,
	Data4: [8]byte{0x8D, 0x69, 0x19, 0x9F, 0xDB, 0xA5, 0x72, 0x3B},
}

// IID_INetworkCostManager {DCB00008-570F-4A9B-8D69-199FDBA5723B}
var iidNetworkCostManager = windows.GUID{
	Data1: 0xDCB00008,
	Data2: 0x570F,
	Data3: 0x4A9B,
	Data4: [8]byte{0x8D, 0x69, 0x19, 0x9F, 0xDB, 0xA5, 0x72, 0x3B},
}

// INetworkCostManager COM vtable layout (partial).
type iNetworkCostManagerVtbl struct {
	QueryInterface uintptr
	AddRef         uintptr
	Release        uintptr
	GetCost        uintptr
	// ... other methods omitted
}

type iNetworkCostManager struct {
	vtbl *iNetworkCostManagerVtbl
}

// IsMeteredNetwork reports whether the current default network connection is
// metered (e.g. a mobile hotspot or a connection flagged as metered in Windows
// Settings). It uses the Windows NLM (Network List Manager) COM API.
//
// Returns false on any error (API unavailable, COM init failure, etc.) so sync
// proceeds normally when detection is unsupported.
func IsMeteredNetwork() bool {
	// Lazy-init COM for this thread (STA/MTA doesn't matter for NLM).
	hr, _, _ := windows.NewLazySystemDLL("ole32.dll").NewProc("CoInitializeEx").Call(0, 0)
	if hr != 0 && hr != 1 { // S_OK or S_FALSE (already initialised)
		slog.Debug("metered: CoInitializeEx failed", "hr", hr)
		return false
	}

	var pUnk unsafe.Pointer
	hr, _, _ = procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidNetworkListManager)), // #nosec G103 -- required Win32/COM syscall pointer marshalling
		0,
		1|4, // CLSCTX_INPROC_SERVER|CLSCTX_LOCAL_SERVER
		uintptr(unsafe.Pointer(&iidNetworkCostManager)), // #nosec G103 -- required Win32/COM syscall pointer marshalling
		uintptr(unsafe.Pointer(&pUnk)),                  // #nosec G103 -- required Win32/COM syscall pointer marshalling
	)
	if hr != 0 || pUnk == nil {
		slog.Debug("metered: CoCreateInstance failed", "hr", hr)
		return false
	}

	mgr := (*iNetworkCostManager)(pUnk)
	defer func() {
		// Release
		_, _, _ = syscall.SyscallN(mgr.vtbl.Release, uintptr(pUnk))
	}()

	var cost uint32
	hr, _, _ = syscall.SyscallN(mgr.vtbl.GetCost, uintptr(pUnk), uintptr(unsafe.Pointer(&cost)), 0) // #nosec G103 -- required Win32/COM syscall pointer marshalling
	if hr != 0 {
		slog.Debug("metered: GetCost failed", "hr", hr)
		return false
	}

	// Metered if fixed, variable, over-limit, or roaming.
	return cost&(nlmConnectionCostFixed|nlmConnectionCostVariable|nlmConnectionCostOverDataLmt|nlmConnectionCostRoaming) != 0
}
