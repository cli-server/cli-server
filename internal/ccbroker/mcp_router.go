package ccbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ToolRouter dispatches MCP tool calls to the appropriate backend:
// executor-registry, local workspace, IM, or scheduler.
type ToolRouter struct {
	executorRegistryURL string
	agentserverURL      string
	workspaceDir        string // set per-worker, local temp dir
	sessionID           string
	workspaceID         string
	httpClient          *http.Client
	logger              *slog.Logger
}

// ToolRouterConfig holds the configuration for creating a ToolRouter.
type ToolRouterConfig struct {
	ExecutorRegistryURL string
	AgentserverURL      string
	WorkspaceDir        string
	SessionID           string
	WorkspaceID         string
}

// NewToolRouter creates a ToolRouter with the given configuration.
func NewToolRouter(cfg ToolRouterConfig, logger *slog.Logger) *ToolRouter {
	if logger == nil {
		logger = slog.Default()
	}
	return &ToolRouter{
		executorRegistryURL: cfg.ExecutorRegistryURL,
		agentserverURL:      cfg.AgentserverURL,
		workspaceDir:        cfg.WorkspaceDir,
		sessionID:           cfg.SessionID,
		workspaceID:         cfg.WorkspaceID,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		logger: logger,
	}
}

// Route dispatches a tool call to the appropriate handler based on tool name.
func (r *ToolRouter) Route(ctx context.Context, toolName string, args map[string]interface{}) (*MCPToolResult, error) {
	switch {
	case strings.HasPrefix(toolName, "remote_"):
		return r.routeToExecutor(ctx, toolName, args)
	case strings.HasPrefix(toolName, "workspace_"):
		return r.routeToWorkspace(ctx, toolName, args)
	case toolName == "list_executors":
		return r.routeListExecutors(ctx, args)
	case toolName == "send_message" || toolName == "send_image" || toolName == "send_file":
		return r.routeToIM(ctx, toolName, args)
	case strings.Contains(toolName, "_scheduled_"):
		return r.routeToScheduler(ctx, toolName, args)
	default:
		return nil, fmt.Errorf("unknown tool: %s", toolName)
	}
}

// routeToExecutor forwards remote_* tool calls to executor-registry.
func (r *ToolRouter) routeToExecutor(ctx context.Context, toolName string, args map[string]interface{}) (*MCPToolResult, error) {
	executorID, _ := args["executor_id"].(string)
	if executorID == "" {
		return textError("executor_id is required"), nil
	}

	// Strip remote_ prefix: remote_bash -> Bash, remote_read -> Read, etc.
	tool := strings.TrimPrefix(toolName, "remote_")
	tool = strings.ToUpper(tool[:1]) + tool[1:] // capitalize first letter

	// Remove executor_id from args (executor-registry doesn't need it in arguments).
	cleanArgs := make(map[string]interface{})
	for k, v := range args {
		if k != "executor_id" {
			cleanArgs[k] = v
		}
	}
	argsJSON, _ := json.Marshal(cleanArgs)

	// POST to executor-registry /api/execute.
	reqBody, _ := json.Marshal(map[string]interface{}{
		"executor_id": executorID,
		"tool":        tool,
		"arguments":   json.RawMessage(argsJSON),
	})

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.executorRegistryURL+"/api/execute", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("executor-registry request creation failed: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := r.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("executor-registry request failed: %w", err)
	}
	defer resp.Body.Close()

	var execResp struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&execResp); err != nil {
		return nil, fmt.Errorf("executor-registry response decode failed: %w", err)
	}

	return &MCPToolResult{
		Content: []MCPContentBlock{{Type: "text", Text: execResp.Output}},
		IsError: execResp.ExitCode != 0,
	}, nil
}

// routeToWorkspace handles workspace_* tool calls against the local workspace directory.
func (r *ToolRouter) routeToWorkspace(ctx context.Context, toolName string, args map[string]interface{}) (*MCPToolResult, error) {
	switch toolName {
	case "workspace_write":
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		if path == "" {
			return textError("path is required"), nil
		}
		fullPath := filepath.Join(r.workspaceDir, filepath.Clean(path))
		// Security: ensure path doesn't escape workspaceDir.
		if !strings.HasPrefix(fullPath, r.workspaceDir) {
			return textError("path escapes workspace directory"), nil
		}
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			return textError("mkdir failed: " + err.Error()), nil
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			return textError("write failed: " + err.Error()), nil
		}
		return textResult("Written successfully: " + path), nil

	case "workspace_read":
		path, _ := args["path"].(string)
		if path == "" {
			return textError("path is required"), nil
		}
		fullPath := filepath.Join(r.workspaceDir, filepath.Clean(path))
		if !strings.HasPrefix(fullPath, r.workspaceDir) {
			return textError("path escapes workspace directory"), nil
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return textError("read failed: " + err.Error()), nil
		}
		return textResult(string(data)), nil

	case "workspace_ls":
		path, _ := args["path"].(string)
		fullPath := filepath.Join(r.workspaceDir, filepath.Clean(path))
		if !strings.HasPrefix(fullPath, r.workspaceDir) {
			return textError("path escapes workspace directory"), nil
		}
		entries, err := os.ReadDir(fullPath)
		if err != nil {
			return textError("ls failed: " + err.Error()), nil
		}
		var lines []string
		for _, e := range entries {
			suffix := ""
			if e.IsDir() {
				suffix = "/"
			}
			lines = append(lines, e.Name()+suffix)
		}
		return textResult(strings.Join(lines, "\n")), nil
	}

	return nil, fmt.Errorf("unknown workspace tool: %s", toolName)
}

// routeListExecutors queries executor-registry for available executors.
func (r *ToolRouter) routeListExecutors(ctx context.Context, args map[string]interface{}) (*MCPToolResult, error) {
	url := r.executorRegistryURL + "/api/executors?workspace_id=" + r.workspaceID

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("executor-registry request creation failed: %w", err)
	}

	resp, err := r.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("executor-registry request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	return textResult(string(body)), nil
}

// routeToIM handles IM-related tools (send_message, send_image, send_file).
// This is a placeholder until the IM integration is connected.
func (r *ToolRouter) routeToIM(ctx context.Context, toolName string, args map[string]interface{}) (*MCPToolResult, error) {
	return textResult("IM tool " + toolName + " is not yet connected to agentserver"), nil
}

// routeToScheduler handles scheduling-related tools (*_scheduled_*).
// This is a placeholder until the scheduler integration is connected.
func (r *ToolRouter) routeToScheduler(ctx context.Context, toolName string, args map[string]interface{}) (*MCPToolResult, error) {
	return textResult("Scheduling tool " + toolName + " is not yet connected to agentserver"), nil
}

// textResult creates a successful MCPToolResult with text content.
func textResult(text string) *MCPToolResult {
	return &MCPToolResult{Content: []MCPContentBlock{{Type: "text", Text: text}}}
}

// textError creates an error MCPToolResult with text content.
func textError(text string) *MCPToolResult {
	return &MCPToolResult{Content: []MCPContentBlock{{Type: "text", Text: text}}, IsError: true}
}
