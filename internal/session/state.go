package session

// Status constants for session lifecycle.
const (
	StatusRunning  = "running"
	StatusPausing  = "pausing"
	StatusPaused   = "paused"
	StatusResuming = "resuming"
	StatusDeleting = "deleting"
)

// ValidTransition checks whether a status transition is allowed.
func ValidTransition(from, to string) bool {
	switch from {
	case StatusRunning:
		return to == StatusPausing || to == StatusDeleting
	case StatusPausing:
		return to == StatusPaused
	case StatusPaused:
		return to == StatusResuming || to == StatusDeleting
	case StatusResuming:
		return to == StatusRunning
	default:
		return false
	}
}
