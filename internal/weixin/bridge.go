package weixin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
	UpdateGetUpdatesBuf(sandboxID, botID, buf string) error
	UpsertContextToken(sandboxID, botID, userID, contextToken string) error
	GetContextToken(sandboxID, botID, userID string) (string, error)
}

// SandboxResolver looks up the current state of a sandbox.
// Used by the poller to get the latest PodIP (which changes on Pod restart).
type SandboxResolver interface {
	GetPodIP(sandboxID string) string
}

// BridgeBinding holds the info needed to run a poller for one WeChat binding.
type BridgeBinding struct {
	SandboxID     string
	BotID         string
	BotToken      string
	ILinkBaseURL  string
	GetUpdatesBuf string
	PodIP         string
	BridgeSecret  string
}

// Bridge manages per-sandbox long-poll goroutines for nanoclaw WeChat bindings.
type Bridge struct {
	db       BridgeDB
	resolver SandboxResolver
	pollers  map[string]context.CancelFunc // key: sandboxID:botID
	mu       sync.Mutex
}

// NewBridge creates a new Bridge instance.
func NewBridge(db BridgeDB, resolver SandboxResolver) *Bridge {
	return &Bridge{
		db:       db,
		resolver: resolver,
		pollers:  make(map[string]context.CancelFunc),
	}
}

func pollerKey(sandboxID, botID string) string {
	return sandboxID + ":" + botID
}

