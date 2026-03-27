package imbridge

import (
	"context"
	"time"
)

// Provider defines the contract for an IM platform integration.
type Provider interface {
	// Name returns the provider identifier: "weixin", "telegram".
	Name() string

	// JIDSuffix returns the suffix used to construct chat JIDs: "@im.wechat", "@tg".
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
// Providers that implement this will have typing indicators started when messages
// are forwarded to NanoClaw and stopped when replies are received.
type TypingProvider interface {
	// StartTyping begins sending typing indicators for a user in a goroutine.
	// Typing continues until ctx is cancelled or times out internally.
	// sendError is called on timeout to notify the user.
	StartTyping(ctx context.Context, creds *Credentials, userID string, meta map[string]string,
		sendError func(text string))
}

// InboundMessage represents a single incoming message from the IM platform.
type InboundMessage struct {
	FromUserID string
	SenderName string
	Text       string
	IsGroup    bool              // true for group/supergroup chats
	Metadata   map[string]string // provider-specific state (e.g., weixin context_token)
	MediaData  []byte            // optional: downloaded media (image/file) binary data
	MediaType  string            // optional: "image", "voice", "file", "video"
}
