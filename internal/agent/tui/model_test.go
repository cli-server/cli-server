// internal/agent/tui/model_test.go
package tui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func newTestModel(t *testing.T) *Model {
	t.Helper()
	return NewModel(ModelConfig{
		ServerURL:   "https://example",
		WorkspaceID: "ws",
		ExecutorID:  "exe_a",
		Bus: NewBus(BusConfig{
			ServerURL: "https://example", WorkspaceID: "ws", ExecutorID: "exe_a",
			Auth: &fakeAuth{tk: "t"},
		}),
		Auth: nil,
	})
}

func TestModel_LoggedOut_DisablesInput(t *testing.T) {
	m := newTestModel(t)
	m.SetAuthState(AuthLoggedOut)
	if m.InputEnabled() {
		t.Errorf("input should be disabled when LoggedOut")
	}
}

func TestModel_EventArrived_AppendsToTimeline(t *testing.T) {
	m := newTestModel(t)
	m.SetAuthState(AuthLoggedIn)
	next, _ := m.Update(EventArrivedMsg{Event: SSEEvent{
		Type: "user_message", Data: []byte(`{"text":"hi"}`), LastEventID: "1",
	}})
	m = next.(*Model)
	if m.timeline.Len() != 1 {
		t.Errorf("timeline len=%d want 1", m.timeline.Len())
	}
}

func TestModel_PermissionRequestEvent_OpensPanel(t *testing.T) {
	m := newTestModel(t)
	m.SetAuthState(AuthLoggedIn)
	next, _ := m.Update(EventArrivedMsg{Event: SSEEvent{
		Type: "permission_request",
		Data: []byte(`{"permission_id":"p1","tool":"remote_bash","executor_id":"exe_a","args":{"command":"ls"}}`),
		LastEventID: "1",
	}})
	m = next.(*Model)
	if m.mode != ModeAwaitPerm {
		t.Errorf("mode=%v want AwaitPerm", m.mode)
	}
	if m.activePanel == nil || m.activePanel.ID() != "p1" {
		t.Errorf("panel = %+v", m.activePanel)
	}
}

func TestModel_SendDecisionMsg_ProducesPostCmd(t *testing.T) {
	m := newTestModel(t)
	m.SetAuthState(AuthLoggedIn)
	m.sessionID = "cse_1"
	_, cmd := m.Update(SendDecisionMsg{PID: "p1", Verdict: "allow", Scope: "once"})
	if cmd == nil {
		t.Fatal("expected a Cmd")
	}
}

func TestModel_SlashLogin_StartsLoginWhenLoggedOut(t *testing.T) {
	m := newTestModel(t)
	m.SetAuthState(AuthLoggedOut)
	var started bool
	m.startLoginFn = func() tea.Cmd {
		started = true
		return func() tea.Msg { return DeviceCodeReadyMsg{Info: LoginInfo{UserCode: "X"}} }
	}
	next, cmd := m.Update(CommandSelectedMsg{Command: "login"})
	if !started {
		t.Errorf("startLoginFn not invoked")
	}
	if cmd == nil {
		t.Fatal("expected cmd")
	}
	msg := cmd()
	if _, ok := msg.(DeviceCodeReadyMsg); !ok {
		t.Errorf("cmd → %T want DeviceCodeReadyMsg", msg)
	}
	_ = next
}

func TestModel_DeviceCodeReady_OpensLoginPanel(t *testing.T) {
	m := newTestModel(t)
	m.SetAuthState(AuthLoggingIn)
	next, _ := m.Update(DeviceCodeReadyMsg{Info: LoginInfo{
		UserCode: "AAA", VerifyURL: "https://x", VerifyURLFull: "https://x/full",
	}})
	m = next.(*Model)
	if m.mode != ModeAwaitLogin {
		t.Errorf("mode=%v want AwaitLogin", m.mode)
	}
	if m.activePanel == nil || m.activePanel.ID() != "login" {
		t.Errorf("panel id %v", m.activePanel)
	}
}

