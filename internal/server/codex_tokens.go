package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/agentserver/agentserver/internal/auth"
	"github.com/agentserver/agentserver/internal/db"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	codexTokenDefaultTTLDays = 90
	codexTokenMinTTLDays     = 1
	codexTokenMaxTTLDays     = 365
)

type mintCodexTokenReq struct {
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	TTLDays     int    `json:"ttl_days,omitempty"`
}

type mintCodexTokenResp struct {
	ID          string    `json:"id"`
	Token       string    `json:"token"`
	Name        string    `json:"name"`
	WorkspaceID string    `json:"workspace_id"`
	ExpiresAt   time.Time `json:"expires_at"`
	CreatedAt   time.Time `json:"created_at"`
}

type listCodexTokenItem struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	WorkspaceID string     `json:"workspace_id"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	Revoked     bool       `json:"revoked"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

func (s *Server) handleMintCodexToken(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req mintCodexTokenReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.WorkspaceID == "" || req.Name == "" {
		http.Error(w, "workspace_id and name are required", http.StatusUnprocessableEntity)
		return
	}
	if req.TTLDays == 0 {
		req.TTLDays = codexTokenDefaultTTLDays
	}
	if req.TTLDays < codexTokenMinTTLDays || req.TTLDays > codexTokenMaxTTLDays {
		http.Error(w, "ttl_days out of range [1, 365]", http.StatusUnprocessableEntity)
		return
	}

	role, err := s.DB.GetWorkspaceMemberRole(req.WorkspaceID, userID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if role == "" || role == "guest" {
		http.Error(w, "not a member of this workspace", http.StatusForbidden)
		return
	}

	full, id, secret, err := generateCodexToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	exp := time.Now().Add(time.Duration(req.TTLDays) * 24 * time.Hour).UTC()
	if err := s.DB.CreateCodexToken(r.Context(), db.CodexToken{
		ID: id, UserID: userID, WorkspaceID: req.WorkspaceID, Name: req.Name,
		TokenHash: string(hash), ExpiresAt: exp,
	}); err != nil {
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(mintCodexTokenResp{
		ID: id, Token: full, Name: req.Name, WorkspaceID: req.WorkspaceID,
		ExpiresAt: exp, CreatedAt: time.Now().UTC(),
	})
}

func (s *Server) handleListCodexTokens(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	wid := r.URL.Query().Get("workspace_id")
	if wid == "" {
		http.Error(w, "workspace_id required", http.StatusBadRequest)
		return
	}
	role, err := s.DB.GetWorkspaceMemberRole(wid, userID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if role == "" {
		http.Error(w, "not a member", http.StatusForbidden)
		return
	}
	includeRevoked := r.URL.Query().Get("include_revoked") == "true"
	rows, err := s.DB.ListCodexTokensForWorkspace(r.Context(), wid, includeRevoked)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]listCodexTokenItem, 0, len(rows))
	for _, t := range rows {
		out = append(out, listCodexTokenItem{
			ID: t.ID, Name: t.Name, WorkspaceID: t.WorkspaceID,
			CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt,
			LastUsedAt: t.LastUsedAt, Revoked: t.RevokedAt != nil, RevokedAt: t.RevokedAt,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleRevokeCodexToken(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	row, err := s.DB.GetCodexToken(r.Context(), id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if row == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	role, _ := s.DB.GetWorkspaceMemberRole(row.WorkspaceID, userID)
	isOwner := row.UserID == userID
	isAdmin := role == "owner" || role == "maintainer"
	if !isOwner && !isAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := s.DB.RevokeCodexToken(r.Context(), id); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// routesForCodexTokens is a small chi sub-router used by tests so the
// `{id}` URL param resolves correctly when calling the handler outside the
// main Routes() wiring.
func (s *Server) routesForCodexTokens() http.Handler {
	r := chi.NewRouter()
	r.Post("/api/codex/tokens", s.handleMintCodexToken)
	r.Get("/api/codex/tokens", s.handleListCodexTokens)
	r.Delete("/api/codex/tokens/{id}", s.handleRevokeCodexToken)
	return r
}
