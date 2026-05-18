package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/agentserver/agentserver/internal/auth"
	"github.com/agentserver/agentserver/internal/notebookjwt"
	"github.com/agentserver/agentserver/internal/notebooksupervisor"
)

// notebookSessionTTL is the lifetime of the JWT minted by
// postNotebookSession. The browser uses the token only long enough to
// open the JupyterLab websocket; the IdentityProvider rejects expired
// tokens on every subsequent HTTP request.
const notebookSessionTTL = 10 * time.Minute

// notebookSessionURL returns the URL the frontend should open to
// reach JupyterLab for the given workspace. When the per-workspace
// subdomain vhost is configured (NotebookHostBaseDomain non-empty)
// it returns an absolute URL that triggers the token-for-cookie
// exchange. Otherwise it falls back to the legacy in-router proxy
// path.
func (s *Server) notebookSessionURL(wsID, tok string) string {
	if s.NotebookHostBaseDomain == "" {
		return "/api/notebooks/" + wsID + "/lab"
	}
	short := wsID
	if len(short) > 8 {
		short = short[:8]
	}
	return "https://" + s.notebookSubdomainPrefixOrDefault() + "-" + short +
		"." + s.NotebookHostBaseDomain + "/auth?token=" + tok
}

// postNotebookSession is POST /api/notebooks/{ws}/session.
//
// Auth: cookie session middleware (s.Auth.Middleware) populates the
// user ID via auth.UserIDFromContext. Workspace membership is enforced
// via db.IsWorkspaceMember (any role).
//
// Body: none.
//
// Response:
//
//	200 {url, token, expires_at}
//	401 missing/invalid user session
//	403 not a workspace member
//	404 workspace not found / no k8s namespace assigned yet
//	500 supervisor or DB failure
//	503 NotebookJWTSecret unset, or NotebookSupervisor nil
//
// `url` is the path under agentserver that the browser hits next; the
// reverse proxy (Task 4) lives at /api/notebooks/{ws}/lab/*.
func (s *Server) postNotebookSession(w http.ResponseWriter, r *http.Request) {
	if len(s.NotebookJWTSecret) == 0 {
		http.Error(w, "notebook feature disabled (no JWT secret configured)", http.StatusServiceUnavailable)
		return
	}
	if s.NotebookSupervisor == nil {
		http.Error(w, "notebook supervisor unavailable", http.StatusServiceUnavailable)
		return
	}

	userID := auth.UserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	wsID := chi.URLParam(r, "ws")
	if wsID == "" {
		http.Error(w, "workspace id required", http.StatusBadRequest)
		return
	}

	ok, err := s.DB.IsWorkspaceMember(wsID, userID)
	if err != nil {
		http.Error(w, "membership check failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	ws, err := s.DB.GetWorkspace(wsID)
	if err != nil {
		http.Error(w, "workspace lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if ws == nil {
		http.Error(w, "workspace not found", http.StatusNotFound)
		return
	}
	if !ws.K8sNamespace.Valid || ws.K8sNamespace.String == "" {
		http.Error(w, "workspace has no k8s namespace assigned", http.StatusNotFound)
		return
	}

	key := notebooksupervisor.Key{WorkspaceID: wsID, Namespace: ws.K8sNamespace.String}
	if _, err := s.NotebookSupervisor.EnsureRunning(r.Context(), key); err != nil {
		http.Error(w, "ensure notebook: "+err.Error(), http.StatusInternalServerError)
		return
	}

	tok, err := notebookjwt.Mint(s.NotebookJWTSecret, userID, wsID, notebookSessionTTL)
	if err != nil {
		http.Error(w, "mint jwt: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := map[string]any{
		"url":        s.notebookSessionURL(wsID, tok),
		"token":      tok,
		"expires_at": time.Now().Add(notebookSessionTTL).Unix(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
