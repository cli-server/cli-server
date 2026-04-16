package ccbroker

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// handleWorkerState handles PUT .../worker — updates the worker state and metadata.
func (s *Server) handleWorkerState(w http.ResponseWriter, r *http.Request) {
	sessionID := SessionIDFromContext(r.Context())

	var req WorkerStateRequest
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

	if err := s.store.UpdateWorkerState(r.Context(), sessionID, req.WorkerEpoch, req.WorkerStatus, req.ExternalMetadata, req.RequiresActionDetails); err != nil {
		s.logger.Error("update worker state failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update worker state")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleWorkerHeartbeat handles POST .../worker/heartbeat — updates the worker heartbeat timestamp.
func (s *Server) handleWorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	sessionID := SessionIDFromContext(r.Context())

	var req HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Use req.WorkerEpoch if set, otherwise fall back to JWT epoch.
	epoch := req.WorkerEpoch
	if epoch == 0 {
		epoch = EpochFromContext(r.Context())
	}

	// Validate epoch.
	currentEpoch, err := s.store.GetSessionEpoch(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check epoch")
		return
	}
	if epoch != currentEpoch {
		writeError(w, http.StatusConflict, fmt.Sprintf("epoch mismatch: got %d, current %d", epoch, currentEpoch))
		return
	}

	if err := s.store.UpdateWorkerHeartbeat(r.Context(), sessionID, epoch); err != nil {
		s.logger.Error("update worker heartbeat failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update heartbeat")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
