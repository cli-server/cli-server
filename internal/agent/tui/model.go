// internal/agent/tui/model.go
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// Mode tracks which input handler owns the keypress stream right now.
type Mode int

const (
	ModeNormal       Mode = iota // input box has focus
	ModeAwaitPerm                // permission panel is up
	ModeAwaitAskUser             // ask_user panel is up
	ModeAwaitLogin               // OAuth Device Flow panel is up
	ModeAwaitLogout              // logout confirmation panel is up
	ModeCommand                  // slash command palette
	ModeAttachPicker             // file picker for /attach
	ModeQuitting
)

type ModelConfig struct {
	ServerURL    string
	WorkspaceID  string
	ExecutorID   string
	Bus          *Bus
	Auth         *AuthController
	Yolo         bool
	InitialModel string
	Resume       string
	Continue     bool

	// OnLoggedIn fires when AuthState transitions to LoggedIn. Used to
	// start ExecutorClient + register executor (lazily, post-login).
	// nil if caller doesn't need this signal.
	OnLoggedIn func()

	// OnSessionReady fires whenever the model's sessionID becomes known
	// or changes (Resume on Init, NewSessionReplyMsg, InboundAcceptedMsg
	// for first-prompt-creates-session, ResumeRequestedMsg). Used to start
	// / restart the SSE consumer goroutine. May be called multiple times;
	// implementations should cancel any previous consumer before starting
	// a new one.
	OnSessionReady func(sessionID string)
}

type Model struct {
	cfg  ModelConfig
	bus  *Bus
	auth *AuthController

	mode         Mode
	authState    AuthState
	sessionID    string
	turnID       string
	cwd          string
	model        string
	permMode     string
	statusTunnel string // online | reconnecting | offline | unknown
	statusEvents string // live | reconnecting | delayed
	statusTurn   string // idle | running | cancelling
	statusAuth   string // mirrors authState.String() with optional decoration

	timeline *Timeline
	viewport viewport.Model
	input    textarea.Model
	keys     KeyMap

	activePanel Panel
	permQueue   []Panel
	askQueue    []Panel

	pendingAttachments []InboundAttachment

	// Test seam: Model uses startLoginFn instead of auth.StartLogin so
	// tests can stub the entire login flow without a real auth controller.
	// nil → use auth.StartLogin.
	startLoginFn func() tea.Cmd
}

func NewModel(cfg ModelConfig) *Model {
	ta := textarea.New()
	ta.Placeholder = "Type a message…"
	ta.SetHeight(3)
	ta.Focus()
	vp := viewport.New(80, 20)
	return &Model{
		cfg:          cfg,
		bus:          cfg.Bus,
		auth:         cfg.Auth,
		timeline:     NewTimeline(5000),
		viewport:     vp,
		input:        ta,
		keys:         NewKeyMap(),
		statusTunnel: "unknown",
		statusEvents: "live",
		statusTurn:   "idle",
		model:        cfg.InitialModel,
	}
}

func (m *Model) SetAuthState(s AuthState) {
	m.authState = s
	if s == AuthLoggedIn {
		m.input.Focus()
	}
}

func (m *Model) InputEnabled() bool {
	return m.authState == AuthLoggedIn || m.authState == AuthRefreshing
}

func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{textarea.Blink}
	if m.auth != nil {
		m.SetAuthState(m.auth.State())
	}
	if m.authState == AuthLoggedIn {
		// Already logged in at startup — fire OnLoggedIn so tui_run.go can
		// start the executor. (AuthStateChangedMsg won't fire because state
		// didn't "change"; it was already LoggedIn.)
		if m.cfg.OnLoggedIn != nil {
			m.cfg.OnLoggedIn()
		}
		cmds = append(cmds, m.startSessionCmds()...)
	}
	return tea.Batch(cmds...)
}

func (m *Model) startSessionCmds() []tea.Cmd {
	var out []tea.Cmd
	if m.cfg.Resume != "" {
		m.sessionID = m.cfg.Resume
		if m.cfg.OnSessionReady != nil {
			m.cfg.OnSessionReady(m.sessionID)
		}
		out = append(out, m.attachAndSubscribe(m.sessionID))
	} else if m.cfg.Continue {
		out = append(out, m.continueLatestCmd())
	}
	out = append(out, m.statusTickCmd())
	return out
}

func (m *Model) statusTickCmd() tea.Cmd {
	return tea.Tick(10*time.Second, func(t time.Time) tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		st, err := m.bus.FetchExecutorStatus(ctx)
		return StatusTickMsg{Tunnel: st, Err: err}
	})
}

