package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DiscoveredAgent is an agent from the discovery API.
type DiscoveredAgent struct {
	AgentID     string `json:"agent_id"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	AgentType   string `json:"agent_type"`
	Status      string `json:"status"`
	Card        struct {
		Tags   []string `json:"tags"`
		Skills []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"skills"`
	} `json:"card"`
}

// AgentListing periodically fetches the available agent list and formats
// it for injection into the delegate_task tool description.
type AgentListing struct {
	mu          sync.RWMutex
	agents      []DiscoveredAgent
	serverURL   string
	token       string
	workspaceID string
	selfID      string
	client      *http.Client
}

// NewAgentListing creates a new listing that will poll for agents.
func NewAgentListing(serverURL, token, workspaceID, selfID string) *AgentListing {
	return &AgentListing{
		serverURL:   serverURL,
		token:       token,
		workspaceID: workspaceID,
		selfID:      selfID,
		client:      &http.Client{Timeout: 10 * time.Second},
	}
}

// Start begins periodic refresh. Call once on startup.
func (l *AgentListing) Start(ctx context.Context) {
	// Initial fetch.
	if err := l.Refresh(); err != nil {
		log.Printf("mcp: initial agent listing fetch failed: %v", err)
	}

	// Periodic refresh every 60s.
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := l.Refresh(); err != nil {
					log.Printf("mcp: agent listing refresh failed: %v", err)
				}
			}
		}
	}()
}

// Refresh fetches the latest agent list from the server.
func (l *AgentListing) Refresh() error {
	url := fmt.Sprintf("%s/api/workspaces/%s/agents", l.serverURL, l.workspaceID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+l.token)

	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var agents []DiscoveredAgent
	if err := json.NewDecoder(resp.Body).Decode(&agents); err != nil {
		return err
	}

	// Filter out self.
	var filtered []DiscoveredAgent
	for _, a := range agents {
		if a.AgentID != l.selfID {
			filtered = append(filtered, a)
		}
	}

	l.mu.Lock()
	l.agents = filtered
	l.mu.Unlock()

	log.Printf("mcp: refreshed agent listing: %d agents", len(filtered))
	return nil
}

// FormatForToolDescription generates the agent list appended to delegate_task description.
func (l *AgentListing) FormatForToolDescription() string {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if len(l.agents) == 0 {
		return "\n\nNo other agents are currently available in this workspace."
	}

	var sb strings.Builder
	sb.WriteString("\n\nAvailable agents in this workspace:\n")
	for _, a := range l.agents {
		sb.WriteString(fmt.Sprintf("- %s (%s): %s", a.DisplayName, a.AgentID, a.Description))
		if len(a.Card.Tags) > 0 {
			sb.WriteString(fmt.Sprintf(" [%s]", strings.Join(a.Card.Tags, ", ")))
		}
		sb.WriteString(fmt.Sprintf(" — %s\n", a.Status))
	}
	return sb.String()
}
