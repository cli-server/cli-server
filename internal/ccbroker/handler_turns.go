package ccbroker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// TurnMetadata carries optional per-turn metadata sent by TUI / API callers.
type TurnMetadata struct {
	ChannelType         string `json:"channel_type,omitempty"`
	CreatorUserID       string `json:"creator_user_id,omitempty"`
	PermissionMode      string `json:"permission_mode,omitempty"`
	Model               string `json:"model,omitempty"`
	PreferredExecutorID string `json:"preferred_executor_id,omitempty"`
	TurnKind            string `json:"turn_kind,omitempty"`
}

type ProcessTurnRequest struct {
	SessionID   string       `json:"session_id"`
	WorkspaceID string       `json:"workspace_id"`
	UserMessage string       `json:"user_message"`
	IMChannelID string       `json:"im_channel_id,omitempty"`
	IMUserID    string       `json:"im_user_id,omitempty"`
	Metadata    TurnMetadata `json:"metadata,omitempty"`
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

const maxPendingPerSession = 16

// handleProcessTurn is the synchronous wrapper around the async queue. It:
//  1. Validates and persists the user message + agent_turns row
//  2. Notifies the per-session worker
//  3. Subscribes to SSEBroker, filters by this turn_id, streams to client
//  4. Returns when a terminal event for this turn arrives, or on disconnect
//
// The handler never calls runner.Run; the worker does.
func (s *Server) handleProcessTurn(w http.ResponseWriter, r *http.Request) {
	var req ProcessTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SessionID == "" || req.WorkspaceID == "" || req.UserMessage == "" {
		writeError(w, http.StatusBadRequest, "session_id, workspace_id, and user_message are required")
		return
	}

	// Ensure session exists.
	sess, err := s.store.GetSession(r.Context(), req.SessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check session")
		return
	}
	if sess == nil {
		if err := s.store.CreateSession(r.Context(), req.SessionID, req.WorkspaceID, "", "api", nil); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create session")
			return
		}
	}

	// Per-session backpressure.
	pending, err := s.store.CountPending(r.Context(), req.SessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check pending depth")
		return
	}
	if pending >= maxPendingPerSession {
		writeError(w, http.StatusTooManyRequests, "too many pending turns for this session")
		return
	}

	turnID := "trn_" + uuid.NewString()

	epoch, err := s.store.GetSessionEpoch(r.Context(), req.SessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get session epoch")
		return
	}

	userEventID := uuid.NewString()
	userPayload, _ := json.Marshal(map[string]interface{}{
		"type": "user",
		"message": map[string]interface{}{
			"role":    "user",
			"content": req.UserMessage,
		},
		"parent_tool_use_id": nil,
		"session_id":         req.SessionID,
	})
	if _, err := s.store.InsertEventsWithTurn(r.Context(), req.SessionID, epoch, turnID, []EventInput{
		{EventID: userEventID, Payload: userPayload, Ephemeral: false},
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to insert user message")
		return
	}

	metaBytes, _ := json.Marshal(req.Metadata)
	turn := AgentTurn{
		ID:          turnID,
		SessionID:   req.SessionID,
		WorkspaceID: req.WorkspaceID,
		UserEventID: userEventID,
		UserMessage: req.UserMessage,
		Metadata:    metaBytes,
	}
	if req.IMChannelID != "" {
		turn.IMChannelID.String, turn.IMChannelID.Valid = req.IMChannelID, true
	}
	if req.IMUserID != "" {
		turn.IMUserID.String, turn.IMUserID.Valid = req.IMUserID, true
	}
	if err := s.store.EnqueueTurn(r.Context(), turn); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to enqueue turn")
		return
	}

	// Subscribe BEFORE notifying so we can't miss the worker's first event.
	sub := s.sse.Subscribe(req.SessionID)
	defer s.sse.Unsubscribe(req.SessionID, sub)

	s.workerRegistry.Notify(req.SessionID)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Send turn_id in the prelude so clients know which turn this stream
	// represents.
	fmt.Fprintf(w, "data: {\"event_type\":\"turn_started\",\"turn_id\":%q}\n\n", turnID)
	flusher.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			// Client disconnected; the worker keeps running. Cancel is a
			// separate explicit endpoint.
			return
		case evt := <-sub.Ch:
			if evt.TurnID != "" && evt.TurnID != turnID {
				continue // belongs to another turn on this session
			}
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			if isTerminalEventType(evt.EventType) && evt.TurnID == turnID {
				fmt.Fprintf(w, "data: {\"event_type\":\"done\",\"turn_id\":%q}\n\n", turnID)
				flusher.Flush()
				return
			}
		case <-sub.Done():
			return
		case <-keepalive.C:
			fmt.Fprintf(w, ":keepalive\n\n")
			flusher.Flush()
		}
	}
}

func isTerminalEventType(t string) bool {
	switch t {
	case "turn_done", "turn_cancelled", "turn_failed":
		return true
	}
	return false
}
