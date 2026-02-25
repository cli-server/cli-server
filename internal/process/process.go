package process

// Process represents a running process with PTY-like I/O.
type Process interface {
	Read(buf []byte) (int, error)
	Write(data []byte) (int, error)
	Resize(rows, cols uint16) error
	Done() <-chan struct{}
}

// Manager manages process lifecycles.
type Manager interface {
	Start(id, command string, args, env []string) (Process, error)
	Get(id string) (Process, bool)
	Stop(id string) error
	Close() error
}
