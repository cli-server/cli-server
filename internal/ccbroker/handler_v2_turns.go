package ccbroker

import (
	"encoding/json"
	"net/http"
)

// handleProcessTurnV2 is the fire-and-forget enqueue endpoint. It validates,
// persists the user message, inserts the agent_turns row, signals the worker,
// then returns 202 with {turn_id, events_url}. Callers fetch events separately
// via GET /api/turns/{turn_id}/events.
//
// Compared to v1 (handleProcessTurn), this never holds a long-lived HTTP
// connection — the SSE stream is consumed on a separate request — so caller
// timeouts on the POST become irrelevant.
func (s *Server) handleProcessTurnV2(w http.ResponseWriter, r *http.Request) {
	var req ProcessTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	res, err := s.enqueueTurn(r.Context(), req)
	if err != nil {
		statusCode, msg := enqueueErrorToHTTP(err)
		writeError(w, statusCode, msg)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{
		"turn_id":    res.TurnID,
		"events_url": "/api/turns/" + res.TurnID + "/events",
	})
}
