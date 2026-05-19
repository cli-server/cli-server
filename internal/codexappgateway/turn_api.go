package codexappgateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/broker"
	"github.com/agentserver/agentserver/internal/codexappgateway/supervisor"
)

// turnRunner abstracts the broker so the handler is unit-testable
// without spinning up real codex subprocesses.
type turnRunner interface {
	StartThread(ctx context.Context, workspaceID string) (string, error)
	Turn(ctx context.Context, workspaceID, threadID string, params json.RawMessage, timeout time.Duration) (json.RawMessage, error)
}

// turnAPIRequest mirrors the REST request defined in the design spec.
// Field names use camelCase to align 1:1 with codex v2 protocol.
type turnAPIRequest struct {
	WorkspaceID string          `json:"workspaceId"`
	ThreadID    *string         `json:"threadId,omitempty"`
	Params      json.RawMessage `json:"params"`
	TimeoutMs   int             `json:"timeoutMs,omitempty"`
}

// turnAPIResponse: either Turn (codex Turn raw) OR Transport, never both.
// ThreadID is always populated (existing or newly-created).
type turnAPIResponse struct {
	ThreadID  string          `json:"threadId"`
	Turn      json.RawMessage `json:"turn,omitempty"`
	Transport *transportError `json:"transport,omitempty"`
}

type transportError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type turnAPIHandler struct {
	runner turnRunner
}

const defaultTurnTimeout = 5 * time.Minute

func (h *turnAPIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req turnAPIRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.WorkspaceID == "" {
		http.Error(w, "workspaceId required", http.StatusBadRequest)
		return
	}
	if len(req.Params) == 0 {
		http.Error(w, "params required", http.StatusBadRequest)
		return
	}
	timeout := defaultTurnTimeout
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}

	ctx := r.Context()
	resp := turnAPIResponse{}

	threadID := ""
	if req.ThreadID != nil {
		threadID = *req.ThreadID
	}
	if threadID == "" {
		newID, err := h.runner.StartThread(ctx, req.WorkspaceID)
		if err != nil {
			resp.Transport = classifyTransport(err)
			writeJSON(w, resp)
			return
		}
		threadID = newID
	}
	resp.ThreadID = threadID

	rawTurn, err := h.runner.Turn(ctx, req.WorkspaceID, threadID, req.Params, timeout)
	if err != nil {
		resp.Transport = classifyTransport(err)
		writeJSON(w, resp)
		return
	}
	resp.Turn = rawTurn
	writeJSON(w, resp)
}

func classifyTransport(err error) *transportError {
	var te *broker.TimeoutError
	if errors.As(err, &te) {
		return &transportError{Code: "brokerTimeout", Message: te.Error()}
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "dial"), strings.Contains(msg, "connection refused"), strings.Contains(msg, "subprocess"):
		return &transportError{Code: "subprocessCrash", Message: msg}
	case strings.Contains(msg, "connection closed"), strings.Contains(msg, "ws"):
		return &transportError{Code: "wsDisconnect", Message: msg}
	default:
		return &transportError{Code: "wsDisconnect", Message: msg}
	}
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// poolRunner adapts *broker.Pool to the turnRunner interface used by
// the handler. Production wiring uses this; tests use fakes.
type poolRunner struct {
	pool *broker.Pool
}

func newPoolRunner(p *broker.Pool) *poolRunner { return &poolRunner{pool: p} }

func (r *poolRunner) StartThread(ctx context.Context, workspaceID string) (string, error) {
	conn, err := r.pool.Get(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	// Issue 3: bump lastUsedAt after the call so the reaper does not kill a
	// connection that was active throughout a long StartThread round-trip.
	defer r.pool.Touch(workspaceID)
	return conn.StartThread(ctx)
}

func (r *poolRunner) Turn(ctx context.Context, workspaceID, threadID string, params json.RawMessage, timeout time.Duration) (json.RawMessage, error) {
	conn, err := r.pool.Get(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	// Issue 3: bump lastUsedAt after Turn so a 4-minute Turn does not miss
	// the 5-minute reap deadline (lastUsedAt was only set on Get, not on
	// completion of the long-running operation).
	defer r.pool.Touch(workspaceID)
	return conn.Turn(ctx, threadID, params, timeout)
}

// makeSupervisorResolver returns a broker.WSURLResolver that uses the
// existing supervisor + buildConfig wiring. Returns the ws URL of the
// loopback codex subprocess for the workspace.
func makeSupervisorResolver(sup *supervisor.Supervisor, build func(context.Context, string, string) (supervisor.SpawnConfig, error)) broker.WSURLResolver {
	return func(ctx context.Context, workspaceID string) (string, error) {
		key := supervisor.Key{WorkspaceID: workspaceID}
		handle, err := sup.EnsureSubprocess(ctx, key, func(loopbackToken string) (supervisor.SpawnConfig, error) {
			return build(ctx, workspaceID, loopbackToken)
		})
		if err != nil {
			return "", err
		}
		return handle.WSURL, nil
	}
}
