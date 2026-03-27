# Unified IM Bridge Layer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extract a generic `imbridge` package from the existing WeChat-specific bridge, implement a Telegram provider, unify database tables and API routes, and add a Telegram configuration UI.

**Architecture:** The existing `internal/weixin/bridge.go` is refactored into a generic `internal/imbridge/` package with a `Provider` interface. WeChat and Telegram each implement this interface. The generic bridge handles polling lifecycle, message forwarding, group registration, and cursor management. NanoClaw receives messages from all IMs via the same HTTP bridge on `:3002`.

**Tech Stack:** Go (agentserver backend), PostgreSQL (migrations), TypeScript/React (frontend), Telegram Bot API (HTTP, no third-party deps)

**Spec:** `docs/superpowers/specs/2026-03-26-unified-im-bridge-design.md`

---

## File Structure

### New files (agentserver)
| File | Responsibility |
|------|---------------|
| `internal/imbridge/provider.go` | `Provider` interface, `Credentials`, `PollResult`, `InboundMessage` types |
| `internal/imbridge/bridge.go` | Generic `Bridge` struct, `StartPoller`, `StopPoller`, `pollLoop`, `forwardToNanoClaw`, `ensureGroupRegistered`, `ensureChatRegistered`, `FindProviderByJID` |
| `internal/imbridge/weixin_provider.go` | `WeixinProvider` implementing `Provider` — wraps `weixin/ilink.go` |
| `internal/imbridge/telegram_provider.go` | `TelegramProvider` implementing `Provider` |
| `internal/imbridge/telegram_api.go` | Pure HTTP client for Telegram Bot API (`GetMe`, `GetUpdates`, `SendMessage`, `SendChatAction`) |
| `internal/db/im_bindings.go` | `BridgeDB` interface impl + `IMBinding` struct, replaces `weixin_bindings.go` |
| `internal/db/migrations/010_unified_im_bindings.sql` | New tables `sandbox_im_bindings`, `im_provider_meta` + data migration |

### Modified files (agentserver)
| File | Changes |
|------|---------|
| `internal/server/server.go` | Replace `WeixinBridge` with `IMBridge`, add new routes (`/im/telegram/configure`, `/im/telegram` DELETE, `/im/bindings`, `/im/send`), update restore functions, keep old route aliases |
| `internal/sandbox/config.go` | Add `NanoclawIMBridgeEnabled`, update `BuildNanoclawConfig` to emit `NANOCLAW_BRIDGE_URL` |
| `web/src/lib/api.ts` | Add `IMBinding` type, `telegramConfigure`, `telegramDisconnect`, `listIMBindings` API functions; update `weixinQRStart`/`weixinQRWait` paths |
| `web/src/components/WeixinLoginModal.tsx` | Update API paths to `/api/sandboxes/{id}/im/weixin/qr-*` |
| `web/src/components/SandboxDetail.tsx` | Replace "WeChat Bindings" with "IM Connections" section, add Telegram config button |

### New files (frontend)
| File | Responsibility |
|------|---------------|
| `web/src/components/TelegramConfigModal.tsx` | Bot token input form, calls `/api/sandboxes/{id}/im/telegram/configure` |

### Files to delete (agentserver)
| File | Reason |
|------|--------|
| `internal/weixin/bridge.go` | Logic moved to `internal/imbridge/bridge.go` |
| `internal/db/weixin_bindings.go` | Replaced by `internal/db/im_bindings.go` |

### Unchanged
| File | Note |
|------|------|
| `internal/weixin/ilink.go` | iLink API client used by `WeixinProvider` — no changes |

---

## Task 1: Create Provider Interface and Types

**Files:**
- Create: `internal/imbridge/provider.go`

- [ ] **Step 1: Create the imbridge package with Provider interface**

```go
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

// InboundMessage represents a single incoming message from the IM platform.
type InboundMessage struct {
	FromUserID string
	SenderName string
	Text       string
	IsGroup    bool              // true for group/supergroup chats
	Metadata   map[string]string // provider-specific state (e.g., weixin context_token)
}
```

- [ ] **Step 2: Verify the file compiles**

Run: `cd /root/agentserver && go build ./internal/imbridge/`
Expected: success (no output)

- [ ] **Step 3: Commit**

```bash
git add internal/imbridge/provider.go
git commit -m "feat(imbridge): add Provider interface and message types"
```

---

## Task 2: Create Generic Bridge

**Files:**
- Create: `internal/imbridge/bridge.go`
- Reference: `internal/weixin/bridge.go` (lines 1-387) — extracting and generalizing logic

- [ ] **Step 1: Create bridge.go with Bridge struct and lifecycle methods**

