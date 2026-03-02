package process

// Process represents a running process with PTY-like I/O.
type Process interface {
	Read(buf []byte) (int, error)
	Write(data []byte) (int, error)
	Resize(rows, cols uint16) error
	Done() <-chan struct{}
}

// VolumeMount describes a PVC or Docker volume to mount into a sandbox.
type VolumeMount struct {
	PVCName   string // PVC name (K8s) or Docker volume name
	MountPath string // container mount path
}

// StartOptions holds optional parameters for starting a process.
type StartOptions struct {
	Namespace        string        // K8s namespace to create sandbox in
	WorkspaceVolumes []VolumeMount // workspace drive volume mounts
	OpencodeToken    string        // per-sandbox token for opencode server auth
	ProxyToken       string        // per-sandbox token for Anthropic API proxy auth
	SandboxType      string        // "opencode" or "openclaw"
	OpenclawToken    string        // openclaw only: gateway auth token
	CPU              int           // CPU limit in millicores (e.g. 2000 = 2 cores)
	Memory           int64         // memory limit in bytes (e.g. 2147483648 = 2Gi)
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
