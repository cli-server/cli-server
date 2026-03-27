package weixin

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	DefaultAPIBaseURL    = "https://ilinkai.weixin.qq.com"
	DefaultCDNBaseURL    = "https://novac2c.cdn.weixin.qq.com/c2c"
	defaultBotType       = "3"
	startTimeout         = 10 * time.Second
	pollTimeout          = 40 * time.Second // slightly longer than ilink's 35s long-poll
	sessionTTL           = 10 * time.Minute
	channelVersion       = "agentserver-bridge-1.0"
	longPollTimeout      = 40 * time.Second // slightly longer than server's 35s
	sendMessageTimeout   = 15 * time.Second
	configTimeout        = 10 * time.Second
	SessionExpiredErrCode = -14

	// TypingStatus values for SendTyping.
	TypingStatusTyping = 1
	TypingStatusCancel = 2
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
	Type      int        `json:"type,omitempty"` // 1=TEXT, 2=IMAGE, 3=VOICE, 4=FILE, 5=VIDEO
	TextItem  *TextItem  `json:"text_item,omitempty"`
	ImageItem *ImageItem `json:"image_item,omitempty"`
	VoiceItem *VoiceItem `json:"voice_item,omitempty"`
	FileItem  *FileItem  `json:"file_item,omitempty"`
}

// TextItem holds the text content of a TEXT message item.
type TextItem struct {
	Text string `json:"text,omitempty"`
}

// VoiceItem holds voice message data. Text is the speech-to-text transcription.
type VoiceItem struct {
	Text string `json:"text,omitempty"` // speech-to-text content
}

// FileItem holds file attachment metadata.
type FileItem struct {
	FileName string `json:"file_name,omitempty"`
}

// ImageItem holds the image data for an IMAGE message item.
type ImageItem struct {
	Media   *CDNMedia `json:"media,omitempty"`
	AESKey  string    `json:"aeskey,omitempty"` // raw AES-128 key as hex string; preferred over media.aes_key for inbound
	MidSize int       `json:"mid_size,omitempty"` // ciphertext size
}

// CDNMedia references a file uploaded to iLink CDN.
type CDNMedia struct {
	EncryptQueryParam string `json:"encrypt_query_param,omitempty"`
	AESKey            string `json:"aes_key,omitempty"`    // base64-encoded
	EncryptType       int    `json:"encrypt_type,omitempty"` // 1 = full packet encryption
}