```go
package imbridge

import (
	"bytes"
	"context"
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
	UpdateCursor(sandboxID, provider, botID, cursor string) error
	UpsertProviderMeta(sandboxID, provider, botID, userID, key, value string) error
	GetProviderMeta(sandboxID, provider, botID, userID, key string) (string, error)
}

// SandboxResolver looks up the current state of a sandbox.
type SandboxResolver interface {
	GetPodIP(sandboxID string) string
}

// ExecCommander can execute a command inside a sandbox pod.
type ExecCommander interface {
	ExecSimple(ctx context.Context, sandboxID string, command []string) (string, error)
}

// BridgeBinding holds the info needed to run a poller for one IM binding.
type BridgeBinding struct {
	Provider     Provider
	Credentials  Credentials
	Cursor       string
	PodIP        string
	BridgeSecret string
}

// Bridge manages per-binding poll goroutines for all IM providers.
type Bridge struct {
	db               BridgeDB
	resolver         SandboxResolver
	exec             ExecCommander
	providers        map[string]Provider           // keyed by provider name
	pollers          map[string]context.CancelFunc // key: "sandboxID:provider:botID"
	registeredGroups map[string]bool               // key: "sandboxID:chatJID"
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
		registeredGroups: make(map[string]bool),
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

func pollerKey(sandboxID, provider, botID string) string {
	return sandboxID + ":" + provider + ":" + botID
}

// StartPoller starts a long-poll goroutine for a single binding.
// If a poller already exists for this binding, it is stopped first.
func (b *Bridge) StartPoller(binding BridgeBinding) {
	b.mu.Lock()
	defer b.mu.Unlock()

	key := pollerKey(binding.Credentials.SandboxID, binding.Provider.Name(), binding.Credentials.BotID)
	if cancel, ok := b.pollers[key]; ok {
		cancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	b.pollers[key] = cancel

	go b.pollLoop(ctx, binding)
}

// StopPoller stops the polling goroutine for a specific binding.
func (b *Bridge) StopPoller(sandboxID, provider, botID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	key := pollerKey(sandboxID, provider, botID)
	if cancel, ok := b.pollers[key]; ok {
		cancel()
		delete(b.pollers, key)
	}
}

// StopPollersForSandbox stops all polling goroutines for a sandbox.
func (b *Bridge) StopPollersForSandbox(sandboxID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	prefix := sandboxID + ":"
	for key, cancel := range b.pollers {
		if strings.HasPrefix(key, prefix) {
			cancel()
			delete(b.pollers, key)
		}
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

// StripJIDSuffix removes the provider's JID suffix from a full JID.
func StripJIDSuffix(jid string, p Provider) string {
	return strings.TrimSuffix(jid, p.JIDSuffix())
}

// pollLoop is the long-poll goroutine for a single binding.
func (b *Bridge) pollLoop(ctx context.Context, binding BridgeBinding) {
	cursor := binding.Cursor
	consecutiveFailures := 0
	providerName := binding.Provider.Name()
	sandboxID := binding.Credentials.SandboxID
	botID := binding.Credentials.BotID

	log.Printf("imbridge: starting poller for sandbox=%s provider=%s bot=%s", sandboxID, providerName, botID)

	for {
		if ctx.Err() != nil {
			log.Printf("imbridge: poller stopped for sandbox=%s provider=%s bot=%s", sandboxID, providerName, botID)
			return
		}

		result, err := binding.Provider.Poll(ctx, &binding.Credentials, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			consecutiveFailures++
			log.Printf("imbridge: poll error sandbox=%s provider=%s bot=%s err=%v (%d/%d)",
				sandboxID, providerName, botID, err, consecutiveFailures, maxConsecutiveFailures)
			if consecutiveFailures >= maxConsecutiveFailures {
				consecutiveFailures = 0
				sleepCtx(ctx, bridgeBackoffDelay)
			} else {
				sleepCtx(ctx, bridgeRetryDelay)
			}
			continue
		}

		if result.ShouldBackoff > 0 {
			sleepCtx(ctx, result.ShouldBackoff)
			continue
		}

		consecutiveFailures = 0

		// Forward messages BEFORE advancing cursor.
		allForwarded := true
		for _, msg := range result.Messages {
			// Persist provider-specific metadata
			for k, v := range msg.Metadata {
				if err := b.db.UpsertProviderMeta(sandboxID, providerName, botID, msg.FromUserID, k, v); err != nil {
					log.Printf("imbridge: failed to save metadata key=%s: %v", k, err)
				}
			}

			chatJID := msg.FromUserID + binding.Provider.JIDSuffix()

			fwdMsg := msg
			fwdMsg.FromUserID = chatJID // replace with suffixed JID
			if err := b.forwardToNanoClaw(ctx, binding, fwdMsg); err != nil {
				log.Printf("imbridge: forward failed sandbox=%s from=%s: %v (will retry next poll)",
					sandboxID, chatJID, err)
				allForwarded = false
				break
			}
		}

		if allForwarded && result.NewCursor != "" {
			cursor = result.NewCursor
			if err := b.db.UpdateCursor(sandboxID, providerName, botID, cursor); err != nil {
				log.Printf("imbridge: failed to save cursor sandbox=%s: %v", sandboxID, err)
			}
		}

		if !allForwarded {
			sleepCtx(ctx, bridgeRetryDelay)
		}
	}
}

// forwardToNanoClaw sends a message to the NanoClaw pod's bridge HTTP endpoint.
func (b *Bridge) forwardToNanoClaw(ctx context.Context, binding BridgeBinding, msg InboundMessage) error {
	sandboxID := binding.Credentials.SandboxID

	podIP := b.resolver.GetPodIP(sandboxID)
	if podIP == "" {
		return fmt.Errorf("sandbox %s has no PodIP (pod may be down or paused)", sandboxID)
	}

	b.ensureGroupRegistered(ctx, sandboxID, msg.FromUserID)

	if err := b.ensureChatRegistered(ctx, podIP, binding.BridgeSecret, msg.FromUserID, msg.SenderName, msg.IsGroup); err != nil {
		log.Printf("imbridge: failed to register chat %s: %v (continuing anyway)", msg.FromUserID, err)
	}

	payload := map[string]interface{}{
		"id":          fmt.Sprintf("im-%d", time.Now().UnixMilli()),
		"chat_jid":    msg.FromUserID,
		"sender":      msg.FromUserID,
		"sender_name": msg.SenderName,
		"content":     msg.Text,
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
	}

	body, err := json.Marshal(payload)
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

// ensureChatRegistered sends a /metadata request to register the chat JID in NanoClaw's
// chats table before sending messages.
func (b *Bridge) ensureChatRegistered(ctx context.Context, podIP, bridgeSecret, chatJID, chatName string, isGroup bool) error {
	meta := map[string]interface{}{
		"chat_jid":  chatJID,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"name":      chatName,
		"is_group":  isGroup,
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

// ensureGroupRegistered registers a chat JID as a NanoClaw group via the IPC mechanism.
func (b *Bridge) ensureGroupRegistered(ctx context.Context, sandboxID, chatJID string) {
	key := sandboxID + ":" + chatJID
	b.mu.Lock()
	already := b.registeredGroups[key]
	if !already {
		b.registeredGroups[key] = true
	}
	b.mu.Unlock()
	if already {
		return
	}

	if b.exec == nil {
		log.Printf("imbridge: no exec commander, cannot register group %s in sandbox %s", chatJID, sandboxID)
		return
	}

	folderName := sanitizeFolder(chatJID)
	ipcJSON := fmt.Sprintf(`{"type":"register_group","jid":"%s","name":"%s","folder":"%s","trigger":"Andy","requiresTrigger":false}`,
		chatJID, chatJID, folderName)

	script := fmt.Sprintf(
		`mkdir -p /app/data/ipc/main/tasks && echo '%s' > /app/data/ipc/main/tasks/register-%s.json`,
		ipcJSON, folderName)

	_, err := b.exec.ExecSimple(ctx, sandboxID, []string{"sh", "-c", script})
	if err != nil {
		log.Printf("imbridge: failed to register group %s in sandbox %s: %v", chatJID, sandboxID, err)
		b.mu.Lock()
		delete(b.registeredGroups, key)
		b.mu.Unlock()
		return
	}
	log.Printf("imbridge: registered group %s (folder=%s) in sandbox %s via IPC", chatJID, folderName, sandboxID)
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
	return string(out)
}

// sleepCtx sleeps for the given duration or until the context is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
```

- [ ] **Step 2: Verify the file compiles**

Run: `cd /root/agentserver && go build ./internal/imbridge/`
Expected: success (no output)

- [ ] **Step 3: Commit**

```bash
git add internal/imbridge/bridge.go
git commit -m "feat(imbridge): add generic Bridge with poll loop and forwarding"
```

---

## Task 3: Create WeChat Provider

**Files:**
- Create: `internal/imbridge/weixin_provider.go`
- Reference: `internal/weixin/ilink.go` (unchanged, used as dependency)

- [ ] **Step 1: Create weixin_provider.go**

```go
package imbridge

import (
	"context"
	"time"

	"github.com/agentserver/agentserver/internal/weixin"
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
		return &PollResult{ShouldBackoff: bridgeRetryDelay}, nil
	}

	var msgs []InboundMessage
	for _, m := range resp.Msgs {
		if m.FromUserID == "" {
			continue
		}
		text := weixin.ExtractText(m)
		if text == "" {
			continue
		}

		meta := map[string]string{}
		if m.ContextToken != "" {
			meta["context_token"] = m.ContextToken
		}

		msgs = append(msgs, InboundMessage{
			FromUserID: m.FromUserID,
			SenderName: m.FromUserID,
			Text:       text,
			Metadata:   meta,
		})
	}

	return &PollResult{Messages: msgs, NewCursor: resp.GetUpdatesBuf}, nil
}

func (p *WeixinProvider) Send(ctx context.Context, creds *Credentials, toUserID, text string, meta map[string]string) error {
	contextToken := ""
	if meta != nil {
		contextToken = meta["context_token"]
	}
	return weixin.SendTextMessage(ctx, creds.BaseURL, creds.BotToken, toUserID, text, contextToken)
}
```

- [ ] **Step 2: Export SessionExpiredErrCode and ExtractText from weixin/ilink.go**

The `WeixinProvider` needs access to `sessionExpiredErrCode` (currently unexported) and `extractText` (currently in `bridge.go`). Export them from `internal/weixin/ilink.go`:

In `internal/weixin/ilink.go`, change the constant name:

```go
// Find:
sessionExpiredErrCode = -14
// Replace with:
SessionExpiredErrCode = -14
```

In `internal/weixin/bridge.go`, the `extractText` function (lines 222-230) needs to be moved to `ilink.go` and exported, since `bridge.go` will be deleted. Add to the end of `internal/weixin/ilink.go`:

