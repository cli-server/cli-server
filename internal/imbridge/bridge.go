package imbridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	bridgeRetryDelay       = 2 * time.Second
	bridgeBackoffDelay     = 30 * time.Second
	maxConsecutiveFailures = 3
	forwardTimeout         = 10 * time.Second
)

// BridgeDB is the DB interface needed by the bridge.
type BridgeDB interface {
	UpdateIMChannelCursor(channelID, cursor string) error
	UpsertChannelMeta(channelID, userID, key, value string) error
	GetChannelMeta(channelID, userID, key string) (string, error)
	GetAllChannelMeta(channelID, userID string) (map[string]string, error)
	GetSandboxForChannel(channelID string) (sandboxID, podIP, bridgeSecret, assistantName string, err error)
}

// SandboxResolver looks up the current state of a sandbox.
type SandboxResolver interface {
	GetPodIP(sandboxID string) string
}

// ExecCommander can execute a command inside a sandbox pod.
type ExecCommander interface {
	ExecSimple(ctx context.Context, sandboxID string, command []string) (string, error)
}

// BridgeBinding holds the info needed to run a poller for one IM channel.
// The sandbox to forward messages to is resolved dynamically from the channel ID.
type BridgeBinding struct {
	Provider    Provider
	Credentials Credentials
	ChannelID   string // workspace_im_channels.id
	Cursor      string
}

// Bridge manages per-binding poll goroutines for all IM providers.
type Bridge struct {
	db               BridgeDB
	resolver         SandboxResolver
	exec             ExecCommander
	providers        map[string]Provider
	pollers          map[string]context.CancelFunc // key: channelID
	registeredGroups map[string]string             // key: "sandboxID:chatJID" → cached settings hash
	channelMention   map[string]bool               // key: channelID → require_mention setting
	typingSessions   map[string]func()             // key: "channelID:userID" → cancel func
	mu               sync.Mutex
}

// NewBridge creates a new Bridge instance with the given providers.
func NewBridge(db BridgeDB, resolver SandboxResolver, exec ExecCommander, providers []Provider) *Bridge {
	pm := make(map[string]Provider, len(providers))
	for _, p := range providers {
		pm[p.Name()] = p
	}
	return &Bridge{
		db:               db,
		resolver:         resolver,
		exec:             exec,
		providers:        pm,
		pollers:          make(map[string]context.CancelFunc),
		registeredGroups: make(map[string]string),
		channelMention:   make(map[string]bool),
		typingSessions:   make(map[string]func()),
	}
}

// Providers returns all registered providers.
func (b *Bridge) Providers() []Provider {
	out := make([]Provider, 0, len(b.providers))
	for _, p := range b.providers {
		out = append(out, p)
	}
	return out
}

// GetProvider returns the provider with the given name, or nil if not found.
func (b *Bridge) GetProvider(name string) Provider {
	return b.providers[name]
}

// StartPoller starts a long-poll goroutine for a channel.
// If a poller already exists for this channel, it is stopped first.
func (b *Bridge) StartPoller(binding BridgeBinding) {
	b.mu.Lock()
	defer b.mu.Unlock()

	key := binding.ChannelID
	if cancel, ok := b.pollers[key]; ok {
		cancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	b.pollers[key] = cancel

	go b.pollLoop(ctx, binding)
}

// StopPoller stops the polling goroutine for a specific channel.
func (b *Bridge) StopPoller(channelID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if cancel, ok := b.pollers[channelID]; ok {
		cancel()
		delete(b.pollers, channelID)
	}
}

// StopAllPollers stops all polling goroutines and typing sessions.
func (b *Bridge) StopAllPollers() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for key, cancel := range b.pollers {
		cancel()
		delete(b.pollers, key)
	}
	for key, cancel := range b.typingSessions {
		cancel()
		delete(b.typingSessions, key)
	}
}

// FindProviderByJID matches a JID suffix to a provider.
// Returns nil if no provider matches.
func (b *Bridge) FindProviderByJID(jid string) Provider {
	for _, p := range b.providers {
		if strings.HasSuffix(jid, p.JIDSuffix()) {
			return p
		}
	}
	return nil
}

func typingKey(channelID, userID string) string {
	return channelID + ":" + userID
}

