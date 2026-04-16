package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/agentserver/agentserver/internal/db"
)

// IMInboundMessage represents an inbound message from an IM channel.
type IMInboundMessage struct {
	ChatJID    string `json:"chat_jid"`
	SenderName string `json:"sender_name"`
	Content    string `json:"content"`
	Provider   string `json:"provider"`
	ChannelID  string `json:"channel_id"`
}

func (s *Server) handleIMInbound(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "wid")

	var msg IMInboundMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if msg.Content == "" {
		http.Error(w, "content is required", http.StatusBadRequest)
		return
	}

	// 1. Resolve session by chat_jid
	session, err := s.DB.GetSessionByExternalID(r.Context(), workspaceID, msg.ChatJID)
	if err != nil || session == nil {
		// Create new session
		sessionID := "cse_" + uuid.NewString()
		title := fmt.Sprintf("IM: %s", msg.SenderName)
		if createErr := s.DB.CreateAgentSession(sessionID, nil, workspaceID, title, nil); createErr != nil {
			http.Error(w, "failed to create session", http.StatusInternalServerError)
			return
		}
		if setErr := s.DB.SetSessionExternalID(r.Context(), sessionID, msg.ChatJID); setErr != nil {
			http.Error(w, "failed to set external ID", http.StatusInternalServerError)
			return
		}
		session, _ = s.DB.GetAgentSession(sessionID)
	}

	if session == nil {
		http.Error(w, "failed to resolve session", http.StatusInternalServerError)
		return
	}

	// 2. Async: call cc-broker
	bgCtx := context.Background()
	go s.processWithCCBroker(bgCtx, session, msg)

	// 3. Return 202 immediately
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) processWithCCBroker(_ context.Context, session *db.AgentSession, msg IMInboundMessage) {
	if s.CCBrokerURL == "" {
		return
	}

	body, _ := json.Marshal(map[string]interface{}{
		"session_id":   session.ID,
		"workspace_id": session.WorkspaceID,
		"user_message": msg.Content,
	})

	resp, err := http.Post(s.CCBrokerURL+"/api/turns", "application/json", bytes.NewReader(body))
	if err != nil {
		// Log error but don't crash — user already got 202
		return
	}
	defer resp.Body.Close()

	// Read SSE stream from cc-broker — events are persisted by cc-broker itself.
	// We just need to wait for completion and extract the final text response.
	var finalResponse string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var event map[string]interface{}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		// Check for done event
		if eventType, _ := event["event_type"].(string); eventType == "done" {
			break
		}

		// Extract text from assistant messages for IM reply.
		// CC events have nested payload; look for text content.
		if payload, ok := event["payload"]; ok {
			payloadMap, _ := payload.(map[string]interface{})
			if payloadMap != nil {
				// Look for assistant message with text content
				if msgType, _ := payloadMap["type"].(string); msgType == "assistant" {
					if content, ok := payloadMap["content"].(string); ok {
						finalResponse = content
					}
				}
			}
		}
	}

	// Reply to IM via imbridge.
	// For now, just capture — IM reply routing will be connected later.
	_ = finalResponse
}