```go
// ExtractText extracts the text content from a WeixinMessage.
func ExtractText(msg WeixinMessage) string {
	for _, item := range msg.ItemList {
		if item.Type == 1 && item.TextItem != nil {
			return item.TextItem.Text
		}
	}
	return ""
}
```

Also update the reference in `internal/weixin/bridge.go` line 193 from `extractText(msg)` to `ExtractText(msg)`, and update the `sessionExpiredErrCode` reference on line 156 to `SessionExpiredErrCode`. These changes keep bridge.go compiling until it's deleted in Task 7.

- [ ] **Step 3: Verify the build**

Run: `cd /root/agentserver && go build ./internal/imbridge/ && go build ./internal/weixin/`
Expected: success

- [ ] **Step 4: Commit**

```bash
git add internal/imbridge/weixin_provider.go internal/weixin/ilink.go internal/weixin/bridge.go
git commit -m "feat(imbridge): add WeixinProvider wrapping ilink API client"
```

---

## Task 4: Create Telegram Bot API Client

**Files:**
- Create: `internal/imbridge/telegram_api.go`

- [ ] **Step 1: Create telegram_api.go with HTTP client functions**

```go
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
```

- [ ] **Step 2: Verify the build**

Run: `cd /root/agentserver && go build ./internal/imbridge/`
Expected: success

- [ ] **Step 3: Commit**

```bash
git add internal/imbridge/telegram_api.go
git commit -m "feat(imbridge): add Telegram Bot API HTTP client"
```

---

## Task 5: Create Telegram Provider

**Files:**
- Create: `internal/imbridge/telegram_provider.go`

- [ ] **Step 1: Create telegram_provider.go**

```go
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
```

- [ ] **Step 2: Verify the build**

Run: `cd /root/agentserver && go build ./internal/imbridge/`
Expected: success

- [ ] **Step 3: Commit**

```bash
git add internal/imbridge/telegram_provider.go
git commit -m "feat(imbridge): add TelegramProvider with Bot API polling"
```

---

## Task 6: Create Database Migration and Unified DB Layer

**Files:**
- Create: `internal/db/migrations/010_unified_im_bindings.sql`
- Create: `internal/db/im_bindings.go`

- [ ] **Step 1: Create the migration file**

```sql
-- Unified IM binding table (replaces sandbox_weixin_bindings for new code)
CREATE TABLE IF NOT EXISTS sandbox_im_bindings (
    id              SERIAL PRIMARY KEY,
    sandbox_id      TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    provider        TEXT NOT NULL,
    bot_id          TEXT NOT NULL,
    user_id         TEXT NOT NULL DEFAULT '',
    bot_token       TEXT,
    base_url        TEXT,
    cursor          TEXT,
    last_poll_at    TIMESTAMPTZ,
    bound_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_im_bindings_sandbox ON sandbox_im_bindings(sandbox_id);
CREATE INDEX idx_im_bindings_provider_bot ON sandbox_im_bindings(provider, bot_id);

-- Provider-specific per-user metadata (replaces weixin_context_tokens for new code)
CREATE TABLE IF NOT EXISTS im_provider_meta (
    sandbox_id  TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    provider    TEXT NOT NULL,
    bot_id      TEXT NOT NULL,
    user_id     TEXT NOT NULL,
    meta_key    TEXT NOT NULL,
    meta_value  TEXT NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (sandbox_id, provider, bot_id, user_id, meta_key)
);

-- Migrate existing WeChat bindings data
INSERT INTO sandbox_im_bindings
    (sandbox_id, provider, bot_id, user_id, bot_token, base_url, cursor, last_poll_at, bound_at)
SELECT
    sandbox_id, 'weixin', bot_id, user_id, bot_token, ilink_base_url, get_updates_buf, last_poll_at, bound_at
FROM sandbox_weixin_bindings
ON CONFLICT DO NOTHING;

-- Migrate existing WeChat context tokens
INSERT INTO im_provider_meta
    (sandbox_id, provider, bot_id, user_id, meta_key, meta_value, updated_at)
SELECT
    sandbox_id, 'weixin', bot_id, user_id, 'context_token', context_token, updated_at
FROM weixin_context_tokens
ON CONFLICT DO NOTHING;

-- Old tables (sandbox_weixin_bindings, weixin_context_tokens) are NOT dropped.
-- They are retained for rollback safety. A future migration will drop them.
```

- [ ] **Step 2: Create im_bindings.go with BridgeDB implementation**

