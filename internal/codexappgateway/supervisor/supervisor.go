package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	ExtraEnv []string     // forwarded to every spawned subprocess
	Logger   *slog.Logger // defaults to slog.Default() if nil
}

// Supervisor owns the in-memory (workspace, thread) → subprocess map.
type Supervisor struct {
	cfg    SupervisorConfig
	logger *slog.Logger

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
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Supervisor{cfg: cfg, logger: logger, children: map[Key]*entry{}}
}

// EnsureSubprocess returns a live ChildHandle for key, spawning one if
// necessary. Concurrent EnsureSubprocess calls for the same key see
// the same handle (one-spawn-per-key invariant; loser of the race
// discards their spawn). If a cached entry's subprocess has crashed,
// it is evicted and a fresh subprocess is spawned.
func (s *Supervisor) EnsureSubprocess(ctx context.Context, key Key, build ConfigBuilder) (*ChildHandle, error) {
	s.mu.Lock()
	if e, ok := s.children[key]; ok {
		if e.handle.IsAlive() {
			e.lastActive = time.Now()
			s.mu.Unlock()
			return e.handle, nil
		}
		// Subprocess crashed since the entry was last seen. Drop it; we'll
		// respawn below. Try to upload its CODEX_HOME state first so the
		// freshly-spawned successor can resume from where the dead one
		// left off (sqlite WAL may still be flushable).
		deadHome := e.codexHome
		delete(s.children, key)
		s.mu.Unlock()
		backend := codexhome.NewS3Backend(s.cfg.Store, key.WorkspaceID, key.ThreadID)
		// Best-effort: ignore upload error here — the dead-process cleanup
		// path can't usefully retry, and we'd rather respawn than block.
		if err := backend.Upload(ctx, deadHome); err != nil {
			s.logger.Warn("dead-subprocess CODEX_HOME upload failed", "key", key, "err", err)
		}
		if err := s.cfg.HomeMgr.RemoveTmpDir(deadHome); err != nil {
			s.logger.Warn("dead-subprocess tmpdir cleanup failed", "key", key, "err", err)
		}
		// Fall through to the spawn path below.
	} else {
		s.mu.Unlock()
	}

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
	if e, ok := s.children[key]; ok {
		// Lost the race; discard our spawn and return theirs.
		e.lastActive = time.Now()
		winner := e.handle
		s.mu.Unlock()
		// Clean up our discarded spawn out-of-band so the lock isn't held
		// during the SIGTERM→SIGKILL window.
		go func() {
			if err := handle.Stop(context.Background()); err != nil {
				s.logger.Warn("race-loser subprocess stop failed", "key", key, "err", err)
			}
			if err := s.cfg.HomeMgr.RemoveTmpDir(codexHome); err != nil {
				s.logger.Warn("race-loser tmpdir cleanup failed", "key", key, "err", err)
			}
		}()
		return winner, nil
	}
	s.children[key] = &entry{handle: handle, codexHome: codexHome, lastActive: time.Now()}
	s.mu.Unlock()
	return handle, nil
}

// Touch bumps the last-active timestamp for a key. Callers must invoke
// it on every proxied frame (see proxy.RunProxy's onFrame callback) so
// the IdleReaper sees fresh activity for the duration of an active
// session, not just at connect/disconnect.
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
	if err := e.handle.Stop(ctx); err != nil {
		s.logger.Warn("subprocess stop failed", "key", key, "err", err)
	}
	// Always reclaim disk before returning, even if S3 upload fails.
	// (S3 upload failure is transient; leaking the tmpdir would compound on
	// long-running pods with intermittent S3 connectivity.)
	defer func() {
		if err := s.cfg.HomeMgr.RemoveTmpDir(e.codexHome); err != nil {
			s.logger.Warn("tmpdir cleanup failed", "key", key, "err", err)
		}
	}()
	backend := codexhome.NewS3Backend(s.cfg.Store, key.WorkspaceID, key.ThreadID)
	if err := backend.Upload(ctx, e.codexHome); err != nil {
		return fmt.Errorf("S3 upload: %w", err)
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
		if err := s.Shutdown(ctx, k); err != nil {
			s.logger.Error("ShutdownAll: subprocess shutdown failed (CODEX_HOME may not be saved to S3)", "key", k, "err", err)
		}
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