// WeixinMessage is a message from/to iLink.
type WeixinMessage struct {
	FromUserID   string        `json:"from_user_id"`
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
	// X-WECHAT-UIN: random uint32 as decimal string, base64-encoded.
	// Required by iLink API (matches openclaw-weixin's randomWechatUin).
	uin := make([]byte, 4)
	rand.Read(uin)
	uint32Val := uint32(uin[0])<<24 | uint32(uin[1])<<16 | uint32(uin[2])<<8 | uint32(uin[3])
	h.Set("X-WECHAT-UIN", base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%d", uint32Val))))
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

	// Check response body for API-level errors (iLink returns HTTP 200 with ret != 0 on failure).
	var result struct {
		Ret    int    `json:"ret"`
		ErrMsg string `json:"errmsg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err == nil && result.Ret != 0 {
		return fmt.Errorf("ilink sendmessage: ret=%d errmsg=%s", result.Ret, result.ErrMsg)
	}
	return nil
}

// --- iLink CDN Media Upload ---

// GetUploadURLResponse is the response from ilink/bot/getuploadurl.
type GetUploadURLResponse struct {
	UploadParam string `json:"upload_param"`
}

// GetUploadURL obtains a pre-signed CDN upload URL from iLink.
func GetUploadURL(ctx context.Context, apiBaseURL, botToken string, params map[string]interface{}) (*GetUploadURLResponse, error) {
	if apiBaseURL == "" {
		apiBaseURL = DefaultAPIBaseURL
	}
	u, err := url.Parse(apiBaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid apiBaseURL: %w", err)
	}
	u.Path = "/ilink/bot/getuploadurl"

	params["base_info"] = BaseInfo{ChannelVersion: channelVersion}
	bodyBytes, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal getuploadurl: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, sendMessageTimeout)
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
		return nil, fmt.Errorf("ilink getuploadurl: %w", err)
	}
	defer resp.Body.Close()

	var result GetUploadURLResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ilink getuploadurl: decode: %w", err)
	}
	if result.UploadParam == "" {
		return nil, fmt.Errorf("ilink getuploadurl: empty upload_param")
	}
	return &result, nil
}

// UploadToCDN uploads encrypted file data to the iLink CDN.
// Returns the download encrypt_query_param from the response header.
func UploadToCDN(ctx context.Context, cdnBaseURL, uploadParam, filekey string, ciphertext []byte) (string, error) {
	cdnURL := fmt.Sprintf("%s/upload?encrypted_query_param=%s&filekey=%s",
		cdnBaseURL, url.QueryEscape(uploadParam), url.QueryEscape(filekey))

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", cdnURL, bytes.NewReader(ciphertext))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("cdn upload: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("cdn upload: status %d: %s", resp.StatusCode, string(body))
	}

	downloadParam := resp.Header.Get("x-encrypted-param")
	if downloadParam == "" {
		return "", fmt.Errorf("cdn upload: missing x-encrypted-param header")
	}
	return downloadParam, nil
}

// SendImageMessage sends an image message to a WeChat user via iLink.
func SendImageMessage(ctx context.Context, apiBaseURL, botToken, toUserID string, encryptQueryParam, aesKeyHex string, ciphertextSize int, contextToken string) error {
	if apiBaseURL == "" {
		apiBaseURL = DefaultAPIBaseURL
	}
	u, err := url.Parse(apiBaseURL)
	if err != nil {
		return fmt.Errorf("invalid apiBaseURL: %w", err)
	}
	u.Path = "/ilink/bot/sendmessage"

	// aes_key in message payload is base64-encoded (not hex)
	aesKeyBytes, err := hex.DecodeString(aesKeyHex)
	if err != nil {
		return fmt.Errorf("invalid aes key hex: %w", err)
	}
	aesKeyB64 := base64.StdEncoding.EncodeToString(aesKeyBytes)

	clientID := fmt.Sprintf("agentserver-%d", time.Now().UnixMilli())
	body := SendMessageRequest{
		Msg: WeixinMessage{
			ToUserID:     toUserID,
			ClientID:     clientID,
			MessageType:  2, // BOT
			MessageState: 2, // FINISH
			ContextToken: contextToken,
			ItemList: []MessageItem{{
				Type: 2, // IMAGE
				ImageItem: &ImageItem{
					Media: &CDNMedia{
						EncryptQueryParam: encryptQueryParam,
						AESKey:            aesKeyB64,
						EncryptType:       1,
					},
					MidSize: ciphertextSize,
				},
			}},
		},
		BaseInfo: BaseInfo{ChannelVersion: channelVersion},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal sendImageMessage: %w", err)
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
		return fmt.Errorf("ilink sendImageMessage: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ilink sendImageMessage: status %d", resp.StatusCode)
	}
	var result struct {
		Ret    int    `json:"ret"`
		ErrMsg string `json:"errmsg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err == nil && result.Ret != 0 {
		return fmt.Errorf("ilink sendImageMessage: ret=%d errmsg=%s", result.Ret, result.ErrMsg)
	}
	return nil
}

// UploadAndSendImage handles the complete flow: encrypt → CDN upload → send message.
func UploadAndSendImage(ctx context.Context, apiBaseURL, cdnBaseURL, botToken, toUserID string, imageData []byte, contextToken string) error {
	// Generate random filekey and AES key
	filekeyBytes := make([]byte, 16)
	aesKeyBytes := make([]byte, 16)
	rand.Read(filekeyBytes)
	rand.Read(aesKeyBytes)
	filekey := hex.EncodeToString(filekeyBytes)
	aesKeyHex := hex.EncodeToString(aesKeyBytes)

	// Calculate sizes and MD5
	rawsize := len(imageData)
	rawMD5 := md5.Sum(imageData)
	rawfilemd5 := hex.EncodeToString(rawMD5[:])
	filesize := AESECBPaddedSize(rawsize)

	// Step 1: Get upload URL
	uploadResp, err := GetUploadURL(ctx, apiBaseURL, botToken, map[string]interface{}{
		"filekey":      filekey,
		"media_type":   1, // IMAGE
		"to_user_id":   toUserID,
		"rawsize":      rawsize,
		"rawfilemd5":   rawfilemd5,
		"filesize":     filesize,
		"no_need_thumb": true,
		"aeskey":       aesKeyHex,
	})
	if err != nil {
		return fmt.Errorf("get upload url: %w", err)
	}

	// Step 2: Encrypt and upload to CDN
	ciphertext := EncryptAESECB(imageData, aesKeyBytes)

	cdnURL := cdnBaseURL
	if cdnURL == "" {
		cdnURL = DefaultCDNBaseURL
	}
	downloadParam, err := UploadToCDN(ctx, cdnURL, uploadResp.UploadParam, filekey, ciphertext)
	if err != nil {
		return fmt.Errorf("cdn upload: %w", err)
	}

	// Step 3: Send image message
	return SendImageMessage(ctx, apiBaseURL, botToken, toUserID, downloadParam, aesKeyHex, len(ciphertext), contextToken)
}

// --- AES-128-ECB encryption ---

// EncryptAESECB encrypts data with AES-128-ECB and PKCS7 padding.
func EncryptAESECB(plaintext, key []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(fmt.Sprintf("aes.NewCipher: %v", err))
	}
	blockSize := block.BlockSize()

	// PKCS7 padding
	padding := blockSize - len(plaintext)%blockSize
	padded := make([]byte, len(plaintext)+padding)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(padding)
	}

	// ECB mode: encrypt each block independently
	ciphertext := make([]byte, len(padded))
	for i := 0; i < len(padded); i += blockSize {
		block.Encrypt(ciphertext[i:i+blockSize], padded[i:i+blockSize])
	}
	return ciphertext
}