```go
package db

import "time"

// IMBinding represents a row in the sandbox_im_bindings table.
type IMBinding struct {
	ID        int
	SandboxID string
	Provider  string
	BotID     string
	UserID    string
	BotToken  string
	BaseURL   string
	Cursor    string
	BoundAt   time.Time
}

// CreateIMBinding inserts a new IM binding record.
func (db *DB) CreateIMBinding(sandboxID, provider, botID, userID string) error {
	_, err := db.Exec(
		`INSERT INTO sandbox_im_bindings (sandbox_id, provider, bot_id, user_id) VALUES ($1, $2, $3, $4)`,
		sandboxID, provider, botID, userID,
	)
	return err
}

// SaveIMCredentials stores bot credentials for an IM binding.
func (db *DB) SaveIMCredentials(sandboxID, provider, botID, botToken, baseURL string) error {
	_, err := db.Exec(
		`UPDATE sandbox_im_bindings SET bot_token = $1, base_url = $2 WHERE sandbox_id = $3 AND provider = $4 AND bot_id = $5`,
		botToken, baseURL, sandboxID, provider, botID,
	)
	return err
}

// GetIMCredentials retrieves bot credentials for an IM binding.
func (db *DB) GetIMCredentials(sandboxID, provider, botID string) (botToken, baseURL string, err error) {
	err = db.QueryRow(
		`SELECT COALESCE(bot_token, ''), COALESCE(base_url, '') FROM sandbox_im_bindings WHERE sandbox_id = $1 AND provider = $2 AND bot_id = $3`,
		sandboxID, provider, botID,
	).Scan(&botToken, &baseURL)
	return
}

// ListIMBindings returns all IM bindings for a sandbox, optionally filtered by provider.
// If provider is empty, all bindings are returned.
func (db *DB) ListIMBindings(sandboxID, provider string) ([]*IMBinding, error) {
	query := `SELECT id, sandbox_id, provider, bot_id, user_id, bound_at FROM sandbox_im_bindings WHERE sandbox_id = $1`
	args := []interface{}{sandboxID}
	if provider != "" {
		query += ` AND provider = $2`
		args = append(args, provider)
	}
	query += ` ORDER BY bound_at DESC`

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bindings []*IMBinding
	for rows.Next() {
		b := &IMBinding{}
		if err := rows.Scan(&b.ID, &b.SandboxID, &b.Provider, &b.BotID, &b.UserID, &b.BoundAt); err != nil {
			return nil, err
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}

// GetActiveBindings returns all bindings with credentials for a given provider,
// filtered to sandboxes of type 'nanoclaw' with status 'running'.
func (db *DB) GetActiveBindings(provider string) ([]*IMBinding, error) {
	rows, err := db.Query(
		`SELECT b.id, b.sandbox_id, b.provider, b.bot_id, b.user_id, b.bot_token, b.base_url, b.cursor, b.bound_at
		FROM sandbox_im_bindings b
		JOIN sandboxes s ON s.id = b.sandbox_id
		WHERE b.provider = $1
		  AND b.bot_token IS NOT NULL AND b.bot_token != ''
		  AND s.type = 'nanoclaw' AND s.status = 'running'`,
		provider,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bindings []*IMBinding
	for rows.Next() {
		b := &IMBinding{}
		var botToken, baseURL, cursor *string
		if err := rows.Scan(&b.ID, &b.SandboxID, &b.Provider, &b.BotID, &b.UserID, &botToken, &baseURL, &cursor, &b.BoundAt); err != nil {
			return nil, err
		}
		if botToken != nil {
			b.BotToken = *botToken
		}
		if baseURL != nil {
			b.BaseURL = *baseURL
		}
		if cursor != nil {
			b.Cursor = *cursor
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}

// GetActiveBindingsForSandbox returns all bindings with credentials for a specific sandbox.
func (db *DB) GetActiveBindingsForSandbox(sandboxID string) ([]*IMBinding, error) {
	rows, err := db.Query(
		`SELECT id, sandbox_id, provider, bot_id, user_id, bot_token, base_url, cursor, bound_at
		FROM sandbox_im_bindings
		WHERE sandbox_id = $1 AND bot_token IS NOT NULL AND bot_token != ''`,
		sandboxID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bindings []*IMBinding
	for rows.Next() {
		b := &IMBinding{}
		var botToken, baseURL, cursor *string
		if err := rows.Scan(&b.ID, &b.SandboxID, &b.Provider, &b.BotID, &b.UserID, &botToken, &baseURL, &cursor, &b.BoundAt); err != nil {
			return nil, err
		}
		if botToken != nil {
			b.BotToken = *botToken
		}
		if baseURL != nil {
			b.BaseURL = *baseURL
		}
		if cursor != nil {
			b.Cursor = *cursor
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}

// UpdateCursor persists the long-poll cursor for an IM binding.
func (db *DB) UpdateCursor(sandboxID, provider, botID, cursor string) error {
	_, err := db.Exec(
		`UPDATE sandbox_im_bindings SET cursor = $1, last_poll_at = NOW() WHERE sandbox_id = $2 AND provider = $3 AND bot_id = $4`,
		cursor, sandboxID, provider, botID,
	)
	return err
}

// UpsertProviderMeta inserts or updates a provider-specific metadata entry.
func (db *DB) UpsertProviderMeta(sandboxID, provider, botID, userID, key, value string) error {
	_, err := db.Exec(
		`INSERT INTO im_provider_meta (sandbox_id, provider, bot_id, user_id, meta_key, meta_value, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (sandbox_id, provider, bot_id, user_id, meta_key)
		DO UPDATE SET meta_value = $6, updated_at = NOW()`,
		sandboxID, provider, botID, userID, key, value,
	)
	return err
}

// GetProviderMeta retrieves a provider-specific metadata value.
func (db *DB) GetProviderMeta(sandboxID, provider, botID, userID, key string) (string, error) {
	var value string
	err := db.QueryRow(
		`SELECT meta_value FROM im_provider_meta WHERE sandbox_id = $1 AND provider = $2 AND bot_id = $3 AND user_id = $4 AND meta_key = $5`,
		sandboxID, provider, botID, userID, key,
	).Scan(&value)
	return value, err
}

// GetAllProviderMeta retrieves all metadata entries for a user.
func (db *DB) GetAllProviderMeta(sandboxID, provider, botID, userID string) (map[string]string, error) {
	rows, err := db.Query(
		`SELECT meta_key, meta_value FROM im_provider_meta WHERE sandbox_id = $1 AND provider = $2 AND bot_id = $3 AND user_id = $4`,
		sandboxID, provider, botID, userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	meta := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		meta[k] = v
	}
	return meta, rows.Err()
}

// DeleteIMBinding deletes an IM binding by sandbox, provider, and bot ID.
func (db *DB) DeleteIMBinding(sandboxID, provider, botID string) error {
	_, err := db.Exec(
		`DELETE FROM sandbox_im_bindings WHERE sandbox_id = $1 AND provider = $2 AND bot_id = $3`,
		sandboxID, provider, botID,
	)
	return err
}
```

- [ ] **Step 3: Verify the build**

Run: `cd /root/agentserver && go build ./internal/db/`
Expected: success

- [ ] **Step 4: Commit**

```bash
git add internal/db/migrations/010_unified_im_bindings.sql internal/db/im_bindings.go
git commit -m "feat(db): add unified IM bindings tables and data access layer"
```

---

## Task 7: Update Server — Replace WeixinBridge with IMBridge

**Files:**
- Modify: `internal/server/server.go`
- Delete: `internal/weixin/bridge.go`
- Delete: `internal/db/weixin_bindings.go`

This is the largest task. It replaces the WeChat-specific bridge with the generic IM bridge, adds new routes, and keeps old routes as aliases.

- [ ] **Step 1: Update Server struct field**

In `internal/server/server.go`, replace:

```go
	// WeChat bridge for NanoClaw sandboxes (long-poll goroutine management)
	WeixinBridge *weixin.Bridge
```

with:

```go
	// IM bridge for NanoClaw sandboxes (long-poll goroutine management)
	IMBridge *imbridge.Bridge
```

Add the import `"github.com/agentserver/agentserver/internal/imbridge"` to the import block.

- [ ] **Step 2: Update Server constructor (New function)**

Replace lines 98-104:

```go
	// Pass ExecCommander if the process manager supports it (K8s backend does).
	var execCmd weixin.ExecCommander
	if ec, ok := processManager.(weixin.ExecCommander); ok {
		execCmd = ec
	}
	s.WeixinBridge = weixin.NewBridge(database, sandboxStore, execCmd)
	s.restoreWeixinBridgePollers()
```

with:

```go
	// Pass ExecCommander if the process manager supports it (K8s backend does).
	var execCmd imbridge.ExecCommander
	if ec, ok := processManager.(imbridge.ExecCommander); ok {
		execCmd = ec
	}
	s.IMBridge = imbridge.NewBridge(database, sandboxStore, execCmd, []imbridge.Provider{
		&imbridge.WeixinProvider{},
		&imbridge.TelegramProvider{},
	})
	s.restoreIMBridgePollers()
```

- [ ] **Step 3: Add new API routes**

In the `Router()` method, after the existing weixin route registrations (lines 236-237), add the new unified routes and keep old routes as aliases:

```go
		// IM Bridge routes (unified)
		r.Post("/api/sandboxes/{id}/im/weixin/qr-start", s.handleIMWeixinQRStart)
		r.Post("/api/sandboxes/{id}/im/weixin/qr-wait", s.handleIMWeixinQRWait)
		r.Post("/api/sandboxes/{id}/im/telegram/configure", s.handleIMTelegramConfigure)
		r.Delete("/api/sandboxes/{id}/im/telegram", s.handleIMTelegramDisconnect)
		r.Get("/api/sandboxes/{id}/im/bindings", s.handleListIMBindings)
```

Update the old route aliases (lines 236-237) to point to the new handlers:

```go
		// Legacy WeChat routes (aliases for backwards compatibility)
		r.Post("/api/sandboxes/{id}/weixin/qr-start", s.handleIMWeixinQRStart)
		r.Post("/api/sandboxes/{id}/weixin/qr-wait", s.handleIMWeixinQRWait)
```

For the internal nanoclaw send route (line 152), add the new unified route and keep the old one:

```go
	// Internal API for NanoClaw pods to send IM replies (auth via bridge secret).
	r.Post("/api/internal/nanoclaw/{id}/im/send", s.handleNanoclawIMSend)
	// Legacy alias
	r.Post("/api/internal/nanoclaw/{id}/weixin/send", s.handleNanoclawIMSend)
```

- [ ] **Step 4: Rename existing handler functions**

Rename the existing handlers to the new names. The logic stays the same initially:

- `handleWeixinQRStart` → `handleIMWeixinQRStart` (no logic changes needed)
- `handleWeixinQRWait` → `handleIMWeixinQRWait` (no logic changes needed)

- [ ] **Step 5: Update saveWeixinCredentials for nanoclaw to use IMBridge**

In `saveWeixinCredentials` (lines 1957-1984), update the nanoclaw branch to use the new DB and bridge:

Replace:

```go
		if dbErr := s.DB.CreateWeixinBinding(sandboxID, accountID, result.UserID); dbErr != nil {
			return fmt.Errorf("save binding: %w", dbErr)
		}
		if dbErr := s.DB.SaveBotCredentials(sandboxID, accountID, result.Token, baseURL); dbErr != nil {
			return fmt.Errorf("save bot credentials: %w", dbErr)
		}
		if sbx.PodIP != "" && s.WeixinBridge != nil {
			s.WeixinBridge.StartPoller(weixin.BridgeBinding{
				SandboxID:     sandboxID,
				BotID:         accountID,
				BotToken:      result.Token,
				ILinkBaseURL:  baseURL,
				GetUpdatesBuf: "",
				PodIP:         sbx.PodIP,
				BridgeSecret:  sbx.NanoclawBridgeSecret,
			})
		}
```

with:

```go
		if dbErr := s.DB.CreateIMBinding(sandboxID, "weixin", accountID, result.UserID); dbErr != nil {
			return fmt.Errorf("save binding: %w", dbErr)
		}
		if dbErr := s.DB.SaveIMCredentials(sandboxID, "weixin", accountID, result.Token, baseURL); dbErr != nil {
			return fmt.Errorf("save bot credentials: %w", dbErr)
		}
		if sbx.PodIP != "" && s.IMBridge != nil {
			s.IMBridge.StartPoller(imbridge.BridgeBinding{
				Provider:    &imbridge.WeixinProvider{},
				Credentials: imbridge.Credentials{SandboxID: sandboxID, BotID: accountID, BotToken: result.Token, BaseURL: baseURL},
				Cursor:      "",
				PodIP:       sbx.PodIP,
				BridgeSecret: sbx.NanoclawBridgeSecret,
			})
		}
```

Also update the openclaw branch (line 2038-2043) to write to the new table:

Replace:

```go
	if dbErr := s.DB.CreateWeixinBinding(sandboxID, accountID, result.UserID); dbErr != nil {
		log.Printf("weixin: failed to save binding record: %v", dbErr)
	}
	if dbErr := s.DB.SaveBotCredentials(sandboxID, accountID, result.Token, baseURL); dbErr != nil {
		log.Printf("weixin: failed to save bot credentials for openclaw: %v", dbErr)
	}
```

with:

```go
	if dbErr := s.DB.CreateIMBinding(sandboxID, "weixin", accountID, result.UserID); dbErr != nil {
		log.Printf("weixin: failed to save binding record: %v", dbErr)
	}
	if dbErr := s.DB.SaveIMCredentials(sandboxID, "weixin", accountID, result.Token, baseURL); dbErr != nil {
		log.Printf("weixin: failed to save bot credentials for openclaw: %v", dbErr)
	}
```

- [ ] **Step 6: Replace handleNanoclawWeixinSend with handleNanoclawIMSend**

Replace the entire `handleNanoclawWeixinSend` function (lines 2069-2137) with a unified handler:

```go
// handleNanoclawIMSend handles outbound messages from NanoClaw pods.
// The NanoClaw bridge channel calls this to send replies to IM users.
func (s *Server) handleNanoclawIMSend(w http.ResponseWriter, r *http.Request) {
	sandboxID := chi.URLParam(r, "id")

	sbx, ok := s.Sandboxes.Get(sandboxID)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if sbx.Type != "nanoclaw" {
		http.Error(w, "not a nanoclaw sandbox", http.StatusBadRequest)
		return
	}

	// Validate bridge secret
	authHeader := r.Header.Get("Authorization")
	expectedAuth := "Bearer " + sbx.NanoclawBridgeSecret
	if sbx.NanoclawBridgeSecret == "" || authHeader != expectedAuth {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		BotID    string `json:"bot_id"`
		ToUserID string `json:"to_user_id"`
		Text     string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.ToUserID == "" || req.Text == "" {
		http.Error(w, "to_user_id and text are required", http.StatusBadRequest)
		return
	}

	// Find provider by JID suffix
	provider := s.IMBridge.FindProviderByJID(req.ToUserID)
	if provider == nil {
		// Legacy: if no suffix matches, assume weixin
		provider = &imbridge.WeixinProvider{}
	}

	// Strip JID suffix to get raw user ID
	rawUserID := imbridge.StripJIDSuffix(req.ToUserID, provider)

	// Resolve bot_id: use provided value, or fall back to the first binding for this sandbox+provider.
	botID := req.BotID
	if botID == "" {
		bindings, err := s.DB.ListIMBindings(sandboxID, provider.Name())
		if err != nil || len(bindings) == 0 {
			http.Error(w, "no IM binding found", http.StatusNotFound)
			return
		}
		botID = bindings[0].BotID
	}

	// Look up bot credentials.
	botToken, baseURL, err := s.DB.GetIMCredentials(sandboxID, provider.Name(), botID)
	if err != nil || botToken == "" {
		http.Error(w, "bot credentials not found", http.StatusNotFound)
		return
	}

	// Get all provider metadata for this user (e.g., context_token for WeChat).
	meta, _ := s.DB.GetAllProviderMeta(sandboxID, provider.Name(), botID, rawUserID)

	creds := &imbridge.Credentials{
		SandboxID: sandboxID,
		BotID:     botID,
		BotToken:  botToken,
		BaseURL:   baseURL,
	}
	if err := provider.Send(r.Context(), creds, rawUserID, req.Text, meta); err != nil {
		log.Printf("nanoclaw im send: failed sandbox=%s provider=%s to=%s: %v", sandboxID, provider.Name(), rawUserID, err)
		http.Error(w, "failed to send message", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "sent"})
}
```

- [ ] **Step 7: Add handleIMTelegramConfigure handler**

Add this new handler function:

```go
// handleIMTelegramConfigure validates a Telegram bot token and creates a binding.
func (s *Server) handleIMTelegramConfigure(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.Sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}
	if sbx.Type != "nanoclaw" {
		http.Error(w, "telegram binding is only available for nanoclaw sandboxes", http.StatusBadRequest)
		return
	}
	if sbx.Status != "running" {
		http.Error(w, "sandbox is not running", http.StatusConflict)
		return
	}

	var req struct {
		BotToken string `json:"bot_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.BotToken == "" {
		http.Error(w, "bot_token is required", http.StatusBadRequest)
		return
	}

	// Validate token by calling getMe
	botInfo, err := imbridge.TelegramGetMe(r.Context(), imbridge.TelegramDefaultBaseURL, req.BotToken)
	if err != nil {
		log.Printf("telegram configure: getMe failed: %v", err)
		http.Error(w, "invalid bot token: "+err.Error(), http.StatusBadRequest)
		return
	}

	botID := botInfo.Username
	if botID == "" {
		botID = fmt.Sprintf("%d", botInfo.ID)
	}

	if err := s.DB.CreateIMBinding(id, "telegram", botID, ""); err != nil {
		log.Printf("telegram configure: create binding: %v", err)
		http.Error(w, "failed to save binding", http.StatusInternalServerError)
		return
	}
	if err := s.DB.SaveIMCredentials(id, "telegram", botID, req.BotToken, imbridge.TelegramDefaultBaseURL); err != nil {
		log.Printf("telegram configure: save credentials: %v", err)
		http.Error(w, "failed to save credentials", http.StatusInternalServerError)
		return
	}

	// Start polling immediately
	if sbx.PodIP != "" && s.IMBridge != nil {
		s.IMBridge.StartPoller(imbridge.BridgeBinding{
			Provider:     &imbridge.TelegramProvider{},
			Credentials:  imbridge.Credentials{SandboxID: id, BotID: botID, BotToken: req.BotToken, BaseURL: imbridge.TelegramDefaultBaseURL},
			Cursor:       "",
			PodIP:        sbx.PodIP,
			BridgeSecret: sbx.NanoclawBridgeSecret,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"connected": true,
		"bot_id":    botID,
		"bot_name":  botInfo.FirstName,
	})
}
```

- [ ] **Step 8: Add handleIMTelegramDisconnect handler**

```go
// handleIMTelegramDisconnect removes a Telegram binding and stops its poller.
func (s *Server) handleIMTelegramDisconnect(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.Sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}

	// Find the telegram binding to get bot_id
	bindings, err := s.DB.ListIMBindings(id, "telegram")
	if err != nil || len(bindings) == 0 {
		http.Error(w, "no telegram binding found", http.StatusNotFound)
		return
	}

	for _, b := range bindings {
		if s.IMBridge != nil {
			s.IMBridge.StopPoller(id, "telegram", b.BotID)
		}
		if err := s.DB.DeleteIMBinding(id, "telegram", b.BotID); err != nil {
			log.Printf("telegram disconnect: delete binding: %v", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "disconnected"})
}
```

- [ ] **Step 9: Add handleListIMBindings handler**

```go
// handleListIMBindings returns all IM bindings for a sandbox.
func (s *Server) handleListIMBindings(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.Sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}

	bindings, err := s.DB.ListIMBindings(id, "")
	if err != nil {
		log.Printf("list im bindings: %v", err)
		http.Error(w, "failed to list bindings", http.StatusInternalServerError)
		return
	}

	type bindingResponse struct {
		Provider string `json:"provider"`
		BotID    string `json:"bot_id"`
		UserID   string `json:"user_id,omitempty"`
		BoundAt  string `json:"bound_at"`
	}
	resp := make([]bindingResponse, 0, len(bindings))
	for _, b := range bindings {
		resp = append(resp, bindingResponse{
			Provider: b.Provider,
			BotID:    b.BotID,
			UserID:   b.UserID,
			BoundAt:  b.BoundAt.Format(time.RFC3339),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"bindings": resp})
}
```

- [ ] **Step 10: Replace restoreWeixinBridgePollers with restoreIMBridgePollers**

Replace `restoreWeixinBridgePollers` (lines 1684-1713) with:

```go
// restoreIMBridgePollers restarts long-poll goroutines for all active
// nanoclaw IM bindings. Called once during server startup.
func (s *Server) restoreIMBridgePollers() {
	if s.IMBridge == nil {
		return
	}
	restored := 0
	for _, provider := range s.IMBridge.Providers() {
		bindings, err := s.DB.GetActiveBindings(provider.Name())
		if err != nil {
			log.Printf("imbridge restore: failed to query %s bindings: %v", provider.Name(), err)
			continue
		}
		for _, b := range bindings {
			sbx, ok := s.Sandboxes.Get(b.SandboxID)
			if !ok || sbx.PodIP == "" {
				continue
			}
			s.IMBridge.StartPoller(imbridge.BridgeBinding{
				Provider:     provider,
				Credentials:  imbridge.Credentials{SandboxID: b.SandboxID, BotID: b.BotID, BotToken: b.BotToken, BaseURL: b.BaseURL},
				Cursor:       b.Cursor,
				PodIP:        sbx.PodIP,
				BridgeSecret: sbx.NanoclawBridgeSecret,
			})
			restored++
		}
	}
	if restored > 0 {
		log.Printf("imbridge restore: started %d poller(s)", restored)
	}
}
```

- [ ] **Step 11: Replace restoreWeixinBridgePollersForSandbox with restoreIMBridgePollersForSandbox**

Replace `restoreWeixinBridgePollersForSandbox` (lines 1717-1741) with:

```go
// restoreIMBridgePollersForSandbox restarts pollers for a single sandbox.
// Called after sandbox resume when the Pod has a new IP.
func (s *Server) restoreIMBridgePollersForSandbox(sandboxID string) {
	if s.IMBridge == nil {
		return
	}
	sbx, ok := s.Sandboxes.Get(sandboxID)
	if !ok || sbx.PodIP == "" {
		return
	}
	for _, provider := range s.IMBridge.Providers() {
		bindings, err := s.DB.GetActiveBindingsForSandbox(sandboxID)
		if err != nil {
			log.Printf("imbridge restore for %s: failed to query bindings: %v", sandboxID, err)
			return
		}
		for _, b := range bindings {
			if b.Provider != provider.Name() {
				continue
			}
			s.IMBridge.StartPoller(imbridge.BridgeBinding{
				Provider:     provider,
				Credentials:  imbridge.Credentials{SandboxID: b.SandboxID, BotID: b.BotID, BotToken: b.BotToken, BaseURL: b.BaseURL},
				Cursor:       b.Cursor,
				PodIP:        sbx.PodIP,
				BridgeSecret: sbx.NanoclawBridgeSecret,
			})
		}
	}
}
```

- [ ] **Step 12: Update handleResumeSandbox references**

In `handleResumeSandbox` (line 1527-1528), replace:

```go
		if ok && sbxNow.Type == "nanoclaw" && s.WeixinBridge != nil {
			s.restoreWeixinBridgePollersForSandbox(id)
		}
