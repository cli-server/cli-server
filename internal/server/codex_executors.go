package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/agentserver/agentserver/internal/auth"

	"github.com/go-chi/chi/v5"
)

type registerExecutorReq struct {
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
	DefaultCwd  string `json:"default_cwd,omitempty"`
}

type registerExecutorResp struct {
	ExeID             string `json:"exe_id"`
	RegistrationToken string `json:"registration_token"`
	// ConnectCommand is the one-liner the user pastes on the machine
	// they want to expose as an executor. Empty when the gateway public
	// host isn't configured — the UI falls back to a generic template.
	ConnectCommand string `json:"connect_command,omitempty"`
}

// handleRegisterExecutor mints a new executor owned by the calling user
// and immediately binds it to the workspace. ACL: caller must be
// owner/maintainer of the workspace.
//
// Returns the raw registration_token ONCE — UI must show it immediately
// and let the user copy. agentserver does not store the raw token.
func (s *Server) handleRegisterExecutor(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	wid := chi.URLParam(r, "wid")
	if wid == "" {
		http.Error(w, "workspace id required", http.StatusBadRequest)
		return
	}
	if !s.requireWorkspaceRole(w, r, wid, "owner", "maintainer") {
		return
	}

	var req registerExecutorReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if s.ExecutorsClient == nil {
		http.Error(w, "executors integration not configured", http.StatusServiceUnavailable)
		return
	}

	reg, err := s.ExecutorsClient.Register(r.Context(), userID, RegisterExecutorRequest{
		DisplayName: req.DisplayName,
		Description: req.Description,
		DefaultCwd:  req.DefaultCwd,
	})
	if err != nil {
		http.Error(w, "register: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Auto-bind to the workspace this request was issued under.
	if err := s.ExecutorsClient.Bind(r.Context(), userID, wid, reg.ExeID, false); err != nil {
		http.Error(w, "bind: "+err.Error(), http.StatusBadGateway)
		return
	}

	resp := registerExecutorResp{
		ExeID:             reg.ExeID,
		RegistrationToken: reg.RegistrationToken,
	}
	if s.CodexExecGatewayPublicHost != "" {
		resp.ConnectCommand = fmt.Sprintf(
			"codex exec-server --connect 'wss://%s:443/codex-exec/%s?token=%s'",
			s.CodexExecGatewayPublicHost, reg.ExeID, reg.RegistrationToken,
		)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleListExecutors returns executors bound to the workspace.
// ACL: any workspace member.
func (s *Server) handleListExecutors(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	wid := chi.URLParam(r, "wid")
	if _, ok := s.requireWorkspaceMember(w, r, wid); !ok {
		return
	}
	if s.ExecutorsClient == nil {
		_ = json.NewEncoder(w).Encode([]ListedExecutor{})
		return
	}

	rows, err := s.ExecutorsClient.List(r.Context(), userID, wid)
	if err != nil {
		http.Error(w, "list: "+err.Error(), http.StatusBadGateway)
		return
	}
	if rows == nil {
		rows = []ListedExecutor{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rows)
}

// handleUnbindExecutor removes an executor from the workspace. ACL:
// owner/maintainer of the workspace.
func (s *Server) handleUnbindExecutor(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	wid := chi.URLParam(r, "wid")
	exeID := chi.URLParam(r, "exe_id")
	if wid == "" || exeID == "" {
		http.Error(w, "wid and exe_id required", http.StatusBadRequest)
		return
	}
	if !s.requireWorkspaceRole(w, r, wid, "owner", "maintainer") {
		return
	}
	if s.ExecutorsClient == nil {
		http.Error(w, "executors integration not configured", http.StatusServiceUnavailable)
		return
	}
	if err := s.ExecutorsClient.Unbind(r.Context(), userID, wid, exeID); err != nil {
		http.Error(w, "unbind: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

