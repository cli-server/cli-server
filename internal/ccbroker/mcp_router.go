package ccbroker

import (
	"bytes"
	"context"
	"encoding/base64"
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
	imbridgeURL         string
	imbridgeSecret      string
	workspaceDir        string // set per-worker, local temp dir
	sessionID           string
	workspaceID         string
	imChannelID         string
	imUserID            string
	httpClient          *http.Client
	logger              *slog.Logger
}

// ToolRouterConfig holds the configuration for creating a ToolRouter.
type ToolRouterConfig struct {
	ExecutorRegistryURL string
	AgentserverURL      string
	IMBridgeURL         string
	IMBridgeSecret      string
	WorkspaceDir        string
	SessionID           string
	WorkspaceID         string
	IMChannelID         string
	IMUserID            string
}

// NewToolRouter creates a ToolRouter with the given configuration.
func NewToolRouter(cfg ToolRouterConfig, logger *slog.Logger) *ToolRouter {
	if logger == nil {
		logger = slog.Default()
	}
	return &ToolRouter{
		executorRegistryURL: cfg.ExecutorRegistryURL,
		agentserverURL:      cfg.AgentserverURL,
		imbridgeURL:         cfg.IMBridgeURL,
		imbridgeSecret:      cfg.IMBridgeSecret,
		workspaceDir:        cfg.WorkspaceDir,
		sessionID:           cfg.SessionID,
		workspaceID:         cfg.WorkspaceID,
		imChannelID:         cfg.IMChannelID,
		imUserID:            cfg.IMUserID,
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
		if !strings.HasPrefix(fullPath, r.workspaceDir+string(os.PathSeparator)) && fullPath != r.workspaceDir {
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
		if !strings.HasPrefix(fullPath, r.workspaceDir+string(os.PathSeparator)) && fullPath != r.workspaceDir {
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
		if !strings.HasPrefix(fullPath, r.workspaceDir+string(os.PathSeparator)) && fullPath != r.workspaceDir {
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

// routeToIM handles send_message / send_image / send_file by forwarding to
// imbridge's internal send endpoints. Requires per-turn IM context
// (imChannelID + imUserID) populated by agentserver on the POST /api/turns
// request when the turn was originated by an IM inbound.
func (r *ToolRouter) routeToIM(ctx context.Context, toolName string, args map[string]interface{}) (*MCPToolResult, error) {
	if r.imbridgeURL == "" {
		return textError("IM tools are not configured (CCBROKER_IMBRIDGE_URL unset)"), nil
	}
	if r.imChannelID == "" || r.imUserID == "" {
		return textError("IM tools require an IM-originated session"), nil
	}

	switch toolName {
	case "send_message":
		text, _ := args["text"].(string)
		if text == "" {
			return textError("text is required"), nil
		}
		return r.imbridgePost(ctx, "/api/internal/imbridge/send", map[string]string{
			"channel_id": r.imChannelID,
			"to_user_id": r.imUserID,
			"text":       text,
		})
	case "send_image":
		source, _ := args["source"].(string)
		if source == "" {
			return textError("source is required"), nil
		}
		data, err := r.resolveMediaSource(ctx, source)
		if err != nil {
			return textError("failed to resolve image source: " + err.Error()), nil
		}
		body := map[string]string{
			"channel_id":   r.imChannelID,
			"to_user_id":   r.imUserID,
			"image_base64": base64.StdEncoding.EncodeToString(data),
		}
		if format, _ := args["format"].(string); format != "" {
			body["format"] = format
		}
		if caption, _ := args["caption"].(string); caption != "" {
			body["caption"] = caption
		}
		return r.imbridgePost(ctx, "/api/internal/imbridge/send-image", body)
	case "send_file":
		return textError("send_file is not yet supported by the IM provider"), nil
	}
	return textError("unknown IM tool: " + toolName), nil
}

// imbridgePost posts a JSON body to an imbridge internal endpoint.
func (r *ToolRouter) imbridgePost(ctx context.Context, path string, body map[string]string) (*MCPToolResult, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal imbridge request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.imbridgeURL+path, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("build imbridge request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if r.imbridgeSecret != "" {
		req.Header.Set("X-Internal-Secret", r.imbridgeSecret)
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("imbridge request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return textError(fmt.Sprintf("imbridge %s returned %d: %s", path, resp.StatusCode, respBody)), nil
	}
	return textResult("sent"), nil
}

// resolveMediaSource decodes an image/file source into raw bytes. Supports:
//   - `executor_id:/absolute/path` — Read the file via executor-registry
//   - `http://…` or `https://…` — fetch over HTTP
//   - anything else — treat as base64-encoded bytes
func (r *ToolRouter) resolveMediaSource(ctx context.Context, source string) ([]byte, error) {
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		return r.fetchURL(ctx, source)
	}
	if idx := strings.Index(source, ":"); idx > 0 && idx < len(source)-1 {
		candidate := source[:idx]
		rest := source[idx+1:]
		if strings.HasPrefix(candidate, "exe_") && strings.HasPrefix(rest, "/") {
			return r.readFromExecutor(ctx, candidate, rest)
		}
	}
	return base64.StdEncoding.DecodeString(source)
}

// fetchURL GETs a URL and returns the body bytes. Rejects oversize responses
// rather than silently truncating.
func (r *ToolRouter) fetchURL(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d", url, resp.StatusCode)
	}
	const maxBytes = 20 << 20
	// Read max+1 so a response that exactly fills max still reports truncation.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("source at %s exceeds %d byte limit", url, maxBytes)
	}
	return data, nil
}

// readFromExecutor reads a file from the executor in a binary-safe way.
//
// The executor's Read tool returns `string(data)`, which is lossy for
// non-UTF-8 bytes: the executor's JSON response goes through
// encoding/json on the wire, which replaces invalid UTF-8 with U+FFFD.
// Running `base64 | tr -d '\n'` on the executor and decoding client-side
// preserves every byte of binary content.
func (r *ToolRouter) readFromExecutor(ctx context.Context, executorID, filePath string) ([]byte, error) {
	if r.executorRegistryURL == "" {
		return nil, fmt.Errorf("executor-registry not configured")
	}
	// Shell-quote the path so crafted filenames (apostrophes, spaces, $()) can't
	// alter the command.
	quoted := "'" + strings.ReplaceAll(filePath, "'", "'\\''") + "'"
	reqBody := map[string]interface{}{
		"executor_id": executorID,
		"tool":        "Bash",
		"arguments": map[string]interface{}{
			// `tr -d '\n'` strips the default 76-column wrapping that GNU
			// base64 adds, so the result is a single clean base64 string
			// portable across GNU and BSD base64 implementations.
			"command": "base64 " + quoted + " | tr -d '\\n'",
		},
	}
	buf, _ := json.Marshal(reqBody)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.executorRegistryURL+"/api/execute", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := r.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("executor-registry returned %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode executor response: %w", err)
	}
	if out.ExitCode != 0 {
		return nil, fmt.Errorf("base64 %s on executor failed: %s", filePath, strings.TrimSpace(out.Output))
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out.Output))
	if err != nil {
		return nil, fmt.Errorf("decode base64 from executor: %w", err)
	}
	return decoded, nil
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
