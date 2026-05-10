package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
)

// Key identifies one (workspace, thread) subprocess slot.
type Key struct {
	WorkspaceID string
	ThreadID    string
}

// SupervisorConfig holds the static dependencies.
type SupervisorConfig struct {
	CodexBin string
	HomeMgr  *codexhome.Manager
	Store    codexhome.ObjectStore
	ExtraEnv []string // forwarded to every spawned subprocess
}

// Supervisor owns the in-memory (workspace, thread) → subprocess map.
type Supervisor struct {
	cfg SupervisorConfig

	mu       sync.Mutex
	children map[Key]*entry
}

type entry struct {
	handle     *ChildHandle
	codexHome  string
	lastActive time.Time
}

// ConfigBuilder produces a fresh ConfigInput at spawn time. Allowed to
// hit the network; errors propagate.
type ConfigBuilder func() (codexhome.ConfigInput, error)

func NewSupervisor(cfg SupervisorConfig) *Supervisor {
	return &Supervisor{cfg: cfg, children: map[Key]*entry{}}
}

// EnsureSubprocess returns a live ChildHandle for key, spawning one if
// necessary. Concurrent EnsureSubprocess calls for the same key see
// the same handle (one-spawn-per-key invariant; loser of the race
// discards their spawn).
func (s *Supervisor) EnsureSubprocess(ctx context.Context, key Key, build ConfigBuilder) (*ChildHandle, error) {
	s.mu.Lock()
	if e, ok := s.children[key]; ok {
		e.lastActive = time.Now()
		s.mu.Unlock()
		return e.handle, nil
	}
	s.mu.Unlock()

	cfg, err := build()
	if err != nil {
		return nil, fmt.Errorf("config builder: %w", err)
	}
	codexHome, err := s.cfg.HomeMgr.NewTmpDir(key.WorkspaceID, key.ThreadID)
	if err != nil {
		return nil, fmt.Errorf("new tmpdir: %w", err)
	}
	backend := codexhome.NewS3Backend(s.cfg.Store, key.WorkspaceID, key.ThreadID)
	if err := backend.Download(ctx, codexHome); err != nil && !errors.Is(err, codexhome.ErrObjectNotFound) {
		_ = s.cfg.HomeMgr.RemoveTmpDir(codexHome)
		return nil, fmt.Errorf("S3 download: %w", err)
	}
	if err := s.cfg.HomeMgr.WriteConfig(codexHome, cfg); err != nil {
		_ = s.cfg.HomeMgr.RemoveTmpDir(codexHome)
		return nil, fmt.Errorf("write config: %w", err)
	}

	handle, err := spawnCodexAppServer(ctx, s.cfg.CodexBin, codexHome, s.cfg.ExtraEnv)
	if err != nil {
		_ = s.cfg.HomeMgr.RemoveTmpDir(codexHome)
		return nil, fmt.Errorf("spawn: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.children[key]; ok {
		// Lost the race; discard our spawn and return theirs.
		_ = handle.Stop(ctx)
		_ = s.cfg.HomeMgr.RemoveTmpDir(codexHome)
		e.lastActive = time.Now()
		return e.handle, nil
	}
	s.children[key] = &entry{handle: handle, codexHome: codexHome, lastActive: time.Now()}
	return handle, nil
}

// Touch bumps the last-active timestamp for a key. Called by the proxy
// pump on every frame so the reaper sees fresh activity.
func (s *Supervisor) Touch(key Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.children[key]; ok {
		e.lastActive = time.Now()
	}
}

// Shutdown terminates the subprocess for key, uploads its CODEX_HOME to
// S3, and drops the in-memory entry. Safe on missing keys.
func (s *Supervisor) Shutdown(ctx context.Context, key Key) error {
	s.mu.Lock()
	e, ok := s.children[key]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	delete(s.children, key)
	s.mu.Unlock()

	// Continue uploading even if Stop errors — flushed sqlite is still useful.
	_ = e.handle.Stop(ctx)
	backend := codexhome.NewS3Backend(s.cfg.Store, key.WorkspaceID, key.ThreadID)
	if err := backend.Upload(ctx, e.codexHome); err != nil {
		return fmt.Errorf("S3 upload: %w", err)
	}
	if err := s.cfg.HomeMgr.RemoveTmpDir(e.codexHome); err != nil {
		return fmt.Errorf("remove tmpdir: %w", err)
	}
	return nil
}

// ShutdownAll shuts down every active subprocess.
func (s *Supervisor) ShutdownAll(ctx context.Context) {
	s.mu.Lock()
	keys := make([]Key, 0, len(s.children))
	for k := range s.children {
		keys = append(keys, k)
	}
	s.mu.Unlock()
	for _, k := range keys {
		_ = s.Shutdown(ctx, k)
	}
}

// snapshot returns the keys + last-active times. Used by the reaper.
func (s *Supervisor) snapshot() map[Key]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[Key]time.Time, len(s.children))
	for k, e := range s.children {
		out[k] = e.lastActive
	}
	return out
}
