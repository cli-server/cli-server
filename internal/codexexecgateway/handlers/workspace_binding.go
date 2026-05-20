package handlers

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/agentserver/agentserver/internal/codexexecgateway/execmodel"
	"github.com/go-chi/chi/v5"
)

// BindingStore is the subset of storage required by the workspace binding handlers.
type BindingStore interface {
	BindWorkspaceExecutor(ctx context.Context, workspaceID, exeID, name, description string, isDefault bool) error
	UnbindWorkspaceExecutor(ctx context.Context, workspaceID, exeID string) error
	ListWorkspaceExecutors(ctx context.Context, workspaceID string) ([]execmodel.ConnectedExecutor, error)
}

// OnlineSet reports whether an exe_id has a live inbound ws right now. The
// gateway's ConnRegistry satisfies this via a tiny adapter in server.go.
// Defined here as a func type so the handler stays loosely coupled.
type OnlineSet func() map[string]struct{}

type bindRequest struct {
	ExeID       string `json:"exe_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	IsDefault   bool   `json:"is_default"`
}

// PostBinding returns an http.HandlerFunc that binds an executor to a workspace.
func PostBinding(store BindingStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wid := chi.URLParam(r, "wid")
		var req bindRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if req.ExeID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "exe_id required"})
			return
		}
		if req.Name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name required"})
			return
		}
		if err := store.BindWorkspaceExecutor(r.Context(), wid, req.ExeID, req.Name, req.Description, req.IsDefault); err != nil {
			// Pq unique-violation surfaces as a Postgres error; we surface
			// "name already taken" generically to avoid leaking schema.
			writeJSON(w, http.StatusConflict, map[string]string{"error": "bind failed (name may already be taken in this workspace)"})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
	}
}

// DeleteBinding returns an http.HandlerFunc that removes a workspace ↔ executor binding.
func DeleteBinding(store BindingStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wid := chi.URLParam(r, "wid")
		exeID := chi.URLParam(r, "exe_id")
		if err := store.UnbindWorkspaceExecutor(r.Context(), wid, exeID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unbind"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ListBinding returns an http.HandlerFunc that lists all executors bound to a
// workspace, annotated with IsOnline from the live registry so the UI doesn't
// have to guess from last_seen_at.
func ListBinding(store BindingStore, online OnlineSet) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wid := chi.URLParam(r, "wid")
		rows, err := store.ListWorkspaceExecutors(r.Context(), wid)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list"})
			return
		}
		if rows == nil {
			rows = []execmodel.ConnectedExecutor{}
		}
		var onlineIDs map[string]struct{}
		if online != nil {
			onlineIDs = online()
		}
		for i := range rows {
			if onlineIDs != nil {
				_, ok := onlineIDs[rows[i].ExeID]
				rows[i].IsOnline = ok
			}
		}
		writeJSON(w, http.StatusOK, rows)
	}
}