```

with:

```go
		if ok && sbxNow.Type == "nanoclaw" && s.IMBridge != nil {
			s.restoreIMBridgePollersForSandbox(id)
		}
```

- [ ] **Step 13: Update restoreOpenclawWeixinCredentials to use new DB**

In `restoreOpenclawWeixinCredentials` (line 1752), replace:

```go
	bindings, err := s.DB.GetBindingsWithBotTokenForSandbox(sandboxID)
```

with:

```go
	imBindings, err := s.DB.GetActiveBindingsForSandbox(sandboxID)
```

Then update the loop to use `imBindings` and the field names `b.BaseURL` (instead of `b.ILinkBaseURL`), `b.BotToken` (same). The fields align because `IMBinding` uses the same field names.

- [ ] **Step 14: Update attachWeixinBindings → attachIMBindings**

Replace the `attachWeixinBindings` function (lines 544-561) and the `weixinBindingResponse` type (lines 426-430):

Replace `weixinBindingResponse` with:

```go
type imBindingResponse struct {
	Provider string `json:"provider"`
	BotID    string `json:"bot_id"`
	UserID   string `json:"user_id,omitempty"`
	BoundAt  string `json:"bound_at"`
}
```

Update `sandboxResponse` (line 450): replace `WeixinBindings []weixinBindingResponse` with:

```go
	WeixinBindings []imBindingResponse `json:"weixin_bindings,omitempty"` // kept as weixin_bindings for API compat
	IMBindings     []imBindingResponse `json:"im_bindings,omitempty"`
