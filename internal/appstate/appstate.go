package appstate

// State represents the application lifecycle state.
type State int

const (
	NotConfigured   State = iota // No config file or missing essential fields
	NeedsPassphrase              // Config exists but no cached passphrase/key
	Connecting                   // Attempting OAuth/token refresh
	NeedsOAuth                   // OAuth token missing or expired, user action needed
	Scanning                     // Initial directory scan in progress
	Syncing                      // Active file sync in progress
	Idle                         // All synced, watching for changes
	Error                        // Recoverable error state
	Disconnected                 // Network/server unreachable
)

// String returns a human-readable name for the state.
func (s State) String() string {
	switch s {
	case NotConfigured:
		return "NotConfigured"
	case NeedsPassphrase:
		return "NeedsPassphrase"
	case Connecting:
		return "Connecting"
	case NeedsOAuth:
		return "NeedsOAuth"
	case Scanning:
		return "Scanning"
	case Syncing:
		return "Syncing"
	case Idle:
		return "Idle"
	case Error:
		return "Error"
	case Disconnected:
		return "Disconnected"
	default:
		return "Unknown"
	}
}
