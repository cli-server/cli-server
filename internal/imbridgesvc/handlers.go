package imbridgesvc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/agentserver/agentserver/internal/imbridge"
	"github.com/agentserver/agentserver/internal/weixin"
)

type imBindingResponse struct {
	Provider string `json:"provider"`
	BotID    string `json:"bot_id"`
	UserID   string `json:"user_id,omitempty"`
	BoundAt  string `json:"bound_at"`
}

// ---------------------------------------------------------------------------
// NanoClaw outbound messages (POST /api/internal/nanoclaw/{id}/im/send)
// ---------------------------------------------------------------------------

func (s *Server) handleNanoclawIMSend(w http.ResponseWriter, r *http.Request) {
	sandboxID := chi.URLParam(r, "id")

	sbx, ok := s.sandboxes.Get(sandboxID)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if sbx.Type != "nanoclaw" {
		http.Error(w, "not a nanoclaw sandbox", http.StatusBadRequest)
		return
	}

	authHeader := r.Header.Get("Authorization")
	expectedAuth := "Bearer " + sbx.NanoclawBridgeSecret
	if sbx.NanoclawBridgeSecret == "" || authHeader != expectedAuth {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Parse request — supports JSON (text) and multipart/form-data (media).
	var reqMeta struct {
		BotID      string `json:"bot_id"`
		ToUserID   string `json:"to_user_id"`
		Text       string `json:"text"`
		ProviderID string `json:"provider"`
	}
	var mediaData []byte

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "invalid multipart body", http.StatusBadRequest)
			return
		}
		metaPart := r.FormValue("meta")
		if metaPart != "" {
			if err := json.Unmarshal([]byte(metaPart), &reqMeta); err != nil {
				http.Error(w, "invalid meta JSON", http.StatusBadRequest)
				return
			}
		}
		if file, _, err := r.FormFile("media"); err == nil {
			defer file.Close()
			mediaData, err = io.ReadAll(file)
			if err != nil {
				http.Error(w, "failed to read media file", http.StatusBadRequest)
				return
			}
		}
	} else {
		if err := json.NewDecoder(r.Body).Decode(&reqMeta); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
	}

	if reqMeta.ToUserID == "" {
		http.Error(w, "to_user_id is required", http.StatusBadRequest)
		return
	}
	if reqMeta.Text == "" && len(mediaData) == 0 {
		http.Error(w, "text or media is required", http.StatusBadRequest)
		return
	}

	channel, err := s.db.GetIMChannelForSandbox(sandboxID)
	if err != nil {
		http.Error(w, "no IM channel bound to this sandbox", http.StatusNotFound)
		return
	}

	var provider imbridge.Provider
	if reqMeta.ProviderID != "" {
		provider = s.bridge.GetProvider(reqMeta.ProviderID)
	}
	if provider == nil {
		provider = s.bridge.FindProviderByJID(reqMeta.ToUserID)
	}
	if provider == nil {
		provider = s.bridge.GetProvider(channel.Provider)
	}
	if provider == nil {
		http.Error(w, "unknown IM provider", http.StatusBadRequest)
		return
	}
	userID := reqMeta.ToUserID

	meta, _ := s.db.GetAllChannelMeta(channel.ID, userID)
	s.bridge.StopTyping(channel.ID, userID)

	creds := &imbridge.Credentials{ChannelID: channel.ID, BotID: channel.BotID, BotToken: channel.BotToken, BaseURL: channel.BaseURL}

	if len(mediaData) > 0 {
		isp, ok := provider.(imbridge.ImageSendProvider)
		if !ok {
			http.Error(w, "image sending not supported for provider: "+provider.Name(), http.StatusBadRequest)
			return
		}
		if err := isp.SendImage(r.Context(), creds, userID, mediaData, reqMeta.Text); err != nil {
			log.Printf("nanoclaw im send image: failed sandbox=%s to=%s: %v", sandboxID, userID, err)
			http.Error(w, "failed to send image: "+err.Error(), http.StatusBadGateway)
			return
		}
	} else {
		if err := provider.Send(r.Context(), creds, userID, reqMeta.Text, meta); err != nil {
			log.Printf("nanoclaw im send: failed sandbox=%s provider=%s to=%s: %v", sandboxID, provider.Name(), userID, err)
			http.Error(w, "failed to send message", http.StatusBadGateway)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "sent"})
}