```

Replace `attachWeixinBindings` with:

```go
// attachIMBindings fetches and attaches IM binding records to a sandbox response.
func (s *Server) attachIMBindings(resp *sandboxResponse) {
	if resp.Type != "openclaw" && resp.Type != "nanoclaw" {
		return
	}
	bindings, err := s.DB.ListIMBindings(resp.ID, "")
	if err != nil {
		log.Printf("list im bindings for %s: %v", resp.ID, err)
		return
	}
	for _, b := range bindings {
		entry := imBindingResponse{
			Provider: b.Provider,
			BotID:    b.BotID,
			UserID:   b.UserID,
			BoundAt:  b.BoundAt.Format(time.RFC3339),
		}
		resp.IMBindings = append(resp.IMBindings, entry)
		// Backwards compatibility: also populate weixin_bindings for existing frontends
		resp.WeixinBindings = append(resp.WeixinBindings, entry)
	}
}
```

Update all call sites of `attachWeixinBindings` to `attachIMBindings`.

- [ ] **Step 15: Delete old files**

Delete `internal/weixin/bridge.go` and `internal/db/weixin_bindings.go`:

```bash
rm internal/weixin/bridge.go internal/db/weixin_bindings.go
```

- [ ] **Step 16: Verify the build**

Run: `cd /root/agentserver && go build ./...`
Expected: success. Fix any compilation errors.

- [ ] **Step 17: Commit**

```bash
git add -A
git commit -m "feat(server): replace WeixinBridge with unified IMBridge, add Telegram routes"
```

---

## Task 8: Update Sandbox Config

**Files:**
- Modify: `internal/sandbox/config.go`

- [ ] **Step 1: Update Config struct**

In `internal/sandbox/config.go`, replace:

```go
	NanoclawWeixinEnabled    bool
