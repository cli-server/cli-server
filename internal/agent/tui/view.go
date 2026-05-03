// internal/agent/tui/view.go
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// RenderView produces the full screen text for the Model. Layout:
//
//	[statusBar 2 lines]
//	[viewport (scrolling timeline)]
//	[activePanel (optional)]
//	[input area + LoggedOut hint]
func RenderView(m *Model) string {
	var sb strings.Builder
	sb.WriteString(renderStatusBar(m))
	sb.WriteByte('\n')
	sb.WriteString(m.viewport.View())
	sb.WriteByte('\n')
	if m.activePanel != nil {
		sb.WriteString(m.activePanel.View(m.viewport.Width))
		sb.WriteByte('\n')
	}
	sb.WriteString(renderInput(m))
	return sb.String()
}

func renderStatusBar(m *Model) string {
	line1 := fmt.Sprintf(" session: %s · cwd: %s · server: %s ",
		emptyDash(m.sessionID), emptyDash(m.cwd), emptyDash(m.cfg.ServerURL))
	line2 := fmt.Sprintf(" auth: %s · tunnel: %s · events: %s · turn: %s · model: %s ",
		m.authState, m.statusTunnel, m.statusEvents, m.statusTurn, emptyDash(m.model))
	style := StyleStatusBar
	if m.authState == AuthLoggedOut {
		style = StyleStatusBarErr
	}
	return style.Render(line1) + "\n" + style.Render(line2)
}

func renderInput(m *Model) string {
	if m.authState == AuthLoggedOut {
		hint := lipgloss.NewStyle().Foreground(lipgloss.Color("#FF7A7A")).Render(
			"Use /login to authenticate")
		return lipgloss.JoinVertical(lipgloss.Left, hint, m.input.View())
	}
	return m.input.View()
}

func emptyDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
