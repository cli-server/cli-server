package ccbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// CCWorker represents a running Claude Code worker process.
type CCWorker struct {
	ID          string
	Process     *exec.Cmd
	SessionID   string
	WorkspaceID string
	Status      string // "running" | "done" | "failed"
	StartedAt   time.Time
	mcpCloser   func()
	tempDir     string
	snapshot    map[string]fileInfo
}

// fileInfo records the size and modification time of a file for snapshot diffing.
type fileInfo struct {
	Size    int64
	ModTime time.Time
}

// fileChange describes a file that was added or modified since the snapshot.
type fileChange struct {
	Path    string
	RelPath string
	IsNew   bool
}

// takeFileSnapshot walks dir and records size + modtime for each regular file.
// The returned map is keyed by path relative to dir.
func takeFileSnapshot(dir string) map[string]fileInfo {
	snap := make(map[string]fileInfo)
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		snap[rel] = fileInfo{Size: info.Size(), ModTime: info.ModTime()}
		return nil
	})
	return snap
}

// diffSnapshot walks dir again and compares against old. It returns files that
// are new (not in old) or modified (size or modtime changed).
func diffSnapshot(dir string, old map[string]fileInfo) []fileChange {
	var changes []fileChange
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		prev, exists := old[rel]
		if !exists {
			changes = append(changes, fileChange{Path: path, RelPath: rel, IsNew: true})
		} else if info.Size() != prev.Size || !info.ModTime().Equal(prev.ModTime) {
			changes = append(changes, fileChange{Path: path, RelPath: rel, IsNew: false})
		}
		return nil
	})
	return changes
}

// SpawnWorker sets up a temporary workspace, downloads files from OpenViking,
// starts an MCP server, and launches a Claude Code worker process. imChannelID
// and imUserID are optional — populated when the turn was originated by an IM
// inbound so the MCP server can route send_* tool calls back through imbridge.
func (s *Server) SpawnWorker(ctx context.Context, sessionID, workspaceID, imChannelID, imUserID string) (*CCWorker, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY environment variable is not set")
	}

	// 1. Create temp dir structure.
	tempDir, err := os.MkdirTemp("", "cc-worker-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}

	claudeDir := filepath.Join(tempDir, "claude-config")
	projectDir := filepath.Join(tempDir, "project")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("create claude-config dir: %w", err)
	}
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("create project dir: %w", err)
	}

	// 2. Download from OpenViking.
	vc := NewVikingClient(s.config.OpenVikingURL, s.config.OpenVikingAPIKey)
	homeURI := fmt.Sprintf("viking://resources/workspace_%s/claude-home/", workspaceID)
	if err := vc.DownloadTree(ctx, homeURI, claudeDir); err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("download claude-home: %w", err)
	}
	projURI := fmt.Sprintf("viking://resources/workspace_%s/project/", workspaceID)
	if err := vc.DownloadTree(ctx, projURI, projectDir); err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("download project: %w", err)
	}

	// 3. Create auto-memory directory.
	memoryDir := filepath.Join(claudeDir, "projects", fmt.Sprintf("ws_%s", workspaceID), "memory")
	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("create memory dir: %w", err)
	}

	// 4. Take file snapshot of claude-config dir.
	snapshot := takeFileSnapshot(claudeDir)

	// 5. Start MCP server.
	_, mcpPort, mcpCloser, err := s.CreateMCPServer(sessionID, workspaceID, claudeDir, imChannelID, imUserID)
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("create MCP server: %w", err)
	}

	// 6. Write MCP config JSON to a temp file.
	mcpConfig := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"cc-broker": map[string]interface{}{
				"url": fmt.Sprintf("http://127.0.0.1:%d", mcpPort),
			},
		},
	}
	mcpConfigBytes, err := json.Marshal(mcpConfig)
	if err != nil {
		mcpCloser()
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("marshal MCP config: %w", err)
	}
	mcpConfigPath := filepath.Join(tempDir, "mcp-config.json")
	if err := os.WriteFile(mcpConfigPath, mcpConfigBytes, 0600); err != nil {
		mcpCloser()
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("write MCP config: %w", err)
	}

	// 7. Build Claude Code command.
	sdkURL := fmt.Sprintf("http://127.0.0.1:%s/v1/sessions/%s", s.config.Port, sessionID)
	cmd := exec.Command("claude",
		"--print",
		"--sdk-url", sdkURL,
		"--tools", "WebSearch,WebFetch",
		"--mcp-config", mcpConfigPath,
		"--permission-mode", "bypassPermissions",
		"--dangerously-skip-permissions",
		"--no-session-persistence",
	)
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// 8. Set environment variables.
	cmd.Env = []string{
		"CLAUDE_CONFIG_DIR=" + claudeDir,
		"CLAUDE_COWORK_MEMORY_PATH_OVERRIDE=" + memoryDir,
		"ANTHROPIC_API_KEY=" + apiKey,
		"CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING=1",
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW=165000",
		"HOME=" + tempDir,
		"PATH=" + os.Getenv("PATH"),
		"TERM=xterm-256color",
	}

	// 9. Start the process.
	if err := cmd.Start(); err != nil {
		mcpCloser()
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("start claude process: %w", err)
	}

	worker := &CCWorker{
		ID:          sessionID,
		Process:     cmd,
		SessionID:   sessionID,
		WorkspaceID: workspaceID,
		Status:      "running",
		StartedAt:   time.Now(),
		mcpCloser:   mcpCloser,
		tempDir:     tempDir,
		snapshot:    snapshot,
	}

	return worker, nil
}

// CleanupWorker diffs the claude-config directory against the initial snapshot,
// uploads changed/new files back to OpenViking, then tears down the MCP server
// and removes the temporary directory.
func (s *Server) CleanupWorker(ctx context.Context, worker *CCWorker) {
	// 1. Diff files against the initial snapshot.
	claudeDir := filepath.Join(worker.tempDir, "claude-config")
	changes := diffSnapshot(claudeDir, worker.snapshot)

	// 2. Upload changed/new files to OpenViking.
	if len(changes) > 0 {
		vc := NewVikingClient(s.config.OpenVikingURL, s.config.OpenVikingAPIKey)
		for _, ch := range changes {
			content, err := os.ReadFile(ch.Path)
			if err != nil {
				s.logger.Warn("cleanup: failed to read changed file",
					"path", ch.Path, "error", err)
				continue
			}
			vikingURI := fmt.Sprintf("viking://resources/workspace_%s/claude-home/%s",
				worker.WorkspaceID, ch.RelPath)

			if ch.IsNew {
				if err := vc.CreateFile(ctx, vikingURI, content); err != nil {
					s.logger.Warn("cleanup: failed to create file in OpenViking",
						"uri", vikingURI, "error", err)
				}
			} else {
				if err := vc.UploadFile(ctx, vikingURI, string(content)); err != nil {
					s.logger.Warn("cleanup: failed to upload file to OpenViking",
						"uri", vikingURI, "error", err)
				}
			}
		}
	}

	// 3. Close MCP server.
	if worker.mcpCloser != nil {
		worker.mcpCloser()
	}

	// 4. Remove temp dir.
	os.RemoveAll(worker.tempDir)
}
