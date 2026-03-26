package imbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const TelegramDefaultBaseURL = "https://api.telegram.org"

// RateLimitError is returned when Telegram returns HTTP 429.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("telegram: rate limited, retry after %s", e.RetryAfter)
}

// TelegramBotInfo is the response from Telegram's getMe API.
type TelegramBotInfo struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// TelegramUpdate represents a single update from Telegram's getUpdates API.
type TelegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *TelegramMessage `json:"message"`
}

// TelegramMessage represents a Telegram message.
type TelegramMessage struct {
	MessageID int64         `json:"message_id"`
	From      *TelegramUser `json:"from"`
	Chat      TelegramChat  `json:"chat"`
	Text      string        `json:"text"`
}

// TelegramUser represents a Telegram user.
type TelegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

// TelegramChat represents a Telegram chat.
type TelegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // "private", "group", "supergroup", "channel"
}

type telegramResponse[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	Description string `json:"description"`
	ErrorCode   int    `json:"error_code"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func telegramRequest[T any](ctx context.Context, baseURL, botToken, method string, body interface{}) (T, error) {
	var zero T
	url := fmt.Sprintf("%s/bot%s/%s", baseURL, botToken, method)

	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return zero, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, reqBody)
	if err != nil {
		return zero, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return zero, fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return zero, fmt.Errorf("telegram %s: read body: %w", method, err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		var parsed telegramResponse[json.RawMessage]
		if json.Unmarshal(respBody, &parsed) == nil && parsed.Parameters != nil && parsed.Parameters.RetryAfter > 0 {
			return zero, &RateLimitError{RetryAfter: time.Duration(parsed.Parameters.RetryAfter) * time.Second}
		}
		return zero, &RateLimitError{RetryAfter: 30 * time.Second}
	}

	var parsed telegramResponse[T]
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return zero, fmt.Errorf("telegram %s: parse response: %w", method, err)
	}
	if !parsed.OK {
		return zero, fmt.Errorf("telegram %s: API error %d: %s", method, parsed.ErrorCode, parsed.Description)
	}
	return parsed.Result, nil
}

// TelegramGetMe validates a bot token and returns bot info.
func TelegramGetMe(ctx context.Context, baseURL, botToken string) (*TelegramBotInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := telegramRequest[TelegramBotInfo](ctx, baseURL, botToken, "getMe", nil)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// TelegramGetUpdates long-polls for new updates.
func TelegramGetUpdates(ctx context.Context, baseURL, botToken string, offset int64, timeout int) ([]TelegramUpdate, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout+5)*time.Second)
	defer cancel()

	body := map[string]interface{}{
		"offset":          offset,
		"timeout":         timeout,
		"allowed_updates": []string{"message"},
	}
	return telegramRequest[[]TelegramUpdate](ctx, baseURL, botToken, "getUpdates", body)
}

// TelegramSendMessage sends a text message to a chat.
func TelegramSendMessage(ctx context.Context, baseURL, botToken string, chatID int64, text string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	body := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	}
	_, err := telegramRequest[json.RawMessage](ctx, baseURL, botToken, "sendMessage", body)
	return err
}

// TelegramSendChatAction sends a typing indicator to a chat.
func TelegramSendChatAction(ctx context.Context, baseURL, botToken string, chatID int64, action string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	body := map[string]interface{}{
		"chat_id": chatID,
		"action":  action,
	}
	_, err := telegramRequest[json.RawMessage](ctx, baseURL, botToken, "sendChatAction", body)
	return err
}
