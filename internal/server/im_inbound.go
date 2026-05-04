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

	// Pass chat_jid through unchanged. Each provider's Send method strips
	// its own JID suffix as needed (Matrix trims "@matrix"; WeChat/iLink
	// REQUIRES the "@im.wechat" suffix on to_user_id and rejects bare
	// openids with ret=-3). Resolve the channel ID once, up-front, so the
	// turn request and the final reply both carry the same IM context.
	toUserID := msg.ChatJID
	// Always reply through the channel the message *came in on*, not whatever
	// the session record happens to have stored. When a workspace re-binds an
	// IM bot (e.g. the original bot's WeChat session expired and a fresh bot
	// was registered for the same chat_jid), session resolution by external_id
	// returns the existing session with its stale IMChannelID — replying there
	// hits a deleted/stale channel and the user never sees the message.
	// Persist the freshest channel so other consumers (downstream tasks,
	// audits) also see the up-to-date binding.
	channelID := msg.ChannelID
	if session != nil && (session.IMChannelID == nil || *session.IMChannelID != channelID) {
		if err := s.DB.SetSessionIMChannel(ctx, session.ID, channelID); err != nil {
			log.Printf("im_inbound: failed to refresh im_channel_id for session %s: %v", session.ID, err)
		}
	}

	body, _ := json.Marshal(map[string]interface{}{
		"session_id":    session.ID,
		"workspace_id":  session.WorkspaceID,
		"user_message":  msg.Content,
		"im_channel_id": channelID,
		"im_user_id":    toUserID,
	})

	tid, err := ccbrokerV2Submit(ctx, s.CCBrokerURL, body)
	if err != nil {
		log.Printf("im_inbound: cc-broker v2 submit failed sid=%s: %v", session.ID, err)
		return
	}
	stream, err := ccbrokerOpenEventStream(ctx, s.CCBrokerURL, tid)
	if err != nil {
		log.Printf("im_inbound: open events stream failed sid=%s tid=%s: %v", session.ID, tid, err)
		return
	}
	defer stream.Close()

	// Read SSE stream from cc-broker — events are persisted by cc-broker
	// itself; we just need the final assistant text to relay back to IM.
	finalResponse := extractFinalText(stream)

	// Reply to IM via imbridge.
	if finalResponse == "" {
		return
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
