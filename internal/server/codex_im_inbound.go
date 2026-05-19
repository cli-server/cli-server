package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/agentserver/agentserver/internal/db"
)

// codexInboundHandler routes inbound WeChat messages destined for the
// codex routing path. POST /api/internal/imbridge/codex/turn body is:
//
//	{
//	  "channel_id": "ch-xxx",
//	  "workspace_id": "ws-xxx",
//	  "wechat_user_id": "wxid_xxx",
//	  "text": "..."
//	}
//
// Returns 202 immediately and processes the codex turn in a goroutine.
// Task 14 wraps this with a per-(channel,user) FIFO dispatcher; this
// task ships the bare path so end-to-end works for one in-flight
// request per user.
type codexInboundHandler struct {
	codex           codexCaller
	sessions        sessionStore
	imbridgeSendURL string
	internalSecret  string
	dispatcher      *codexDispatcher
}

type codexCaller interface {
	RunTurn(ctx context.Context, req CodexTurnRequest) (*CodexTurnResponse, error)
}

// sessionStore is what the handler needs from the DB. Defined as an
// interface so tests can inject fakes without a real *sql.DB. The
// production adapter (Task 15) wraps *db.DB.
type sessionStore interface {
	GetSessionByExternalID(ctx context.Context, workspaceID, externalID string) (sessionView, error)
	SetSessionCodexThreadID(ctx context.Context, sessionID string, threadID *string) error
	CreateSession(ctx context.Context, workspaceID, externalID, title, imChannelID string) (sessionView, error)
}

// sessionView is the subset of agent_sessions fields the codex handler
// needs. Decoupled from db.AgentSession to keep test fakes small.
type sessionView struct {
	ID            string
	CodexThreadID *string
}

type codexInboundRequest struct {
	ChannelID    string `json:"channel_id"`
	WorkspaceID  string `json:"workspace_id"`
	WechatUserID string `json:"wechat_user_id"`
	WechatSender string `json:"wechat_sender_name,omitempty"`
	Text         string `json:"text"`
	QuotedText   string `json:"quoted_text,omitempty"`
	QuotedSender string `json:"quoted_sender,omitempty"`
}

