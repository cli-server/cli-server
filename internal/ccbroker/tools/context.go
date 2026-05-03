package tools

import (
	"net/http"

	"github.com/agentserver/agentserver/internal/ccbroker/workspace"
)

// Context bundles the per-turn dependencies that tool handlers close over.
// Constructed in handler_turns once per request and discarded after the turn.
type Context struct {
	SessionID           string
	WorkspaceID         string
	IMChannelID         string
	IMUserID            string
	ExecutorRegistryURL string
	AgentserverURL      string
	IMBridgeURL         string
	InternalAPISecret   string
	Workspace           *workspace.Workspace // for workspace_* tools
	Viking              *workspace.VikingClient
	HTTP                *http.Client // shared HTTP client

	// new (TUI / permission gate, added in Phase 1 Task 5)
	ChannelType            string  // "im" | "tui"
	CreatorUserID          string  // for cross-user check
	PermissionMode         string  // "ask" | "bypass"
	PreferredExecutorID    string  // optional; injected into system prompt
	Gate                   *Gate   // reference to per-broker singleton
	AgentserverInternalURL string  // for turn-finished callback
	CurrentTurnID          string  // set per turn by handler_turns
}
