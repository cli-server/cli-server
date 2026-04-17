package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

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
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if session == nil {
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
		// Store IM channel so CC's response can be routed back to the user.
		if msg.ChannelID != "" {
			if err := s.DB.SetSessionIMChannel(r.Context(), sessionID, msg.ChannelID); err != nil {
				log.Printf("im_inbound: failed to set im_channel_id for session %s: %v", sessionID, err)
			}
		}
		channelID := msg.ChannelID
		session = &db.AgentSession{
			ID:          sessionID,
			WorkspaceID: workspaceID,
			IMChannelID: &channelID,
		}
	}

	// 2. Async: call cc-broker
	bgCtx := context.Background()
	go s.processWithCCBroker(bgCtx, session, msg)

	// 3. Return 202 immediately
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) processWithCCBroker(ctx context.Context, session *db.AgentSession, msg IMInboundMessage) {
	if s.CCBrokerURL == "" {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	body, _ := json.Marshal(map[string]interface{}{
		"session_id":   session.ID,
		"workspace_id": session.WorkspaceID,
		"user_message": msg.Content,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", s.CCBrokerURL+"/api/turns", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Log error but don't crash — user already got 202
		return
	}
	defer resp.Body.Close()

	// Read SSE stream from cc-broker — events are persisted by cc-broker itself.
	// We just need to wait for completion and extract the final text response.
	finalResponse := extractFinalText(resp.Body)

	// Reply to IM via imbridge.
	if finalResponse == "" {
		return
	}

	// Extract to_user_id from chat_jid (e.g., "user123@im.wechat" → "user123").
	toUserID := msg.ChatJID
	if idx := strings.Index(toUserID, "@"); idx > 0 {
		toUserID = toUserID[:idx]
	}

	// Use channel_id from session if available, falling back to the inbound message.
	channelID := msg.ChannelID
	if session != nil && session.IMChannelID != nil && *session.IMChannelID != "" {
		channelID = *session.IMChannelID
	}
	if channelID == "" {
		log.Printf("im_inbound: no IM channel for session %s, dropping reply", session.ID)
		return
	}

	if err := s.sendIMReply(ctx, channelID, toUserID, finalResponse); err != nil {
		log.Printf("im_inbound: reply failed session=%s channel=%s to=%s: %v",
			session.ID, channelID, toUserID, err)
	}
}

// sendIMReply calls imbridge's internal send endpoint to deliver a CC response
// back to the originating IM user.
func (s *Server) sendIMReply(ctx context.Context, channelID, toUserID, text string) error {
	if s.IMBridgeURL == "" {
		return fmt.Errorf("IMBridgeURL not configured")
	}

	body, _ := json.Marshal(map[string]string{
		"channel_id": channelID,
		"to_user_id": toUserID,
		"text":       text,
	})

	req, err := http.NewRequestWithContext(ctx, "POST",
		s.IMBridgeURL+"/api/internal/imbridge/send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if secret := os.Getenv("INTERNAL_API_SECRET"); secret != "" {
		req.Header.Set("X-Internal-Secret", secret)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST imbridge: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("imbridge returned %d: %s", resp.StatusCode, respBody)
	}
	return nil
}
