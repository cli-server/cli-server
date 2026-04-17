package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ExecutorSession holds the credentials for a registered executor.
// Persisted at ~/.agentserver/executors/{executor_id}.json so the local
// machine can reconnect to executor-registry across restarts.
type ExecutorSession struct {
	ExecutorID    string    `json:"executor_id"`
	Name          string    `json:"name"`
	WorkspaceID   string    `json:"workspace_id"`
	TunnelToken   string    `json:"tunnel_token"`
	RegistryToken string    `json:"registry_token"`
	ServerURL     string    `json:"server_url"`
	CreatedAt     time.Time `json:"created_at"`
}

// executorSessionsDir returns the directory where executor sessions are stored.
func executorSessionsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".agentserver", "executors"), nil
}

// loadLatestExecutorSession loads the most recently created session
// matching the given server URL + workspace ID. Returns nil if none found.
func loadLatestExecutorSession(serverURL, workspaceID string) (*ExecutorSession, error) {
	dir, err := executorSessionsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var sessions []*ExecutorSession
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var sess ExecutorSession
		if err := json.Unmarshal(data, &sess); err != nil {
			continue
		}
		if sess.ServerURL == serverURL && sess.WorkspaceID == workspaceID {
			sessions = append(sessions, &sess)
		}
	}

	if len(sessions) == 0 {
		return nil, nil
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].CreatedAt.After(sessions[j].CreatedAt)
	})
	return sessions[0], nil
}

// removeExecutorSession deletes the persisted session file for the given
// executor ID. Missing files are not considered an error.
func removeExecutorSession(executorID string) error {
	dir, err := executorSessionsDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, executorID+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// saveExecutorSession writes the session JSON to disk.
func saveExecutorSession(sess *ExecutorSession) error {
	dir, err := executorSessionsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, sess.ExecutorID+".json")
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// registerExecutorWithRegistry POSTs /api/executors/register and returns a session.
// executor-registry is currently unauthenticated by design; name + workspace_id
// are sufficient for the MVP.
func registerExecutorWithRegistry(serverURL, name, workspaceID string) (*ExecutorSession, error) {
	reqBody, _ := json.Marshal(map[string]string{
		"name":         name,
		"workspace_id": workspaceID,
	})
	resp, err := http.Post(serverURL+"/api/executors/register", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("register returned %d: %s", resp.StatusCode, body)
	}

	var regResp struct {
		ExecutorID    string `json:"executor_id"`
		TunnelToken   string `json:"tunnel_token"`
		RegistryToken string `json:"registry_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&regResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if regResp.ExecutorID == "" || regResp.TunnelToken == "" || regResp.RegistryToken == "" {
		return nil, fmt.Errorf("register response missing required fields")
	}

	return &ExecutorSession{
		ExecutorID:    regResp.ExecutorID,
		Name:          name,
		WorkspaceID:   workspaceID,
		TunnelToken:   regResp.TunnelToken,
		RegistryToken: regResp.RegistryToken,
		ServerURL:     serverURL,
		CreatedAt:     time.Now().UTC(),
	}, nil
}

// LoadOrRegisterExecutor reuses a saved session for the given serverURL +
// workspaceID if one exists, otherwise registers a new executor with the
// registry and persists the result. Read errors other than "sessions dir
// doesn't exist" are surfaced to the caller rather than silently triggering
// a re-registration.
func LoadOrRegisterExecutor(opts ExecutorOpts) (*ExecutorSession, error) {
	if opts.WorkspaceID == "" {
		return nil, fmt.Errorf("workspace_id is required")
	}
	sess, err := loadLatestExecutorSession(opts.ServerURL, opts.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("load saved session: %w", err)
	}
	if sess != nil {
		return sess, nil
	}
	sess, err = registerExecutorWithRegistry(opts.ServerURL, opts.Name, opts.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if err := saveExecutorSession(sess); err != nil {
		// Non-fatal: we can still run, just won't persist across restarts.
		fmt.Fprintf(os.Stderr, "warning: failed to save executor session: %v\n", err)
	}
	return sess, nil
}
