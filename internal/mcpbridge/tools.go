package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ToolDef defines an MCP tool.
type ToolDef struct {
	Name        string
	description string                      // static description
	dynamicDesc func() string               // dynamic description (overrides static if non-nil)
	InputSchema map[string]any
	Annotations map[string]any
}

// Description returns the tool description, using dynamic if available.
func (t *ToolDef) Description() string {
	if t.dynamicDesc != nil {
		return t.dynamicDesc()
	}
	return t.description
}

// ToolResult is the MCP tool call result.
type ToolResult struct {
	Content []map[string]string `json:"content"`
	IsError bool                `json:"isError,omitempty"`
}

func textResult(text string) *ToolResult {
	return &ToolResult{Content: []map[string]string{{"type": "text", "text": text}}}
}

func errorResult(msg string) *ToolResult {
	return &ToolResult{Content: []map[string]string{{"type": "text", "text": msg}}, IsError: true}
}

// BridgeConfig holds the agentserver connection settings.
type BridgeConfig struct {
	ServerURL   string
	Token       string // tunnel_token or proxy_token
	WorkspaceID string
	SandboxID   string // self (exclude from discovery)
}

// Bridge is the MCP bridge server connecting Claude Code to agentserver.
type Bridge struct {
	config  BridgeConfig
	listing *AgentListing
	client  *http.Client
}

// NewBridge creates a new MCP bridge.
func NewBridge(cfg BridgeConfig) *Bridge {
	b := &Bridge{
		config: cfg,
		client: &http.Client{Timeout: 30 * time.Second},
	}
	b.listing = NewAgentListing(cfg.ServerURL, cfg.Token, cfg.WorkspaceID, cfg.SandboxID)
	return b
}

// StartListing begins periodic agent listing refresh.
func (b *Bridge) StartListing(ctx context.Context) {
	b.listing.Start(ctx)
}

// Tools returns the MCP tool definitions.
func (b *Bridge) Tools() []ToolDef {
	return []ToolDef{
		{
			Name:        "discover_agents",
			description: "Search for available agents in this workspace by skill, tag, or status.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"skill":  map[string]any{"type": "string", "description": "Filter by skill name"},
					"tag":    map[string]any{"type": "string", "description": "Filter by tag"},
					"status": map[string]any{"type": "string", "description": "Filter by status (available, busy, offline). Default: all."},
				},
			},
			Annotations: map[string]any{"readOnlyHint": true},
		},
		{
			Name: "delegate_task",
			dynamicDesc: func() string {
				base := "Delegate a task to another agent in your workspace. The target agent will execute the task and stream results back."
				return base + b.listing.FormatForToolDescription()
			},
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target_id": map[string]any{"type": "string", "description": "The agent_id (sandbox ID) of the target agent"},
					"prompt":    map[string]any{"type": "string", "description": "The task prompt to send to the target agent"},
					"skill":     map[string]any{"type": "string", "description": "Optional: the skill to invoke on the target agent"},
				},
				"required": []string{"target_id", "prompt"},
			},
		},
		{
			Name:        "check_task",
			description: "Check the status and result of a previously delegated task.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{"type": "string", "description": "The task ID returned by delegate_task"},
				},
				"required": []string{"task_id"},
			},
			Annotations: map[string]any{"readOnlyHint": true},
		},
		{
			Name:        "send_message",
			description: "Send a message to another agent's mailbox. (Not yet implemented — coming soon.)",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"to":   map[string]any{"type": "string", "description": "Target agent ID"},
					"text": map[string]any{"type": "string", "description": "Message text"},
				},
				"required": []string{"to", "text"},
			},
		},
		{
			Name:        "read_inbox",
			description: "Read messages from your inbox. (Not yet implemented — coming soon.)",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{},
			},
			Annotations: map[string]any{"readOnlyHint": true},
		},
	}
}

