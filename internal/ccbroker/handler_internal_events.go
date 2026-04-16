package ccbroker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// handleWorkerInternalEvents handles POST .../worker/internal-events — persists a batch of internal events.
func (s *Server) handleWorkerInternalEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := SessionIDFromContext(r.Context())

	var req InternalEventBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate epoch.
	currentEpoch, err := s.store.GetSessionEpoch(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check epoch")
		return
	}
	if req.WorkerEpoch != currentEpoch {
		writeError(w, http.StatusConflict, fmt.Sprintf("epoch mismatch: got %d, current %d", req.WorkerEpoch, currentEpoch))
		return
	}

	// Build InternalEventInput list, extracting event_type from each payload.
	inputs := make([]InternalEventInput, 0, len(req.Events))
	for _, item := range req.Events {
		// Extract "type" field from payload JSON.
		var payloadMap map[string]json.RawMessage
		eventType := ""
		if err := json.Unmarshal(item.Payload, &payloadMap); err == nil {
			if typeRaw, ok := payloadMap["type"]; ok {
				var t string
				if err := json.Unmarshal(typeRaw, &t); err == nil {
					eventType = t
				}
			}
		}

		inputs = append(inputs, InternalEventInput{
			EventType:    eventType,
			Payload:      item.Payload,
			IsCompaction: item.IsCompaction,
			AgentID:      item.AgentID,
		})
	}

	if err := s.store.InsertInternalEvents(r.Context(), sessionID, inputs); err != nil {
		s.logger.Error("insert internal events failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to persist internal events")
		return
	}

	w.WriteHeader(http.StatusOK)
}

// handleGetInternalEvents handles GET .../worker/internal-events — returns internal events since a sequence number.
func (s *Server) handleGetInternalEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := SessionIDFromContext(r.Context())

	var fromSeq int64
	if v := r.URL.Query().Get("from_sequence_num"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid from_sequence_num")
			return
		}
		fromSeq = parsed
	}

	events, err := s.store.GetInternalEventsSince(r.Context(), sessionID, fromSeq, 1000)
	if err != nil {
		s.logger.Error("get internal events failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to retrieve internal events")
		return
	}

	if events == nil {
		events = []SessionEvent{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": events})
}
