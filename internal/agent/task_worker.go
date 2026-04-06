package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"
)

// TaskWorkerOptions configures the task worker.
type TaskWorkerOptions struct {
	ServerURL  string // agentserver base URL (e.g. https://agentserver.example.com)
	ProxyToken string // sandbox proxy_token for auth
	SandboxID  string // this agent's sandbox ID
	Workdir    string // working directory for claude CLI
	CLIPath    string // path to claude binary (default: "claude")
}

// TaskWorker receives and executes delegated tasks using the Go Agent SDK.
type TaskWorker struct {
	opts   TaskWorkerOptions
	client *http.Client
}

func NewTaskWorker(opts TaskWorkerOptions) *TaskWorker {
	if opts.CLIPath == "" {
		opts.CLIPath = "claude"
	}
	return &TaskWorker{
		opts:   opts,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// ExecuteTask runs a single task: connects bridge, executes via Agent SDK.
func (w *TaskWorker) ExecuteTask(ctx context.Context, taskID, sessionID, prompt, systemContext string, maxTurns int, maxBudgetUSD float64) error {
	log.Printf("task-worker: executing task %s (session=%s)", taskID, sessionID)

	// Create session only if not provided by the server.
	if sessionID == "" {
		var err error
		sessionID, err = agentsdk.CreateSession(agentsdk.CreateSessionOptions{
			BaseURL:    w.opts.ServerURL,
			Token:      w.opts.ProxyToken,
			Title:      fmt.Sprintf("Task %s", taskID),
			PathPrefix: "/v1/agent/sessions",
			TimeoutMs:  10000,
		})
		if err != nil {
			return fmt.Errorf("create session: %w", err)
		}
		log.Printf("task-worker: created session %s", sessionID)
	}

	// 2. Fetch bridge credentials via POST /v1/agent/sessions/{id}/bridge.
	creds, err := agentsdk.FetchRemoteCredentials(agentsdk.FetchRemoteCredentialsOptions{
		SessionID:  sessionID,
		BaseURL:    w.opts.ServerURL,
		Token:      w.opts.ProxyToken,
		PathPrefix: "/v1/agent/sessions",
		TimeoutMs:  10000,
	})
	if err != nil {
		return fmt.Errorf("fetch credentials: %w", err)
	}
	log.Printf("task-worker: got bridge credentials (epoch=%d)", creds.WorkerEpoch)

	// 3. Attach bridge session (starts SSE reader + heartbeat).
	bridge, err := agentsdk.AttachBridgeSession(ctx, agentsdk.AttachBridgeSessionOptions{
		SessionID:    sessionID,
		IngressToken: creds.WorkerJWT,
		APIBaseURL:   creds.APIBaseURL,
		Epoch:        &creds.WorkerEpoch,
		OutboundOnly: false,
	})
	if err != nil {
		return fmt.Errorf("attach bridge: %w", err)
	}
	defer bridge.Close()

	// 4. Report running state.
	bridge.ReportState(agentsdk.SessionStateRunning)

	// 5. Build query options.
	opts := []agentsdk.QueryOption{
		agentsdk.WithCwd(w.opts.Workdir),
		agentsdk.WithPermissionMode(agentsdk.PermissionBypassAll),
		agentsdk.WithAllowDangerouslySkipPermissions(),
	}
	if w.opts.CLIPath != "" && w.opts.CLIPath != "claude" {
		opts = append(opts, agentsdk.WithCLIPath(w.opts.CLIPath))
	}
	if maxTurns > 0 {
		opts = append(opts, agentsdk.WithMaxTurns(maxTurns))
	}
	if maxBudgetUSD > 0 {
		opts = append(opts, agentsdk.WithMaxBudgetUSD(maxBudgetUSD))
	}
	if systemContext != "" {
		opts = append(opts, agentsdk.WithSystemPrompt(systemContext))
	}

	// 6. Execute query and stream results back through bridge.
	stream := agentsdk.Query(ctx, prompt, opts...)
	defer stream.Close()

	for stream.Next() {
		msg := stream.Current()

		// Use msg.Raw — the original JSON from claude CLI.
		// json.Marshal(msg) would lose content because Raw and Subtype are tagged json:"-".
		raw := msg.Raw
		if len(raw) == 0 {
			continue
		}

		if writeErr := bridge.WriteBatch([]json.RawMessage{raw}); writeErr != nil {
			log.Printf("task-worker: bridge write error: %v", writeErr)
		}
	}

	if err := stream.Err(); err != nil {
		bridge.ReportState(agentsdk.SessionStateIdle)
		return fmt.Errorf("query execution: %w", err)
	}

	// 7. Send final result — also use Raw.
	if result, err := stream.Result(); err == nil && result != nil {
		resultData, _ := json.Marshal(result)
		bridge.WriteBatch([]json.RawMessage{resultData})
	}

	bridge.ReportState(agentsdk.SessionStateIdle)
	log.Printf("task-worker: task %s completed", taskID)
	return nil
}

// RunTaskWorker starts the task worker that polls for tasks. Blocks until ctx is cancelled.
func RunTaskWorker(ctx context.Context, opts TaskWorkerOptions) {
	log.Printf("task-worker: starting (server=%s, sandbox=%s)", opts.ServerURL, opts.SandboxID)
	worker := NewTaskWorker(opts)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tasks, err := worker.pollTasks(ctx)
			if err != nil {
				continue
			}
			for _, task := range tasks {
				if ctx.Err() != nil {
					return
				}
				if err := worker.ExecuteTask(ctx, task.ID, task.SessionID, task.Prompt, task.SystemContext, task.MaxTurns, task.MaxBudgetUSD); err != nil {
					log.Printf("task-worker: task %s failed: %v", task.ID, err)
					worker.reportTaskFailure(ctx, task.ID, err.Error())
				} else {
					worker.reportTaskComplete(ctx, task.ID)
				}
			}
		}
	}
}

