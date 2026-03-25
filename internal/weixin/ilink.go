package weixin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	DefaultAPIBaseURL    = "https://ilinkai.weixin.qq.com"
	defaultBotType       = "3"
	startTimeout         = 10 * time.Second
	pollTimeout          = 40 * time.Second // slightly longer than ilink's 35s long-poll
	sessionTTL           = 10 * time.Minute
	channelVersion       = "agentserver-bridge-1.0"
	longPollTimeout      = 40 * time.Second // slightly longer than server's 35s
	sendMessageTimeout   = 15 * time.Second
	sessionExpiredErrCode = -14
)

// Session holds the state of an in-progress QR login for a single sandbox.
type Session struct {
	QRCode    string    // opaque qrcode string for status polling
	QRCodeURL string    // image URL for frontend rendering
	StartedAt time.Time
}

// StatusResult is the parsed response from get_qrcode_status.
type StatusResult struct {
	Status  string `json:"status"` // "wait", "scaned", "confirmed", "expired"
	Token   string `json:"bot_token,omitempty"`
	BotID   string `json:"ilink_bot_id,omitempty"`
	BaseURL string `json:"baseurl,omitempty"`
	UserID  string `json:"ilink_user_id,omitempty"`
}

// --- iLink Message API types ---

// BaseInfo is common metadata attached to every iLink API request.
type BaseInfo struct {
	ChannelVersion string `json:"channel_version,omitempty"`
}

// MessageItem represents a single content item in a WeixinMessage.
type MessageItem struct {
	Type     int       `json:"type,omitempty"` // 1=TEXT, 2=IMAGE, 3=VOICE, 4=FILE, 5=VIDEO
	TextItem *TextItem `json:"text_item,omitempty"`
}

// TextItem holds the text content of a TEXT message item.
type TextItem struct {
	Text string `json:"text,omitempty"`
}

// WeixinMessage is a message from/to iLink.
type WeixinMessage struct {
	FromUserID   string        `json:"from_user_id,omitempty"`
	ToUserID     string        `json:"to_user_id,omitempty"`
	ClientID     string        `json:"client_id,omitempty"`
	CreateTimeMs int64         `json:"create_time_ms,omitempty"`
	MessageType  int           `json:"message_type,omitempty"`  // 1=USER, 2=BOT
	MessageState int           `json:"message_state,omitempty"` // 0=NEW, 1=GENERATING, 2=FINISH
	ItemList     []MessageItem `json:"item_list,omitempty"`
	ContextToken string        `json:"context_token,omitempty"`
}

// GetUpdatesRequest is the body for POST /ilink/bot/getupdates.
type GetUpdatesRequest struct {
	GetUpdatesBuf string   `json:"get_updates_buf"`
	BaseInfo      BaseInfo `json:"base_info"`
}

// GetUpdatesResponse is the response from getupdates.
type GetUpdatesResponse struct {
	Ret                  int             `json:"ret"`
	ErrCode              int             `json:"errcode,omitempty"`
	ErrMsg               string          `json:"errmsg,omitempty"`
	Msgs                 []WeixinMessage `json:"msgs,omitempty"`
	GetUpdatesBuf        string          `json:"get_updates_buf,omitempty"`
	LongPollingTimeoutMs int             `json:"longpolling_timeout_ms,omitempty"`
}

// SendMessageRequest is the body for POST /ilink/bot/sendmessage.
type SendMessageRequest struct {
	Msg      WeixinMessage `json:"msg"`
	BaseInfo BaseInfo      `json:"base_info"`
}

var (
	mu       sync.Mutex
	sessions = map[string]*Session{}
)

// purgeExpired removes sessions older than sessionTTL. Must be called with mu held.
func purgeExpired() {
	now := time.Now()
	for id, s := range sessions {
		if now.Sub(s.StartedAt) > sessionTTL {
			delete(sessions, id)
		}
	}
}

func GetSession(sandboxID string) *Session {
	mu.Lock()
	defer mu.Unlock()
	purgeExpired()
	s := sessions[sandboxID]
	if s != nil && time.Since(s.StartedAt) > sessionTTL {
		delete(sessions, sandboxID)
		return nil
	}
	return s
}

func SetSession(sandboxID string, s *Session) {
	mu.Lock()
	defer mu.Unlock()
	purgeExpired()
	sessions[sandboxID] = s
}

// TakeSession atomically returns and removes the session (used on confirmed).
func TakeSession(sandboxID string) *Session {
	mu.Lock()
	defer mu.Unlock()
	s := sessions[sandboxID]
	delete(sessions, sandboxID)
	return s
}

func ClearSession(sandboxID string) {
	mu.Lock()
	defer mu.Unlock()
	delete(sessions, sandboxID)
}

