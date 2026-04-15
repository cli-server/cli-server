package agentsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Complete reports a task as completed with the given result.
func (t *Task) Complete(ctx context.Context, result TaskResult) error {
	payload := map[string]interface{}{
		"status": "completed",
		"output": result.Output,
	}
	if result.CostUSD > 0 {
		payload["total_cost_usd"] = result.CostUSD
	}
	if result.NumTurns > 0 {
		payload["num_turns"] = result.NumTurns
	}
	return t.updateStatus(ctx, payload)
}

// Fail reports a task as failed with the given error message.
func (t *Task) Fail(ctx context.Context, errMsg string) error {
	payload := map[string]interface{}{
		"status":         "failed",
		"failure_reason": errMsg,
	}
	return t.updateStatus(ctx, payload)
}

// Running reports a task as currently running.
func (t *Task) Running(ctx context.Context) error {
	payload := map[string]interface{}{
		"status": "running",
	}
	return t.updateStatus(ctx, payload)
}

func (t *Task) updateStatus(ctx context.Context, payload map[string]interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal task status: %w", err)
	}

	url := strings.TrimRight(t.serverURL, "/") + "/api/agent/tasks/" + t.ID + "/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create status request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.proxyToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("update task status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("task status update failed (%d): %s", resp.StatusCode, respBody)
	}
	return nil
}

// taskPollLoop polls for assigned tasks and dispatches them to the handler.
func (c *Client) taskPollLoop(ctx context.Context, handler TaskHandler, interval time.Duration) {
	idleStart := time.Now()
	currentInterval := interval

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(currentInterval):
		}

		task, err := c.pollTask(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}

		if task == nil {
			// No task available. Backoff to 30s after 5min idle.
			if time.Since(idleStart) > 5*time.Minute && currentInterval < 30*time.Second {
				currentInterval = 30 * time.Second
			}
			continue
		}

		// Got a task; reset idle timer and interval.
		idleStart = time.Now()
		currentInterval = interval

		// Report running.
		if err := task.Running(ctx); err != nil {
			log.Printf("agentsdk: failed to report task running: %v", err)
		}

		// Execute the handler.
		if err := handler(ctx, task); err != nil {
			log.Printf("agentsdk: task %s handler failed: %v", task.ID, err)
			if failErr := task.Fail(ctx, err.Error()); failErr != nil {
				log.Printf("agentsdk: failed to report task failure: %v", failErr)
			}
		}
	}
}

// pollTask makes a single poll request for a new task.
func (c *Client) pollTask(ctx context.Context) (*Task, error) {
	url := strings.TrimRight(c.config.ServerURL, "/") + "/api/agent/tasks/poll"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.reg.ProxyToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil, nil // No task available.
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("poll tasks: unexpected status %d", resp.StatusCode)
	}

	var task Task
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		return nil, fmt.Errorf("decode task: %w", err)
	}
	task.proxyToken = c.reg.ProxyToken
	task.serverURL = c.config.ServerURL
	return &task, nil
}