// ---------------------------------------------------------------------------
// Stateless CC outbound messages (POST /api/internal/imbridge/send)
// ---------------------------------------------------------------------------

// handleImbridgeDirectSend sends a text message to an IM user without a
// sandbox binding. Used by agentserver's stateless CC flow to route CC
// responses back to the originating IM user. Authenticated via the
// INTERNAL_API_SECRET shared secret.
func (s *Server) handleImbridgeDirectSend(w http.ResponseWriter, r *http.Request) {
	if secret := os.Getenv("INTERNAL_API_SECRET"); secret != "" {
		if r.Header.Get("X-Internal-Secret") != secret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var req struct {
		ChannelID string `json:"channel_id"`
		ToUserID  string `json:"to_user_id"`
		Text      string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.ChannelID == "" || req.ToUserID == "" || req.Text == "" {
		http.Error(w, "channel_id, to_user_id, and text are required", http.StatusBadRequest)
		return
	}

	channel, err := s.db.GetIMChannel(req.ChannelID)
	if err != nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}

	provider := s.bridge.GetProvider(channel.Provider)
	if provider == nil {
		http.Error(w, "unknown IM provider: "+channel.Provider, http.StatusBadRequest)
		return
	}

	meta, _ := s.db.GetAllChannelMeta(channel.ID, req.ToUserID)
	s.bridge.StopTyping(channel.ID, req.ToUserID)

	creds := &imbridge.Credentials{
		ChannelID: channel.ID,
		BotID:     channel.BotID,
		BotToken:  channel.BotToken,
		BaseURL:   channel.BaseURL,
	}

	if err := provider.Send(r.Context(), creds, req.ToUserID, req.Text, meta); err != nil {
		log.Printf("imbridge direct send: failed channel=%s provider=%s to=%s: %v",
			channel.ID, provider.Name(), req.ToUserID, err)
		http.Error(w, "failed to send message", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "sent"})
}

// maxDirectSendImageBytes caps the decoded image payload size for the
// direct send-image endpoint. 20 MiB covers any reasonable screenshot or
// AI-generated image while bounding memory use per request.
const maxDirectSendImageBytes = 20 << 20

// maxDirectSendImageRequestBytes bounds the raw request body before JSON
// decode. A 20 MiB image base64-encodes to ~26.67 MiB; the extra headroom
// covers JSON overhead and the other fields.
const maxDirectSendImageRequestBytes = 32 << 20

// handleImbridgeDirectSendImage sends an image to an IM user without a
// sandbox binding. Parallel to handleImbridgeDirectSend but carries
// base64-encoded image bytes. Auth via INTERNAL_API_SECRET.
func (s *Server) handleImbridgeDirectSendImage(w http.ResponseWriter, r *http.Request) {
	if secret := os.Getenv("INTERNAL_API_SECRET"); secret != "" {
		if r.Header.Get("X-Internal-Secret") != secret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	// Cap the raw body before JSON decode so we never buffer an unbounded
	// request. MaxBytesReader returns an error from subsequent Reads once
	// the limit is exceeded, which Decode will surface.
	r.Body = http.MaxBytesReader(w, r.Body, maxDirectSendImageRequestBytes)

	var req struct {
		ChannelID   string `json:"channel_id"`
		ToUserID    string `json:"to_user_id"`
		ImageBase64 string `json:"image_base64"`
		Format      string `json:"format,omitempty"`
		Caption     string `json:"caption,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid or oversized request body", http.StatusBadRequest)
		return
	}
	if req.ChannelID == "" || req.ToUserID == "" || req.ImageBase64 == "" {
		http.Error(w, "channel_id, to_user_id, and image_base64 are required", http.StatusBadRequest)
		return
	}

	data, err := base64.StdEncoding.DecodeString(req.ImageBase64)
	if err != nil {
		http.Error(w, "invalid image_base64: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(data) > maxDirectSendImageBytes {
		http.Error(w, "image exceeds 20 MiB limit", http.StatusRequestEntityTooLarge)
		return
	}

	channel, err := s.db.GetIMChannel(req.ChannelID)
	if err != nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}

	provider := s.bridge.GetProvider(channel.Provider)
	if provider == nil {
		http.Error(w, "unknown IM provider: "+channel.Provider, http.StatusBadRequest)
		return
	}

	isp, ok := provider.(imbridge.ImageSendProvider)
	if !ok {
		http.Error(w, "image sending not supported for provider: "+provider.Name(),
			http.StatusNotImplemented)
		return
	}

	s.bridge.StopTyping(channel.ID, req.ToUserID)

	creds := &imbridge.Credentials{
		ChannelID: channel.ID,
		BotID:     channel.BotID,
		BotToken:  channel.BotToken,
		BaseURL:   channel.BaseURL,
	}

	if err := isp.SendImage(r.Context(), creds, req.ToUserID, data, req.Caption); err != nil {
		log.Printf("imbridge direct send-image: failed channel=%s provider=%s to=%s: %v",
			channel.ID, provider.Name(), req.ToUserID, err)
		http.Error(w, "failed to send image", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "sent"})
}

// ---------------------------------------------------------------------------
// Legacy sandbox-level WeChat QR login
// ---------------------------------------------------------------------------

func (s *Server) handleIMWeixinQRStart(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}
	if sbx.Type != "openclaw" && sbx.Type != "nanoclaw" {
		http.Error(w, "weixin login is only available for openclaw and nanoclaw sandboxes", http.StatusBadRequest)
		return
	}
	if sbx.Status != "running" {
		http.Error(w, "sandbox is not running", http.StatusConflict)
		return
	}

	wp := s.bridge.GetProvider("weixin").(*imbridge.WeixinProvider)
	session, err := wp.StartQRLogin(r.Context())
	if err != nil {
		log.Printf("weixin qr-start: %v", err)
		http.Error(w, "failed to start weixin login", http.StatusBadGateway)
		return
	}
	wp.SetSession(id, session)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"qrcode_url": session.QRCodeURL,
		"message":    "Scan the QR code with WeChat",
	})
}

func (s *Server) handleIMWeixinQRWait(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}
	if sbx.Type != "openclaw" && sbx.Type != "nanoclaw" {
		http.Error(w, "weixin login is only available for openclaw and nanoclaw sandboxes", http.StatusBadRequest)
		return
	}
	if sbx.Status != "running" {
		http.Error(w, "sandbox is not running", http.StatusConflict)
		return
	}

	wp := s.bridge.GetProvider("weixin").(*imbridge.WeixinProvider)
	session := wp.GetSession(id)
	if session == nil {
		http.Error(w, "no active weixin login session", http.StatusBadRequest)
		return
	}

	result, err := wp.PollQRLogin(r.Context(), session.QRCode)
	if err != nil {
		log.Printf("weixin qr-wait: poll error: %v", err)
		http.Error(w, "poll failed", http.StatusBadGateway)
		return
	}

	switch result.Status {
	case "confirmed":
		if wp.TakeSession(id) == nil {
			http.Error(w, "login already processed", http.StatusConflict)
			return
		}
		if err := s.saveWeixinCredentials(r.Context(), id, result, wp); err != nil {
			log.Printf("weixin qr-wait: save credentials: %v", err)
			http.Error(w, "login succeeded but failed to save credentials", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"connected": true,
			"status":    "confirmed",
			"message":   "WeChat connected successfully",
			"bot_id":    normalizeAccountID(result.BotID),
			"user_id":   result.UserID,
		})

	case "expired":
		newSession, err := wp.StartQRLogin(r.Context())
		if err != nil {
			wp.ClearSession(id)
			http.Error(w, "QR code expired and refresh failed", http.StatusBadGateway)
			return
		}
		wp.SetSession(id, newSession)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"connected":  false,
			"status":     "expired",
			"message":    "QR code expired, new code generated",
			"qrcode_url": newSession.QRCodeURL,
		})

	default:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"connected": false,
			"status":    result.Status,
			"message":   statusMessage(result.Status),
		})
	}
}

func statusMessage(status string) string {
	switch status {
	case "scaned":
		return "QR code scanned, confirm on WeChat"
	default:
		return "Waiting for QR code scan"
	}
}

func (s *Server) saveWeixinCredentials(ctx context.Context, sandboxID string, result *weixin.StatusResult, wp *imbridge.WeixinProvider) error {
	accountID := normalizeAccountID(result.BotID)
	if accountID == "" {
		return fmt.Errorf("empty bot ID from ilink response")
	}

	sbx, ok := s.sandboxes.Get(sandboxID)
	if !ok {
		return fmt.Errorf("sandbox %s not found", sandboxID)
	}

	if sbx.Type == "nanoclaw" {
		// NanoClaw: store credentials in workspace IM channels (bridge mode).
		baseURL := result.BaseURL
		if baseURL == "" {
			baseURL = wp.DefaultBaseURL()
		}
		channelID, err := s.db.CreateIMChannel(sbx.WorkspaceID, "weixin", accountID, result.UserID)
		if err != nil {
			return fmt.Errorf("create IM channel: %w", err)
		}
		if err := s.db.SaveIMChannelCredentials(channelID, result.Token, baseURL); err != nil {
			return fmt.Errorf("save channel credentials: %w", err)
		}
		if err := s.db.BindSandboxToChannel(sandboxID, channelID); err != nil {
			return fmt.Errorf("bind sandbox to channel: %w", err)
		}
		s.bridge.StartPoller(imbridge.BridgeBinding{
			Provider: wp,
			Credentials: imbridge.Credentials{
				ChannelID: channelID,
				BotID:     accountID,
				BotToken:  result.Token,
				BaseURL:   baseURL,
			},
			ChannelID:   channelID,
			Cursor:      "",
			WorkspaceID: sbx.WorkspaceID,
		})
		return nil
	}

	// Openclaw: the standalone imbridge service does not have K8s exec access,
	// so openclaw credential injection is not supported. Store the binding
	// record and credentials in DB for the agentserver to pick up.
	baseURL := result.BaseURL
	if baseURL == "" {
		baseURL = wp.DefaultBaseURL()
	}
	if dbErr := s.db.CreateIMBinding(sandboxID, "weixin", accountID, result.UserID); dbErr != nil {
		log.Printf("weixin: failed to save binding record: %v", dbErr)
	}
	if dbErr := s.db.SaveIMCredentials(sandboxID, "weixin", accountID, result.Token, baseURL); dbErr != nil {
		log.Printf("weixin: failed to save bot credentials for openclaw: %v", dbErr)
	}
	return nil
}

func normalizeAccountID(raw string) string {
	var out []byte
	for _, c := range []byte(raw) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

// ---------------------------------------------------------------------------
// Legacy sandbox-level Telegram
// ---------------------------------------------------------------------------

func (s *Server) handleIMTelegramConfigure(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}
	if sbx.Type != "nanoclaw" {
		http.Error(w, "telegram binding is only available for nanoclaw sandboxes", http.StatusBadRequest)
		return
	}
	if sbx.Status != "running" {
		http.Error(w, "sandbox is not running", http.StatusConflict)
		return
	}

	var req struct {
		BotToken string `json:"bot_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.BotToken == "" {
		http.Error(w, "bot_token is required", http.StatusBadRequest)
		return
	}

	provider := s.bridge.GetProvider("telegram")
	cp, ok := provider.(imbridge.ConfigurableProvider)
	if !ok {
		http.Error(w, "telegram provider does not support configuration", http.StatusInternalServerError)
		return
	}
	botID, err := cp.ValidateCredentials(r.Context(), "", req.BotToken)
	if err != nil {
		log.Printf("telegram configure: validate failed: %v", err)
		http.Error(w, "invalid bot token: "+err.Error(), http.StatusBadRequest)
		return
	}

	type defaulter interface{ DefaultBaseURL() string }
	tgBaseURL := ""
	if d, ok := provider.(defaulter); ok {
		tgBaseURL = d.DefaultBaseURL()
	}

	channelID, err := s.db.CreateIMChannel(sbx.WorkspaceID, "telegram", botID, "")
	if err != nil {
		log.Printf("telegram configure: create channel: %v", err)
		http.Error(w, "failed to save channel", http.StatusInternalServerError)
		return
	}
	if err := s.db.SaveIMChannelCredentials(channelID, req.BotToken, tgBaseURL); err != nil {
		log.Printf("telegram configure: save credentials: %v", err)
		http.Error(w, "failed to save credentials", http.StatusInternalServerError)
		return
	}
	if err := s.db.BindSandboxToChannel(id, channelID); err != nil {
		log.Printf("telegram configure: bind sandbox: %v", err)
		http.Error(w, "failed to bind sandbox", http.StatusInternalServerError)
		return
	}

	s.bridge.StartPoller(imbridge.BridgeBinding{
		Provider: provider,
		Credentials: imbridge.Credentials{
			ChannelID: channelID,
			BotID:     botID,
			BotToken:  req.BotToken,
			BaseURL:   tgBaseURL,
		},
		ChannelID:   channelID,
		Cursor:      "",
		WorkspaceID: sbx.WorkspaceID,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"connected": true,
		"bot_id":    botID,
	})
}

func (s *Server) handleIMTelegramDisconnect(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}

	ch, err := s.db.GetIMChannelForSandbox(id)
	if err != nil || ch.Provider != "telegram" {
		http.Error(w, "no telegram binding found for this sandbox", http.StatusNotFound)
		return
	}
	s.bridge.StopPoller(ch.ID)
	_ = s.db.UnbindSandboxFromChannel(id)
	if err := s.db.DeleteIMChannel(ch.ID); err != nil {
		log.Printf("telegram disconnect: delete channel %s: %v", ch.ID, err)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "disconnected"})
}

