// internal/agent/tui/view_test.go
package tui

import (
	"strings"
	"testing"
)

func TestRenderView_LoggedOut_HasLoginHint(t *testing.T) {
	m := newTestModel(t)
	m.SetAuthState(AuthLoggedOut)
	m.viewport.Width = 80
	m.viewport.Height = 10
	out := RenderView(m)
	if !strings.Contains(out, "/login") {
		t.Errorf("missing /login hint: %s", out)
	}
}

func TestRenderView_LoggedIn_ShowsStatusBar(t *testing.T) {
	m := newTestModel(t)
	m.SetAuthState(AuthLoggedIn)
	m.sessionID = "cse_x"
	m.cwd = "/home/me"
	m.statusTunnel = "online"
	m.statusTurn = "idle"
	m.viewport.Width = 80
	m.viewport.Height = 10
	out := RenderView(m)
	for _, want := range []string{"cse_x", "/home/me", "online", "idle"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in view:\n%s", want, out)
		}
	}
}

func TestRenderView_PermissionPanelVisible(t *testing.T) {
	m := newTestModel(t)
	m.SetAuthState(AuthLoggedIn)
	m.activePanel = NewPermissionPanel(PermissionPanelInput{
		PID:        "p1",
		Tool:       "remote_bash",
		ExecutorID: "e",
		SelfExecID: "e",
		Args:       []byte(`{}`),
	})
	m.mode = ModeAwaitPerm
	m.viewport.Width = 80
	m.viewport.Height = 10
	out := RenderView(m)
	if !strings.Contains(out, "p1") || !strings.Contains(out, "remote_bash") {
		t.Errorf("panel not in view: %s", out)
	}
}