func TestModel_AuthStateChanged_LoggedIn_ClearsLoginPanel(t *testing.T) {
	m := newTestModel(t)
	m.SetAuthState(AuthLoggingIn)
	m.activePanel = NewLoginPanel(LoginInfo{UserCode: "X"})
	m.mode = ModeAwaitLogin
	next, _ := m.Update(AuthStateChangedMsg{State: AuthLoggedIn})
	m = next.(*Model)
	if m.mode != ModeNormal {
		t.Errorf("mode=%v want Normal", m.mode)
	}
	if m.activePanel != nil {
		t.Errorf("activePanel should be cleared")
	}
}

func TestModel_OnLoggedIn_FiresOnAuthTransition(t *testing.T) {
	m := newTestModel(t)
	m.SetAuthState(AuthLoggedOut)
	var fired bool
	m.cfg.OnLoggedIn = func() { fired = true }
	m.Update(AuthStateChangedMsg{State: AuthLoggedIn})
	if !fired {
		t.Error("OnLoggedIn should fire on transition to LoggedIn")
	}
}

func TestModel_OnSessionReady_FiresOnInboundAccepted(t *testing.T) {
	m := newTestModel(t)
	m.SetAuthState(AuthLoggedIn)
	var got string
	m.cfg.OnSessionReady = func(sid string) { got = sid }
	m.Update(InboundAcceptedMsg{SessionID: "cse_new", TurnID: "trn_x"})
	if got != "cse_new" {
		t.Errorf("OnSessionReady got %q want cse_new", got)
	}
}

func TestModel_SendAnswerMsg_AppendsToTimeline(t *testing.T) {
	m := newTestModel(t)
	m.SetAuthState(AuthLoggedIn)
	before := m.timeline.Len()
	m.Update(SendAnswerMsg{QID: "q1", Selected: []string{"foo"}})
	if m.timeline.Len() != before+1 {
		t.Errorf("timeline len did not grow: before=%d after=%d", before, m.timeline.Len())
	}
}

func TestModel_LogoutDoneMsg_NoError_NoTimelineEntry(t *testing.T) {
	m := newTestModel(t)
	m.SetAuthState(AuthLoggedIn)
	before := m.timeline.Len()
	m.Update(LogoutDoneMsg{Err: nil})
	if m.timeline.Len() != before {
		t.Errorf("logout success should NOT add timeline entry; before=%d after=%d", before, m.timeline.Len())
	}
}

func TestModel_LogoutDoneMsg_WithError_AppendsErrorEntry(t *testing.T) {
	m := newTestModel(t)
	m.SetAuthState(AuthLoggedIn)
	before := m.timeline.Len()
	m.Update(LogoutDoneMsg{Err: errors.New("boom")})
	if m.timeline.Len() != before+1 {
		t.Errorf("logout error should add timeline entry")
	}
}

func TestModel_YoloCallsPostControl(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		if !strings.HasSuffix(r.URL.Path, "/control") {
			t.Errorf("path %q want /control", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"mode":"bypass"`) {
			t.Errorf("body %s missing mode=bypass", body)
		}
		w.Write([]byte(`{"applied":true,"mode":"bypass"}`))
	}))
	defer srv.Close()
	m := NewModel(ModelConfig{
		ServerURL: srv.URL, WorkspaceID: "ws", ExecutorID: "e",
		Bus: NewBus(BusConfig{ServerURL: srv.URL, WorkspaceID: "ws", ExecutorID: "e", Auth: &fakeAuth{tk: "t"}}),
	})
	m.SetAuthState(AuthLoggedIn)
	m.sessionID = "cse_test"
	_, cmd := m.Update(CommandSelectedMsg{Command: "yolo"})
	if cmd == nil {
		t.Fatal("expected cmd")
	}
	_ = cmd()
	if !hit {
		t.Error("PostControl was not invoked")
	}
	if m.permMode != "bypass" {
		t.Errorf("permMode=%q", m.permMode)
	}
}

// silence unused imports
var _ = strings.Builder{}
var _ = json.RawMessage{}
