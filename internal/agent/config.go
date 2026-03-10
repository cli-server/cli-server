package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const basePort = 4096

// RegistryEntry represents a single agent registration keyed by (Dir, WorkspaceID).
type RegistryEntry struct {
	Dir          string `json:"dir"`
	Server       string `json:"server"`
	SandboxID    string `json:"sandbox_id"`
	TunnelToken  string `json:"tunnel_token"`
	WorkspaceID  string `json:"workspace_id"`
	Name         string `json:"name"`
	OpencodePort int    `json:"opencode_port"`
}

// Registry holds all agent registrations on this machine.
type Registry struct {
	Entries []*RegistryEntry `json:"entries"`
}

// Find returns the entry matching (dir, workspaceID), or nil if not found.
func (r *Registry) Find(dir, workspaceID string) *RegistryEntry {
	for _, e := range r.Entries {
		if e.Dir == dir && e.WorkspaceID == workspaceID {
			return e
		}
	}
	return nil
}

// FindByDir returns all entries for the given directory.
func (r *Registry) FindByDir(dir string) []*RegistryEntry {
	var result []*RegistryEntry
	for _, e := range r.Entries {
		if e.Dir == dir {
			result = append(result, e)
		}
	}
	return result
}

// Put adds or replaces an entry keyed by (Dir, WorkspaceID).
func (r *Registry) Put(entry *RegistryEntry) {
	for i, e := range r.Entries {
		if e.Dir == entry.Dir && e.WorkspaceID == entry.WorkspaceID {
			r.Entries[i] = entry
			return
		}
	}
	r.Entries = append(r.Entries, entry)
}

// Remove deletes the entry matching (dir, workspaceID).
// Returns true if an entry was removed, false if not found.
func (r *Registry) Remove(dir, workspaceID string) bool {
	for i, e := range r.Entries {
		if e.Dir == dir && e.WorkspaceID == workspaceID {
			r.Entries = append(r.Entries[:i], r.Entries[i+1:]...)
			return true
		}
	}
	return false
}

// NextPort returns the lowest available port starting from basePort.
// It finds the first gap in the used port range to reuse freed ports.
func (r *Registry) NextPort() int {
	if len(r.Entries) == 0 {
		return basePort
	}
	used := make(map[int]bool, len(r.Entries))
	for _, e := range r.Entries {
		used[e.OpencodePort] = true
	}
	for port := basePort; ; port++ {
		if !used[port] {
			return port
		}
	}
}

// DefaultRegistryDir returns the default directory for agentserver config.
func DefaultRegistryDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".agentserver")
}

// DefaultRegistryPath returns the default path for the registry file.
func DefaultRegistryPath() string {
	return filepath.Join(DefaultRegistryDir(), "registry.json")
}

// LoadRegistry reads the registry from disk.
// Returns an empty registry if the file does not exist.
func LoadRegistry(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Registry{}, nil
		}
		return nil, fmt.Errorf("read registry: %w", err)
	}
	var reg Registry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parse registry: %w", err)
	}
	return &reg, nil
}

// SaveRegistry writes the registry to disk.
func SaveRegistry(path string, reg *Registry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create registry dir: %w", err)
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal registry: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write registry: %w", err)
	}
	return nil
}

// LockedRegistryFile holds an exclusive lock on the registry for safe
// read-modify-write operations. Call Close() when done.
type LockedRegistryFile struct {
	Path string
	Reg  *Registry
	lock *os.File
}

// LockRegistry acquires an exclusive file lock and loads the registry.
// The caller must call Close() when done to release the lock and
// optionally save changes via Save() before Close().
func LockRegistry(path string) (*LockedRegistryFile, error) {
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return nil, fmt.Errorf("create registry dir: %w", err)
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("acquire lock: %w", err)
	}

	reg, err := LoadRegistry(path)
	if err != nil {
		f.Close()
		return nil, err
	}

	return &LockedRegistryFile{Path: path, Reg: reg, lock: f}, nil
}

// Save writes the registry to disk while the lock is held.
func (l *LockedRegistryFile) Save() error {
	return SaveRegistry(l.Path, l.Reg)
}

// Close releases the file lock.
func (l *LockedRegistryFile) Close() {
	if l.lock != nil {
		l.lock.Close()
		l.lock = nil
	}
}
