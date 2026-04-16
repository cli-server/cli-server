package executorregistry

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleUpdateCapabilities(w http.ResponseWriter, r *http.Request) {
	executorID := chi.URLParam(r, "id")

	existing, err := s.store.GetExecutor(r.Context(), executorID)
	if err != nil {
		s.logger.Error("get executor", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "executor not found")
		return
	}

	var cap ExecutorCapability
	if err := json.NewDecoder(r.Body).Decode(&cap); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	cap.ExecutorID = executorID

	if err := s.store.UpdateCapabilities(r.Context(), executorID, cap); err != nil {
		s.logger.Error("update capabilities", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	w.WriteHeader(http.StatusOK)
}
