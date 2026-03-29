package imbridge

import (
	"context"
	"time"
)

// Provider defines the contract for an IM platform integration.
type Provider interface {
	// Name returns the provider identifier: "weixin", "telegram", "matrix".
	Name() string

	// JIDSuffix returns the suffix used to construct chat JIDs: "@im.wechat", "@tg", "@matrix".
	JIDSuffix() string

	// Poll long-polls the IM API for new messages.
	// cursor is opaque state from the previous poll (empty string on first call).
	Poll(ctx context.Context, creds *Credentials, cursor string) (*PollResult, error)

	// Send sends a text message to a user via the IM API.
	// meta carries provider-specific state (e.g., WeChat context_token). May be nil.
	Send(ctx context.Context, creds *Credentials, toUserID, text string, meta map[string]string) error
}

// Credentials holds the authentication info needed to talk to an IM API.
type Credentials struct {
	SandboxID string
	BotID     string
	BotToken  string
	BaseURL   string
}

// PollResult is returned by Provider.Poll.
type PollResult struct {
	Messages      []InboundMessage
	NewCursor     string
	ShouldBackoff time.Duration // >0 means pause before next poll
}

// TypingProvider is an optional interface for providers that support typing indicators.
type TypingProvider interface {
	StartTyping(ctx context.Context, creds *Credentials, userID string, meta map[string]string,
		sendError func(text string))
}

// ImageSendProvider is an optional interface for providers that support sending images.
type ImageSendProvider interface {
	SendImage(ctx context.Context, creds *Credentials, toUserID string, imageData []byte, caption string) error
}

// ConfigurableProvider validates credentials during configure.
// Returns the botID derived from the provider's validation response.
type ConfigurableProvider interface {
	ValidateCredentials(ctx context.Context, baseURL, token string) (botID string, err error)
}

// DisconnectProvider handles cleanup when a binding is explicitly disconnected.
type DisconnectProvider interface {
	Disconnect(sandboxID, botID string)
}

// InitializableProvider can be initialized with server-level configuration.
type InitializableProvider interface {
	InitProvider(dbURL string) error
}

// InboundMessage represents a single incoming message from the IM platform.
type InboundMessage struct {
	FromUserID    string
	SenderName    string
	Text          string
	IsGroup       bool              // true for group/supergroup chats
	Metadata      map[string]string // provider-specific state (e.g., weixin context_token)
	MediaData     []byte            // optional: downloaded media (image/file) binary data
	MediaType     string            // optional: "image", "voice", "file", "video"
	MediaFilename string            // optional: original filename for file attachments
}