// StartPoller starts a long-poll goroutine for a single binding.
// If a poller already exists for this binding, it is stopped first.
func (b *Bridge) StartPoller(binding BridgeBinding) {
	b.mu.Lock()
	defer b.mu.Unlock()

	key := pollerKey(binding.SandboxID, binding.BotID)
	if cancel, ok := b.pollers[key]; ok {
		cancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	b.pollers[key] = cancel

	go b.pollLoop(ctx, binding)
}

// StopPoller stops the polling goroutine for a specific binding.
func (b *Bridge) StopPoller(sandboxID, botID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	key := pollerKey(sandboxID, botID)
	if cancel, ok := b.pollers[key]; ok {
		cancel()
		delete(b.pollers, key)
	}
}

// StopPollersForSandbox stops all polling goroutines for a sandbox.
func (b *Bridge) StopPollersForSandbox(sandboxID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for key, cancel := range b.pollers {
		// Key format is "sandboxID:botID"
		if len(key) > len(sandboxID) && key[:len(sandboxID)+1] == sandboxID+":" {
			cancel()
			delete(b.pollers, key)
		}
	}
}

// pollLoop is the long-poll goroutine for a single binding.
func (b *Bridge) pollLoop(ctx context.Context, binding BridgeBinding) {
	getUpdatesBuf := binding.GetUpdatesBuf
	consecutiveFailures := 0

	log.Printf("weixin bridge: starting poller for sandbox=%s bot=%s", binding.SandboxID, binding.BotID)

	for {
		select {
		case <-ctx.Done():
			log.Printf("weixin bridge: poller stopped for sandbox=%s bot=%s", binding.SandboxID, binding.BotID)
			return
		default:
		}

		resp, err := GetUpdates(ctx, binding.ILinkBaseURL, binding.BotToken, getUpdatesBuf)
		if err != nil {
			if ctx.Err() != nil {
				return // context cancelled
			}
			consecutiveFailures++
			log.Printf("weixin bridge: getUpdates error sandbox=%s bot=%s err=%v (%d/%d)",
				binding.SandboxID, binding.BotID, err, consecutiveFailures, maxConsecutiveFailures)
			if consecutiveFailures >= maxConsecutiveFailures {
				consecutiveFailures = 0
				sleepCtx(ctx, bridgeBackoffDelay)
			} else {
				sleepCtx(ctx, bridgeRetryDelay)
			}
			continue
		}

		// Check for API-level errors
		isError := (resp.Ret != 0) || (resp.ErrCode != 0)
		if isError {
			if resp.ErrCode == sessionExpiredErrCode || resp.Ret == sessionExpiredErrCode {
				log.Printf("weixin bridge: session expired sandbox=%s bot=%s, pausing 5min",
					binding.SandboxID, binding.BotID)
				sleepCtx(ctx, 5*time.Minute)
				continue
			}
			consecutiveFailures++
			log.Printf("weixin bridge: getUpdates API error sandbox=%s bot=%s ret=%d errcode=%d errmsg=%s",
				binding.SandboxID, binding.BotID, resp.Ret, resp.ErrCode, resp.ErrMsg)
			if consecutiveFailures >= maxConsecutiveFailures {
				consecutiveFailures = 0
				sleepCtx(ctx, bridgeBackoffDelay)
			} else {
				sleepCtx(ctx, bridgeRetryDelay)
			}
			continue
		}

		consecutiveFailures = 0

		// Forward messages BEFORE advancing cursor.
		// If any forward fails, we do NOT advance the cursor so the next
		// getUpdates returns the same messages and we can retry.
		allForwarded := true
		for _, msg := range resp.Msgs {
			if msg.FromUserID == "" {
				continue
			}
			// Store context token (safe to save even if forward fails later;
			// the token is per-user state, not per-message)
			if msg.ContextToken != "" {
				if err := b.db.UpsertContextToken(binding.SandboxID, binding.BotID, msg.FromUserID, msg.ContextToken); err != nil {
					log.Printf("weixin bridge: failed to save context token: %v", err)
				}
			}

			text := extractText(msg)
			if text == "" {
				continue
			}

			if err := b.forwardToNanoClaw(ctx, binding, msg.FromUserID, text); err != nil {
				log.Printf("weixin bridge: forward failed sandbox=%s from=%s: %v (will retry next poll)",
					binding.SandboxID, msg.FromUserID, err)
				allForwarded = false
				break // stop processing remaining messages; retry all on next poll
			}
		}

		// Only advance cursor after ALL messages are successfully forwarded.
		// This ensures no messages are lost if the NanoClaw pod is unreachable.
		if allForwarded && resp.GetUpdatesBuf != "" {
			getUpdatesBuf = resp.GetUpdatesBuf
			if err := b.db.UpdateGetUpdatesBuf(binding.SandboxID, binding.BotID, getUpdatesBuf); err != nil {
				log.Printf("weixin bridge: failed to save cursor sandbox=%s: %v", binding.SandboxID, err)
			}
		}

		// If forwarding failed, back off before retrying to avoid hammering iLink
		if !allForwarded {
			sleepCtx(ctx, bridgeRetryDelay)
		}
	}
}

// extractText extracts the text content from a WeixinMessage.
func extractText(msg WeixinMessage) string {
	for _, item := range msg.ItemList {
		if item.Type == 1 && item.TextItem != nil {
			return item.TextItem.Text
		}
	}
	return ""
}

// ensureChatRegistered sends a /metadata request to register the chat JID in NanoClaw's
// chats table before sending messages. NanoClaw's messages table has a FOREIGN KEY on
// chat_jid → chats(jid), so the chat must exist first.
func (b *Bridge) ensureChatRegistered(ctx context.Context, podIP, bridgeSecret, chatJID string) error {
	meta := map[string]interface{}{
		"chat_jid":  chatJID,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"name":      chatJID,
		"is_group":  false,
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

// forwardToNanoClaw sends a message to the NanoClaw pod's weixin channel HTTP endpoint.
// PodIP is resolved dynamically via the SandboxResolver to handle Pod restarts.
func (b *Bridge) forwardToNanoClaw(ctx context.Context, binding BridgeBinding, fromUserID, text string) error {
	// Resolve current PodIP — it may have changed since the poller started.
	podIP := b.resolver.GetPodIP(binding.SandboxID)
	if podIP == "" {
		return fmt.Errorf("sandbox %s has no PodIP (pod may be down or paused)", binding.SandboxID)
	}

	// Ensure the chat is registered in NanoClaw's DB before sending messages.
	// NanoClaw's messages table has a FOREIGN KEY on chat_jid → chats(jid).
	if err := b.ensureChatRegistered(ctx, podIP, binding.BridgeSecret, fromUserID); err != nil {
		log.Printf("weixin bridge: failed to register chat %s: %v (continuing anyway)", fromUserID, err)
	}

	msg := map[string]interface{}{
		"id":          fmt.Sprintf("weixin-%d", time.Now().UnixMilli()),
		"chat_jid":    fromUserID,
		"sender":      fromUserID,
		"sender_name": fromUserID,
		"content":     text,
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
	}

	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	url := fmt.Sprintf("http://%s:3002/message", podIP)
	ctx, cancel := context.WithTimeout(ctx, forwardTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+binding.BridgeSecret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("forward to nanoclaw: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("nanoclaw returned status %d", resp.StatusCode)
	}
	return nil
}

// sleepCtx sleeps for the given duration or until the context is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
