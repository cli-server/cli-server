package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/agentserver/agentserver/internal/db"
)

// handleCreateTask creates a new delegated task.
// POST /api/workspaces/{wid}/tasks
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	s.handleCreateTaskForWorkspace(w, r, chi.URLParam(r, "wid"))
}

func (s *Server) handleCreateTaskForWorkspace(w http.ResponseWriter, r *http.Request, wid string) {
	var req struct {
		TargetID        string   `json:"target_id"`
		Skill           string   `json:"skill,omitempty"`
		Prompt          string   `json:"prompt"`
		SystemContext   string   `json:"system_context,omitempty"`
		MaxTurns        int      `json:"max_turns,omitempty"`
		MaxBudgetUSD    float64  `json:"max_budget_usd,omitempty"`
		TimeoutSeconds  int      `json:"timeout_seconds,omitempty"`
		DelegationChain []string `json:"delegation_chain,omitempty"`
		RequesterID     string   `json:"requester_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.TargetID == "" || req.Prompt == "" {
		http.Error(w, "target_id and prompt required", http.StatusBadRequest)
		return
	}

	// Validate target exists and belongs to workspace.
	targetSbx, err := s.DB.GetSandbox(req.TargetID)
	if err != nil || targetSbx == nil {
		http.Error(w, "target agent not found", http.StatusNotFound)
		return
	}
	if targetSbx.WorkspaceID != wid {
		http.Error(w, "target agent not in workspace", http.StatusForbidden)
		return
	}

	if req.TimeoutSeconds <= 0 {
		req.TimeoutSeconds = 300
	}

	taskID := "task_" + uuid.New().String()

	task := &db.AgentTask{
		ID:              taskID,
		WorkspaceID:     wid,
		RequesterID:     sql.NullString{String: req.RequesterID, Valid: req.RequesterID != ""},
		TargetID:        req.TargetID,
		Skill:           sql.NullString{String: req.Skill, Valid: req.Skill != ""},
		Prompt:          req.Prompt,
		SystemContext:   sql.NullString{String: req.SystemContext, Valid: req.SystemContext != ""},
		Status:          "pending",
		TimeoutSeconds:  req.TimeoutSeconds,
		DelegationChain: req.DelegationChain,
	}

	if err := s.DB.CreateAgentTask(task); err != nil {
		log.Printf("create task: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	actor := req.RequesterID
	var actorPtr *string
	if actor != "" {
		actorPtr = &actor
	}
	s.logInteraction(wid, actorPtr, "task_created", taskID, "task", map[string]any{
		"target_id": req.TargetID, "skill": req.Skill,
	})

	// Create a bridge session for the task if BridgeHandler is available.
	var sessionID string
	if s.BridgeHandler != nil {
		sessionID = "cse_" + uuid.New().String()
		if err := s.DB.CreateAgentSession(sessionID, req.TargetID, wid, fmt.Sprintf("Task: %s", taskID), nil); err != nil {
			log.Printf("create task session: %v", err)
		} else {
			s.DB.Exec(`UPDATE agent_tasks SET session_id = $1 WHERE id = $2`, sessionID, taskID)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{
		"task_id":    taskID,
		"session_id": sessionID,
		"status":     "pending",
	})
}

// handleListTasks lists tasks for a workspace.
// GET /api/workspaces/{wid}/tasks
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	wid := chi.URLParam(r, "wid")
	tasks, err := s.DB.ListAgentTasksByWorkspace(wid, 100)
	if err != nil {
		log.Printf("list tasks: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if tasks == nil {
		tasks = []db.AgentTask{}
	}

	type taskResponse struct {
		ID          string   `json:"task_id"`
		TargetID    string   `json:"target_id"`
		RequesterID string   `json:"requester_id"`
		Skill       string   `json:"skill,omitempty"`
		Status      string   `json:"status"`
		Prompt      string   `json:"prompt"`
		NumTurns    int      `json:"num_turns"`
		TotalCost   *float64 `json:"total_cost_usd,omitempty"`
		CreatedAt   string   `json:"created_at"`
		CompletedAt *string  `json:"completed_at,omitempty"`
	}

	result := make([]taskResponse, len(tasks))
	for i, t := range tasks {
		tr := taskResponse{
			ID:          t.ID,
			TargetID:    t.TargetID,
			RequesterID: t.RequesterID.String,
			Status:      t.Status,
			Prompt:      t.Prompt,
			NumTurns:    t.NumTurns,
			TotalCost:   t.TotalCostUSD,
			CreatedAt:   t.CreatedAt.Format(time.RFC3339),
		}
		if t.Skill.Valid {
			tr.Skill = t.Skill.String
		}
		if t.CompletedAt.Valid {
			s := t.CompletedAt.Time.Format(time.RFC3339)
			tr.CompletedAt = &s
		}
		result[i] = tr
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleGetTask returns a single task.
// GET /api/tasks/{id}
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "id")
	task, err := s.DB.GetAgentTask(taskID)
	if err != nil {
		log.Printf("get task: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if task == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	resp := map[string]any{
		"task_id":      task.ID,
		"workspace_id": task.WorkspaceID,
		"requester_id": task.RequesterID.String,
		"target_id":    task.TargetID,
		"prompt":       task.Prompt,
		"status":       task.Status,
		"num_turns":    task.NumTurns,
		"created_at":   task.CreatedAt.Format(time.RFC3339),
	}
	if task.SessionID.Valid {
		resp["session_id"] = task.SessionID.String
	}
	if task.Skill.Valid {
		resp["skill"] = task.Skill.String
	}
	if task.TotalCostUSD != nil {
		resp["total_cost_usd"] = *task.TotalCostUSD
	}
	if task.ResultJSON != nil {
		resp["result"] = task.ResultJSON
	}
	if task.FailureReason.Valid {
		resp["failure_reason"] = task.FailureReason.String
	}
	if task.CompletedAt.Valid {
		resp["completed_at"] = task.CompletedAt.Time.Format(time.RFC3339)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handlePollTasks returns pending tasks for a worker agent.
// GET /api/agent/tasks/poll?sandbox_id=xxx
// Auth: proxy_token
func (s *Server) handlePollTasks(w http.ResponseWriter, r *http.Request) {
	// Auth via proxy_token.
	token := r.Header.Get("Authorization")
	if len(token) > 7 && token[:7] == "Bearer " {
		token = token[7:]
	} else {
		http.Error(w, "missing authorization", http.StatusUnauthorized)
		return
	}

	sbx, err := s.DB.GetSandboxByAnyToken(token)
	if err != nil || sbx == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	sandboxID := r.URL.Query().Get("sandbox_id")
	if sandboxID == "" {
		sandboxID = sbx.ID
	}
	if sandboxID != sbx.ID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	tasks, err := s.DB.ListPendingAgentTasksByTarget(sandboxID, 5)
	if err != nil {
		log.Printf("poll tasks: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if tasks == nil {
		tasks = []db.AgentTask{}
	}

	type pollResponse struct {
		ID            string  `json:"task_id"`
		Prompt        string  `json:"prompt"`
		SystemContext string  `json:"system_context"`
		MaxTurns      int     `json:"max_turns"`
		MaxBudgetUSD  float64 `json:"max_budget_usd"`
		SessionID     string  `json:"session_id,omitempty"`
	}

	result := make([]pollResponse, len(tasks))
	for i, t := range tasks {
		result[i] = pollResponse{
			ID:     t.ID,
			Prompt: t.Prompt,
		}
		if t.SystemContext.Valid {
			result[i].SystemContext = t.SystemContext.String
		}
		if t.SessionID.Valid {
			result[i].SessionID = t.SessionID.String
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleUpdateTaskStatus updates task status from the worker.
// PUT /api/agent/tasks/{id}/status
// Auth: proxy_token
func (s *Server) handleUpdateTaskStatus(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("Authorization")
	if len(token) > 7 && token[:7] == "Bearer " {
		token = token[7:]
	} else {
		http.Error(w, "missing authorization", http.StatusUnauthorized)
		return
	}
	sbx, err := s.DB.GetSandboxByAnyToken(token)
	if err != nil || sbx == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	taskID := chi.URLParam(r, "id")
	var req struct {
		Status        string          `json:"status"`
		FailureReason string          `json:"failure_reason,omitempty"`
		Result        json.RawMessage `json:"result,omitempty"`
		TotalCostUSD  *float64        `json:"total_cost_usd,omitempty"`
		NumTurns      int             `json:"num_turns,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	switch req.Status {
	case "running", "completed", "failed", "cancelled":
	default:
		http.Error(w, "invalid status", http.StatusBadRequest)
		return
	}

	switch {
	case req.Status == "failed" && req.FailureReason != "":
		s.DB.FailAgentTask(taskID, req.FailureReason)
	case req.Status == "completed" && len(req.Result) > 0:
		s.DB.UpdateAgentTaskResult(taskID, req.Result, req.TotalCostUSD, req.NumTurns)
	default:
		s.DB.UpdateAgentTaskStatus(taskID, req.Status)
	}

	s.logInteraction(sbx.WorkspaceID, &sbx.ID, "task_status_changed", taskID, "task", map[string]any{
		"status": req.Status,
	})

	w.WriteHeader(http.StatusOK)
}

