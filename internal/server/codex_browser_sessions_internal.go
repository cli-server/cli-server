package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/agentserver/agentserver/internal/db"
)

// sessionOpenReq is what CXG POSTs to /api/internal/codex/tokens/session-open
// when a `codex --remote` ws connection is accepted. The bearer token is
// re-verified (same bcrypt + expiry + revocation checks as Verify) so a
// malicious or buggy CXG can't fabricate sessions for arbitrary token ids.
type sessionOpenReq struct {
	Token        string `json:"token"`
	ClientIP     string `json:"client_ip,omitempty"`
	ClientUA     string `json:"client_ua,omitempty"`
	CodexVersion string `json:"codex_version,omitempty"`
	OS           string `json:"os,omitempty"`
}

type sessionOpenResp struct {
	UserID      string `json:"user_id"`
	WorkspaceID string `json:"workspace_id"`
	SessionID   string `json:"session_id"`
}

// handleCodexSessionOpen verifies the token, inserts a new browser-session
// row, and returns the session id so CXG can call session-close on ws
// disconnect.
func (s *Server) handleCodexSessionOpen(w http.ResponseWriter, r *http.Request) {
	var req sessionOpenReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeVerifyUnauthorized(w)
		return
	}
	id, secret, err := parseCodexToken(req.Token)
	if err != nil {
		writeVerifyUnauthorized(w)
		return
	}
	row, err := s.DB.GetCodexToken(r.Context(), id)
	if err != nil || row == nil {
		writeVerifyUnauthorized(w)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(row.TokenHash), []byte(secret)); err != nil {
		writeVerifyUnauthorized(w)
		return
	}
	if row.RevokedAt != nil || time.Now().UTC().After(row.ExpiresAt) {
		writeVerifyUnauthorized(w)
		return
	}

	sessionID, err := newSessionID()
	if err != nil {
		log.Printf("session-open: gen id: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.DB.CreateCodexBrowserSession(r.Context(), db.CodexBrowserSession{
		ID:           sessionID,
		TokenID:      row.ID,
		ClientIP:     req.ClientIP,
		ClientUA:     req.ClientUA,
		CodexVersion: req.CodexVersion,
		OS:           req.OS,
	}); err != nil {
		log.Printf("session-open: insert: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Best-effort touch on the parent token row so existing UI keeps showing
	// "last used" updates.
	go func(tid string) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.DB.TouchCodexToken(ctx, tid); err != nil {
			log.Printf("session-open: touch %s: %v", tid, err)
		}
	}(row.ID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sessionOpenResp{
		UserID:      row.UserID,
		WorkspaceID: row.WorkspaceID,
		SessionID:   sessionID,
	})
}

type sessionCloseReq struct {
	SessionID string `json:"session_id"`
}

type sessionUpdateReq struct {
	SessionID    string `json:"session_id"`
	ClientUA     string `json:"client_ua"`
	CodexVersion string `json:"codex_version"`
	OS           string `json:"os"`
}

// handleCodexSessionUpdate refreshes the meta columns on an existing
// browser-session row (set by CXG after parsing the JSON-RPC initialize
// frame). Idempotent — missing rows return 204 same as present.
func (s *Server) handleCodexSessionUpdate(w http.ResponseWriter, r *http.Request) {
	var req sessionUpdateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}
	if err := s.DB.UpdateCodexBrowserSessionMeta(r.Context(), req.SessionID, req.ClientUA, req.CodexVersion, req.OS); err != nil {
		log.Printf("session-update: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCodexSessionClose stamps disconnected_at on the row. Idempotent.
// Auth: X-Internal-Secret (same as the other internal endpoints) so any
// agentserver-pod-local caller can close — no per-session capability is
// needed, the session_id itself is unguessable.
func (s *Server) handleCodexSessionClose(w http.ResponseWriter, r *http.Request) {
	var req sessionCloseReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}
	if err := s.DB.CloseCodexBrowserSession(r.Context(), req.SessionID); err != nil {
		log.Printf("session-close: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// newSessionID returns an unguessable 128-bit hex id, prefixed so a leak
// can't be mistaken for a codex token (which start with "cxt_").
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "cbs_" + hex.EncodeToString(b[:]), nil
}
