package sbxstore

// Status constants for sandbox lifecycle.
const (
	StatusCreating = "creating"
	StatusRunning  = "running"
	StatusPausing  = "pausing"
	StatusPaused   = "paused"
	StatusResuming = "resuming"
	StatusDeleting = "deleting"
	StatusOffline  = "offline"
)

// ValidTransition checks whether a status transition is allowed.
func ValidTransition(from, to string) bool {
	switch from {
	case StatusCreating:
		return to == StatusRunning || to == StatusDeleting
	case StatusRunning:
		return to == StatusPausing || to == StatusDeleting || to == StatusOffline
	case StatusPausing:
		return to == StatusPaused
	case StatusPaused:
		return to == StatusResuming || to == StatusDeleting
	case StatusResuming:
		return to == StatusRunning
	case StatusOffline:
		return to == StatusRunning || to == StatusDeleting
	default:
		return false
	}
}
