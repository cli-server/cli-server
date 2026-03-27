package imbridge

import (
	"context"
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
			return &PollResult{ShouldBackoff: 5 * time.Minute}, nil
		}
		return nil, fmt.Errorf("ilink API error: ret=%d errcode=%d errmsg=%s", resp.Ret, resp.ErrCode, resp.ErrMsg)
	}

	var msgs []InboundMessage
	for _, m := range resp.Msgs {
		if m.FromUserID == "" {
			continue
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

		// Download image from CDN if present (best-effort, don't block on failure).
		if imageData, mediaType := downloadWeixinMedia(creds, m.ItemList); imageData != nil {
			msg.MediaData = imageData
			msg.MediaType = mediaType
		}

		msgs = append(msgs, msg)
	}

	return &PollResult{Messages: msgs, NewCursor: resp.GetUpdatesBuf}, nil
}

// downloadWeixinMedia attempts to download the first media item from iLink CDN.
// Returns (data, mediaType) or (nil, "") on failure or no media.
func downloadWeixinMedia(creds *Credentials, items []weixin.MessageItem) ([]byte, string) {
	for _, item := range items {
		if item.Type == 2 && item.ImageItem != nil && item.ImageItem.Media != nil {
			// IMAGE: download and decrypt from CDN
			media := item.ImageItem.Media
			if media.EncryptQueryParam == "" {
				continue
			}
			aesKey := media.AESKey
			if aesKey == "" {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			data, err := weixin.DownloadAndDecryptMedia(ctx, "", media.EncryptQueryParam, aesKey)
			cancel()
			if err != nil {
				log.Printf("imbridge: weixin image download failed: %v", err)
				continue
			}
			return data, "image"
		}
	}
	return nil, ""
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
