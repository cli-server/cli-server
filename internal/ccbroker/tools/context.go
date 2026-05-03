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
	HTTP                *http.Client         // shared HTTP client
}
