package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// Session represents a sandbox session created by a single agent process.
// Stored in ~/.agentserver/sessions/{sandbox-id}.json.
type Session struct {
	SandboxID   string    `json:"sandboxId"`
	TunnelToken string    `json:"tunnelToken"`
	ProxyToken  string    `json:"proxyToken"`
	WorkspaceID string    `json:"workspaceId"`
	Name        string    `json:"name"`
	Type        string    `json:"type"` // "opencode" or "claudecode"
	ServerURL   string    `json:"serverUrl"`
	Dir         string    `json:"dir"`
	CreatedAt   time.Time `json:"createdAt"`
	PID         int       `json:"pid,omitempty"`
}

// DefaultSessionsDir returns ~/.agentserver/sessions.
func DefaultSessionsDir() string {
	return filepath.Join(DefaultRegistryDir(), "sessions")
}

// SaveSession writes a session file and records the current PID.
func SaveSession(session *Session) error {
	session.PID = os.Getpid()
	dir := DefaultSessionsDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create sessions dir: %w", err)
	}
	path := filepath.Join(dir, session.SandboxID+".json")
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}
	return os.WriteFile(path, data, 0600)
}

// LoadSession reads a session by sandbox ID.
func LoadSession(sandboxID string) (*Session, error) {
	path := filepath.Join(DefaultSessionsDir(), sandboxID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("session %s not found", sandboxID)
		}
		return nil, fmt.Errorf("read session: %w", err)
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse session: %w", err)
	}
	return &s, nil
}

// CleanupSession clears the PID from the session file.
func CleanupSession(session *Session) {
	session.PID = 0
	if err := SaveSession(session); err != nil {
		log.Printf("warning: failed to clean up session %s: %v", session.SandboxID, err)
	}
}

// FindLatestSession finds the most recent inactive session for the given directory and type.
// If dir is empty, matches any directory. If sessionType is empty, matches any type.
func FindLatestSession(dir, sessionType string) (*Session, error) {
	sessions, err := ListSessions()
	if err != nil {
		return nil, err
	}

	// Sort by creation time descending.
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].CreatedAt.After(sessions[j].CreatedAt)
	})

	for _, s := range sessions {
		if dir != "" && s.Dir != dir {
			continue
		}
		if sessionType != "" && s.Type != sessionType {
			continue
		}
		if isProcessAlive(s.PID) {
			continue
		}
		return s, nil
	}

	return nil, fmt.Errorf("no resumable session found")
}

// ListSessions reads all session files.
func ListSessions() ([]*Session, error) {
	dir := DefaultSessionsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read sessions dir: %w", err)
	}

	var sessions []*Session
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := entry.Name()[:len(entry.Name())-5] // strip .json
		s, err := LoadSession(id)
		if err != nil {
			continue
		}
		sessions = append(sessions, s)
	}
	return sessions, nil
}

// IsSessionActive checks if a session's process is still running.
func IsSessionActive(s *Session) bool {
	return isProcessAlive(s.PID)
}

// isProcessAlive checks if a process with the given PID is still running.
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks process existence without actually sending a signal.
	return proc.Signal(syscall.Signal(0)) == nil
}
