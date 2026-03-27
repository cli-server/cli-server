# Matrix Channel Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Matrix as a third IM channel alongside WeChat and Telegram in the nanoclaw IM bridge, using the mautrix/go SDK.

**Architecture:** Matrix follows the same Provider interface pattern as Telegram — a `MatrixProvider` implements `Provider` + `TypingProvider`, backed by a `matrix_api.go` client that wraps the mautrix/go SDK. Configuration is done via an HTTP endpoint that accepts a homeserver URL and access token. The mautrix `Client.Sync()` loop is adapted into the polling model by using short-lived syncs with a `since` token as the cursor.

**Tech Stack:** Go, mautrix/go (`maunium.net/go/mautrix`), existing imbridge Provider interface

---

## File Structure

| File | Action | Responsibility |
|------|--------|----------------|
| `internal/imbridge/matrix_api.go` | Create | Matrix Client-Server API wrapper using mautrix/go — sync, send text, send image, typing indicators, whoami validation |
| `internal/imbridge/matrix_provider.go` | Create | `MatrixProvider` struct implementing `Provider` + `TypingProvider` interfaces |
| `internal/imbridge/matrix_api_test.go` | Create | Tests for matrix API functions |
| `internal/imbridge/matrix_provider_test.go` | Create | Tests for MatrixProvider.Poll and MatrixProvider.Send |
| `internal/server/server.go` | Modify | Register MatrixProvider, add configure/disconnect routes, add matrix case in media send handler |
| `go.mod` / `go.sum` | Modify | Add `maunium.net/go/mautrix` dependency |

**No database migration needed** — the existing `sandbox_im_bindings` and `im_provider_meta` tables are provider-agnostic and work with any provider name.

---

### Task 1: Add mautrix/go Dependency

**Files:**
- Modify: `go.mod`

- [ ] **Step 1: Add the mautrix/go module**

```bash
cd /root/agentserver && go get maunium.net/go/mautrix@latest
```

- [ ] **Step 2: Tidy modules**

```bash
cd /root/agentserver && go mod tidy
```

- [ ] **Step 3: Verify the dependency resolves**

```bash
cd /root/agentserver && go list maunium.net/go/mautrix
```

Expected: `maunium.net/go/mautrix` (no errors)

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum
git commit -m "feat: add mautrix/go SDK dependency for Matrix channel"
```

---

### Task 2: Matrix API Client (`matrix_api.go`)

**Files:**
- Create: `internal/imbridge/matrix_api.go`
- Create: `internal/imbridge/matrix_api_test.go`

This file wraps the mautrix/go SDK into standalone functions matching the pattern used by `telegram_api.go` — stateless functions that take credentials as parameters. We use the mautrix `Client` internally but create a fresh instance per call to remain stateless (matching the polling architecture).

- [ ] **Step 1: Write the test file with basic tests**

Create `internal/imbridge/matrix_api_test.go`:

```go
package imbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestMatrixWhoami(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v3/account/whoami" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token" {
			t.Errorf("unexpected auth header: %s", auth)
		}
		json.NewEncoder(w).Encode(map[string]string{
			"user_id":   "@bot:example.com",
			"device_id": "DEVICE1",
		})
	}))
	defer srv.Close()

	userID, err := MatrixWhoami(context.Background(), srv.URL, "test-token")
	if err != nil {
		t.Fatalf("MatrixWhoami error: %v", err)
	}
	if userID != "@bot:example.com" {
		t.Errorf("expected @bot:example.com, got %s", userID)
	}
}

