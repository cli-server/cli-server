package ccbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"
	"github.com/google/uuid"

	"github.com/agentserver/agentserver/internal/ccbroker/runner"
	"github.com/agentserver/agentserver/internal/ccbroker/tools"
	"github.com/agentserver/agentserver/internal/ccbroker/workspace"
)

// Test seams. Do not reassign in production.
var (
	workspaceSetup    = workspace.Setup
	workspaceTeardown = workspace.Teardown
	runnerRun         = func(ctx context.Context, ws *workspace.Workspace, sessionID, userMessage string, cfg runner.Config, mcp *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
		return runner.Run(ctx, ws, sessionID, userMessage, cfg, mcp)
	}
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
// the session, ensures the session exists, inserts the user message, runs
// the SDK session in-process, and streams SSE events back to the caller
// until the SDK stream ends or the client disconnects.
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

	// Set up the per-turn workspace (download claude-home tarball from S3).
	ws, err := workspaceSetup(r.Context(), req.WorkspaceID, req.SessionID, s.s3)
	if err != nil {
		s.logger.Error("workspace setup failed", "session_id", req.SessionID, "error", err)
		writeError(w, http.StatusInternalServerError, "workspace setup failed")
		return
	}

	// Build the in-process MCP server with this turn's identity + dependencies.
	tctx := &tools.Context{
		SessionID:           req.SessionID,
		WorkspaceID:         req.WorkspaceID,
		IMChannelID:         req.IMChannelID,
		IMUserID:            req.IMUserID,
		ExecutorRegistryURL: s.config.ExecutorRegistryURL,
		AgentserverURL:      s.config.AgentserverURL,
		IMBridgeURL:         s.config.IMBridgeURL,
		InternalAPISecret:   s.config.IMBridgeSecret,
		Workspace:           ws,
		HTTP:                http.DefaultClient,
	}
	mcp := tools.BuildMcpServer(tctx)

	// Build runner config from process env.
	runCfg := runner.Config{
		SystemPrompt:             "", // CC default; override later if we add a workspace prompt
		MaxTurns:                 0,  // unlimited; rely on auto-compact
		AnthropicAPIKey:          os.Getenv("ANTHROPIC_API_KEY"),
		AnthropicAuthToken:       os.Getenv("ANTHROPIC_AUTH_TOKEN"),
		AnthropicBaseURL:         os.Getenv("ANTHROPIC_BASE_URL"),
		DisableFileCheckpointing: true,
		AutoCompactWindow:        165000,
	}

	// Start the SDK session. Returns a channel of SDKMessages that closes
	// when the CC subprocess exits, ctx is cancelled, or the SDK errors.
	msgCh, err := runnerRun(r.Context(), ws, req.SessionID, req.UserMessage, runCfg, mcp)
	if err != nil {
		s.logger.Error("runner.Run failed", "session_id", req.SessionID, "error", err)
		go workspaceTeardown(context.Background(), ws, s.s3) //nolint:errcheck
		writeError(w, http.StatusInternalServerError, "failed to start SDK session")
		return
	}

	// Check flusher BEFORE setting SSE headers.
	flusher, ok := w.(http.Flusher)
	if !ok {
		go workspaceTeardown(context.Background(), ws, s.s3) //nolint:errcheck
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// Set SSE response headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Subscribe to session SSE events (the pump goroutine below broadcasts here).
	sub := s.sse.Subscribe(req.SessionID)
	defer s.sse.Unsubscribe(req.SessionID, sub)

	// Pump SDKMessages → DB + SSE broadcast. Closes `done` when the SDK channel
	// drains so the for-select loop below can flush the SSE subscription and
	// emit the "done" sentinel.
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer workspaceTeardown(context.Background(), ws, s.s3) //nolint:errcheck
		for sdkMsg := range msgCh {
			evt, convErr := runner.ToEventPayload(sdkMsg)
			if convErr != nil {
				s.logger.Warn("ToEventPayload failed", "session_id", req.SessionID, "error", convErr)
				continue
			}
			eventID := uuid.NewString()
			var seqNum int64
			if !evt.Ephemeral {
				inserted, insertErr := s.store.InsertEvents(context.Background(), req.SessionID, epoch, []EventInput{
					{EventID: eventID, Payload: evt.Payload, Ephemeral: false},
				})
				if insertErr != nil {
					s.logger.Warn("InsertEvents failed", "session_id", req.SessionID, "error", insertErr)
				} else if len(inserted) > 0 {
					seqNum = inserted[0].SeqNum
				}
			}
			s.sse.Publish(req.SessionID, &StreamClientEvent{
				EventID:     eventID,
				SequenceNum: seqNum,
				EventType:   "client_event",
				Source:      "worker",
				Payload:     evt.Payload,
				CreatedAt:   time.Now().Format(time.RFC3339Nano),
			})
		}
	}()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			// Client disconnected. runner.Run's internal goroutine already
			// watches ctx and closes the SDK; teardown happens via the pump
			// goroutine's defer.
			return

		case evt := <-sub.Ch:
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()

		case <-sub.Done():
			// Subscriber was closed (e.g. channel overflow).
			return

		case <-done:
			// SDK stream ended. Drain remaining buffered events.
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
			return

		case <-keepalive.C:
			fmt.Fprintf(w, ":keepalive\n\n")
			flusher.Flush()
		}
	}
}
