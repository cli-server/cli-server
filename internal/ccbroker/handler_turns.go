package ccbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// ProcessTurnRequest is the external API request body for POST /api/turns.
//
// IMChannelID and IMUserID are optional. When set (for turns originated by an
// IM inbound) the cc-broker ToolRouter can route send_* MCP tool calls back
// through imbridge to the originating IM channel / user.
type ProcessTurnRequest struct {
	SessionID   string `json:"session_id"`
	WorkspaceID string `json:"workspace_id"`
	UserMessage string `json:"user_message"`
	IMChannelID string `json:"im_channel_id,omitempty"`
	IMUserID    string `json:"im_user_id,omitempty"`
}

// handleProcessTurn handles POST /api/turns. It acquires the turn lock for
// the session, ensures the session exists, inserts the user message, spawns
// a CC worker, and streams SSE events back to the caller until the worker
// exits or the client disconnects.
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

	// Acquire turn lock so only one turn runs per session at a time.
	s.turnLock.Acquire(req.SessionID)
	defer s.turnLock.Release(req.SessionID)

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

	// Get current epoch for event insertion.
	epoch, err := s.store.GetSessionEpoch(r.Context(), req.SessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get session epoch")
		return
	}

	// Insert user message as an event. The payload follows the Claude Code
	// SDK's SDKUserMessage shape (type:"user", message:{role,content},
	// parent_tool_use_id, session_id) — CC parses events from the bridge
	// event-stream against this structure. A simpler `{type, content}`
	// payload is silently ignored by CC and the turn runs with no user input.
	eventUUID := uuid.NewString()
	payload, _ := json.Marshal(map[string]interface{}{
		"type": "user",
		"message": map[string]interface{}{
			"role":    "user",
			"content": req.UserMessage,
		},
		"parent_tool_use_id": nil,
		"session_id":         req.SessionID,
	})
	_, err = s.store.InsertEvents(r.Context(), req.SessionID, epoch, []EventInput{
		{
			EventID:   eventUUID,
			Payload:   payload,
			Ephemeral: false,
		},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to insert user message")
		return
	}

	// Spawn CC worker.
	worker, err := s.SpawnWorker(r.Context(), req.SessionID, req.WorkspaceID, req.IMChannelID, req.IMUserID)
	if err != nil {
		s.logger.Error("spawn worker failed", "session_id", req.SessionID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to spawn worker")
		return
	}

	// Check flusher BEFORE setting SSE headers.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		go s.CleanupWorker(context.Background(), worker)
		return
	}

	// Set SSE response headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Subscribe to session SSE events.
	sub := s.sse.Subscribe(req.SessionID)
	defer s.sse.Unsubscribe(req.SessionID, sub)

	// Wait for CC process to exit in a background goroutine.
	done := make(chan struct{})
	go func() {
		worker.Process.Wait() //nolint:errcheck
		close(done)
	}()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			// Client disconnected. Kill the CC process and clean up in background.
			worker.Process.Process.Kill() //nolint:errcheck
			go s.CleanupWorker(context.Background(), worker)
			return

		case evt := <-sub.Ch:
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()

		case <-sub.Done():
			// Subscriber was closed (e.g. channel overflow). Clean up.
			go s.CleanupWorker(context.Background(), worker)
			return

		case <-done:
			// CC process exited. Drain remaining buffered events.
			for {
				select {
				case evt := <-sub.Ch:
					data, _ := json.Marshal(evt)
					fmt.Fprintf(w, "data: %s\n\n", data)
					flusher.Flush()
				default:
					goto drained
				}
			}
		drained:
			// Send done sentinel.
			fmt.Fprintf(w, "data: {\"event_type\":\"done\"}\n\n")
			flusher.Flush()
			// Clean up in background (r.Context may be cancelled).
			go s.CleanupWorker(context.Background(), worker)
			return

		case <-keepalive.C:
			fmt.Fprintf(w, ":keepalive\n\n")
			flusher.Flush()
		}
	}
}