// RegisterDefaultCard registers a default agent card for a claudecode agent.
func RegisterDefaultCard(serverURL, proxyToken, displayName string) error {
	card := map[string]any{
		"display_name": displayName,
		"description":  "Claude Code agent with full coding, terminal, and search capabilities",
		"agent_type":   "claudecode",
		"card": map[string]any{
			"skills": []map[string]string{
				{"name": "code-editing", "description": "Read, write, and edit source code"},
				{"name": "code-review", "description": "Review code for bugs and best practices"},
				{"name": "terminal", "description": "Execute shell commands"},
				{"name": "code-search", "description": "Search and navigate codebases"},
			},
			"tags": []string{"code", "terminal", "search"},
		},
	}
	body, _ := json.Marshal(card)
	req, err := http.NewRequest("POST", serverURL+"/api/agent/discovery/cards", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+proxyToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("register card: HTTP %d", resp.StatusCode)
	}
	return nil
}

type pollTask struct {
	ID            string  `json:"task_id"`
	SessionID     string  `json:"session_id"`
	Prompt        string  `json:"prompt"`
	SystemContext string  `json:"system_context"`
	MaxTurns      int     `json:"max_turns"`
	MaxBudgetUSD  float64 `json:"max_budget_usd"`
}

func (w *TaskWorker) pollTasks(ctx context.Context) ([]pollTask, error) {
	// Poll server for tasks assigned to this sandbox.
	url := fmt.Sprintf("%s/api/agent/tasks/poll?sandbox_id=%s", w.opts.ServerURL, w.opts.SandboxID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+w.opts.ProxyToken)

	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil // no tasks or error
	}

	var tasks []pollTask
	json.NewDecoder(resp.Body).Decode(&tasks)
	return tasks, nil
}

func (w *TaskWorker) reportTaskFailure(ctx context.Context, taskID, reason string) {
	log.Printf("task-worker: task %s failed: %s", taskID, reason)
	body, _ := json.Marshal(map[string]string{"status": "failed", "failure_reason": reason})
	w.updateTaskStatus(ctx, taskID, body)
}

func (w *TaskWorker) reportTaskComplete(ctx context.Context, taskID string) {
	log.Printf("task-worker: task %s completed", taskID)
	body, _ := json.Marshal(map[string]string{"status": "completed"})
	w.updateTaskStatus(ctx, taskID, body)
}

func (w *TaskWorker) updateTaskStatus(ctx context.Context, taskID string, body []byte) {
	url := fmt.Sprintf("%s/api/agent/tasks/%s/status", w.opts.ServerURL, taskID)
	req, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewReader(body))
	if err != nil {
		log.Printf("task-worker: failed to create status update request: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+w.opts.ProxyToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		log.Printf("task-worker: failed to update task status: %v", err)
		return
	}
	resp.Body.Close()
}