// HandleTool dispatches a tool call to the appropriate handler.
func (b *Bridge) HandleTool(name string, args json.RawMessage) (*ToolResult, error) {
	switch name {
	case "discover_agents":
		return b.handleDiscoverAgents(args)
	case "delegate_task":
		return b.handleDelegateTask(args)
	case "check_task":
		return b.handleCheckTask(args)
	case "send_message":
		return errorResult("send_message is not yet implemented"), nil
	case "read_inbox":
		return errorResult("read_inbox is not yet implemented"), nil
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

// --- Tool Handlers ---

func (b *Bridge) handleDiscoverAgents(args json.RawMessage) (*ToolResult, error) {
	url := fmt.Sprintf("%s/api/workspaces/%s/agents", b.config.ServerURL, b.config.WorkspaceID)
	body, err := b.apiGet(url)
	if err != nil {
		return errorResult(fmt.Sprintf("Failed to discover agents: %v", err)), nil
	}

	// Parse and format nicely
	var agents []json.RawMessage
	json.Unmarshal(body, &agents)
	if len(agents) == 0 {
		return textResult("No agents found in this workspace."), nil
	}

	return textResult(string(body)), nil
}

func (b *Bridge) handleDelegateTask(args json.RawMessage) (*ToolResult, error) {
	var params struct {
		TargetID string `json:"target_id"`
		Prompt   string `json:"prompt"`
		Skill    string `json:"skill"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return errorResult("Invalid arguments: " + err.Error()), nil
	}

	reqBody := map[string]any{
		"target_id":    params.TargetID,
		"prompt":       params.Prompt,
		"requester_id": b.config.SandboxID,
	}
	if params.Skill != "" {
		reqBody["skill"] = params.Skill
	}

	url := fmt.Sprintf("%s/api/workspaces/%s/tasks", b.config.ServerURL, b.config.WorkspaceID)
	respBody, err := b.apiPost(url, reqBody)
	if err != nil {
		return errorResult(fmt.Sprintf("Failed to delegate task: %v", err)), nil
	}

	var result struct {
		TaskID    string `json:"task_id"`
		SessionID string `json:"session_id"`
		Status    string `json:"status"`
	}
	json.Unmarshal(respBody, &result)

	return textResult(fmt.Sprintf("Task delegated successfully.\n\nTask ID: %s\nSession ID: %s\nStatus: %s\n\nUse check_task with task_id to monitor progress.", result.TaskID, result.SessionID, result.Status)), nil
}

func (b *Bridge) handleCheckTask(args json.RawMessage) (*ToolResult, error) {
	var params struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return errorResult("Invalid arguments: " + err.Error()), nil
	}

	url := fmt.Sprintf("%s/api/tasks/%s", b.config.ServerURL, params.TaskID)
	body, err := b.apiGet(url)
	if err != nil {
		return errorResult(fmt.Sprintf("Failed to check task: %v", err)), nil
	}

	// Extract key fields for a readable summary
	var task map[string]any
	json.Unmarshal(body, &task)

	status, _ := task["status"].(string)
	summary := fmt.Sprintf("Task %s: %s", params.TaskID, status)

	if result, ok := task["result"]; ok && result != nil {
		resultJSON, _ := json.MarshalIndent(result, "", "  ")
		summary += "\n\nResult:\n" + string(resultJSON)
	}
	if reason, ok := task["failure_reason"].(string); ok && reason != "" {
		summary += "\n\nFailure reason: " + reason
	}

	// Also check session events for actual output
	if sessionID, ok := task["session_id"].(string); ok && sessionID != "" {
		eventsURL := fmt.Sprintf("%s/v1/agent/sessions/%s/worker/events/stream", b.config.ServerURL, sessionID)
		_ = eventsURL // SSE not practical here; just show status
	}

	return textResult(summary), nil
}

// --- HTTP helpers ---

func (b *Bridge) apiGet(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.config.Token)
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

func (b *Bridge) apiPost(url string, payload any) ([]byte, error) {
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", url, jsonReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.config.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

func jsonReader(data []byte) io.Reader {
	return io.NopCloser(jsonBytesReader(data))
}

type bytesReader struct{ data []byte; pos int }
func jsonBytesReader(data []byte) *bytesReader { return &bytesReader{data: data} }
func (r *bytesReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) { return 0, io.EOF }
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
