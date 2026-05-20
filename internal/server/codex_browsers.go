package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// codexBrowser is the per-row payload of GET /api/workspaces/{wid}/browsers.
// Shape mirrors RemoteExecutor (the Connectors panel) so the frontend can
// render both with the same DeviceListPanel component.
//
// IsOnline = there is at least one open codex_browser_session for this token.
// ClientIP / ClientUA / CodexVersion / OS / ConnectedAt / DisconnectedAt all
// come from the latest session (open if any, otherwise the most recent
// historical row).
type codexBrowser struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	WorkspaceID    string     `json:"workspace_id"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	LastUsedAt     *time.Time `json:"last_used_at,omitempty"`
	IsOnline       bool       `json:"is_online"`
	ClientIP       string     `json:"client_ip,omitempty"`
	ClientUA       string     `json:"client_ua,omitempty"`
	CodexVersion   string     `json:"codex_version,omitempty"`
	OS             string     `json:"os,omitempty"`
	ConnectedAt    *time.Time `json:"connected_at,omitempty"`
	DisconnectedAt *time.Time `json:"disconnected_at,omitempty"`
}

// handleListCodexBrowsers returns one row per non-revoked codex token for
// the workspace, annotated with live session info from
// codex_browser_sessions. Auth: any workspace member.
func (s *Server) handleListCodexBrowsers(w http.ResponseWriter, r *http.Request) {
	wid := chi.URLParam(r, "wid")
	if _, ok := s.requireWorkspaceMember(w, r, wid); !ok {
		return
	}
	tokens, err := s.DB.ListCodexTokensForWorkspace(r.Context(), wid, false)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]codexBrowser, 0, len(tokens))
	for _, t := range tokens {
		row := codexBrowser{
			ID: t.ID, Name: t.Name, WorkspaceID: t.WorkspaceID,
			CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt,
		}
		// One row + one count per token. Browsers panels are small (single
		// digit tokens per workspace typically); N+1 here is acceptable and
		// keeps the query trivial. If this becomes hot, fold into a single
		// JOIN+LEFT-LATERAL query.
		openCount, _ := s.DB.CountOpenCodexBrowserSessions(r.Context(), t.ID)
		row.IsOnline = openCount > 0
		if latest, _ := s.DB.LatestCodexBrowserSession(r.Context(), t.ID); latest != nil {
			row.ClientIP = latest.ClientIP
			row.ClientUA = latest.ClientUA
			row.CodexVersion = latest.CodexVersion
			row.OS = latest.OS
			ca := latest.ConnectedAt
			row.ConnectedAt = &ca
			row.DisconnectedAt = latest.DisconnectedAt
		}
		out = append(out, row)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