// ---------------------------------------------------------------------------
// Legacy sandbox-level Matrix
// ---------------------------------------------------------------------------

func (s *Server) handleIMMatrixConfigure(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}
	if sbx.Type != "nanoclaw" {
		http.Error(w, "matrix binding is only available for nanoclaw sandboxes", http.StatusBadRequest)
		return
	}
	if sbx.Status != "running" {
		http.Error(w, "sandbox is not running", http.StatusConflict)
		return
	}

	var req struct {
		HomeserverURL string `json:"homeserver_url"`
		AccessToken   string `json:"access_token"`
		RecoveryKey   string `json:"recovery_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.HomeserverURL == "" {
		http.Error(w, "homeserver_url is required", http.StatusBadRequest)
		return
	}
	if req.AccessToken == "" {
		http.Error(w, "access_token is required", http.StatusBadRequest)
		return
	}

	provider := s.bridge.GetProvider("matrix")
	cp, ok := provider.(imbridge.ConfigurableProvider)
	if !ok {
		http.Error(w, "matrix provider does not support configuration", http.StatusInternalServerError)
		return
	}
	botID, err := cp.ValidateCredentials(r.Context(), req.HomeserverURL, req.AccessToken)
	if err != nil {
		log.Printf("matrix configure: validate failed: %v", err)
		http.Error(w, "invalid credentials: "+err.Error(), http.StatusBadRequest)
		return
	}

	channelID, err := s.db.CreateIMChannel(sbx.WorkspaceID, "matrix", botID, "")
	if err != nil {
		log.Printf("matrix configure: create channel: %v", err)
		http.Error(w, "failed to save channel", http.StatusInternalServerError)
		return
	}
	if err := s.db.SaveIMChannelCredentials(channelID, req.AccessToken, req.HomeserverURL); err != nil {
		log.Printf("matrix configure: save credentials: %v", err)
		http.Error(w, "failed to save credentials", http.StatusInternalServerError)
		return
	}
	if err := s.db.BindSandboxToChannel(id, channelID); err != nil {
		log.Printf("matrix configure: bind sandbox: %v", err)
		http.Error(w, "failed to bind sandbox", http.StatusInternalServerError)
		return
	}

	type e2eeConfigurer interface {
		ConfigureE2EE(ctx context.Context, creds *imbridge.Credentials, recoveryKey string) error
	}
	if ec, ok := provider.(e2eeConfigurer); ok && req.RecoveryKey != "" {
		creds := imbridge.Credentials{ChannelID: channelID, BotID: botID, BotToken: req.AccessToken, BaseURL: req.HomeserverURL}
		if err := ec.ConfigureE2EE(r.Context(), &creds, req.RecoveryKey); err != nil {
			log.Printf("matrix configure: E2EE init failed: %v", err)
		}
	}

	s.bridge.StartPoller(imbridge.BridgeBinding{
		Provider: provider,
		Credentials: imbridge.Credentials{
			ChannelID: channelID,
			BotID:     botID,
			BotToken:  req.AccessToken,
			BaseURL:   req.HomeserverURL,
		},
		ChannelID:   channelID,
		Cursor:      "",
		WorkspaceID: sbx.WorkspaceID,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"connected": true,
		"bot_id":    botID,
	})
}

func (s *Server) handleIMMatrixDisconnect(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}

	ch, err := s.db.GetIMChannelForSandbox(id)
	if err != nil || ch.Provider != "matrix" {
		http.Error(w, "no matrix binding found for this sandbox", http.StatusNotFound)
		return
	}
	s.bridge.StopPoller(ch.ID)
	provider := s.bridge.GetProvider("matrix")
	if dp, ok := provider.(imbridge.DisconnectProvider); ok {
		dp.Disconnect(id, ch.BotID)
	}
	_ = s.db.UnbindSandboxFromChannel(id)
	if err := s.db.DeleteIMChannel(ch.ID); err != nil {
		log.Printf("matrix disconnect: delete channel %s: %v", ch.ID, err)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "disconnected"})
}

// ---------------------------------------------------------------------------
// Sandbox IM bindings
// ---------------------------------------------------------------------------

func (s *Server) handleListIMBindings(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}

	var resp []imBindingResponse
	ch, err := s.db.GetIMChannelForSandbox(id)
	if err == nil {
		resp = append(resp, imBindingResponse{
			Provider: ch.Provider,
			BotID:    ch.BotID,
			UserID:   ch.UserID,
			BoundAt:  ch.BoundAt.Format(time.RFC3339),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"bindings": resp})
}

func (s *Server) handleBindSandboxToChannel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}

	var req struct {
		ChannelID string `json:"channel_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChannelID == "" {
		http.Error(w, "channel_id is required", http.StatusBadRequest)
		return
	}

	ch, err := s.db.GetIMChannel(req.ChannelID)
	if err != nil || ch.WorkspaceID != sbx.WorkspaceID {
		http.Error(w, "channel not found in this workspace", http.StatusNotFound)
		return
	}

	if err := s.db.BindSandboxToChannel(id, req.ChannelID); err != nil {
		http.Error(w, "failed to bind channel", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "bound"})
}

