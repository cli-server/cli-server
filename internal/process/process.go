package process

// Process represents a running process with PTY-like I/O.
type Process interface {
	Read(buf []byte) (int, error)
	Write(data []byte) (int, error)
	Resize(rows, cols uint16) error
	Done() <-chan struct{}
}

// StartOptions holds optional parameters for starting a process.
type StartOptions struct {
	WorkspaceDiskPVC string // pre-existing PVC name for workspace drive mount
	OpencodePassword string // per-sandbox password for opencode server auth
	ProxyToken       string // per-sandbox token for Anthropic API proxy auth
}

// Manager manages process lifecycles.
type Manager interface {
	Start(id, command string, args, env []string, opts StartOptions) (Process, error)
	// StartContainer creates/starts the container without exec-ing into it.
	// Used by the chat flow where the sidecar handles exec via the SDK.
	StartContainer(id string, opts StartOptions) error
	Get(id string) (Process, bool)
	Stop(id string) error
	Pause(id string) error
	Resume(id, sandboxName, command string, args []string) (Process, error)
	Close() error
}
