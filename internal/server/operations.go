package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/agentserver/agentserver/internal/db"
)

// postInternalOperations is POST /internal/operations.
// Body is a JSON object matching the agentserver "operation record" shape.
// Auth: X-Internal-Secret matching INTERNAL_API_SECRET (enforced by caller in
// server.go), same pattern as the other /internal/* endpoints.
//
// Fire-and-forget from the gateway: returns 204 on insert success. Validation
// errors return 400; DB errors return 500.
func (s *Server) postInternalOperations(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID            string          `json:"id"`
		WorkspaceID   string          `json:"workspace_id"`
		UserID        *string         `json:"user_id,omitempty"`
		Source        string          `json:"source"`
		ThreadID      *string         `json:"thread_id,omitempty"`
		RequestID     *string         `json:"request_id,omitempty"`
		EnvID         string          `json:"env_id"`
		Tool          string          `json:"tool"`
		Arguments     json.RawMessage `json:"arguments,omitempty"`
		ArgumentsMeta json.RawMessage `json:"arguments_meta,omitempty"`
		IsError       bool            `json:"is_error"`
		ResultSummary *string         `json:"result_summary,omitempty"`
		ResultMeta    json.RawMessage `json:"result_meta,omitempty"`
		StartedAt     time.Time       `json:"started_at"`
		CompletedAt   time.Time       `json:"completed_at"`
		DurationMs    int32           `json:"duration_ms"`
		NotebookPath  *string         `json:"notebook_path,omitempty"`
		CellID        *string         `json:"cell_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.ID == "" || body.WorkspaceID == "" || body.Source == "" ||
		body.EnvID == "" || body.Tool == "" {
		http.Error(w, "missing required fields (id, workspace_id, source, env_id, tool)",
			http.StatusBadRequest)
		return
	}
	op := db.Operation{
		ID:            body.ID,
		WorkspaceID:   body.WorkspaceID,
		UserID:        body.UserID,
		Source:        body.Source,
		ThreadID:      body.ThreadID,
		RequestID:     body.RequestID,
		EnvID:         body.EnvID,
		Tool:          body.Tool,
		Arguments:     body.Arguments,
		ArgumentsMeta: body.ArgumentsMeta,
		IsError:       body.IsError,
		ResultSummary: body.ResultSummary,
		ResultMeta:    body.ResultMeta,
		StartedAt:     body.StartedAt,
		CompletedAt:   body.CompletedAt,
		DurationMs:    body.DurationMs,
		NotebookPath:  body.NotebookPath,
		CellID:        body.CellID,
	}
	if err := s.DB.InsertOperation(op); err != nil {
		log.Printf("postInternalOperations: insert id=%s ws=%s: %v",
			op.ID, op.WorkspaceID, err)
		http.Error(w, "insert failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getInternalOperations is GET /internal/operations.
// Query params: workspace_id (REQUIRED), env_id, tool, source, is_error,
// since (RFC3339), id, limit.
func (s *Server) getInternalOperations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	wsID := q.Get("workspace_id")
	if wsID == "" {
		http.Error(w, "workspace_id required", http.StatusBadRequest)
		return
	}
	f := db.OperationFilter{
		WorkspaceID: wsID,
		EnvID:       q.Get("env_id"),
		Tool:        q.Get("tool"),
		Source:      q.Get("source"),
		ID:          q.Get("id"),
	}
	if v := q.Get("is_error"); v != "" {
		b := v == "true" || v == "1"
		f.IsError = &b
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			http.Error(w, "since: "+err.Error(), http.StatusBadRequest)
			return
		}
		f.Since = &t
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			http.Error(w, "limit: invalid", http.StatusBadRequest)
			return
		}
		f.Limit = n
	}
	rows, err := s.DB.ListOperations(f)
	if err != nil {
		log.Printf("getInternalOperations: list ws=%s: %v", wsID, err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	type respRow struct {
		ID            string          `json:"id"`
		WorkspaceID   string          `json:"workspace_id"`
		UserID        *string         `json:"user_id,omitempty"`
		Source        string          `json:"source"`
		ThreadID      *string         `json:"thread_id,omitempty"`
		RequestID     *string         `json:"request_id,omitempty"`
		EnvID         string          `json:"env_id"`
		Tool          string          `json:"tool"`
		Arguments     json.RawMessage `json:"arguments,omitempty"`
		ArgumentsMeta json.RawMessage `json:"arguments_meta,omitempty"`
		IsError       bool            `json:"is_error"`
		ResultSummary *string         `json:"result_summary,omitempty"`
		ResultMeta    json.RawMessage `json:"result_meta,omitempty"`
		StartedAt     time.Time       `json:"started_at"`
		CompletedAt   time.Time       `json:"completed_at"`
		DurationMs    int32           `json:"duration_ms"`
		NotebookPath  *string         `json:"notebook_path,omitempty"`
		CellID        *string         `json:"cell_id,omitempty"`
	}
	out := make([]respRow, 0, len(rows))
	for _, o := range rows {
		out = append(out, respRow{
			ID:            o.ID,
			WorkspaceID:   o.WorkspaceID,
			UserID:        o.UserID,
			Source:        o.Source,
			ThreadID:      o.ThreadID,
			RequestID:     o.RequestID,
			EnvID:         o.EnvID,
			Tool:          o.Tool,
			Arguments:     o.Arguments,
			ArgumentsMeta: o.ArgumentsMeta,
			IsError:       o.IsError,
			ResultSummary: o.ResultSummary,
			ResultMeta:    o.ResultMeta,
			StartedAt:     o.StartedAt,
			CompletedAt:   o.CompletedAt,
			DurationMs:    o.DurationMs,
			NotebookPath:  o.NotebookPath,
			CellID:        o.CellID,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"operations": out})
}

// getWorkspaceOperations is GET /api/workspaces/{id}/operations.
// User-session authed via the existing chi protected group; workspace
// membership enforced. Filter query params are the same as
// getInternalOperations; workspace_id is forced from the URL so a
// caller can't query a workspace they're not a member of.
func (s *Server) getWorkspaceOperations(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if wsID == "" {
		http.Error(w, "workspace id required", http.StatusBadRequest)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, wsID); !ok {
		return // requireWorkspaceMember has already written the response
	}
	// Force workspace_id from the URL; ignore any user-supplied query value.
	q := r.URL.Query()
	q.Set("workspace_id", wsID)
	r.URL.RawQuery = q.Encode()
	s.getInternalOperations(w, r)
}