```

with:

```go
	NanoclawIMBridgeEnabled  bool
```

Update the `DefaultConfig` function or wherever `NanoclawWeixinEnabled` is read from env vars. Change the env var name from `NANOCLAW_WEIXIN_ENABLED` to `NANOCLAW_IM_BRIDGE_ENABLED`.

- [ ] **Step 2: Update BuildNanoclawConfig**

In `BuildNanoclawConfig`, update the function signature and body:

Change the parameter name from `weixinBridgeURL` to `bridgeURL`. Update the env var output:

Replace:

```go
	if weixinBridgeURL != "" {
		lines = append(lines, "NANOCLAW_WEIXIN_BRIDGE_URL="+weixinBridgeURL)
	}
```

with:

```go
	if bridgeURL != "" {
		lines = append(lines, "NANOCLAW_BRIDGE_URL="+bridgeURL)
		// Backwards compat (remove after all pods updated)
		lines = append(lines, "NANOCLAW_WEIXIN_BRIDGE_URL="+bridgeURL)
	}
```

- [ ] **Step 3: Update all callers of BuildNanoclawConfig**

Search for `BuildNanoclawConfig` call sites and update any references to the old config field name `NanoclawWeixinEnabled` → `NanoclawIMBridgeEnabled`.

Also update the bridge URL construction. The current URL pattern is:
```
{NANOCLAW_BRIDGE_BASE_URL}/api/internal/nanoclaw/{sandboxID}/weixin/send
```

Change to:
```
{NANOCLAW_BRIDGE_BASE_URL}/api/internal/nanoclaw/{sandboxID}/im/send
```

(The old `/weixin/send` route is kept as an alias in the server, so existing pods will still work.)

- [ ] **Step 4: Verify the build**

Run: `cd /root/agentserver && go build ./...`
Expected: success

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/config.go
git commit -m "feat(config): unify IM bridge config, emit NANOCLAW_BRIDGE_URL"
```

---

## Task 9: Update Frontend API Layer

**Files:**
- Modify: `web/src/lib/api.ts`

- [ ] **Step 1: Add new types and API functions**

Add the `IMBinding` interface and new API functions:

```typescript
export interface IMBinding {
  provider: string
  bot_id: string
  user_id?: string
  bound_at: string
}

export interface TelegramConfigureResult {
  connected: boolean
  bot_id: string
  bot_name: string
}

export async function telegramConfigure(sandboxId: string, botToken: string): Promise<TelegramConfigureResult> {
  const res = await fetch(`/api/sandboxes/${sandboxId}/im/telegram/configure`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ bot_token: botToken }),
  })
  if (!res.ok) {
    const text = await res.text()
    throw new Error(text || 'Failed to configure Telegram bot')
  }
  return res.json()
}

export async function telegramDisconnect(sandboxId: string): Promise<void> {
  const res = await fetch(`/api/sandboxes/${sandboxId}/im/telegram`, { method: 'DELETE' })
  if (!res.ok) throw new Error('Failed to disconnect Telegram')
}

export async function listIMBindings(sandboxId: string): Promise<{ bindings: IMBinding[] }> {
  const res = await fetch(`/api/sandboxes/${sandboxId}/im/bindings`)
  if (!res.ok) throw new Error('Failed to list IM bindings')
  return res.json()
}
```

- [ ] **Step 2: Update Sandbox interface**

Add `im_bindings` to the Sandbox interface:

```typescript
export interface Sandbox {
  // ... existing fields ...
  weixin_bindings?: WeixinBinding[]  // keep for backwards compat
  im_bindings?: IMBinding[]
}
```

- [ ] **Step 3: Update WeChat API paths**

Update `weixinQRStart` and `weixinQRWait` to use the new paths:

```typescript
export async function weixinQRStart(sandboxId: string): Promise<WeixinQRStartResult> {
  const res = await fetch(`/api/sandboxes/${sandboxId}/im/weixin/qr-start`, { method: 'POST' })
  if (!res.ok) throw new Error('Failed to start WeChat login')
  return res.json()
}

export async function weixinQRWait(sandboxId: string): Promise<WeixinQRWaitResult> {
  const res = await fetch(`/api/sandboxes/${sandboxId}/im/weixin/qr-wait`, { method: 'POST' })
  if (!res.ok) throw new Error('Failed to poll WeChat login status')
  return res.json()
}
```

- [ ] **Step 4: Commit**

```bash
git add web/src/lib/api.ts
git commit -m "feat(web): add Telegram API functions, update WeChat paths"
```

---

## Task 10: Create TelegramConfigModal Component

**Files:**
- Create: `web/src/components/TelegramConfigModal.tsx`

- [ ] **Step 1: Create the component**

```tsx
import { useState } from 'react'
import { X, Bot, Loader2, CheckCircle2 } from 'lucide-react'
import { telegramConfigure } from '../lib/api'

interface TelegramConfigModalProps {
  sandboxId: string
  onClose: () => void
  onConnected: () => void
}

export function TelegramConfigModal({ sandboxId, onClose, onConnected }: TelegramConfigModalProps) {
  const [botToken, setBotToken] = useState('')
  const [status, setStatus] = useState<'idle' | 'loading' | 'connected' | 'error'>('idle')
  const [error, setError] = useState('')
  const [botName, setBotName] = useState('')

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!botToken.trim()) return

    setStatus('loading')
    setError('')
    try {
      const result = await telegramConfigure(sandboxId, botToken.trim())
      setBotName(result.bot_name || result.bot_id)
      setStatus('connected')
      onConnected()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to configure bot')
      setStatus('error')
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 backdrop-blur-sm">
      <div className="relative w-full max-w-md rounded-xl border border-[var(--border)] bg-[var(--card)] p-6 shadow-2xl">
        <button
          onClick={onClose}
          className="absolute right-4 top-4 text-[var(--muted-foreground)] hover:text-[var(--foreground)]"
        >
          <X size={16} />
        </button>

        <div className="flex items-center gap-2 mb-4">
          <Bot size={20} className="text-blue-400" />
          <h2 className="text-lg font-semibold text-[var(--foreground)]">Configure Telegram Bot</h2>
        </div>

        {status === 'connected' ? (
          <div className="flex flex-col items-center gap-3 py-6">
            <CheckCircle2 size={48} className="text-green-400" />
            <p className="text-sm text-[var(--foreground)]">
              Bot <span className="font-mono font-medium">@{botName}</span> connected successfully
            </p>
          </div>
        ) : (
          <form onSubmit={handleSubmit}>
            <p className="text-xs text-[var(--muted-foreground)] mb-3">
              Enter your Telegram Bot Token from <span className="font-mono">@BotFather</span>.
            </p>
            <input
              type="password"
              value={botToken}
              onChange={(e) => setBotToken(e.target.value)}
              placeholder="123456:ABC-DEF..."
              autoFocus
              disabled={status === 'loading'}
              className="w-full rounded-md border border-[var(--border)] bg-[var(--background)] px-3 py-2 text-sm font-mono text-[var(--foreground)] placeholder:text-[var(--muted-foreground)] focus:outline-none focus:ring-1 focus:ring-[var(--primary)] disabled:opacity-50"
            />
            {error && (
              <p className="mt-2 text-xs text-red-400">{error}</p>
            )}
            <button
              type="submit"
              disabled={!botToken.trim() || status === 'loading'}
              className="mt-4 w-full inline-flex items-center justify-center gap-2 rounded-md bg-blue-600 px-4 py-2 text-sm font-medium text-white hover:bg-blue-500 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
            >
              {status === 'loading' ? (
                <>
                  <Loader2 size={14} className="animate-spin" />
                  Validating...
                </>
              ) : (
                'Connect Bot'
              )}
            </button>
          </form>
        )}
      </div>
    </div>
  )
}
```