func (m *Model) continueLatestCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		list, err := m.bus.ListSessions(ctx)
		if err != nil || len(list) == 0 {
			return ListSessionsReplyMsg{Err: err}
		}
		return ResumeRequestedMsg{SessionID: list[0].SessionID}
	}
}

func (m *Model) attachAndSubscribe(sid string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := m.bus.AttachSession(ctx, sid, "operator")
		return AttachReplyMsg{Resp: resp, Err: err}
	}
}

// Update dispatches a Msg to the right handler. The shape:
//
//  1. EventArrivedMsg / AuthStateChangedMsg / panel keys / Send*Msg are
//     handled regardless of mode.
//  2. CommandSelectedMsg routes through runCommand (LocalClass /
//     SessionClass / RemoteClass).
//  3. Plain KeyMsg in ModeNormal goes to handleNormalKey, then falls
//     through to the textarea if not handled.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ev, ok := msg.(EventArrivedMsg); ok {
		m.timeline.Append(ev.Event)
		m.viewport.SetContent(m.timeline.Render(m.viewport.Width, m.cfg.ExecutorID))
		m.viewport.GotoBottom()
		if cmd := m.maybeOpenPanelForEvent(ev.Event); cmd != nil {
			return m, cmd
		}
		return m, nil
	}
	if a, ok := msg.(AuthStateChangedMsg); ok {
		m.SetAuthState(a.State)
		if a.State == AuthLoggedIn {
			if m.cfg.OnLoggedIn != nil {
				m.cfg.OnLoggedIn()
			}
			if m.mode == ModeAwaitLogin {
				m.mode = ModeNormal
				m.activePanel = nil
				return m, tea.Batch(m.startSessionCmds()...)
			}
		}
		return m, nil
	}

	// Panel-driven input
	if m.activePanel != nil {
		if k, ok := msg.(tea.KeyMsg); ok {
			np, cmd, dismissed := m.activePanel.HandleKey(k)
			m.activePanel = np
			if dismissed {
				m.activePanel = nil
				m.popPanelQueue()
				if m.activePanel == nil {
					m.mode = ModeNormal
				}
			}
			return m, cmd
		}
	}

	switch v := msg.(type) {
	case DeviceCodeReadyMsg:
		m.activePanel = NewLoginPanel(v.Info)
		m.mode = ModeAwaitLogin
		return m, nil
	case SendDecisionMsg:
		sid := m.sessionID
		bus := m.bus
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := bus.PostDecision(ctx, sid, v.PID, v.Verdict, v.Scope)
			return DecisionAckMsg{PermissionID: v.PID, Err: err}
		}
	case ConfirmLogoutMsg:
		auth := m.auth
		return m, func() tea.Msg {
			err := auth.Logout()
			return LogoutDoneMsg{Err: err}
		}
	case CancelLoginMsg:
		if m.auth != nil {
			m.auth.CancelLogin()
		}
		m.mode = ModeNormal
		m.activePanel = nil
		return m, nil
	case CommandSelectedMsg:
		return m.runCommand(v)
	case ResumeRequestedMsg:
		m.sessionID = v.SessionID
		return m, m.attachAndSubscribe(v.SessionID)
	case StatusTickMsg:
		if v.Tunnel != nil {
			m.statusTunnel = v.Tunnel.Status
		} else if v.Err != nil {
			m.statusTunnel = "unknown"
		}
		return m, m.statusTickCmd()
	case AttachReplyMsg:
		if v.Err == nil && v.Resp != nil {
			// OnSessionReady was already called in startSessionCmds for the
			// Resume path. For dynamic attach (take-control/observe), the
			// sessionID is unchanged so no restart is needed.
		}
		return m, nil
	case NewSessionReplyMsg:
		if v.Err == nil && v.SessionID != "" {
			m.sessionID = v.SessionID
			if m.cfg.OnSessionReady != nil {
				m.cfg.OnSessionReady(v.SessionID)
			}
		}
		return m, nil
	case InboundAcceptedMsg:
		m.turnID = v.TurnID
		m.statusTurn = "running"
		if v.SessionID != "" && v.SessionID != m.sessionID {
			// First prompt created a session implicitly.
			m.sessionID = v.SessionID
			if m.cfg.OnSessionReady != nil {
				m.cfg.OnSessionReady(v.SessionID)
			}
		}
		return m, nil
	case InboundRejectedMsg:
		// Render an error in timeline so user sees it.
		return m, nil
	case SendAnswerMsg:
		// For now just log via timeline; agentserver doesn't yet have a
		// /answers endpoint. v1.x adds the real wire path.
		m.timeline.Append(SSEEvent{
			Type: "ask_user_answered",
			Data: []byte(fmt.Sprintf(`{"qid":%q,"selected":%s}`, v.QID, mustJSONList(v.Selected))),
		})
		m.viewport.SetContent(m.timeline.Render(m.viewport.Width, m.cfg.ExecutorID))
		m.viewport.GotoBottom()
		return m, nil
	case LogoutDoneMsg:
		if v.Err != nil {
			m.timeline.Append(SSEEvent{
				Type: "logout_error",
				Data: []byte(fmt.Sprintf(`{"error":%q}`, v.Err.Error())),
			})
			m.viewport.SetContent(m.timeline.Render(m.viewport.Width, m.cfg.ExecutorID))
			m.viewport.GotoBottom()
		}
		// SetAuthState was already called inside Logout → callback fired.
		return m, nil
	case LoginPollDoneMsg:
		if v.Err != nil {
			m.timeline.Append(SSEEvent{
				Type: "login_failed",
				Data: []byte(fmt.Sprintf(`{"error":%q}`, v.Err.Error())),
			})
			m.viewport.SetContent(m.timeline.Render(m.viewport.Width, m.cfg.ExecutorID))
			m.viewport.GotoBottom()
		}
		return m, nil
	case RequeuePermissionMsg:
		m.permQueue = append(m.permQueue, v.Panel)
		return m, nil
	}

	// Plain key handling in ModeNormal.
	var cmd tea.Cmd
	if k, ok := msg.(tea.KeyMsg); ok && m.mode == ModeNormal {
		if handled, c := m.handleNormalKey(k); handled {
			return m, c
		}
		if m.InputEnabled() {
			m.input, cmd = m.input.Update(msg)
		}
		return m, cmd
	}
	if m.InputEnabled() {
		m.input, cmd = m.input.Update(msg)
	}
	return m, cmd
}

