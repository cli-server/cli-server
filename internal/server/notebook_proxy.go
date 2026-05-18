package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/agentserver/agentserver/internal/notebookjwt"
	"github.com/agentserver/agentserver/internal/notebooksupervisor"
)

// notebookProxy handles "/api/notebooks/{ws}/*". Validates the JWT
// (from ?token=… or Authorization: Bearer …), strips the prefix,
// adds X-Forwarded-User, and reverse-proxies to the workspace's
// Jupyter Server. HTTP + WS share one path — httputil.ReverseProxy
// (Go 1.20+) handles WebSocket upgrades automatically.
//
// The more-specific POST /api/notebooks/{ws}/session route MUST be
// registered before this wildcard so it isn't swallowed.
func (s *Server) notebookProxy(w http.ResponseWriter, r *http.Request) {
	if len(s.NotebookJWTSecret) == 0 || s.NotebookSupervisor == nil {
		http.Error(w, "notebook feature disabled", http.StatusServiceUnavailable)
		return
	}

	wsID := chi.URLParam(r, "ws")
	if wsID == "" {
		http.Error(w, "workspace id required", http.StatusBadRequest)
		return
	}

	tok := extractNotebookToken(r)
	if tok == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	claims, err := notebookjwt.Verify(s.NotebookJWTSecret, tok)
	if err != nil {
		http.Error(w, "invalid token: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if claims.WorkspaceID != wsID {
		http.Error(w, "token workspace mismatch", http.StatusForbidden)
		return
	}

	upstreamURLStr, err := s.resolveNotebookUpstream(r.Context(), wsID)
	if err != nil {
		http.Error(w, "notebook resolve: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	upstreamURL, err := url.Parse(upstreamURLStr)
	if err != nil {
		http.Error(w, "bad upstream url", http.StatusInternalServerError)
		return
	}

	prefix := "/api/notebooks/" + wsID
	rp := httputil.NewSingleHostReverseProxy(upstreamURL)
	originalDirector := rp.Director
	rp.Director = func(req *http.Request) {
		originalDirector(req)
		if strings.HasPrefix(req.URL.Path, prefix) {
			req.URL.Path = strings.TrimPrefix(req.URL.Path, prefix)
			if req.URL.Path == "" {
				req.URL.Path = "/"
			}
		}
		q := req.URL.Query()
		q.Del("token")
		req.URL.RawQuery = q.Encode()
		req.Header.Set("X-Forwarded-User", claims.UserID)
	}
	rp.ServeHTTP(w, r)
}

// resolveNotebookUpstream returns the upstream Jupyter Server URL for
// the given workspace. In production: looks up the workspace's k8s
// namespace, calls EnsureRunning, touches the idle clock, returns
// handle.ServiceURL. testNotebookUpstream (test-only) short-circuits
// the lookup so tests don't need a working supervisor.
func (s *Server) resolveNotebookUpstream(ctx context.Context, wsID string) (string, error) {
	if s.testNotebookUpstream != nil {
		return s.testNotebookUpstream(wsID)
	}
	ws, err := s.DB.GetWorkspace(wsID)
	if err != nil {
		return "", fmt.Errorf("workspace lookup: %w", err)
	}
	if ws == nil {
		return "", fmt.Errorf("workspace not found")
	}
	if !ws.K8sNamespace.Valid || ws.K8sNamespace.String == "" {
		return "", fmt.Errorf("workspace has no k8s namespace assigned")
	}
	k := notebooksupervisor.Key{WorkspaceID: wsID, Namespace: ws.K8sNamespace.String}
	handle, err := s.NotebookSupervisor.EnsureRunning(ctx, k)
	if err != nil {
		return "", fmt.Errorf("ensure notebook: %w", err)
	}
	s.NotebookSupervisor.Touch(k)
	return handle.ServiceURL, nil
}

func extractNotebookToken(r *http.Request) string {
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}
