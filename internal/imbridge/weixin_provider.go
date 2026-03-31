package imbridge

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/agentserver/agentserver/internal/weixin"
)

const (
	typingKeepaliveInterval = 5 * time.Second
	typingTimeout           = 5 * time.Minute
)

// WeixinProvider implements Provider for WeChat via iLink API.
type WeixinProvider struct{}

func (p *WeixinProvider) Name() string      { return "weixin" }
func (p *WeixinProvider) JIDSuffix() string { return "@im.wechat" }

func (p *WeixinProvider) Poll(ctx context.Context, creds *Credentials, cursor string) (*PollResult, error) {
	resp, err := weixin.GetUpdates(ctx, creds.BaseURL, creds.BotToken, cursor)
	if err != nil {
		return nil, err
	}

	// Handle API-level errors
	if resp.Ret != 0 || resp.ErrCode != 0 {
		if resp.ErrCode == weixin.SessionExpiredErrCode || resp.Ret == weixin.SessionExpiredErrCode {
			log.Printf("imbridge: weixin session expired for bot=%s, backing off 5m", creds.BotID)
			return &PollResult{ShouldBackoff: 5 * time.Minute}, nil
		}
		return nil, fmt.Errorf("ilink API error: ret=%d errcode=%d errmsg=%s", resp.Ret, resp.ErrCode, resp.ErrMsg)
	}

	var msgs []InboundMessage
	for _, m := range resp.Msgs {
		if m.FromUserID == "" {
			continue
		}

		// Log full message for debugging media issues.
		if hasMedia(m.ItemList) {
			msgJSON, _ := json.Marshal(m)
			log.Printf("imbridge: weixin inbound media message: %s", string(msgJSON))
		}

		// Extract text, falling back to media descriptions for non-text messages.
		text := weixin.ExtractText(m)
		if text == "" {
			text = describeWeixinMedia(m.ItemList)
		}
		if text == "" {
			continue
		}

		meta := map[string]string{}
		if m.ContextToken != "" {
			meta["context_token"] = m.ContextToken
		}

		msg := InboundMessage{
			FromUserID: m.FromUserID,
			SenderName: m.FromUserID,
			Text:       text,
			Metadata:   meta,
		}

		// Download media from CDN if present (best-effort, don't block on failure).
		if mediaData, mediaType, filename := downloadWeixinMedia(creds, m.ItemList); mediaData != nil {
			msg.MediaData = mediaData
			msg.MediaType = mediaType
			msg.MediaFilename = filename
		}

		msgs = append(msgs, msg)
	}

	return &PollResult{Messages: msgs, NewCursor: resp.GetUpdatesBuf}, nil
}

func hasMedia(items []weixin.MessageItem) bool {
	for _, item := range items {
		if item.Type >= 2 && item.Type <= 5 {
			return true
		}
	}
	return false
}

// downloadWeixinMedia attempts to download the first media item from iLink CDN.
// Returns (data, mediaType, filename) or (nil, "", "") on failure or no media.
// Matches openclaw-weixin's downloadMediaFromItem AES key resolution:
// prefers image_item.aeskey (hex) over image_item.media.aes_key (base64).
func downloadWeixinMedia(creds *Credentials, items []weixin.MessageItem) ([]byte, string, string) {
	for _, item := range items {
		switch {
		case item.Type == 2 && item.ImageItem != nil && item.ImageItem.Media != nil:
			media := item.ImageItem.Media
			if media.EncryptQueryParam == "" {
				continue
			}

			// Resolve AES key: prefer image_item.aeskey (hex) → convert to base64.
			// Fallback to media.aes_key (already base64).
			var aesKeyB64 string
			if item.ImageItem.AESKey != "" {
				// hex → raw bytes → base64 (matching openclaw-weixin)
				rawKey, err := hex.DecodeString(item.ImageItem.AESKey)
				if err == nil {
					aesKeyB64 = base64.StdEncoding.EncodeToString(rawKey)
				}
			}
			if aesKeyB64 == "" {
				aesKeyB64 = media.AESKey
			}

			itemJSON, _ := json.Marshal(item)
			log.Printf("imbridge: weixin image item: %s", string(itemJSON))

			data, err := downloadFromCDN(media.EncryptQueryParam, aesKeyB64, media.FullURL)
			if err != nil {
				log.Printf("imbridge: weixin image download failed: %v", err)
				continue
			}
			return data, "image", ""

		case item.Type == 4 && item.FileItem != nil && item.FileItem.Media != nil:
			media := item.FileItem.Media
			if media.EncryptQueryParam == "" {
				continue
			}

			log.Printf("imbridge: weixin file item: name=%s size=%s", item.FileItem.FileName, item.FileItem.Len)

			data, err := downloadFromCDN(media.EncryptQueryParam, media.AESKey, media.FullURL)
			if err != nil {
				log.Printf("imbridge: weixin file download failed: %v", err)
				continue
			}
			return data, "file", item.FileItem.FileName
		}
	}
	return nil, "", ""
}

// downloadFromCDN downloads and optionally decrypts media from the WeChat CDN.
func downloadFromCDN(encryptQueryParam, aesKeyB64, fullURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if aesKeyB64 != "" {
		return weixin.DownloadAndDecryptMedia(ctx, "", encryptQueryParam, aesKeyB64, fullURL)
	}
	return weixin.DownloadFromCDN(ctx, "", encryptQueryParam, fullURL)
}

