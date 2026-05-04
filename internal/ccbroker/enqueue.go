package ccbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

// errEnqueueValidation and errEnqueueDepth are sentinel errors so handlers
// can map them to HTTP status codes.
var (
	errEnqueueValidation = errors.New("session_id, workspace_id, and user_message are required")
	errEnqueueDepth      = errors.New("too many pending turns for this session")
)

// enqueueResult carries the IDs the handler needs to subscribe / respond.
type enqueueResult struct {
	TurnID      string
	UserEventID string
	Epoch       int
}

// enqueueTurn is the shared work between v1 (handleProcessTurn) and v2
// (handleProcessTurnV2): validate, ensure session, depth-check, persist user
// message + agent_turns row, Notify the worker. Streaming/response is the
// caller's responsibility.
func (s *Server) enqueueTurn(ctx context.Context, req ProcessTurnRequest) (*enqueueResult, error) {
	if req.SessionID == "" || req.WorkspaceID == "" || req.UserMessage == "" {
		return nil, errEnqueueValidation
	}

	sess, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	if sess == nil {
		if err := s.store.CreateSession(ctx, req.SessionID, req.WorkspaceID, "", "api", nil); err != nil {
			return nil, fmt.Errorf("create session: %w", err)
		}
	}

	pending, err := s.store.CountPending(ctx, req.SessionID)
	if err != nil {
		return nil, fmt.Errorf("count pending: %w", err)
	}
	if pending >= maxPendingPerSession {
		return nil, errEnqueueDepth
	}

	turnID := req.TurnID
	if turnID == "" {
		turnID = "trn_" + uuid.NewString()
	}

	epoch, err := s.store.GetSessionEpoch(ctx, req.SessionID)
	if err != nil {
		return nil, fmt.Errorf("get epoch: %w", err)
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
	if _, err := s.store.InsertEventsWithTurn(ctx, req.SessionID, epoch, turnID, []EventInput{
		{EventID: userEventID, Payload: userPayload, Ephemeral: false},
	}); err != nil {
		return nil, fmt.Errorf("insert user message: %w", err)
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
	if err := s.store.EnqueueTurn(ctx, turn); err != nil {
		return nil, fmt.Errorf("enqueue: %w", err)
	}

	s.workerRegistry.Notify(req.SessionID)

	return &enqueueResult{TurnID: turnID, UserEventID: userEventID, Epoch: epoch}, nil
}

// enqueueErrorToHTTP maps sentinel errors to (status, message) pairs.
// Used by both v1 and v2 handlers to return consistent error responses.
func enqueueErrorToHTTP(err error) (int, string) {
	switch {
	case errors.Is(err, errEnqueueValidation):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, errEnqueueDepth):
		return http.StatusTooManyRequests, err.Error()
	default:
		return http.StatusInternalServerError, "internal error"
	}
}
