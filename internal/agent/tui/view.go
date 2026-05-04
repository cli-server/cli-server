// internal/agent/tui/view.go
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/agentserver/agentserver/internal/agent"
)

// RenderView produces the full screen text for the Model. Layout:
//
//	[viewport (scrolling timeline; replaced by welcome banner if empty + LoggedOut)]
//	[activePanel (optional)]
//	[input box]
//	[status bar 1 line]
//	[hint line (LoggedOut only)]
func RenderView(m *Model) string {
	var sb strings.Builder

	if m.timeline.Len() == 0 && m.authState == AuthLoggedOut {
		sb.WriteString(renderWelcome(m))
	} else {
		sb.WriteString(m.viewport.View())
	}
	sb.WriteByte('\n')

	if m.activePanel != nil {
		sb.WriteString(m.activePanel.View(m.viewport.Width))
		sb.WriteByte('\n')
	}

	sb.WriteString(renderInput(m))
	sb.WriteByte('\n')
	sb.WriteString(renderStatusBar(m))

	if m.authState == AuthLoggedOut {
		sb.WriteByte('\n')
		sb.WriteString(StyleInputHint.Render("  Type /login to authenticate  ·  /quit to exit"))
	}
	return sb.String()
}

// renderStatusBar produces a single line of color-coded status pills:
//
//	✻ agentserver  ws/abcd  cwd/.../foo  cse/wxyz  ⏵ idle  ▼ online  𝓂 sonnet
func renderStatusBar(m *Model) string {
	var parts []string
	parts = append(parts, StyleWelcomeTitle.Render(" ✻ agentserver"))
	if v := emptyDash(short(m.cfg.WorkspaceID, 8)); v != "—" {
		parts = append(parts, kv("ws", v, StyleStatusValue))
	}
	if v := emptyDash(shortPath(m.cwd)); v != "—" {
		parts = append(parts, kv("cwd", v, StyleStatusValue))
	}
	if v := emptyDash(short(m.sessionID, 8)); v != "—" {
		parts = append(parts, kv("cse", v, StyleStatusValue))
	}
	parts = append(parts, kv("turn", m.statusTurn, turnStyle(m.statusTurn)))
	parts = append(parts, kv("tunnel", m.statusTunnel, tunnelStyle(m.statusTunnel)))
	if m.model != "" {
		parts = append(parts, kv("model", m.model, StyleStatusValue))
	}
	if m.authState != AuthLoggedIn {
		parts = append(parts, kv("auth", m.authState.String(), authStyle(m.authState)))
	}
	return strings.Join(parts, StyleStatusSep.String())
}

func renderInput(m *Model) string {
	style := StyleInputBox
	if m.authState == AuthLoggedIn {
		style = StyleInputBoxFocus
	}
	return style.Render(strings.TrimRight(m.input.View(), "\n"))
}

func renderWelcome(m *Model) string {
	title := fmt.Sprintf("✻ Welcome to agentserver tui  v%s", agent.Version)
	body := []string{
		"",
		"This is the local TUI for cc-broker — your harness lives remotely;",
		"this terminal is just I/O and a permission gate.",
		"",
		"  /login      sign in via OAuth Device Flow",
		"  /quit       exit",
		"",
		"After login, type a message to start a turn.",
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorAccent).
		Padding(1, 2).
		Width(min(72, m.viewport.Width-2))
	content := StyleWelcomeTitle.Render(title) + "\n" +
		StyleWelcomeBody.Render(strings.Join(body, "\n"))
	return box.Render(content)
}

// kv renders "label/value" with the value styled and the label muted.
func kv(label, value string, valStyle lipgloss.Style) string {
	return StyleStatusLabel.Render(label+"/") + valStyle.Render(value)
}

func tunnelStyle(s string) lipgloss.Style {
	switch s {
	case "online", "connected":
		return StyleStatusOk
	case "reconnecting":
		return StyleStatusWarn
	case "offline", "disconnected":
		return StyleStatusBad
	default:
		return StyleStatusLabel
	}
}

func turnStyle(s string) lipgloss.Style {
	switch s {
	case "running":
		return StyleStatusWarn
	case "cancelling":
		return StyleStatusBad
	default:
		return StyleStatusValue
	}
}

func authStyle(a AuthState) lipgloss.Style {
	switch a {
	case AuthLoggedIn:
		return StyleStatusOk
	case AuthLoggingIn, AuthRefreshing:
		return StyleStatusWarn
	default:
		return StyleStatusBad
	}
}

// short returns id truncated to n runes (no ellipsis to keep the bar tight).
func short(id string, n int) string {
	if len(id) <= n {
		return id
	}
	return id[:n]
}

// shortPath collapses a long path: …/parent/child for depths > 2.
func shortPath(p string) string {
	if p == "" {
		return ""
	}
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) <= 2 {
		return p
	}
	return ".../" + strings.Join(parts[len(parts)-2:], "/")
}

func emptyDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func min(a, b int) int {
	if b < a {
		return b
	}
	return a
}