func (s *Server) handleUnbindSandboxFromChannel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}

	if err := s.db.UnbindSandboxFromChannel(id); err != nil {
		http.Error(w, "failed to unbind channel", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "unbound"})
}

// ---------------------------------------------------------------------------
// Workspace-level IM channel management
// ---------------------------------------------------------------------------

func (s *Server) handleListWorkspaceIMChannels(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if _, ok := s.requireWorkspaceMember(w, r, wsID); !ok {
		return
	}

	channels, err := s.db.ListIMChannels(wsID)
	if err != nil {
		http.Error(w, "failed to list channels", http.StatusInternalServerError)
		return
	}

	type channelResp struct {
		ID             string `json:"id"`
		Provider       string `json:"provider"`
		BotID          string `json:"bot_id"`
		UserID         string `json:"user_id,omitempty"`
		RequireMention bool   `json:"require_mention"`
		BoundAt        string `json:"bound_at"`
	}
	resp := make([]channelResp, 0, len(channels))
	for _, ch := range channels {
		resp = append(resp, channelResp{
			ID:             ch.ID,
			Provider:       ch.Provider,
			BotID:          ch.BotID,
			UserID:         ch.UserID,
			RequireMention: ch.RequireMention,
			BoundAt:        ch.BoundAt.Format(time.RFC3339),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"channels": resp})
}

func (s *Server) handleDeleteWorkspaceIMChannel(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	channelID := chi.URLParam(r, "channelId")
	if _, ok := s.requireWorkspaceMember(w, r, wsID); !ok {
		return
	}

	ch, err := s.db.GetIMChannel(channelID)
	if err != nil || ch.WorkspaceID != wsID {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}

	s.bridge.StopPoller(channelID)
	provider := s.bridge.GetProvider(ch.Provider)
	if dp, ok := provider.(imbridge.DisconnectProvider); ok {
		dp.Disconnect("", ch.BotID)
	}
	if err := s.db.DeleteIMChannel(channelID); err != nil {
		log.Printf("delete im channel: %v", err)
		http.Error(w, "failed to delete channel", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUpdateWorkspaceIMChannel(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	channelID := chi.URLParam(r, "channelId")
	if _, ok := s.requireWorkspaceMember(w, r, wsID); !ok {
		return
	}

	ch, err := s.db.GetIMChannel(channelID)
	if err != nil || ch.WorkspaceID != wsID {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}

	var req struct {
		RequireMention *bool `json:"require_mention"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.RequireMention != nil {
		if err := s.db.UpdateIMChannelSettings(channelID, *req.RequireMention); err != nil {
			http.Error(w, "failed to update channel", http.StatusInternalServerError)
			return
		}
		s.bridge.SetChannelRequireMention(channelID, *req.RequireMention)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// ---------------------------------------------------------------------------
// Workspace-level WeChat
// ---------------------------------------------------------------------------

func (s *Server) handleWorkspaceWeixinQRStart(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if _, ok := s.requireWorkspaceMember(w, r, wsID); !ok {
		return
	}

	wp := s.bridge.GetProvider("weixin").(*imbridge.WeixinProvider)
	session, err := wp.StartQRLogin(r.Context())
	if err != nil {
		log.Printf("weixin qr-start: %v", err)
		http.Error(w, "failed to start weixin login", http.StatusBadGateway)
		return
	}
	wp.SetSession(wsID, session)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"qrcode_url": session.QRCodeURL,
		"message":    "Scan the QR code with WeChat",
	})
}

func (s *Server) handleWorkspaceWeixinQRWait(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if _, ok := s.requireWorkspaceMember(w, r, wsID); !ok {
		return
	}

	wp := s.bridge.GetProvider("weixin").(*imbridge.WeixinProvider)
	session := wp.GetSession(wsID)
	if session == nil {
		http.Error(w, "no active weixin login session", http.StatusBadRequest)
		return
	}

	result, err := wp.PollQRLogin(r.Context(), session.QRCode)
	if err != nil {
		log.Printf("weixin qr-wait: poll error: %v", err)
		http.Error(w, "poll failed", http.StatusBadGateway)
		return
	}

	switch result.Status {
	case "confirmed":
		if wp.TakeSession(wsID) == nil {
			http.Error(w, "login already processed", http.StatusConflict)
			return
		}

		accountID := normalizeAccountID(result.BotID)
		if accountID == "" {
			http.Error(w, "empty bot ID", http.StatusInternalServerError)
			return
		}
		baseURL := result.BaseURL
		if baseURL == "" {
			baseURL = wp.DefaultBaseURL()
		}

		channelID, err := s.db.CreateIMChannel(wsID, "weixin", accountID, result.UserID)
		if err != nil {
			http.Error(w, "failed to save channel", http.StatusInternalServerError)
			return
		}
		if err := s.db.SaveIMChannelCredentials(channelID, result.Token, baseURL); err != nil {
			http.Error(w, "failed to save credentials", http.StatusInternalServerError)
			return
		}

		provider := s.bridge.GetProvider("weixin")
		s.bridge.StartPoller(imbridge.BridgeBinding{
			Provider:    provider,
			Credentials: imbridge.Credentials{ChannelID: channelID, BotID: accountID, BotToken: result.Token, BaseURL: baseURL},
			ChannelID:   channelID,
			WorkspaceID: wsID,
		})

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"connected": true,
			"status":    "confirmed",
			"bot_id":    accountID,
		})

	case "expired":
		newSession, err := wp.StartQRLogin(r.Context())
		if err != nil {
			wp.ClearSession(wsID)
			http.Error(w, "QR code expired and refresh failed", http.StatusBadGateway)
			return
		}
		wp.SetSession(wsID, newSession)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"connected":  false,
			"status":     "expired",
			"qrcode_url": newSession.QRCodeURL,
		})

	default:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"connected": false,
			"status":    result.Status,
		})
	}
}

// ---------------------------------------------------------------------------
// Workspace-level Telegram
// ---------------------------------------------------------------------------

func (s *Server) handleWorkspaceTelegramConfigure(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if _, ok := s.requireWorkspaceMember(w, r, wsID); !ok {
		return
	}

	var req struct {
		BotToken string `json:"bot_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BotToken == "" {
		http.Error(w, "bot_token is required", http.StatusBadRequest)
		return
	}

	provider := s.bridge.GetProvider("telegram")
	cp, ok := provider.(imbridge.ConfigurableProvider)
	if !ok {
		http.Error(w, "telegram provider does not support configuration", http.StatusInternalServerError)
		return
	}
	botID, err := cp.ValidateCredentials(r.Context(), "", req.BotToken)
	if err != nil {
		http.Error(w, "invalid bot token: "+err.Error(), http.StatusBadRequest)
		return
	}

	type defaulter interface{ DefaultBaseURL() string }
	baseURL := ""
	if d, ok := provider.(defaulter); ok {
		baseURL = d.DefaultBaseURL()
	}

	channelID, err := s.db.CreateIMChannel(wsID, "telegram", botID, "")
	if err != nil {
		http.Error(w, "failed to save channel", http.StatusInternalServerError)
		return
	}
	if err := s.db.SaveIMChannelCredentials(channelID, req.BotToken, baseURL); err != nil {
		http.Error(w, "failed to save credentials", http.StatusInternalServerError)
		return
	}

	s.bridge.StartPoller(imbridge.BridgeBinding{
		Provider:    provider,
		Credentials: imbridge.Credentials{ChannelID: channelID, BotID: botID, BotToken: req.BotToken, BaseURL: baseURL},
		ChannelID:   channelID,
		WorkspaceID: wsID,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"connected": true, "bot_id": botID})
}

// ---------------------------------------------------------------------------
// Workspace-level Matrix
// ---------------------------------------------------------------------------

func (s *Server) handleWorkspaceMatrixConfigure(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	if _, ok := s.requireWorkspaceMember(w, r, wsID); !ok {
		return
	}

	var req struct {
		HomeserverURL string `json:"homeserver_url"`
		AccessToken   string `json:"access_token"`
		RecoveryKey   string `json:"recovery_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.HomeserverURL == "" || req.AccessToken == "" {
		http.Error(w, "homeserver_url and access_token are required", http.StatusBadRequest)
		return
	}

	provider := s.bridge.GetProvider("matrix")
	cp, ok := provider.(imbridge.ConfigurableProvider)
	if !ok {
		http.Error(w, "matrix provider does not support configuration", http.StatusInternalServerError)
		return
	}
	botID, err := cp.ValidateCredentials(r.Context(), req.HomeserverURL, req.AccessToken)
	if err != nil {
		http.Error(w, "invalid credentials: "+err.Error(), http.StatusBadRequest)
		return
	}

	channelID, err := s.db.CreateIMChannel(wsID, "matrix", botID, "")
	if err != nil {
		http.Error(w, "failed to save channel", http.StatusInternalServerError)
		return
	}
	if err := s.db.SaveIMChannelCredentials(channelID, req.AccessToken, req.HomeserverURL); err != nil {
		http.Error(w, "failed to save credentials", http.StatusInternalServerError)
		return
	}

	type e2eeConfigurer interface {
		ConfigureE2EE(ctx context.Context, creds *imbridge.Credentials, recoveryKey string) error
	}
	if ec, ok := provider.(e2eeConfigurer); ok && req.RecoveryKey != "" {
		creds := imbridge.Credentials{ChannelID: channelID, BotID: botID, BotToken: req.AccessToken, BaseURL: req.HomeserverURL}
		if err := ec.ConfigureE2EE(r.Context(), &creds, req.RecoveryKey); err != nil {
			log.Printf("matrix configure: E2EE init failed: %v", err)
		}
	}

	s.bridge.StartPoller(imbridge.BridgeBinding{
		Provider:    provider,
		Credentials: imbridge.Credentials{ChannelID: channelID, BotID: botID, BotToken: req.AccessToken, BaseURL: req.HomeserverURL},
		ChannelID:   channelID,
		WorkspaceID: wsID,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"connected": true, "bot_id": botID})
}
