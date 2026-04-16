package executorregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	var req ExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if strings.TrimSpace(req.ExecutorID) == "" {
		writeError(w, http.StatusBadRequest, "executor_id is required")
		return
	}
	if strings.TrimSpace(req.Tool) == "" {
		writeError(w, http.StatusBadRequest, "tool is required")
		return
	}

	ctx := r.Context()

	executor, err := s.store.GetExecutor(ctx, req.ExecutorID)
	if err != nil {
		s.logger.Error("get executor", "executor_id", req.ExecutorID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if executor == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("executor %s not found", req.ExecutorID))
		return
	}

	timeout := 120 * time.Second
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}

	var resp ExecuteResponse
	switch executor.Type {
	case "local_agent":
		resp, err = s.execViaTunnel(ctx, req, timeout)
	case "sandbox":
		resp, err = s.execViaPodHTTP(ctx, executor, req, timeout)
	default:
		err = fmt.Errorf("unknown executor type: %s", executor.Type)
	}

	if err != nil {
		s.logger.Error("execute tool", "executor_id", req.ExecutorID, "tool", req.Tool, "error", err)
		writeJSON(w, http.StatusOK, ExecuteResponse{
			Output:   "Error: " + err.Error(),
			ExitCode: 1,
		})
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) execViaTunnel(ctx context.Context, req ExecuteRequest, timeout time.Duration) (ExecuteResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", "/tool/execute", bytes.NewReader(body))
	if err != nil {
		return ExecuteResponse{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	return s.tunnels.ExecViaTunnel(req.ExecutorID, httpReq)
}

func (s *Server) execViaPodHTTP(ctx context.Context, executor *ExecutorInfo, req ExecuteRequest, timeout time.Duration) (ExecuteResponse, error) {
	return ExecuteResponse{}, fmt.Errorf("sandbox HTTP execution not yet implemented for %s", executor.ID)
}