func (h *codexInboundHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req codexInboundRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.ChannelID == "" || req.WorkspaceID == "" || req.WechatUserID == "" {
		http.Error(w, "channel_id, workspace_id, wechat_user_id required", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"queued":true}`))
	h.dispatcher.Enqueue(req)
}

func (h *codexInboundHandler) processTurn(ctx context.Context, req codexInboundRequest) {
	// Issue 1: use WechatUserID directly — bridge already sets it from
	// msg.FromUserID (bare wxid_xxx, same convention as stateless_cc's
	// chat_jid). Appending "@im.wechat" caused every lookup to miss.
	externalID := req.WechatUserID
	sess, err := h.sessions.GetSessionByExternalID(ctx, req.WorkspaceID, externalID)
	if err != nil {
		log.Printf("codex_im: resolve session channel=%s user=%s: %v", req.ChannelID, externalID, err)
		h.sendError(ctx, req, "⚠️ 内部错误，请重试")
		return
	}
	// Issue 2: create session on first contact (mirror stateless_cc pattern).
	if sess.ID == "" {
		title := "IM: " + req.WechatSender
		if title == "IM: " {
			title = "IM: " + req.WechatUserID
		}
		sess, err = h.sessions.CreateSession(ctx, req.WorkspaceID, externalID, title, req.ChannelID)
		if err != nil {
			log.Printf("codex_im: create session channel=%s user=%s: %v", req.ChannelID, externalID, err)
			h.sendError(ctx, req, "⚠️ 内部错误，请重试")
			return
		}
	}

	params := buildCodexInput(req)
	cresp, err := h.codex.RunTurn(ctx, CodexTurnRequest{
		WorkspaceID: req.WorkspaceID,
		ThreadID:    sess.CodexThreadID,
		Params:      params,
	})
	if err != nil {
		log.Printf("codex_im: cxg call: %v", err)
		h.sendError(ctx, req, "⚠️ Codex 处理失败，请稍后重试")
		return
	}

	// Transport-layer failure.
	if cresp.Transport != nil {
		h.sendError(ctx, req, transportToUserMessage(cresp.Transport))
		return
	}

	// Persist thread id if new or changed.
	if cresp.ThreadID != "" && (sess.CodexThreadID == nil || *sess.CodexThreadID != cresp.ThreadID) {
		tid := cresp.ThreadID
		if err := h.sessions.SetSessionCodexThreadID(ctx, sess.ID, &tid); err != nil {
			log.Printf("codex_im: persist thread id: %v", err)
		}
	}

	// Decode turn.status / items / error.
	var turn struct {
		Status string            `json:"status"`
		Items  []json.RawMessage `json:"items"`
		Error  *struct {
			Message        string  `json:"message"`
			CodexErrorInfo *string `json:"codexErrorInfo,omitempty"`
		} `json:"error"`
	}
	if err := json.Unmarshal(cresp.Turn, &turn); err != nil {
		log.Printf("codex_im: decode turn: %v", err)
		h.sendError(ctx, req, "⚠️ Codex 返回格式异常")
		return
	}

	switch turn.Status {
	case "completed":
		text := lastAgentMessageText(turn.Items)
		if text == "" {
			h.sendError(ctx, req, "⚠️ Codex 没有返回文本内容")
			return
		}
		h.sendText(ctx, req, text)
	case "failed":
		if turn.Error != nil && turn.Error.CodexErrorInfo != nil {
			switch *turn.Error.CodexErrorInfo {
			case "contextWindowExceeded":
				_ = h.sessions.SetSessionCodexThreadID(ctx, sess.ID, nil)
				h.sendError(ctx, req, "⚠️ 上下文已满，请新开会话")
				return
			case "usageLimitExceeded":
				h.sendError(ctx, req, "⚠️ Codex 配额已用尽")
				return
			case "serverOverloaded":
				h.sendError(ctx, req, "⚠️ Codex 繁忙，请稍后重试")
				return
			}
		}
		// Heuristic: thread-not-found.
		msg := ""
		if turn.Error != nil {
			msg = turn.Error.Message
		}
		lo := strings.ToLower(msg)
		if strings.Contains(lo, "thread") && (strings.Contains(lo, "not found") || strings.Contains(lo, "unknown") || strings.Contains(lo, "missing")) {
			_ = h.sessions.SetSessionCodexThreadID(ctx, sess.ID, nil)
			h.sendError(ctx, req, "⚠️ 会话已重置，请重发消息")
			return
		}
		log.Printf("codex_im: turn failed: %s", msg)
		h.sendError(ctx, req, "⚠️ Codex 处理失败")
	case "interrupted":
		h.sendError(ctx, req, "⚠️ 处理已取消，请重发")
	default:
		log.Printf("codex_im: unexpected status %q", turn.Status)
		h.sendError(ctx, req, "⚠️ Codex 返回异常状态")
	}
}

// lastAgentMessageText scans the items list in reverse for the last
// {type:"agentMessage"} entry and returns its text. Returns "" if none.
func lastAgentMessageText(items []json.RawMessage) string {
	for i := len(items) - 1; i >= 0; i-- {
		var shell struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(items[i], &shell); err != nil {
			continue
		}
		if shell.Type == "agentMessage" && shell.Text != "" {
			return shell.Text
		}
	}
	return ""
}

func transportToUserMessage(t *CodexTransportError) string {
	switch t.Code {
	case "brokerTimeout":
		return "⚠️ 处理超时，请稍后重试"
	default:
		return "⚠️ Codex 处理失败，请稍后重试"
	}
}

// buildCodexInput constructs the codex turn/start params.input from the
// inbound WeChat message. MVP: text only. Quoted text is concatenated
// into the same text item with a "引用:" prefix; image/media ignored.
func buildCodexInput(req codexInboundRequest) json.RawMessage {
	text := req.Text
	if req.QuotedText != "" {
		quoter := req.QuotedSender
		if quoter == "" {
			quoter = "之前的消息"
		}
		text = fmt.Sprintf("[引用 %s] %s\n%s", quoter, req.QuotedText, req.Text)
	}
	wrapped := map[string]any{
		"input": []map[string]any{
			{"type": "text", "text": text},
		},
	}
	b, _ := json.Marshal(wrapped)
	return b
}

// sendText / sendError both POST /api/internal/imbridge/send. The
// endpoint's StopTyping side-effect kicks in automatically.

func (h *codexInboundHandler) sendText(ctx context.Context, req codexInboundRequest, text string) {
	h.postSend(ctx, map[string]any{
		"channel_id": req.ChannelID,
		"to_user_id": req.WechatUserID,
		"text":       text,
	})
}

func (h *codexInboundHandler) sendError(ctx context.Context, req codexInboundRequest, text string) {
	h.postSend(ctx, map[string]any{
		"channel_id": req.ChannelID,
		"to_user_id": req.WechatUserID,
		"text":       text,
	})
}

func (h *codexInboundHandler) postSend(ctx context.Context, body map[string]any) {
	b, _ := json.Marshal(body)
	r, err := http.NewRequestWithContext(ctx, "POST", h.imbridgeSendURL+"/api/internal/imbridge/send", bytes.NewReader(b))
	if err != nil {
		log.Printf("codex_im: build send req: %v", err)
		return
	}
	r.Header.Set("Content-Type", "application/json")
	if h.internalSecret != "" {
		r.Header.Set("X-Internal-Secret", h.internalSecret)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		log.Printf("codex_im: send POST: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("codex_im: send status=%d body=%s", resp.StatusCode, body)
	}
}

// newCodexInboundHandler wires up the handler with its dispatcher
// already running. Cap is the per-(channel,user) queue depth — past
// cap, drop-oldest applies.
func newCodexInboundHandler(codex codexCaller, sessions sessionStore, imbridgeSendURL, internalSecret string) *codexInboundHandler {
	h := &codexInboundHandler{
		codex:           codex,
		sessions:        sessions,
		imbridgeSendURL: imbridgeSendURL,
		internalSecret:  internalSecret,
	}
	h.dispatcher = newCodexDispatcher(func(req codexInboundRequest) {
		h.processTurn(context.Background(), req)
	}, 5)
	return h
}

// --- per-(channel,user) FIFO dispatcher ---

type codexDispatcher struct {
	processFn func(codexInboundRequest)
	cap       int

	mu      sync.Mutex
	workers map[string]*dispatcherSlot
	stopped bool
}

type dispatcherSlot struct {
	ch    chan codexInboundRequest
	ready chan struct{}
}

func newCodexDispatcher(processFn func(codexInboundRequest), cap int) *codexDispatcher {
	return &codexDispatcher{
		processFn: processFn,
		cap:       cap,
		workers:   make(map[string]*dispatcherSlot),
	}
}

func dispatcherKey(req codexInboundRequest) string {
	return req.ChannelID + ":" + req.WechatUserID
}

// Enqueue adds req to the per-key channel. If the channel is full,
// drains the oldest queued item to make room (drop-oldest policy).
// Starts a worker for this key if none is running.
//
// When a new worker is spawned the first item is placed on the channel and
// Enqueue then blocks on <-slot.ready until the worker has dequeued it.
// This ensures subsequent Enqueues always observe an empty channel rather
// than racing to evict the first item via drop-oldest.
func (d *codexDispatcher) Enqueue(req codexInboundRequest) {
	key := dispatcherKey(req)
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	slot, ok := d.workers[key]
	if !ok {
		slot = &dispatcherSlot{
			ch:    make(chan codexInboundRequest, d.cap),
			ready: make(chan struct{}),
		}
		d.workers[key] = slot
		slot.ch <- req // buffered, never blocks (fresh channel, cap >= 1)
		go d.runWorker(key, slot)
		d.mu.Unlock()
		// Block until the worker has dequeued the first item. The
		// closed-channel receive is a Go memory-model happens-before barrier;
		// no runtime.Gosched advisory yield needed.
		<-slot.ready
		return
	}
	d.mu.Unlock()

	for {
		select {
		case slot.ch <- req:
			return
		default:
			// Full — drop oldest then retry.
			select {
			case <-slot.ch:
			default:
			}
		}
	}
}

func (d *codexDispatcher) runWorker(key string, slot *dispatcherSlot) {
	// Dequeue and signal the first item separately so Enqueue's
	// <-slot.ready barrier fires as soon as the item is out of the
	// channel, not after the full processFn call returns.
	first, ok := <-slot.ch
	close(slot.ready) // unblock the spawning Enqueue
	if !ok {
		return
	}
	d.processFn(first)
	// Issue 4: no idle-timeout exit. Workers persist for the process
	// lifetime; Stop() closes all channels and exits via range below.
	// The idle-cleanup approach had a race: between Enqueue dropping the
	// lock and the channel-send the worker could exit, leaving a message
	// in a dead channel. Memory cost is O(active conversations) — bounded
	// by pool idle-reap of upstream codex connections, so fine in practice.
	for req := range slot.ch {
		d.processFn(req)
	}
	_ = key // keep parameter for symmetry with future debug logging
}

func (d *codexDispatcher) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	d.stopped = true
	for _, slot := range d.workers {
		close(slot.ch)
	}
	d.workers = nil
}

// Close stops the FIFO dispatcher. Safe to call multiple times.
// In-flight worker goroutines complete their current task then exit.
// Call from the agentserver shutdown sequence.
func (h *codexInboundHandler) Close() {
	h.dispatcher.Stop()
}

// dbSessionStore is the production sessionStore that reads/writes the
// real agent_sessions table.
type dbSessionStore struct {
	db *db.DB
}

func (s *dbSessionStore) GetSessionByExternalID(ctx context.Context, workspaceID, externalID string) (sessionView, error) {
	sess, err := s.db.GetSessionByExternalID(ctx, workspaceID, externalID)
	if err != nil {
		return sessionView{}, err
	}
	if sess == nil {
		// Not found — return empty sessionView so caller can create.
		return sessionView{}, nil
	}
	return sessionView{ID: sess.ID, CodexThreadID: sess.CodexThreadID}, nil
}

func (s *dbSessionStore) SetSessionCodexThreadID(ctx context.Context, sessionID string, threadID *string) error {
	return s.db.SetSessionCodexThreadID(ctx, sessionID, threadID)
}

func (s *dbSessionStore) CreateSession(ctx context.Context, workspaceID, externalID, title, imChannelID string) (sessionView, error) {
	sessionID := "cse_" + uuid.NewString()
	if err := s.db.CreateAgentSession(sessionID, nil, workspaceID, title, nil); err != nil {
		return sessionView{}, fmt.Errorf("create session: %w", err)
	}
	if err := s.db.SetSessionExternalID(ctx, sessionID, externalID); err != nil {
		return sessionView{}, fmt.Errorf("set external_id: %w", err)
	}
	if imChannelID != "" {
		if err := s.db.SetSessionIMChannel(ctx, sessionID, imChannelID); err != nil {
			// Non-fatal — log only (matches im_inbound.go pattern).
			log.Printf("codex_im: failed to set im_channel_id for session %s: %v", sessionID, err)
		}
	}
	return sessionView{ID: sessionID, CodexThreadID: nil}, nil
}