type qrCodeResponse struct {
	QRCode           string `json:"qrcode"`
	QRCodeImgContent string `json:"qrcode_img_content"`
}

// StartLogin calls the ilink API to generate a new QR code for WeChat login.
func StartLogin(ctx context.Context, apiBaseURL string) (*Session, error) {
	if apiBaseURL == "" {
		apiBaseURL = DefaultAPIBaseURL
	}
	u, err := url.Parse(apiBaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid apiBaseURL: %w", err)
	}
	u.Path = "/ilink/bot/get_bot_qrcode"
	q := u.Query()
	q.Set("bot_type", defaultBotType)
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ilink get_bot_qrcode: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ilink get_bot_qrcode: status %d", resp.StatusCode)
	}

	var qr qrCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		return nil, fmt.Errorf("ilink get_bot_qrcode: decode: %w", err)
	}
	if qr.QRCode == "" || qr.QRCodeImgContent == "" {
		return nil, fmt.Errorf("ilink get_bot_qrcode: empty response")
	}

	return &Session{
		QRCode:    qr.QRCode,
		QRCodeURL: qr.QRCodeImgContent,
		StartedAt: time.Now(),
	}, nil
}

// buildILinkHeaders builds the standard authentication headers for iLink bot API calls.
func buildILinkHeaders(botToken string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("AuthorizationType", "ilink_bot_token")
	if botToken != "" {
		h.Set("Authorization", "Bearer "+botToken)
	}
	return h
}

// GetUpdates long-polls iLink for new messages. Blocks for up to ~35s.
// Returns an empty response (Ret=0, no msgs) on client-side timeout.
func GetUpdates(ctx context.Context, apiBaseURL, botToken, getUpdatesBuf string) (*GetUpdatesResponse, error) {
	if apiBaseURL == "" {
		apiBaseURL = DefaultAPIBaseURL
	}
	u, err := url.Parse(apiBaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid apiBaseURL: %w", err)
	}
	u.Path = "/ilink/bot/getupdates"

	body := GetUpdatesRequest{
		GetUpdatesBuf: getUpdatesBuf,
		BaseInfo:      BaseInfo{ChannelVersion: channelVersion},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal getUpdates request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, longPollTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", u.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	for k, v := range buildILinkHeaders(botToken) {
		req.Header[k] = v
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Client-side timeout is normal for long-poll
		if ctx.Err() != nil {
			return &GetUpdatesResponse{Ret: 0, GetUpdatesBuf: getUpdatesBuf}, nil
		}
		return nil, fmt.Errorf("ilink getupdates: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ilink getupdates: status %d", resp.StatusCode)
	}

	var result GetUpdatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ilink getupdates: decode: %w", err)
	}
	return &result, nil
}

// SendTextMessage sends a text message to a WeChat user via iLink.
func SendTextMessage(ctx context.Context, apiBaseURL, botToken, toUserID, text, contextToken string) error {
	if apiBaseURL == "" {
		apiBaseURL = DefaultAPIBaseURL
	}
	u, err := url.Parse(apiBaseURL)
	if err != nil {
		return fmt.Errorf("invalid apiBaseURL: %w", err)
	}
	u.Path = "/ilink/bot/sendmessage"

	clientID := fmt.Sprintf("agentserver-%d", time.Now().UnixMilli())
	body := SendMessageRequest{
		Msg: WeixinMessage{
			ToUserID:     toUserID,
			ClientID:     clientID,
			MessageType:  2, // BOT
			MessageState: 2, // FINISH
			ContextToken: contextToken,
			ItemList: []MessageItem{{
				Type:     1, // TEXT
				TextItem: &TextItem{Text: text},
			}},
		},
		BaseInfo: BaseInfo{ChannelVersion: channelVersion},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal sendMessage request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, sendMessageTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", u.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	for k, v := range buildILinkHeaders(botToken) {
		req.Header[k] = v
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ilink sendmessage: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ilink sendmessage: status %d", resp.StatusCode)
	}
	return nil
}

// PollLoginStatus long-polls the ilink API for QR code scan status.
// Blocks for up to ~35 seconds (server-side long-poll).
func PollLoginStatus(ctx context.Context, apiBaseURL, qrcode string) (*StatusResult, error) {
	if apiBaseURL == "" {
		apiBaseURL = DefaultAPIBaseURL
	}
	u, err := url.Parse(apiBaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid apiBaseURL: %w", err)
	}
	u.Path = "/ilink/bot/get_qrcode_status"
	q := u.Query()
	q.Set("qrcode", qrcode)
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("iLink-App-ClientVersion", "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ilink get_qrcode_status: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ilink get_qrcode_status: status %d", resp.StatusCode)
	}

	var result StatusResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ilink get_qrcode_status: decode: %w", err)
	}
	return &result, nil
}
