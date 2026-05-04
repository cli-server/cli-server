// internal/agent/tui/styles.go
package tui

import "github.com/charmbracelet/lipgloss"

// Color palette. Kept narrow on purpose — terminal themes vary, so we lean on
// adaptive contrast (Faint, lipgloss adaptive) where possible.
var (
	colorAccent = lipgloss.AdaptiveColor{Light: "#7C5CFF", Dark: "#A48BFF"} // brand violet
	colorOk     = lipgloss.AdaptiveColor{Light: "#1F8A4C", Dark: "#5FFF87"} // tunnel online, success
	colorWarn   = lipgloss.AdaptiveColor{Light: "#B97A00", Dark: "#FFD787"} // turn running, reconnecting
	colorBad    = lipgloss.AdaptiveColor{Light: "#B53A3A", Dark: "#FF7A7A"} // logged out, error
	colorMuted  = lipgloss.AdaptiveColor{Light: "#8C8C8C", Dark: "#666"}    // dim labels / separators
)

var (
	StyleBorder     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	StylePanelTitle = lipgloss.NewStyle().Bold(true)
	StyleHint       = lipgloss.NewStyle().Faint(true)
	StyleAuthErr    = lipgloss.NewStyle().Foreground(colorBad).Bold(true)
	StyleAuthOk     = lipgloss.NewStyle().Foreground(colorOk)

	// Status bar pieces.
	StyleStatusLabel = lipgloss.NewStyle().Foreground(colorMuted)
	StyleStatusValue = lipgloss.NewStyle()
	StyleStatusOk    = lipgloss.NewStyle().Foreground(colorOk)
	StyleStatusWarn  = lipgloss.NewStyle().Foreground(colorWarn)
	StyleStatusBad   = lipgloss.NewStyle().Foreground(colorBad)
	StyleStatusSep   = lipgloss.NewStyle().Foreground(colorMuted).SetString(" · ")

	// Input box.
	StyleInputBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorMuted).
			Padding(0, 1)
	StyleInputBoxFocus = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorAccent).
				Padding(0, 1)
	StyleInputHint = lipgloss.NewStyle().Foreground(colorMuted).Faint(true)

	// Welcome banner.
	StyleWelcomeTitle = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	StyleWelcomeBody  = lipgloss.NewStyle().Faint(true)
)