func TestMatrixSendText(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && r.URL.Path[:len("/_matrix/client/v3/rooms/")] == "/_matrix/client/v3/rooms/" {
			called.Store(true)
			json.NewEncoder(w).Encode(map[string]string{"event_id": "$abc123"})
			return
		}
		// whoami for client init
		if r.URL.Path == "/_matrix/client/v3/account/whoami" {
			json.NewEncoder(w).Encode(map[string]string{"user_id": "@bot:example.com"})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	err := MatrixSendText(context.Background(), srv.URL, "test-token", "!room:example.com", "hello")
	if err != nil {
		t.Fatalf("MatrixSendText error: %v", err)
	}
	if !called.Load() {
		t.Error("send endpoint was not called")
	}
}

func TestMatrixSync(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_matrix/client/v3/sync" {
			resp := map[string]interface{}{
				"next_batch": "s123_456",
				"rooms": map[string]interface{}{
					"join": map[string]interface{}{
						"!room:example.com": map[string]interface{}{
							"timeline": map[string]interface{}{
								"events": []map[string]interface{}{
									{
										"type":    "m.room.message",
										"sender":  "@user:example.com",
										"event_id": "$evt1",
										"origin_server_ts": 1700000000000,
										"content": map[string]interface{}{
											"msgtype": "m.text",
											"body":    "hello bot",
										},
									},
								},
							},
						},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		if r.URL.Path == "/_matrix/client/v3/account/whoami" {
			json.NewEncoder(w).Encode(map[string]string{"user_id": "@bot:example.com"})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	messages, nextBatch, err := MatrixSync(context.Background(), srv.URL, "test-token", "", 0)
	if err != nil {
		t.Fatalf("MatrixSync error: %v", err)
	}
	if nextBatch != "s123_456" {
		t.Errorf("expected next_batch s123_456, got %s", nextBatch)
	}
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0].Text != "hello bot" {
		t.Errorf("expected text 'hello bot', got %q", messages[0].Text)
	}
	if messages[0].RoomID != "!room:example.com" {
		t.Errorf("expected room !room:example.com, got %s", messages[0].RoomID)
	}
	if messages[0].SenderID != "@user:example.com" {
		t.Errorf("expected sender @user:example.com, got %s", messages[0].SenderID)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd /root/agentserver && go test ./internal/imbridge/ -run TestMatrix -v
```

Expected: compilation errors (functions not defined)

- [ ] **Step 3: Implement `matrix_api.go`**

Create `internal/imbridge/matrix_api.go`:

```go
package imbridge

import (
	"context"
	"fmt"
	"log"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// MatrixMessage represents an incoming Matrix message extracted from a sync response.
type MatrixMessage struct {
	RoomID    string
	EventID   string
	SenderID  string
	Text      string
	Timestamp int64
}

// newMatrixClient creates a mautrix Client pointed at the given homeserver with the given access token.
// It does NOT call Sync — callers use it for one-shot API calls.
func newMatrixClient(homeserverURL, accessToken string) (*mautrix.Client, error) {
	cli, err := mautrix.NewClient(homeserverURL, "", "")
	if err != nil {
		return nil, fmt.Errorf("matrix: create client: %w", err)
	}
	cli.AccessToken = accessToken
	return cli, nil
}

// MatrixWhoami validates an access token and returns the user ID.
func MatrixWhoami(ctx context.Context, homeserverURL, accessToken string) (string, error) {
	cli, err := newMatrixClient(homeserverURL, accessToken)
	if err != nil {
		return "", err
	}
	resp, err := cli.Whoami(ctx)
	if err != nil {
		return "", fmt.Errorf("matrix whoami: %w", err)
	}
	return resp.UserID.String(), nil
}

// MatrixSync performs a single /sync request and returns parsed messages and the next_batch token.
// If since is empty, this is an initial sync (the caller should discard historical messages).
// timeoutSec controls the server-side long-poll timeout (0 for immediate return).
func MatrixSync(ctx context.Context, homeserverURL, accessToken, since string, timeoutSec int) ([]MatrixMessage, string, error) {
	cli, err := newMatrixClient(homeserverURL, accessToken)
	if err != nil {
		return nil, "", err
	}

	// Set a client-side timeout slightly longer than the server-side timeout.
	clientTimeout := time.Duration(timeoutSec+10) * time.Second
	if timeoutSec == 0 {
		clientTimeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, clientTimeout)
	defer cancel()

	// Build filter to only get message events.
	filter := &mautrix.Filter{
		Room: mautrix.RoomFilter{
			Timeline: mautrix.FilterPart{
				Types: []event.Type{event.EventMessage},
			},
		},
	}
	filterJSON, _ := filter.MarshalJSON()
	filterStr := string(filterJSON)

	resp, err := cli.SyncRequest(ctx, 0, since, filterStr, false, event.PresenceOffline, timeoutSec*1000)
	if err != nil {
		return nil, "", fmt.Errorf("matrix sync: %w", err)
	}

	// Determine our own user ID so we can skip our own messages.
	whoamiResp, err := cli.Whoami(ctx)
	var botUserID id.UserID
	if err == nil {
		botUserID = whoamiResp.UserID
	}

	var messages []MatrixMessage
	for roomID, roomData := range resp.Rooms.Join {
		for _, evt := range roomData.Timeline.Events {
			// Skip our own messages.
			if evt.Sender == botUserID {
				continue
			}
			if evt.Type != event.EventMessage {
				continue
			}
			err := evt.Content.ParseRaw(evt.Type)
			if err != nil {
				log.Printf("matrix: failed to parse event content in %s: %v", roomID, err)
				continue
			}
			content := evt.Content.AsMessage()
			if content == nil {
				continue
			}
			// Only handle text messages for now.
			if content.MsgType != event.MsgText && content.MsgType != event.MsgNotice && content.MsgType != event.MsgEmote {
				continue
			}
			messages = append(messages, MatrixMessage{
				RoomID:    roomID.String(),
				EventID:   evt.ID.String(),
				SenderID:  evt.Sender.String(),
				Text:      content.Body,
				Timestamp: evt.Timestamp,
			})
		}
	}

	// Also check invited rooms and auto-join them.
	for roomID := range resp.Rooms.Invite {
		go func(rid id.RoomID) {
			joinCtx, joinCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer joinCancel()
			if _, err := cli.JoinRoomByID(joinCtx, rid); err != nil {
				log.Printf("matrix: failed to auto-join room %s: %v", rid, err)
			} else {
				log.Printf("matrix: auto-joined room %s", rid)
			}
		}(roomID)
	}

	return messages, resp.NextBatch, nil
}

// MatrixSendText sends a text message to a Matrix room.
func MatrixSendText(ctx context.Context, homeserverURL, accessToken, roomID, text string) error {
	cli, err := newMatrixClient(homeserverURL, accessToken)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	_, err = cli.SendMessageEvent(ctx, id.RoomID(roomID), event.EventMessage, &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    text,
	})
	if err != nil {
		return fmt.Errorf("matrix send text: %w", err)
	}
	return nil
}

// MatrixSendImage uploads an image to the homeserver's media repo and sends it to a room.
func MatrixSendImage(ctx context.Context, homeserverURL, accessToken, roomID string, imageData []byte, caption string) error {
	cli, err := newMatrixClient(homeserverURL, accessToken)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// Upload image to Matrix content repository.
	uploadResp, err := cli.UploadBytes(ctx, imageData, "image/png")
	if err != nil {
		return fmt.Errorf("matrix upload image: %w", err)
	}

	// Send image message event.
	body := "image.png"
	if caption != "" {
		body = caption
	}
	_, err = cli.SendMessageEvent(ctx, id.RoomID(roomID), event.EventMessage, &event.MessageEventContent{
		MsgType: event.MsgImage,
		Body:    body,
		URL:     uploadResp.ContentURI.CUString(),
		Info: &event.FileInfo{
			MimeType: "image/png",
			Size:     len(imageData),
		},
	})
	if err != nil {
		return fmt.Errorf("matrix send image: %w", err)
	}

	// Send caption as separate text if both image and caption are provided.
	if caption != "" {
		_, _ = cli.SendMessageEvent(ctx, id.RoomID(roomID), event.EventMessage, &event.MessageEventContent{
			MsgType: event.MsgText,
			Body:    caption,
		})
	}

	return nil
}

// MatrixSendTyping sends a typing indicator to a room.
func MatrixSendTyping(ctx context.Context, homeserverURL, accessToken, roomID string, typing bool, timeoutMs int64) error {
	cli, err := newMatrixClient(homeserverURL, accessToken)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	whoamiResp, err := cli.Whoami(ctx)
	if err != nil {
		return fmt.Errorf("matrix typing whoami: %w", err)
	}

	req := mautrix.ReqTyping{
		Typing:  typing,
		Timeout: timeoutMs,
	}
	_, err = cli.UserTyping(ctx, id.RoomID(roomID), whoamiResp.UserID, typing, req.Timeout)
	if err != nil {
		return fmt.Errorf("matrix send typing: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests**

```bash
cd /root/agentserver && go test ./internal/imbridge/ -run TestMatrix -v
```

Expected: all 3 tests PASS. If `MatrixSendTyping` or `MatrixSendImage` have compilation issues due to mautrix API differences, fix the API calls to match the actual SDK signatures.

- [ ] **Step 5: Fix any SDK API mismatches**

The mautrix/go SDK may have slightly different method signatures than documented. Check:
- `cli.SyncRequest` — verify the parameter order matches the SDK version
- `cli.UserTyping` — verify it exists; if not, use raw HTTP: `PUT /_matrix/client/v3/rooms/{roomId}/typing/{userId}`
- `cli.UploadBytes` — verify it exists; the method might be `cli.Upload` with an `io.Reader`
- `uploadResp.ContentURI.CUString()` — verify this accessor exists

Adjust the code to match the actual SDK API. Run `go vet ./internal/imbridge/` to catch issues.

- [ ] **Step 6: Commit**

```bash
git add internal/imbridge/matrix_api.go internal/imbridge/matrix_api_test.go
git commit -m "feat: add Matrix API client using mautrix/go SDK"
```

---

### Task 3: Matrix Provider (`matrix_provider.go`)

**Files:**
- Create: `internal/imbridge/matrix_provider.go`
- Create: `internal/imbridge/matrix_provider_test.go`

- [ ] **Step 1: Write the test file**

Create `internal/imbridge/matrix_provider_test.go`:

```go
package imbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMatrixProviderName(t *testing.T) {
	p := &MatrixProvider{}
	if p.Name() != "matrix" {
		t.Errorf("expected 'matrix', got %q", p.Name())
	}
}

func TestMatrixProviderJIDSuffix(t *testing.T) {
	p := &MatrixProvider{}
	if p.JIDSuffix() != "@matrix" {
		t.Errorf("expected '@matrix', got %q", p.JIDSuffix())
	}
}

func TestMatrixProviderPoll(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_matrix/client/v3/account/whoami" {
			json.NewEncoder(w).Encode(map[string]string{"user_id": "@bot:example.com"})
			return
		}
		if r.URL.Path == "/_matrix/client/v3/sync" {
			callCount++
			resp := map[string]interface{}{
				"next_batch": "batch_2",
				"rooms": map[string]interface{}{
					"join": map[string]interface{}{
						"!room:example.com": map[string]interface{}{
							"timeline": map[string]interface{}{
								"events": []map[string]interface{}{
									{
										"type":    "m.room.message",
										"sender":  "@alice:example.com",
										"event_id": "$msg1",
										"origin_server_ts": 1700000000000,
										"content": map[string]interface{}{
											"msgtype": "m.text",
											"body":    "test message",
										},
									},
								},
							},
						},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	p := &MatrixProvider{}
	creds := &Credentials{
		SandboxID: "sandbox-1",
		BotID:     "@bot:example.com",
		BotToken:  "test-token",
		BaseURL:   srv.URL,
	}

	result, err := p.Poll(context.Background(), creds, "batch_1")
	if err != nil {
		t.Fatalf("Poll error: %v", err)
	}
	if result.NewCursor != "batch_2" {
		t.Errorf("expected cursor batch_2, got %s", result.NewCursor)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result.Messages))
	}
	msg := result.Messages[0]
	if msg.Text != "test message" {
		t.Errorf("expected text 'test message', got %q", msg.Text)
	}
	// FromUserID should be the room ID (used as chat JID)
	if msg.FromUserID != "!room:example.com" {
		t.Errorf("expected FromUserID !room:example.com, got %s", msg.FromUserID)
	}
	if msg.SenderName != "@alice:example.com" {
		t.Errorf("expected SenderName @alice:example.com, got %s", msg.SenderName)
	}
}

func TestMatrixProviderSend(t *testing.T) {
	sent := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			sent = true
			json.NewEncoder(w).Encode(map[string]string{"event_id": "$sent1"})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	p := &MatrixProvider{}
	creds := &Credentials{
		SandboxID: "sandbox-1",
		BotID:     "@bot:example.com",
		BotToken:  "test-token",
		BaseURL:   srv.URL,
	}

	err := p.Send(context.Background(), creds, "!room:example.com", "reply text", nil)
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if !sent {
		t.Error("send endpoint was not called")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd /root/agentserver && go test ./internal/imbridge/ -run TestMatrixProvider -v
```

Expected: compilation errors (MatrixProvider not defined)

- [ ] **Step 3: Implement `matrix_provider.go`**

Create `internal/imbridge/matrix_provider.go`:

```go
package imbridge

import (
	"context"
	"log"
	"time"
)

const (
	matrixSyncTimeoutSec      = 30
	matrixTypingKeepaliveMs   = 10000 // typing indicator timeout in ms
	matrixTypingKeepalive     = 5 * time.Second
	matrixTypingTotalTimeout  = 5 * time.Minute
)

// MatrixProvider implements Provider and TypingProvider for Matrix via mautrix/go.
type MatrixProvider struct{}

func (p *MatrixProvider) Name() string      { return "matrix" }
func (p *MatrixProvider) JIDSuffix() string { return "@matrix" }

// Poll performs a /sync request against the Matrix homeserver.
// The cursor is the `since` (next_batch) token from the previous sync.
// On the initial call (cursor == ""), we do an immediate sync (timeout=0) to get
// the initial next_batch token without processing old messages.
func (p *MatrixProvider) Poll(ctx context.Context, creds *Credentials, cursor string) (*PollResult, error) {
	timeoutSec := matrixSyncTimeoutSec
	isInitial := cursor == ""
	if isInitial {
		timeoutSec = 0 // initial sync: return immediately to get next_batch
	}

	messages, nextBatch, err := MatrixSync(ctx, creds.BaseURL, creds.BotToken, cursor, timeoutSec)
	if err != nil {
		return nil, err
	}

	var msgs []InboundMessage
	// On initial sync, skip all messages (they're historical).
	if !isInitial {
		for _, m := range messages {
			msgs = append(msgs, InboundMessage{
				FromUserID: m.RoomID,
				SenderName: m.SenderID,
				Text:       m.Text,
				IsGroup:    true, // Matrix rooms are always group-like
				Metadata: map[string]string{
					"room_id":  m.RoomID,
					"event_id": m.EventID,
				},
			})
		}
	}

	return &PollResult{Messages: msgs, NewCursor: nextBatch}, nil
}

// Send sends a text message to a Matrix room.
// The toUserID is actually the room ID (e.g. "!abc:example.com").
func (p *MatrixProvider) Send(ctx context.Context, creds *Credentials, toUserID, text string, meta map[string]string) error {
	return MatrixSendText(ctx, creds.BaseURL, creds.BotToken, toUserID, text)
}

// StartTyping implements TypingProvider for Matrix.
// Sends typing indicators every 5s until cancelled or timed out (5min).
func (p *MatrixProvider) StartTyping(ctx context.Context, creds *Credentials, userID string, meta map[string]string,
	sendError func(text string)) (cancel func()) {

	ctx, cancelFn := context.WithTimeout(ctx, matrixTypingTotalTimeout)

	go func() {
		defer cancelFn()

		// Determine the room ID. userID is the room ID for Matrix.
		roomID := userID

		// Send initial typing indicator.
		if err := MatrixSendTyping(ctx, creds.BaseURL, creds.BotToken, roomID, true, matrixTypingKeepaliveMs); err != nil {
			log.Printf("imbridge: matrix typing failed for %s: %v", roomID, err)
		}

		ticker := time.NewTicker(matrixTypingKeepalive)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				// Stop typing (best-effort).
				bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = MatrixSendTyping(bgCtx, creds.BaseURL, creds.BotToken, roomID, false, 0)
				bgCancel()

				if ctx.Err() == context.DeadlineExceeded {
					sendError("⚠️ Message processing timed out. Please try again later.")
				}
				return
			case <-ticker.C:
				if err := MatrixSendTyping(ctx, creds.BaseURL, creds.BotToken, roomID, true, matrixTypingKeepaliveMs); err != nil {
					log.Printf("imbridge: matrix typing keepalive failed for %s: %v", roomID, err)
				}
			}
		}
	}()

	return cancelFn
}
```

- [ ] **Step 4: Run tests**

```bash
cd /root/agentserver && go test ./internal/imbridge/ -run TestMatrixProvider -v
```

Expected: all 4 tests PASS

- [ ] **Step 5: Commit**

```bash
git add internal/imbridge/matrix_provider.go internal/imbridge/matrix_provider_test.go
git commit -m "feat: add MatrixProvider implementing Provider and TypingProvider"
```

---

### Task 4: Register Matrix Provider in Server

**Files:**
- Modify: `internal/server/server.go:105-108` (provider registration)

- [ ] **Step 1: Add MatrixProvider to the provider list**

In `internal/server/server.go`, find the provider registration at line 105-108:

```go
s.IMBridge = imbridge.NewBridge(database, sandboxStore, execCmd, []imbridge.Provider{
	&imbridge.WeixinProvider{},
	&imbridge.TelegramProvider{},
})
```

Change to:

```go
s.IMBridge = imbridge.NewBridge(database, sandboxStore, execCmd, []imbridge.Provider{
	&imbridge.WeixinProvider{},
	&imbridge.TelegramProvider{},
	&imbridge.MatrixProvider{},
})
```

- [ ] **Step 2: Verify compilation**

```bash
cd /root/agentserver && go build ./...
```

Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add internal/server/server.go
git commit -m "feat: register MatrixProvider in IM bridge"
```

---

### Task 5: Add Matrix Configure/Disconnect HTTP Handlers

**Files:**
- Modify: `internal/server/server.go` (add routes and handlers)

- [ ] **Step 1: Add routes**

In `internal/server/server.go`, find the IM Bridge routes section (around line 241-249):

```go
// IM Bridge routes (unified)
r.Post("/api/sandboxes/{id}/im/weixin/qr-start", s.handleIMWeixinQRStart)
r.Post("/api/sandboxes/{id}/im/weixin/qr-wait", s.handleIMWeixinQRWait)
r.Post("/api/sandboxes/{id}/im/telegram/configure", s.handleIMTelegramConfigure)
r.Delete("/api/sandboxes/{id}/im/telegram", s.handleIMTelegramDisconnect)
r.Get("/api/sandboxes/{id}/im/bindings", s.handleListIMBindings)
```

Add after the telegram routes:

```go
r.Post("/api/sandboxes/{id}/im/matrix/configure", s.handleIMMatrixConfigure)
r.Delete("/api/sandboxes/{id}/im/matrix", s.handleIMMatrixDisconnect)
```

- [ ] **Step 2: Add the configure handler**

Add the following handler function after the `handleIMTelegramDisconnect` function (after line 2341):

```go
// handleIMMatrixConfigure configures a Matrix bot for a nanoclaw sandbox.
func (s *Server) handleIMMatrixConfigure(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "matrix binding is only available for nanoclaw sandboxes", http.StatusBadRequest)
		return
	}
	if sbx.Status != "running" {
		http.Error(w, "sandbox is not running", http.StatusConflict)
		return
	}

	var req struct {
		HomeserverURL string `json:"homeserver_url"`
		AccessToken   string `json:"access_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.HomeserverURL == "" {
		http.Error(w, "homeserver_url is required", http.StatusBadRequest)
		return
	}
	if req.AccessToken == "" {
		http.Error(w, "access_token is required", http.StatusBadRequest)
		return
	}

	userID, err := imbridge.MatrixWhoami(r.Context(), req.HomeserverURL, req.AccessToken)
	if err != nil {
		log.Printf("matrix configure: whoami failed: %v", err)
		http.Error(w, "invalid credentials: "+err.Error(), http.StatusBadRequest)
		return
	}

	botID := userID
	if err := s.DB.CreateIMBinding(id, "matrix", botID, ""); err != nil {
		log.Printf("matrix configure: create binding: %v", err)
		http.Error(w, "failed to save binding", http.StatusInternalServerError)
		return
	}
	if err := s.DB.SaveIMCredentials(id, "matrix", botID, req.AccessToken, req.HomeserverURL); err != nil {
		log.Printf("matrix configure: save credentials: %v", err)
		http.Error(w, "failed to save credentials", http.StatusInternalServerError)
		return
	}

	if sbx.PodIP != "" && s.IMBridge != nil {
		s.IMBridge.StartPoller(imbridge.BridgeBinding{
			Provider:     &imbridge.MatrixProvider{},
			Credentials:  imbridge.Credentials{SandboxID: id, BotID: botID, BotToken: req.AccessToken, BaseURL: req.HomeserverURL},
			Cursor:       "",
			BridgeSecret: sbx.NanoclawBridgeSecret,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"connected": true,
		"bot_id":    botID,
		"user_id":   userID,
	})
}
```

- [ ] **Step 3: Add the disconnect handler**

Add the following handler function after `handleIMMatrixConfigure`:

```go
// handleIMMatrixDisconnect disconnects a Matrix bot from a sandbox.
func (s *Server) handleIMMatrixDisconnect(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sbx, ok := s.Sandboxes.Get(id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if _, ok := s.requireWorkspaceMember(w, r, sbx.WorkspaceID); !ok {
		return
	}

	bindings, err := s.DB.ListIMBindings(id, "matrix")
	if err != nil || len(bindings) == 0 {
		http.Error(w, "no matrix binding found", http.StatusNotFound)
		return
	}

	for _, b := range bindings {
		if s.IMBridge != nil {
			s.IMBridge.StopPoller(id, "matrix", b.BotID)
		}
		if err := s.DB.DeleteIMBinding(id, "matrix", b.BotID); err != nil {
			log.Printf("matrix disconnect: delete binding: %v", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "disconnected"})
}
```

- [ ] **Step 4: Verify compilation**

```bash
cd /root/agentserver && go build ./...
```

Expected: no errors

- [ ] **Step 5: Commit**

```bash
git add internal/server/server.go
git commit -m "feat: add Matrix configure/disconnect HTTP handlers"
```

---

### Task 6: Add Matrix Image Sending to IM Send Handler

**Files:**
- Modify: `internal/server/server.go` (handleNanoclawIMSend, around line 2191-2225)

- [ ] **Step 1: Add the matrix case in the media switch**

In `handleNanoclawIMSend`, find the media sending switch (around line 2191):

```go
if len(mediaData) > 0 {
	switch provider.Name() {
	case "weixin":
		// ... existing WeChat code ...
	case "telegram":
		// ... existing Telegram code ...
	default:
		http.Error(w, "image sending not supported for provider: "+provider.Name(), http.StatusBadRequest)
		return
	}
```

Add a `case "matrix":` before the `default:`:

```go
	case "matrix":
		// Matrix: upload to content repository → send m.image event
		if err := imbridge.MatrixSendImage(r.Context(), baseURL, botToken, userID, mediaData, reqMeta.Text); err != nil {
			log.Printf("nanoclaw im send image: failed sandbox=%s to=%s: %v", sandboxID, userID, err)
			http.Error(w, "failed to send image: "+err.Error(), http.StatusBadGateway)
			return
		}
```

- [ ] **Step 2: Verify compilation**

```bash
cd /root/agentserver && go build ./...
```

Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add internal/server/server.go
git commit -m "feat: add Matrix image sending in IM send handler"
```

---

### Task 7: Integration Verification

**Files:** (no changes — verification only)

- [ ] **Step 1: Run all tests**

```bash
cd /root/agentserver && go test ./internal/imbridge/ -v
```

Expected: all tests pass (existing telegram tests + new matrix tests)

- [ ] **Step 2: Run full build**

```bash
cd /root/agentserver && go build ./...
```

Expected: clean build

- [ ] **Step 3: Run vet**

```bash
cd /root/agentserver && go vet ./internal/imbridge/ ./internal/server/
```

Expected: no issues

- [ ] **Step 4: Verify JID routing works**

Mentally verify the routing chain:
1. Matrix messages come in with `FromUserID = "!room:example.com"` (room ID)
2. The JID format becomes `!room:example.com@matrix` in the bridge
3. `FindProviderByJID("!room:example.com@matrix")` matches `@matrix` suffix → returns `MatrixProvider`
4. The send handler strips the suffix and sends to `!room:example.com`

Wait — review the actual code: `FromUserID` in `InboundMessage` is set to the raw room ID `!room:example.com`, and the bridge uses it as-is. Check if the bridge appends JIDSuffix anywhere. Looking at `bridge.go:248-249`, the `forwardToNanoClaw` uses `msg.FromUserID` directly as `chat_jid` and `sender`. The NanoClaw pod stores this JID and uses it when sending back. The send handler at `server.go:2160` calls `FindProviderByJID(reqMeta.ToUserID)` — this means the `to_user_id` from NanoClaw MUST end with `@matrix` for routing to work.

**Important:** The bridge does NOT automatically append JIDSuffix to the user ID. Some providers handle this differently:
- WeChat: user IDs from iLink already contain a domain-like suffix
- Telegram: user IDs are numeric chat IDs — but the existing code appears to work because it falls back to WeixinProvider when no suffix matches

For Matrix, we need the room ID to be routed correctly. The cleanest approach: append `@matrix` suffix in the provider's Poll so that the JID sent to NanoClaw includes the suffix, and strip it in Send.

- [ ] **Step 5: Fix JID handling in MatrixProvider**

Update `matrix_provider.go` Poll method — change `FromUserID`:

```go
FromUserID: m.RoomID + "@matrix",
```

Update `matrix_provider.go` Send method — strip suffix:

```go
func (p *MatrixProvider) Send(ctx context.Context, creds *Credentials, toUserID, text string, meta map[string]string) error {
	roomID := strings.TrimSuffix(toUserID, "@matrix")
	return MatrixSendText(ctx, creds.BaseURL, creds.BotToken, roomID, text)
}
```

Add `"strings"` to imports.

Similarly update `StartTyping`:

```go
roomID := strings.TrimSuffix(userID, "@matrix")
```

And update the metadata in Poll:

```go
Metadata: map[string]string{
	"room_id":  m.RoomID,
	"event_id": m.EventID,
},
```

And update the image sending case in `handleNanoclawIMSend`:

```go
case "matrix":
	roomID := strings.TrimSuffix(userID, "@matrix")
	if err := imbridge.MatrixSendImage(r.Context(), baseURL, botToken, roomID, mediaData, reqMeta.Text); err != nil {
```

- [ ] **Step 6: Update tests to match JID format**

In `matrix_provider_test.go`, update the `TestMatrixProviderPoll` assertion:

```go
if msg.FromUserID != "!room:example.com@matrix" {
	t.Errorf("expected FromUserID !room:example.com@matrix, got %s", msg.FromUserID)
}
```

- [ ] **Step 7: Run all tests again**

```bash
cd /root/agentserver && go test ./internal/imbridge/ -v
```

Expected: all tests PASS

- [ ] **Step 8: Run full build**

```bash
cd /root/agentserver && go build ./...
```

Expected: clean build

- [ ] **Step 9: Final commit**

```bash
git add internal/imbridge/matrix_provider.go internal/imbridge/matrix_provider_test.go internal/server/server.go
git commit -m "fix: Matrix JID routing with @matrix suffix"
```

---

## Design Decisions

### Why Room ID as JID (not user ID)?

Matrix is room-based, not user-based. A bot talks to users inside rooms, not directly. Using the room ID as the "chat JID" means:
- 1:1 DMs work (room with 2 members)
- Group chats work (room with N members)
- It matches how NanoClaw already handles groups

### Why Stateless Client per Call?

The existing architecture (Telegram, WeChat) uses stateless API calls per-poll. Mautrix's `Client.Sync()` is designed for long-running connections, but we adapt it by calling `SyncRequest()` directly with the `since` token as our cursor. This keeps Matrix consistent with the other providers.

### Why Auto-Join on Invite?

Matrix bots must explicitly join rooms. Auto-joining on invite mirrors how most Matrix bots behave and ensures the bot is reachable when users invite it.

### Credential Mapping

| Field | Telegram | Matrix |
|-------|----------|--------|
| `BotID` | Bot username (`@BotFather_bot`) | User ID (`@bot:example.com`) |
| `BotToken` | Bot API token | Access token |
| `BaseURL` | `https://api.telegram.org` | Homeserver URL (`https://matrix.example.com`) |

### What About Encryption (E2EE)?

This implementation does NOT support end-to-end encrypted rooms. Mautrix supports E2EE via the `crypto` module, but it requires persistent state (Olm sessions), which doesn't fit the stateless-per-call model. This can be added later as a Task if needed.