// AESECBPaddedSize returns the size after AES-128-ECB encryption with PKCS7 padding.
// PKCS7 always adds at least 1 byte, so output = ceil((size+1)/16) * 16.
func AESECBPaddedSize(plaintextSize int) int {
	return (plaintextSize/16 + 1) * 16
}

// DownloadFromCDN downloads a file from the iLink CDN using encrypt_query_param.
func DownloadFromCDN(ctx context.Context, cdnBaseURL, encryptQueryParam string) ([]byte, error) {
	if cdnBaseURL == "" {
		cdnBaseURL = DefaultCDNBaseURL
	}
	dlURL := fmt.Sprintf("%s/download?encrypted_query_param=%s",
		cdnBaseURL, url.QueryEscape(encryptQueryParam))

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", dlURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cdn download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cdn download: status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// DecryptAESECB decrypts AES-128-ECB encrypted data with PKCS7 padding.
func DecryptAESECB(ciphertext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes.NewCipher: %w", err)
	}
	blockSize := block.BlockSize()
	if len(ciphertext)%blockSize != 0 {
		return nil, fmt.Errorf("ciphertext size %d is not a multiple of block size %d", len(ciphertext), blockSize)
	}

	plaintext := make([]byte, len(ciphertext))
	for i := 0; i < len(ciphertext); i += blockSize {
		block.Decrypt(plaintext[i:i+blockSize], ciphertext[i:i+blockSize])
	}

	// Remove PKCS7 padding
	if len(plaintext) == 0 {
		return plaintext, nil
	}
	padding := int(plaintext[len(plaintext)-1])
	if padding > blockSize || padding == 0 {
		return nil, fmt.Errorf("invalid PKCS7 padding: %d", padding)
	}
	return plaintext[:len(plaintext)-padding], nil
}