- [ ] **Step 2: Commit**

```bash
git add web/src/components/TelegramConfigModal.tsx
git commit -m "feat(web): add TelegramConfigModal component"
```

---

## Task 11: Update SandboxDetail — IM Connections Section

**Files:**
- Modify: `web/src/components/SandboxDetail.tsx`

- [ ] **Step 1: Add imports and state**

Add the import for `TelegramConfigModal` and `IMBinding`:

```typescript
import { TelegramConfigModal } from './TelegramConfigModal'
import { IMBinding, listIMBindings, telegramDisconnect } from '../lib/api'
```

Add new state variable alongside the existing `showWeixinLogin`:

```typescript
const [showTelegramConfig, setShowTelegramConfig] = useState(false)
const [imBindings, setImBindings] = useState<IMBinding[]>(sandbox.im_bindings || sandbox.weixin_bindings?.map(b => ({ ...b, provider: 'weixin' })) || [])
```

- [ ] **Step 2: Update refresh function**

Replace `refreshWeixinBindings` with:

```typescript
const refreshIMBindings = useCallback(() => {
  listIMBindings(sandbox.id)
    .then((r) => setImBindings(r.bindings || []))
    .catch(() => {})
}, [sandbox.id])
```

Update the useEffect that calls it to use `refreshIMBindings` instead of `refreshWeixinBindings`.

- [ ] **Step 3: Update action buttons**

Replace the single "WeChat" button (lines 241-249) with two buttons:

```tsx
{(isOpenClaw || isNanoClaw) && isRunning && (
  <>
    <button
      onClick={() => setShowWeixinLogin(true)}
      className="inline-flex items-center gap-1.5 rounded-md border border-green-500/30 bg-green-500/10 px-3 py-1.5 text-xs font-medium text-green-400 hover:bg-green-500/20 transition-colors"
    >
      <MessageSquare size={13} />
      WeChat
    </button>
    {isNanoClaw && (
      <button
        onClick={() => setShowTelegramConfig(true)}
        className="inline-flex items-center gap-1.5 rounded-md border border-blue-500/30 bg-blue-500/10 px-3 py-1.5 text-xs font-medium text-blue-400 hover:bg-blue-500/20 transition-colors"
      >
        <Bot size={13} />
        Telegram
      </button>
    )}
  </>
)}
```

Add `Bot` to the lucide-react import.

- [ ] **Step 4: Update OverviewTab — Replace WeChat Bindings with IM Connections**

Replace the "WeChat Bindings" section (lines 449-472) with:

```tsx
{(isOpenClaw || isNanoClaw) && imBindings.length > 0 && (
  <div className="rounded-lg border border-[var(--border)] bg-[var(--card)]">
    <div className="flex items-center gap-2 border-b border-[var(--border)] px-5 py-3">
      <MessageSquare size={14} className="text-green-400" />
      <span className="text-sm font-medium text-[var(--foreground)]">IM Connections</span>
    </div>
    <div className="divide-y divide-[var(--border)]">
      {imBindings.map((b, i) => (
        <div key={i} className="flex items-center justify-between px-5 py-3">
          <div className="flex items-center gap-3">
            <span className={`inline-flex items-center rounded px-1.5 py-0.5 text-[10px] font-medium ${
              b.provider === 'telegram'
                ? 'bg-blue-500/10 text-blue-400'
                : 'bg-green-500/10 text-green-400'
            }`}>
              {b.provider === 'telegram' ? 'Telegram' : 'WeChat'}
            </span>
            <div className="flex flex-col gap-0.5">
              <span className="text-xs font-mono text-[var(--foreground)]">{b.bot_id}</span>
              {b.user_id && (
                <span className="text-[11px] text-[var(--muted-foreground)]">user: {b.user_id}</span>
              )}
            </div>
          </div>
          <span className="text-xs text-[var(--muted-foreground)]">
            {new Date(b.bound_at).toLocaleString()}
          </span>
        </div>
      ))}
    </div>
  </div>
)}
```

Pass `imBindings` to `OverviewTab` instead of `weixinBindings`.

- [ ] **Step 5: Add TelegramConfigModal render**

After the `WeixinLoginModal` render (line 329-331), add:

```tsx
{showTelegramConfig && (
  <TelegramConfigModal
    sandboxId={sandbox.id}
    onClose={() => setShowTelegramConfig(false)}
    onConnected={() => refreshIMBindings()}
  />
)}
```

- [ ] **Step 6: Verify frontend build**

Run: `cd /root/agentserver/web && npm run build`
Expected: success

- [ ] **Step 7: Commit**

```bash
git add web/src/components/SandboxDetail.tsx
git commit -m "feat(web): add IM Connections section with Telegram support"
```

---

## Task 12: Build and Verify

**Files:** (none — verification only)

- [ ] **Step 1: Run full Go build**

Run: `cd /root/agentserver && go build ./...`
Expected: success, no errors

- [ ] **Step 2: Run Go vet**

Run: `cd /root/agentserver && go vet ./...`
Expected: no warnings

- [ ] **Step 3: Run frontend build**

Run: `cd /root/agentserver/web && npm run build`
Expected: success

- [ ] **Step 4: Verify migration file is well-formed**

Run: `cat internal/db/migrations/010_unified_im_bindings.sql`
Expected: Valid SQL with no syntax errors

- [ ] **Step 5: Final commit with built frontend**

```bash
git add web/dist/
git commit -m "build: rebuild frontend with IM bridge UI"
```

---

## Implementation Notes

### Migration Strategy (from spec)
1. **Deploy agentserver first** — new unified tables + routes. Old routes kept as aliases. Existing WeChat bindings migrated to `sandbox_im_bindings` automatically. Old tables retained.
2. **Deploy nanoclaw second** — new image with telegram channel + bridge-server. Reads `NANOCLAW_BRIDGE_URL` (new) with fallback to `NANOCLAW_WEIXIN_BRIDGE_URL` (old).
3. **After all pods updated** — remove legacy route aliases and `NANOCLAW_WEIXIN_BRIDGE_URL` env var.
4. **After stable** — drop old `sandbox_weixin_bindings` and `weixin_context_tokens` tables.

### NanoClaw Changes (out of scope for this plan)
The spec describes changes to nanoclaw's TypeScript code (`bridge-server.ts`, `telegram/index.ts`, etc.). These should be planned and implemented separately in the nanoclaw repository. The agentserver changes in this plan are fully self-contained and work with the existing nanoclaw images via the legacy aliases.

### JID Backwards Compatibility
- New messages use suffixed JIDs: `{fromUserID}@im.wechat`, `{chatID}@tg`
- The `handleNanoclawIMSend` handler falls back to weixin provider if no JID suffix matches, supporting legacy unsuffixed JIDs from existing nanoclaw pods.
