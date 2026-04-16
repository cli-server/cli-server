package executorregistry

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleListExecutors(w http.ResponseWriter, r *http.Request) {
	workspaceID := r.URL.Query().Get("workspace_id")
	if strings.TrimSpace(workspaceID) == "" {
		writeError(w, http.StatusBadRequest, "workspace_id query parameter is required")
		return
	}

	executors, err := s.store.ListExecutors(r.Context(), workspaceID)
	if err != nil {
		s.logger.Error("list executors", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if executors == nil {
		executors = []ExecutorInfo{}
	}

	writeJSON(w, http.StatusOK, executors)
}

func (s *Server) handleGetExecutor(w http.ResponseWriter, r *http.Request) {
	executorID := chi.URLParam(r, "id")

	info, err := s.store.GetExecutor(r.Context(), executorID)
	if err != nil {
		s.logger.Error("get executor", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if info == nil {
		writeError(w, http.StatusNotFound, "executor not found")
		return
	}

	writeJSON(w, http.StatusOK, info)
}
