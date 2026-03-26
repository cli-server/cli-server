package imbridge

import (
	"context"
	"fmt"
	"strconv"
)

// TelegramProvider implements Provider for Telegram Bot API.
type TelegramProvider struct{}

func (p *TelegramProvider) Name() string      { return "telegram" }
func (p *TelegramProvider) JIDSuffix() string { return "@tg" }

func (p *TelegramProvider) Poll(ctx context.Context, creds *Credentials, cursor string) (*PollResult, error) {
	var offset int64
	if cursor != "" {
		offset, _ = strconv.ParseInt(cursor, 10, 64)
	}

	baseURL := creds.BaseURL
	if baseURL == "" {
		baseURL = TelegramDefaultBaseURL
	}

	updates, err := TelegramGetUpdates(ctx, baseURL, creds.BotToken, offset, 35)
	if err != nil {
		// Convert rate limit errors to backoff
		if rle, ok := err.(*RateLimitError); ok {
			return &PollResult{ShouldBackoff: rle.RetryAfter}, nil
		}
		return nil, err
	}

	var msgs []InboundMessage
	var maxID int64
	for _, u := range updates {
		if u.Message == nil || u.Message.Text == "" {
			continue
		}
		if u.UpdateID > maxID {
			maxID = u.UpdateID
		}

		senderName := ""
		if u.Message.From != nil {
			senderName = u.Message.From.FirstName
			if u.Message.From.Username != "" {
				senderName = u.Message.From.Username
			}
		}

		isGroup := u.Message.Chat.Type == "group" || u.Message.Chat.Type == "supergroup"

		msgs = append(msgs, InboundMessage{
			FromUserID: fmt.Sprintf("%d", u.Message.Chat.ID),
			SenderName: senderName,
			Text:       u.Message.Text,
			IsGroup:    isGroup,
		})
	}

	newCursor := cursor
	if maxID > 0 {
		newCursor = strconv.FormatInt(maxID+1, 10)
	}

	return &PollResult{Messages: msgs, NewCursor: newCursor}, nil
}

func (p *TelegramProvider) Send(ctx context.Context, creds *Credentials, toUserID, text string, meta map[string]string) error {
	chatID, err := strconv.ParseInt(toUserID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid telegram chat ID %q: %w", toUserID, err)
	}
	baseURL := creds.BaseURL
	if baseURL == "" {
		baseURL = TelegramDefaultBaseURL
	}
	return TelegramSendMessage(ctx, baseURL, creds.BotToken, chatID, text)
}