// describeWeixinMedia returns a text description for non-text message items.
// Uses speech-to-text for voice messages and file names for file attachments,
// matching openclaw-weixin's inbound processing.
func describeWeixinMedia(items []weixin.MessageItem) string {
	for _, item := range items {
		switch item.Type {
		case 2: // IMAGE
			return "[User sent an image]"
		case 3: // VOICE
			if item.VoiceItem != nil && item.VoiceItem.Text != "" {
				return "[Voice message] " + item.VoiceItem.Text
			}
			return "[User sent a voice message]"
		case 4: // FILE
			if item.FileItem != nil && item.FileItem.FileName != "" {
				return "[User sent a file: " + item.FileItem.FileName + "]"
			}
			return "[User sent a file]"
		case 5: // VIDEO
			return "[User sent a video]"
		}
	}
	return ""
}

// SendImage implements ImageSendProvider for WeChat.
// Uploads the image to CDN and sends it, then sends the caption as a separate text message.
func (p *WeixinProvider) SendImage(ctx context.Context, creds *Credentials, toUserID string, imageData []byte, caption string) error {
	contextToken := ""
	if err := weixin.UploadAndSendImage(ctx, creds.BaseURL, "", creds.BotToken, toUserID, imageData, contextToken); err != nil {
		return err
	}
	if caption != "" {
		return weixin.SendTextMessage(ctx, creds.BaseURL, creds.BotToken, toUserID, caption, contextToken)
	}
	return nil
}

// --- QR Login session management (delegates to weixin package) ---

// StartQRLogin initiates a WeChat QR code login flow.
func (p *WeixinProvider) StartQRLogin(ctx context.Context) (*weixin.Session, error) {
	return weixin.StartLogin(ctx, weixin.DefaultAPIBaseURL)
}

// PollQRLogin polls for the QR code scan status.
func (p *WeixinProvider) PollQRLogin(ctx context.Context, qrCode string) (*weixin.StatusResult, error) {
	return weixin.PollLoginStatus(ctx, weixin.DefaultAPIBaseURL, qrCode)
}

// SetSession stores a QR login session for a sandbox.
func (p *WeixinProvider) SetSession(sandboxID string, session *weixin.Session) {
	weixin.SetSession(sandboxID, session)
}

// GetSession retrieves the current QR login session for a sandbox.
func (p *WeixinProvider) GetSession(sandboxID string) *weixin.Session {
	return weixin.GetSession(sandboxID)
}

// TakeSession atomically retrieves and removes the QR login session.
func (p *WeixinProvider) TakeSession(sandboxID string) *weixin.Session {
	return weixin.TakeSession(sandboxID)
}

// ClearSession removes the QR login session for a sandbox.
func (p *WeixinProvider) ClearSession(sandboxID string) {
	weixin.ClearSession(sandboxID)
}

// DefaultBaseURL returns the default iLink API base URL.
func (p *WeixinProvider) DefaultBaseURL() string {
	return weixin.DefaultAPIBaseURL
}

func (p *WeixinProvider) Send(ctx context.Context, creds *Credentials, toUserID, text string, meta map[string]string) error {
	contextToken := ""
	if meta != nil {
		contextToken = meta["context_token"]
	}
	return weixin.SendTextMessage(ctx, creds.BaseURL, creds.BotToken, toUserID, text, contextToken)
}

// StartTyping implements TypingProvider for WeChat.
// It fetches a typing_ticket via GetConfig, then sends typing keepalives every 5s.
// On timeout (5min), it sends an error notice to the user and stops.
func (p *WeixinProvider) StartTyping(ctx context.Context, creds *Credentials, userID string, meta map[string]string,
	sendError func(text string)) {

	go func() {

		contextToken := ""
		if meta != nil {
			contextToken = meta["context_token"]
		}

		// Fetch typing ticket from iLink config API.
		configResp, err := weixin.GetConfig(ctx, creds.BaseURL, creds.BotToken, userID, contextToken)
		if err != nil || configResp.TypingTicket == "" {
			if err != nil {
				log.Printf("imbridge: weixin getConfig failed for %s: %v (typing disabled)", userID, err)
			}
			// No typing ticket — just wait for timeout to send error notice.
			<-ctx.Done()
			if ctx.Err() == context.DeadlineExceeded {
				sendError("⚠️ 消息处理超时，请稍后重试。")
			}
			return
		}

		typingTicket := configResp.TypingTicket

		// Send initial typing indicator.
		if err := weixin.SendTyping(ctx, creds.BaseURL, creds.BotToken, userID, typingTicket, weixin.TypingStatusTyping); err != nil {
			log.Printf("imbridge: weixin sendTyping failed for %s: %v", userID, err)
		}

		// Keepalive loop: send typing every 5s until cancelled or timed out.
		ticker := time.NewTicker(typingKeepaliveInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				// Send cancel typing (best-effort, use background context).
				bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = weixin.SendTyping(bgCtx, creds.BaseURL, creds.BotToken, userID, typingTicket, weixin.TypingStatusCancel)
				bgCancel()

				if ctx.Err() == context.DeadlineExceeded {
					sendError("⚠️ 消息处理超时，请稍后重试。")
				}
				return
			case <-ticker.C:
				if err := weixin.SendTyping(ctx, creds.BaseURL, creds.BotToken, userID, typingTicket, weixin.TypingStatusTyping); err != nil {
					log.Printf("imbridge: weixin typing keepalive failed for %s: %v", userID, err)
				}
			}
		}
	}()
}