// DownloadAndDecryptMedia downloads and decrypts a media file from iLink CDN.
// aesKeyBase64 is the base64-encoded AES key from the CDNMedia.aes_key field.
// Also supports hex-encoded keys wrapped in base64 (32 hex chars).
func DownloadAndDecryptMedia(ctx context.Context, cdnBaseURL, encryptQueryParam, aesKeyBase64 string) ([]byte, error) {
	// Parse AES key (supports two formats, matching openclaw-weixin's parseAesKey)
	decoded, err := base64.StdEncoding.DecodeString(aesKeyBase64)
	if err != nil {
		return nil, fmt.Errorf("decode aes key: %w", err)
	}
	var aesKey []byte
	if len(decoded) == 16 {
		aesKey = decoded
	} else if len(decoded) == 32 {
		// hex-encoded: base64 → hex string → raw bytes
		aesKey, err = hex.DecodeString(string(decoded))
		if err != nil {
			return nil, fmt.Errorf("decode hex aes key: %w", err)
		}
	} else {
		return nil, fmt.Errorf("aes key must be 16 raw bytes or 32-char hex, got %d bytes", len(decoded))
	}

	encrypted, err := DownloadFromCDN(ctx, cdnBaseURL, encryptQueryParam)
	if err != nil {
		return nil, err
	}

	return DecryptAESECB(encrypted, aesKey)
}

// ExtractText extracts the text content from a WeixinMessage.
func ExtractText(msg WeixinMessage) string {
	for _, item := range msg.ItemList {
		if item.Type == 1 && item.TextItem != nil {
			return item.TextItem.Text
		}
	}
	return ""
}

// GetConfigResponse is the response from ilink/bot/getconfig.
type GetConfigResponse struct {
	Ret          int    `json:"ret"`
	ErrMsg       string `json:"errmsg"`
	TypingTicket string `json:"typing_ticket"`
}

// GetConfig fetches bot config for a user, including the typing_ticket needed for SendTyping.
func GetConfig(ctx context.Context, apiBaseURL, botToken, userID, contextToken string) (*GetConfigResponse, error) {
	if apiBaseURL == "" {
		apiBaseURL = DefaultAPIBaseURL
	}
	u, err := url.Parse(apiBaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid apiBaseURL: %w", err)
	}
	u.Path = "/ilink/bot/getconfig"

	body := map[string]interface{}{
		"ilink_user_id": userID,
		"context_token": contextToken,
		"base_info":     BaseInfo{ChannelVersion: channelVersion},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal getconfig request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, configTimeout)
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
		return nil, fmt.Errorf("ilink getconfig: %w", err)
	}
	defer resp.Body.Close()

	var result GetConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ilink getconfig: decode: %w", err)
	}
	return &result, nil
}

// SendTyping sends a typing indicator to a WeChat user via iLink.
// status should be TypingStatusTyping (1) or TypingStatusCancel (2).
func SendTyping(ctx context.Context, apiBaseURL, botToken, userID, typingTicket string, status int) error {
	if apiBaseURL == "" {
		apiBaseURL = DefaultAPIBaseURL
	}
	u, err := url.Parse(apiBaseURL)
	if err != nil {
		return fmt.Errorf("invalid apiBaseURL: %w", err)
	}
	u.Path = "/ilink/bot/sendtyping"

	body := map[string]interface{}{
		"ilink_user_id": userID,
		"typing_ticket": typingTicket,
		"status":        status,
		"base_info":     BaseInfo{ChannelVersion: channelVersion},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal sendtyping request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, configTimeout)
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
		return fmt.Errorf("ilink sendtyping: %w", err)
	}
	defer resp.Body.Close()
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
