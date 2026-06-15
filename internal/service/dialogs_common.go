package service

// MessageBox flag/return constants, shared across platforms so cross-platform
// orchestration code (setup.go) can reference them. On non-Windows the values
// are inert (messageBox is a stub).
const (
	mbOK          = 0x00000000
	mbOKCancel    = 0x00000001
	mbYesNo       = 0x00000004
	mbIconError   = 0x00000010
	mbIconWarning = 0x00000030
	mbIconInfo    = 0x00000040

	mbIDOK  = 1
	mbIDYes = 6
	mbIDNo  = 7
)