// handleCancelTask cancels a running task.
// POST /api/tasks/{id}/cancel
func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "id")
	task, err := s.DB.GetAgentTask(taskID)
	if err != nil || task == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if task.Status == "completed" || task.Status == "failed" || task.Status == "cancelled" {
		http.Error(w, "task already finished", http.StatusConflict)
		return
	}

	if err := s.DB.UpdateAgentTaskStatus(taskID, "cancelled"); err != nil {
		log.Printf("cancel task: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.logInteraction(task.WorkspaceID, nil, "task_cancelled", taskID, "task", nil)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "cancelled"})
}

// handleTaskStream proxies the task's bridge session SSE stream.
// GET /api/tasks/{id}/stream
func (s *Server) handleTaskStream(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "id")
	task, err := s.DB.GetAgentTask(taskID)
	if err != nil || task == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !task.SessionID.Valid || s.BridgeHandler == nil {
		http.Error(w, "no session for this task", http.StatusNotFound)
		return
	}

	sessionID := task.SessionID.String
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Parse from_sequence_num.
	var fromSeq int64
	if v := r.URL.Query().Get("from_sequence_num"); v != "" {
		fmt.Sscanf(v, "%d", &fromSeq)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Replay from DB.
	events, _ := s.DB.GetAgentSessionEventsSince(sessionID, fromSeq, 1000)
	for _, e := range events {
		data, _ := json.Marshal(map[string]any{
			"event_id":     e.EventID,
			"sequence_num": e.ID,
			"event_type":   e.EventType,
			"source":       e.Source,
			"payload":      e.Payload,
			"created_at":   e.CreatedAt.Format(time.RFC3339Nano),
		})
		fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", e.EventType, e.ID, data)
	}
	flusher.Flush()

	// Subscribe to live events.
	sub := s.BridgeHandler.SSE.Subscribe(sessionID)
	defer s.BridgeHandler.SSE.Unsubscribe(sessionID, sub)

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case event := <-sub.Ch:
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", event.EventType, event.SequenceNum, data)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ":keepalive\n\n")
			flusher.Flush()
		case <-sub.Done():
			return
		case <-r.Context().Done():
			return
		}
	}
}
