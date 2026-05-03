package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"
)

// lookupResult is the subset of executor-registry's ExecutorInfo we need
// for the cross-user permission check.
type lookupResult struct {
	OwnerUserID       string
	SharedToWorkspace bool
}

// lookupExecutor fetches owner_user_id + shared_to_workspace from
// executor-registry. Test seam: tests overwrite this var.
var lookupExecutor = func(ctx context.Context, tctx *Context, executorID string) (lookupResult, error) {
	if tctx.ExecutorRegistryURL == "" {
		return lookupResult{}, fmt.Errorf("executor registry URL not configured")
	}
	req, err := http.NewRequestWithContext(ctx, "GET",
		tctx.ExecutorRegistryURL+"/api/executors/"+url.PathEscape(executorID), nil)
	if err != nil {
		return lookupResult{}, err
	}
	resp, err := tctx.HTTP.Do(req)
	if err != nil {
		return lookupResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return lookupResult{}, fmt.Errorf("executor lookup %d", resp.StatusCode)
	}
	var info struct {
		OwnerUserID       string `json:"owner_user_id"`
		SharedToWorkspace bool   `json:"shared_to_workspace"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return lookupResult{}, err
	}
	return lookupResult{
		OwnerUserID:       info.OwnerUserID,
		SharedToWorkspace: info.SharedToWorkspace,
	}, nil
}

// gateCheck wraps a remote_* tool dispatch. On deny / timeout / cross-user,
// returns an IsError McpToolResult with reason-code prefix; otherwise returns
// nil signalling the caller may proceed to forwardExecute.
func gateCheck(ctx context.Context, tctx *Context, toolName, executorID string,
	args json.RawMessage) *agentsdk.McpToolResult {

	if tctx.Gate == nil {
		// Backward-compat: if Gate is not wired (legacy callers, tests),
		// skip gate check. Production path always sets it (handler_turns).
		return nil
	}
	info, err := lookupExecutor(ctx, tctx, executorID)
	if err != nil {
		return errResult(fmt.Errorf("executor_unknown: %w", err))
	}
	err = tctx.Gate.Check(ctx, CheckRequest{
		SessionID:                 tctx.SessionID,
		TurnID:                    tctx.CurrentTurnID,
		Tool:                      toolName,
		ExecutorID:                executorID,
		Args:                      args,
		PermissionMode:            tctx.PermissionMode,
		SessionCreatorUserID:      tctx.CreatorUserID,
		ExecutorOwnerUserID:       info.OwnerUserID,
		ExecutorSharedToWorkspace: info.SharedToWorkspace,
		Timeout:                   30 * time.Second,
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrCrossUserDenied):
		return errResult(fmt.Errorf("cross_user_denied: executor %s belongs to a different user", executorID))
	case errors.Is(err, ErrPermissionDenied):
		return errResult(fmt.Errorf("permission_denied: user declined %s on %s", toolName, executorID))
	default:
		return errResult(fmt.Errorf("permission_error: %w", err))
	}
}

// --- input shapes ---

type remoteBashInput struct {
	ExecutorID  string `json:"executor_id"`
	Command     string `json:"command"`
	Description string `json:"description,omitempty"`
	Timeout     int    `json:"timeout,omitempty"`
}

type remoteReadInput struct {
	ExecutorID string `json:"executor_id"`
	FilePath   string `json:"file_path"`
	Offset     int    `json:"offset,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

type remoteEditInput struct {
	ExecutorID string `json:"executor_id"`
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

type remoteWriteInput struct {
	ExecutorID string `json:"executor_id"`
	FilePath   string `json:"file_path"`
	Content    string `json:"content"`
}

type remoteGlobInput struct {
	ExecutorID string `json:"executor_id"`
	Pattern    string `json:"pattern"`
	Path       string `json:"path,omitempty"`
}

type remoteGrepInput struct {
	ExecutorID string `json:"executor_id"`
	Pattern    string `json:"pattern"`
	Path       string `json:"path,omitempty"`
	Glob       string `json:"glob,omitempty"`
}

type remoteLSInput struct {
	ExecutorID string `json:"executor_id"`
	Path       string `json:"path,omitempty"`
}

type listExecutorsInput struct {
	StatusFilter string `json:"status_filter,omitempty"`
}

// executorTools returns all 8 executor-related tools, closures over tctx.
func executorTools(tctx *Context) []agentsdk.McpTool {
	return []agentsdk.McpTool{
		agentsdk.Tool[remoteBashInput]("remote_bash",
			"Execute a shell command on the specified executor.",
			func(ctx context.Context, in remoteBashInput) (*agentsdk.McpToolResult, error) {
				return forwardExecute(ctx, tctx, "remote_bash", "Bash", in)
			}),
		agentsdk.Tool[remoteReadInput]("remote_read",
			"Read a file on the specified executor.",
			func(ctx context.Context, in remoteReadInput) (*agentsdk.McpToolResult, error) {
				return forwardExecute(ctx, tctx, "remote_read", "Read", in)
			}),
		agentsdk.Tool[remoteEditInput]("remote_edit",
			"Edit a file on the specified executor.",
			func(ctx context.Context, in remoteEditInput) (*agentsdk.McpToolResult, error) {
				return forwardExecute(ctx, tctx, "remote_edit", "Edit", in)
			}),
		agentsdk.Tool[remoteWriteInput]("remote_write",
			"Write content to a file on the specified executor.",
			func(ctx context.Context, in remoteWriteInput) (*agentsdk.McpToolResult, error) {
				return forwardExecute(ctx, tctx, "remote_write", "Write", in)
			}),
		agentsdk.Tool[remoteGlobInput]("remote_glob",
			"Find files matching a glob pattern on the specified executor.",
			func(ctx context.Context, in remoteGlobInput) (*agentsdk.McpToolResult, error) {
				return forwardExecute(ctx, tctx, "remote_glob", "Glob", in)
			}),
		agentsdk.Tool[remoteGrepInput]("remote_grep",
			"Search for a regex pattern in files on the specified executor.",
			func(ctx context.Context, in remoteGrepInput) (*agentsdk.McpToolResult, error) {
				return forwardExecute(ctx, tctx, "remote_grep", "Grep", in)
			}),
		agentsdk.Tool[remoteLSInput]("remote_ls",
			"List directory contents on the specified executor.",
			func(ctx context.Context, in remoteLSInput) (*agentsdk.McpToolResult, error) {
				return forwardExecute(ctx, tctx, "remote_ls", "LS", in)
			}),
		agentsdk.Tool[listExecutorsInput]("list_executors",
			"List available executors in this workspace with their capabilities.",
			func(ctx context.Context, in listExecutorsInput) (*agentsdk.McpToolResult, error) {
				return listExecutors(ctx, tctx)
			}),
	}
}

// forwardExecute routes a remote_* tool call to executor-registry POST /api/execute.
// mcpToolName is the MCP-side name (e.g. "remote_bash") used for gate checks and
// sticky rule keys. registryToolName is the cc-tool name forwarded to executor-registry
// (e.g. "Bash"). args must be a struct whose JSON representation contains an
// "executor_id" field; that field is stripped before forwarding in the "arguments" payload.
func forwardExecute(ctx context.Context, tctx *Context, mcpToolName, registryToolName string, args any) (*agentsdk.McpToolResult, error) {
	// Marshal the typed input so we can extract executor_id and strip it.
	rawArgs, err := json.Marshal(args)
	if err != nil {
		return errResult(fmt.Errorf("marshal tool args: %w", err)), nil
	}

	var argsMap map[string]json.RawMessage
	if err := json.Unmarshal(rawArgs, &argsMap); err != nil {
		return errResult(fmt.Errorf("unmarshal tool args: %w", err)), nil
	}

	executorIDRaw, ok := argsMap["executor_id"]
	if !ok {
		return errResult(fmt.Errorf("executor_id is required")), nil
	}
	var executorID string
	if err := json.Unmarshal(executorIDRaw, &executorID); err != nil || executorID == "" {
		return errResult(fmt.Errorf("executor_id must be a non-empty string")), nil
	}

	// Build arguments without executor_id.
	delete(argsMap, "executor_id")
	cleanArgs, err := json.Marshal(argsMap)
	if err != nil {
		return errResult(fmt.Errorf("marshal clean args: %w", err)), nil
	}

	// Permission gate: cross-user check + sticky lookup + ask-and-block.
	// Returns nil on allow; non-nil on any denial, in which case we short-circuit.
	if blocked := gateCheck(ctx, tctx, mcpToolName, executorID, cleanArgs); blocked != nil {
		return blocked, nil
	}

	body, err := json.Marshal(map[string]any{
		"executor_id": executorID,
		"tool":        registryToolName,
		"arguments":   json.RawMessage(cleanArgs),
	})
	if err != nil {
		return errResult(fmt.Errorf("marshal execute request: %w", err)), nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tctx.ExecutorRegistryURL+"/api/execute", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build execute request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := tctx.HTTP.Do(req)
	if err != nil {
		return errResult(fmt.Errorf("executor-registry request failed: %w", err)), nil
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return errResult(fmt.Errorf("read executor-registry response: %w", err)), nil
	}

	return textResult(string(respBody)), nil
}

// listExecutors queries executor-registry GET /api/executors?workspace_id=<wid>.
func listExecutors(ctx context.Context, tctx *Context) (*agentsdk.McpToolResult, error) {
	u, err := url.Parse(tctx.ExecutorRegistryURL + "/api/executors")
	if err != nil {
		return nil, fmt.Errorf("parse executor-registry URL: %w", err)
	}
	q := u.Query()
	q.Set("workspace_id", tctx.WorkspaceID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build list-executors request: %w", err)
	}

	resp, err := tctx.HTTP.Do(req)
	if err != nil {
		return errResult(fmt.Errorf("executor-registry request failed: %w", err)), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return errResult(fmt.Errorf("read executor-registry response: %w", err)), nil
	}

	return textResult(string(body)), nil
}

// errResult wraps an error in an IsError McpToolResult.
func errResult(err error) *agentsdk.McpToolResult {
	return &agentsdk.McpToolResult{
		Content: []agentsdk.McpToolContent{{Type: "text", Text: err.Error()}},
		IsError: true,
	}
}

// textResult wraps a plain string in a successful McpToolResult.
func textResult(s string) *agentsdk.McpToolResult {
	return &agentsdk.McpToolResult{
		Content: []agentsdk.McpToolContent{{Type: "text", Text: s}},
	}
}
