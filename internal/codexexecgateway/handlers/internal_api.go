package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// InternalConnectedStore is the subset of storage required by Connected.
// It uses the local ConnectedExecutor type (defined in workspace_binding.go)
// to avoid an import cycle with the parent codexexecgateway package.
type InternalConnectedStore interface {
	ConnectedExecutorsForWorkspace(ctx context.Context, workspaceID string, connectedIDs []string) ([]ConnectedExecutor, error)
}

// Registry is satisfied by *codexexecgateway.ConnRegistry.
type Registry interface {
	ConnectedIDs() []string
}

// Connected returns the intersection of (workspace's bound executors) ∩
// (currently-connected exe_ids). Used by codex-app-gateway when composing
// the per-turn manifest.
func Connected(store InternalConnectedStore, reg Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wid := r.URL.Query().Get("workspace_id")
		if wid == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "workspace_id required"})
			return
		}
		ids := reg.ConnectedIDs()
		rows, err := store.ConnectedExecutorsForWorkspace(r.Context(), wid, ids)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list"})
			return
		}
		if rows == nil {
			rows = []ConnectedExecutor{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rows) //nolint:errcheck
	}
}

// RevokedAdder is satisfied by *codexexecgateway.RevokedSet.
type RevokedAdder interface {
	Add(turnID string, exp int64) (evictedLive bool)
}

type revokeRequest struct {
	TurnID string `json:"turn_id"`
	Exp    int64  `json:"exp"`
}

// RevokeTurn adds a turn_id to the in-memory revoked set so future bridge
// connect attempts presenting that turn's CODEX_EXEC_GATEWAY_TOKEN are
// rejected even within the token's exp window.
func RevokeTurn(rev RevokedAdder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req revokeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if req.TurnID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "turn_id required"})
			return
		}
		// If caller omits exp, default to "1 hour from now" (spec turn slack).
		if req.Exp == 0 {
			req.Exp = timeNowUnix() + 3600
		}
		if evictedLive := rev.Add(req.TurnID, req.Exp); evictedLive {
			slog.Warn("revoke-turn: revoked-set at capacity, evicted a still-live revocation; previously-revoked token may be usable until its own expiry",
				"turn_id", req.TurnID)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// timeNowUnix exists as a small indirection so tests could later swap time.
func timeNowUnix() int64 { return time.Now().Unix() }
