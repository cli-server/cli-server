package ccbroker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
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
	TurnID      string       `json:"turn_id,omitempty"` // optional caller-supplied id
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

const maxPendingPerSession = 16

// handleProcessTurn is the synchronous wrapper around the async queue. It:
//  1. Validates and persists the user message + agent_turns row (via enqueueTurn)
//  2. Notifies the per-session worker (via enqueueTurn)
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

	// Subscribe BEFORE enqueueing so we can't miss the worker's first event.
	// On enqueue error we unsubscribe and return.
	sub := s.sse.Subscribe(req.SessionID)
	res, err := s.enqueueTurn(r.Context(), req)
	if err != nil {
		s.sse.Unsubscribe(req.SessionID, sub)
		statusCode, msg := enqueueErrorToHTTP(err)
		writeError(w, statusCode, msg)
		return
	}
	defer s.sse.Unsubscribe(req.SessionID, sub)

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
	fmt.Fprintf(w, "data: {\"event_type\":\"turn_started\",\"turn_id\":%q}\n\n", res.TurnID)
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
			if evt.TurnID != "" && evt.TurnID != res.TurnID {
				continue // belongs to another turn on this session
			}
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			if isTerminalEventType(evt.EventType) && evt.TurnID == res.TurnID {
				fmt.Fprintf(w, "data: {\"event_type\":\"done\",\"turn_id\":%q}\n\n", res.TurnID)
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