// startTypingForUser starts a typing indicator session if the provider supports it.
func (b *Bridge) startTypingForUser(binding BridgeBinding, msg InboundMessage) {
	tp, ok := binding.Provider.(TypingProvider)
	if !ok {
		return
	}

	key := typingKey(binding.ChannelID, msg.FromUserID)

	sendError := func(text string) {
		if err := binding.Provider.Send(context.Background(), &binding.Credentials, msg.FromUserID, text, msg.Metadata); err != nil {
			log.Printf("imbridge: failed to send error notice to %s: %v", msg.FromUserID, err)
		}
	}

	// Create context with timeout and register cancel in map BEFORE starting
	// the typing goroutine, so StopTyping can find it even if a reply arrives
	// quickly. The 5-minute timeout ensures goroutines don't leak if NanoClaw
	// never replies, and triggers an error notice to the user.
	ctx, cancelFn := context.WithTimeout(context.Background(), 5*time.Minute)

	b.mu.Lock()
	if existingCancel, exists := b.typingSessions[key]; exists {
		existingCancel()
	}
	b.typingSessions[key] = cancelFn
	b.mu.Unlock()

	// Start typing asynchronously using the pre-registered context.
	tp.StartTyping(ctx, &binding.Credentials, msg.FromUserID, msg.Metadata, sendError)
}

// SetChannelRequireMention updates the in-memory require_mention setting for a channel.
func (b *Bridge) SetChannelRequireMention(channelID string, requireMention bool) {
	b.mu.Lock()
	b.channelMention[channelID] = requireMention
	b.mu.Unlock()
}

// getChannelRequireMention reads the in-memory require_mention setting.
func (b *Bridge) getChannelRequireMention(channelID string) bool {
	b.mu.Lock()
	v := b.channelMention[channelID]
	b.mu.Unlock()
	return v
}

// StopTyping stops the typing indicator for a user in a channel.
func (b *Bridge) StopTyping(channelID, userID string) {
	key := typingKey(channelID, userID)
	b.mu.Lock()
	cancel, ok := b.typingSessions[key]
	if ok {
		delete(b.typingSessions, key)
	}
	b.mu.Unlock()
	if ok {
		cancel()
	}
}

// pollLoop is the long-poll goroutine for a single channel.
func (b *Bridge) pollLoop(ctx context.Context, binding BridgeBinding) {
	cursor := binding.Cursor
	consecutiveFailures := 0
	providerName := binding.Provider.Name()
	channelID := binding.ChannelID
	botID := binding.Credentials.BotID

	log.Printf("imbridge: starting poller for channel=%s provider=%s bot=%s", channelID, providerName, botID)

	for {
		if ctx.Err() != nil {
			log.Printf("imbridge: poller stopped for channel=%s provider=%s bot=%s", channelID, providerName, botID)
			return
		}

		result, err := binding.Provider.Poll(ctx, &binding.Credentials, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			consecutiveFailures++
			log.Printf("imbridge: poll error channel=%s provider=%s bot=%s err=%v (%d/%d)",
				channelID, providerName, botID, err, consecutiveFailures, maxConsecutiveFailures)
			if consecutiveFailures >= maxConsecutiveFailures {
				consecutiveFailures = 0
				sleepCtx(ctx, bridgeBackoffDelay)
			} else {
				sleepCtx(ctx, bridgeRetryDelay)
			}
			continue
		}

		if result.ShouldBackoff > 0 {
			log.Printf("imbridge: provider requested backoff=%v channel=%s provider=%s", result.ShouldBackoff, channelID, providerName)
			sleepCtx(ctx, result.ShouldBackoff)
			continue
		}

		consecutiveFailures = 0

		// Forward messages BEFORE advancing cursor.
		allForwarded := true
		for _, msg := range result.Messages {
			// Persist provider-specific metadata.
			for k, v := range msg.Metadata {
				if err := b.db.UpsertChannelMeta(channelID, msg.FromUserID, k, v); err != nil {
					log.Printf("imbridge: failed to save metadata key=%s: %v", k, err)
				}
			}

			forwarded, err := b.forwardToNanoClaw(ctx, binding, msg)
			if err != nil {
				log.Printf("imbridge: forward failed channel=%s from=%s: %v (will retry next poll)",
					channelID, msg.FromUserID, err)
				allForwarded = false
				break
			}
			if forwarded {
				b.startTypingForUser(binding, msg)
			}
		}

		if allForwarded && result.NewCursor != "" {
			cursor = result.NewCursor
			if err := b.db.UpdateIMChannelCursor(channelID, cursor); err != nil {
				log.Printf("imbridge: failed to save cursor channel=%s: %v", channelID, err)
			}
		}

		if !allForwarded {
			sleepCtx(ctx, bridgeRetryDelay)
		}
	}
}

