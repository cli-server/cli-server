package imbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
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
	MessageID int64            `json:"message_id"`
	From      *TelegramUser    `json:"from"`
	Chat      TelegramChat     `json:"chat"`
	Text      string           `json:"text"`
	Caption   string           `json:"caption"`
	Photo     []TelegramPhoto  `json:"photo"`
	Document  *TelegramDocument `json:"document"`
}

// TelegramPhoto represents a photo size in a Telegram message.
type TelegramPhoto struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int    `json:"file_size"`
}

// TelegramDocument represents a document/file in a Telegram message.
type TelegramDocument struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int    `json:"file_size"`
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

// TelegramGetFile retrieves the file path for a file_id, then downloads the file content.
func TelegramGetFile(ctx context.Context, baseURL, botToken, fileID string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if baseURL == "" {
		baseURL = TelegramDefaultBaseURL
	}

	// Step 1: getFile to get the file_path.
	type fileResult struct {
		FilePath string `json:"file_path"`
		FileSize int    `json:"file_size"`
	}
	body := map[string]interface{}{"file_id": fileID}
	result, err := telegramRequest[fileResult](ctx, baseURL, botToken, "getFile", body)
	if err != nil {
		return nil, fmt.Errorf("telegram getFile: %w", err)
	}
	if result.FilePath == "" {
		return nil, fmt.Errorf("telegram getFile: empty file_path")
	}

	// Step 2: Download the file.
	fileURL := fmt.Sprintf("%s/file/bot%s/%s", baseURL, botToken, result.FilePath)
	req, err := http.NewRequestWithContext(ctx, "GET", fileURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram download file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram download file: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("telegram download file: %w", err)
	}
	return data, nil
}

// TelegramSendPhoto sends a photo to a chat via multipart upload.
func TelegramSendPhoto(ctx context.Context, baseURL, botToken string, chatID int64, photoData []byte, caption string) error {
	if baseURL == "" {
		baseURL = TelegramDefaultBaseURL
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if caption != "" {
		w.WriteField("caption", caption)
	}
	part, err := w.CreateFormFile("photo", "image.png")
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(photoData); err != nil {
		return fmt.Errorf("write photo data: %w", err)
	}
	w.Close()

	url := fmt.Sprintf("%s/bot%s/sendPhoto", baseURL, botToken)
	req, err := http.NewRequestWithContext(ctx, "POST", url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram sendPhoto: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram sendPhoto: status %d: %s", resp.StatusCode, string(body))
	}
	return nil
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