// handleNormalKey returns (true, cmd) if it consumed the keypress; otherwise
// (false, nil) to let the textarea handle it. Cmd is the side-effect of the
// keypress (e.g. POST inbound after Enter).
func (m *Model) handleNormalKey(k tea.KeyMsg) (bool, tea.Cmd) {
	if k.Type != tea.KeyEnter {
		return false, nil
	}
	if !m.InputEnabled() {
		return true, nil
	}
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return true, nil
	}
	m.input.Reset()
	if cmd, ok := ParseSlashCommand(text); ok {
		return true, func() tea.Msg {
			return CommandSelectedMsg{Command: cmd.Name, Args: cmd.Args}
		}
	}
	sid := m.sessionID
	bus := m.bus
	attachments := m.pendingAttachments
	m.pendingAttachments = nil
	return true, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req := InboundRequest{
			SessionID:           sid,
			Text:                text,
			Attachments:         attachments,
			PermissionResponder: true,
		}
		resp, err := bus.PostInbound(ctx, req)
		if err != nil {
			return InboundRejectedMsg{Code: "post_failed", Message: err.Error()}
		}
		return InboundAcceptedMsg{SessionID: resp.SessionID, TurnID: resp.TurnID}
	}
}

func (m *Model) runCommand(c CommandSelectedMsg) (tea.Model, tea.Cmd) {
	parsed, _ := ParseSlashCommand("/" + c.Command + " " + c.Args)
	switch parsed.Class {
	case LocalClass:
		return m.runLocalCommand(parsed.Name, parsed.Args)
	case SessionClass:
		return m.runSessionCommand(parsed.Name, parsed.Args)
	case RemoteClass:
		return m, m.runRemoteCommand(parsed.Name, parsed.Args)
	}
	return m, nil
}