// forwardToNanoClaw sends a message to the NanoClaw pod's bridge HTTP endpoint.
// Returns (true, nil) if forwarded, (false, nil) if skipped (e.g. not mentioned), or (false, err) on failure.
func (b *Bridge) forwardToNanoClaw(ctx context.Context, binding BridgeBinding, msg InboundMessage) (bool, error) {
	// Resolve which sandbox is bound to this channel.
	sandboxID, podIP, bridgeSecret, assistantName, err := b.db.GetSandboxForChannel(binding.ChannelID)
	if err != nil {
		return false, fmt.Errorf("no running sandbox bound to channel %s", binding.ChannelID)
	}
	if podIP == "" {
		return false, fmt.Errorf("sandbox %s has no PodIP (pod may be down or paused)", sandboxID)
	}

	// Skip messages in group chats that don't mention the bot (when require_mention is enabled).
	if b.getChannelRequireMention(binding.ChannelID) && msg.IsGroup && msg.Metadata["mentioned"] != "true" {
		return false, nil // not mentioned — skip silently, advance cursor
	}

	b.ensureGroupRegistered(ctx, sandboxID, msg.FromUserID, assistantName)

	if err := b.ensureChatRegistered(ctx, podIP, bridgeSecret, msg.FromUserID, msg.SenderName, binding.Provider.Name(), msg.IsGroup); err != nil {
		log.Printf("imbridge: failed to register chat %s: %v (continuing anyway)", msg.FromUserID, err)
	}

	payload := map[string]interface{}{
		"id":          fmt.Sprintf("im-%d", time.Now().UnixMilli()),
		"chat_jid":    msg.FromUserID,
		"sender":      msg.FromUserID,
		"sender_name": msg.SenderName,
		"content":     msg.Text,
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
		"provider":    binding.Provider.Name(),
	}
	if len(msg.MediaData) > 0 {
		payload["media_data"] = base64.StdEncoding.EncodeToString(msg.MediaData)
		payload["media_type"] = msg.MediaType
		if msg.MediaFilename != "" {
			payload["media_filename"] = msg.MediaFilename
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("marshal message: %w", err)
	}

	url := fmt.Sprintf("http://%s:3002/message", podIP)
	ctx, cancel := context.WithTimeout(ctx, forwardTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bridgeSecret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("forward to nanoclaw: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("nanoclaw returned status %d", resp.StatusCode)
	}
	return true, nil
}

// ensureChatRegistered sends a /metadata request to register the chat JID.
func (b *Bridge) ensureChatRegistered(ctx context.Context, podIP, bridgeSecret, chatJID, chatName, provider string, isGroup bool) error {
	meta := map[string]interface{}{
		"chat_jid":  chatJID,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"name":      chatName,
		"is_group":  isGroup,
		"provider":  provider,
	}
	body, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	url := fmt.Sprintf("http://%s:3002/metadata", podIP)
	ctx, cancel := context.WithTimeout(ctx, forwardTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bridgeSecret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("register chat metadata: %w", err)
	}
	defer resp.Body.Close()
	return nil
}

// ensureGroupRegistered registers a chat JID as a NanoClaw group via IPC.
// assistantName is used as the trigger name; defaults to "Andy" if empty.
// Re-registers if the settings (e.g. requireMention) have changed.
func (b *Bridge) ensureGroupRegistered(ctx context.Context, sandboxID, chatJID, assistantName string) {
	if b.exec == nil {
		return
	}
	if assistantName == "" {
		assistantName = "Andy"
	}

	// Cache includes settings so changes trigger re-registration.
	key := sandboxID + ":" + chatJID
	settingsHash := "v2:" + assistantName
	b.mu.Lock()
	if b.registeredGroups[key] == settingsHash {
		b.mu.Unlock()
		return
	}
	b.registeredGroups[key] = settingsHash
	b.mu.Unlock()

	folderName := sanitizeFolder(chatJID)

	// Use json.Marshal to safely encode the payload (avoids shell injection via chatJID).
	ipcData, _ := json.Marshal(map[string]interface{}{
		"type":            "register_group",
		"jid":             chatJID,
		"name":            chatJID,
		"folder":          folderName,
		"trigger":         assistantName,
		"requiresTrigger": false,
	})
	b64 := base64.StdEncoding.EncodeToString(ipcData)

	script := fmt.Sprintf(
		`mkdir -p /app/data/ipc/main/tasks && echo %s | base64 -d > /app/data/ipc/main/tasks/register-%s.json`,
		b64, folderName)

	if _, err := b.exec.ExecSimple(ctx, sandboxID, []string{"sh", "-c", script}); err != nil {
		log.Printf("imbridge: failed to register group %s in sandbox %s: %v", chatJID, sandboxID, err)
		b.mu.Lock()
		delete(b.registeredGroups, key)
		b.mu.Unlock()
	}
}

// sanitizeFolder converts a JID to a filesystem-safe folder name.
func sanitizeFolder(jid string) string {
	var out []byte
	for _, c := range []byte(jid) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	return "im-" + string(out)
}

// sleepCtx sleeps for the given duration or until the context is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
