package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Workspace is the ephemeral local filesystem view a single CC turn operates in.
type Workspace struct {
	WorkspaceID string
	SessionID   string

	TempDir    string // root: /tmp/cc-broker/sess_<sessionID>
	ClaudeDir  string // <TempDir>/claude-config — CLAUDE_CONFIG_DIR
	ProjectDir string // <TempDir>/project       — CLI cwd (kept empty; only used for proj_hash)
	MemoryDir  string // <ClaudeDir>/projects/ws_<wid>/memory — auto-memory override
}

// TempDirBase is the parent under which per-session work directories are
// created. Tests override it via t.TempDir(); production uses os.TempDir().
var TempDirBase = ""

func tempDirBase() string {
	if TempDirBase != "" {
		return TempDirBase
	}
	return os.TempDir()
}

// claudeHomeKey is the deterministic S3 object key for a workspace's
// claude-home tarball. One workspace, one object.
func claudeHomeKey(workspaceID string) string {
	return fmt.Sprintf("workspaces/%s/claude-home.tar.gz", workspaceID)
}

// Setup creates the temp directory tree and downloads the workspace's
// claude-home tarball from S3. The returned Workspace must be passed to
// Teardown so the temp directory is removed and ClaudeDir is uploaded back.
//
// On any error after the directory tree is created, Setup removes TempDir
// before returning, so callers do not leak per-session directories.
func Setup(ctx context.Context, workspaceID, sessionID string, store *S3Store) (*Workspace, error) {
	// Path is deterministic in (sessionID) so Claude CLI's proj_hash lookup
	// (derived from Cwd = ProjectDir) finds the same session jsonl across
	// turns. Per-session turn serialization is enforced by the in-memory
	// TurnLock in handler_turns. cc-broker runs replicas: 1 in production;
	// multi-replica deployments would need a distributed lock.
	tempDir := filepath.Join(tempDirBase(), "cc-broker", "sess_"+sessionID)
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir temp: %w", err)
	}

	ws := &Workspace{
		WorkspaceID: workspaceID,
		SessionID:   sessionID,
		TempDir:     tempDir,
		ClaudeDir:   filepath.Join(tempDir, "claude-config"),
		ProjectDir:  filepath.Join(tempDir, "project"),
	}
	ws.MemoryDir = filepath.Join(ws.ClaudeDir, "projects", "ws_"+workspaceID, "memory")

	for _, d := range []string{ws.ClaudeDir, ws.ProjectDir, ws.MemoryDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			_ = os.RemoveAll(tempDir)
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	if err := store.DownloadTarGz(ctx, claudeHomeKey(workspaceID), ws.ClaudeDir); err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("download claude-home: %w", err)
	}

	return ws, nil
}

// Teardown packages ClaudeDir as a tar.gz, uploads it to S3, then removes
// the temp dir. Upload failures are logged but do not propagate — a flaky
// upload must not block the caller's turn response. TempDir is always
// removed.
func Teardown(ctx context.Context, ws *Workspace, store *S3Store) error {
	if ws == nil {
		return nil
	}
	defer func() { _ = os.RemoveAll(ws.TempDir) }()

	if err := store.UploadTarGz(ctx, ws.ClaudeDir, claudeHomeKey(ws.WorkspaceID)); err != nil {
		fmt.Fprintf(os.Stderr, "workspace.Teardown: upload claude-home: %v\n", err)
	}
	return nil
}