func (m *Model) runLocalCommand(name, args string) (tea.Model, tea.Cmd) {
	switch name {
	case "quit":
		return m, tea.Quit
	case "yolo":
		m.permMode = "bypass"
		sid := m.sessionID
		if sid == "" {
			return m, nil // no session yet; permMode will apply on next session
		}
		bus := m.bus
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			body, err := bus.PostControl(ctx, sid, "permission", map[string]any{"mode": "bypass"})
			return ControlReplyMsg{Command: "permission", Body: body, Err: err}
		}
	case "login":
		if m.startLoginFn != nil {
			return m, m.startLoginFn()
		}
		if m.auth == nil {
			return m, nil
		}
		auth := m.auth
		return m, func() tea.Msg {
			info, err := auth.StartLogin(context.Background())
			if err != nil {
				return LoginPollDoneMsg{Err: err}
			}
			return DeviceCodeReadyMsg{Info: info}
		}
	case "logout":
		m.activePanel = NewLogoutPanel()
		m.mode = ModeAwaitLogout
		return m, nil
	case "cd":
		m.cwd = args
		// T16 wires writeRuntimeCwd; until then this is a no-op stub.
		if err := writeRuntimeCwd(m.cfg.ExecutorID, args); err != nil {
			return m, func() tea.Msg { return FatalErrorMsg{Err: err} }
		}
		return m, nil
	case "help":
		return m, nil // v1 stub; later task adds a help panel
	case "attach":
		if args == "" {
			return m, nil
		}
		a, err := AttachFromPath(args)
		if err != nil {
			return m, func() tea.Msg { return FatalErrorMsg{Err: err} }
		}
		m.pendingAttachments = append(m.pendingAttachments, a)
		return m, func() tea.Msg { return AttachmentPickedMsg{Attachment: a} }
	}
	return m, nil
}

func (m *Model) runSessionCommand(name, args string) (tea.Model, tea.Cmd) {
	switch name {
	case "clear":
		bus := m.bus
		execID := m.cfg.ExecutorID
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sid, err := bus.NewSession(ctx, "ask", execID)
			return NewSessionReplyMsg{SessionID: sid, Err: err}
		}
	case "resume":
		if args == "" {
			return m, nil
		}
		return m, func() tea.Msg { return ResumeRequestedMsg{SessionID: args} }
	case "take-control":
		return m, m.attachAndSubscribe(m.sessionID)
	case "observe":
		sid := m.sessionID
		bus := m.bus
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			resp, err := bus.AttachSession(ctx, sid, "observer")
			return AttachReplyMsg{Resp: resp, Err: err}
		}
	}
	return m, nil
}

func (m *Model) runRemoteCommand(name, args string) tea.Cmd {
	sid := m.sessionID
	bus := m.bus
	body := map[string]any{}
	switch name {
	case "model":
		body["model"] = args
	case "permission":
		body["mode"] = args
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := bus.PostControl(ctx, sid, name, body)
		return ControlReplyMsg{Command: name, Body: out, Err: err}
	}
}

func (m *Model) maybeOpenPanelForEvent(ev SSEEvent) tea.Cmd {
	if ev.Type == "permission_request" {
		var p struct {
			PermissionID string          `json:"permission_id"`
			Tool         string          `json:"tool"`
			ExecutorID   string          `json:"executor_id"`
			Args         json.RawMessage `json:"args"`
		}
		_ = json.Unmarshal(ev.Data, &p)
		panel := NewPermissionPanel(PermissionPanelInput{
			PID:        p.PermissionID,
			Tool:       p.Tool,
			ExecutorID: p.ExecutorID,
			SelfExecID: m.cfg.ExecutorID,
			Args:       p.Args,
		})
		if m.activePanel != nil {
			m.permQueue = append(m.permQueue, panel)
		} else {
			m.activePanel = panel
			m.mode = ModeAwaitPerm
		}
	}
	if ev.Type == "ask_user" {
		var p struct {
			QuestionID  string   `json:"question_id"`
			Question    string   `json:"question"`
			Options     []string `json:"options"`
			MultiSelect bool     `json:"multi_select"`
		}
		_ = json.Unmarshal(ev.Data, &p)
		panel := NewAskUserPanel(AskUserPanelInput{
			QID: p.QuestionID, Question: p.Question, Options: p.Options, MultiSelect: p.MultiSelect,
		})
		if m.activePanel != nil {
			m.askQueue = append(m.askQueue, panel)
		} else {
			m.activePanel = panel
			m.mode = ModeAwaitAskUser
		}
	}
	return nil
}

func (m *Model) popPanelQueue() {
	if len(m.permQueue) > 0 {
		m.activePanel = m.permQueue[0]
		m.permQueue = m.permQueue[1:]
		m.mode = ModeAwaitPerm
		return
	}
	if len(m.askQueue) > 0 {
		m.activePanel = m.askQueue[0]
		m.askQueue = m.askQueue[1:]
		m.mode = ModeAwaitAskUser
		return
	}
}

// View is a thin wrapper. The actual rendering lives in T13.
func (m *Model) View() string {
	return RenderView(m)
}

// mustJSONList marshals a []string to a JSON array string. Never fails for
// plain string slices.
func mustJSONList(ss []string) string {
	b, _ := json.Marshal(ss)
	return string(b)
}
