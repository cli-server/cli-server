package sandboxproxy

import (
	_ "embed"
	"net/http"
)

//go:embed claudecode_terminal.html
var claudeCodeTerminalHTML []byte

// serveClaudeCodeTerminalPage serves the embedded xterm.js terminal page.
func serveClaudeCodeTerminalPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(claudeCodeTerminalHTML)
}
