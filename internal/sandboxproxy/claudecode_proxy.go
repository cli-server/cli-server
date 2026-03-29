package sandboxproxy

import (
	"io"
	"log"
	"net/http"
	"time"

	"github.com/agentserver/agentserver/internal/sbxstore"
	"github.com/agentserver/agentserver/internal/tunnel"
	"nhooyr.io/websocket"
)

const claudecodeCookieKey = "claude-token"

// handleClaudeCodeSubdomainProxy handles all requests on claude-{sandboxID}.{baseDomain}.
//
// Auth flow is identical to opencode/openclaw:
//  1. GET /auth?token=xxx — validates, sets per-subdomain cookie, redirects to /.
//  2. All other requests — validated via the per-subdomain cookie.
//
// Routing:
//   - /ws/terminal — WebSocket terminal proxy via tunnel
//   - everything else — serves the embedded xterm.js terminal page
func (s *Server) handleClaudeCodeSubdomainProxy(w http.ResponseWriter, r *http.Request, sandboxID string) {
	// Step 1: handle /auth?token=xxx.
	if r.URL.Path == "/auth" {
		token := r.URL.Query().Get("token")
		if token == "" {
			http.Error(w, "missing token", http.StatusBadRequest)
			return
		}
		userID, ok := s.Auth.ValidateToken(token)
		if !ok {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		sbx, found := s.Sandboxes.Resolve(sandboxID)
		if !found {
			writeErrorPage(w, errPageSandboxNotFound)
			return
		}
		isMember, err := s.DB.IsWorkspaceMember(sbx.WorkspaceID, userID)
		if err != nil || !isMember {
			writeErrorPage(w, errPageSandboxNotFound)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     claudecodeCookieKey,
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int((7 * 24 * time.Hour).Seconds()),
		})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// Step 2: validate per-subdomain cookie.
	cookie, err := r.Cookie(claudecodeCookieKey)
	if err != nil {
		loginURL := "https://" + s.matchedBaseDomain(r) + "/"
		http.Redirect(w, r, loginURL, http.StatusFound)
		return
	}
	userID, ok := s.Auth.ValidateToken(cookie.Value)
	if !ok {
		loginURL := "https://" + s.matchedBaseDomain(r) + "/"
		http.Redirect(w, r, loginURL, http.StatusFound)
		return
	}

	sbx, found := s.Sandboxes.Resolve(sandboxID)
	if !found {
		writeErrorPage(w, errPageSandboxNotFound)
		return
	}
	isMember, err := s.DB.IsWorkspaceMember(sbx.WorkspaceID, userID)
	if err != nil || !isMember {
		writeErrorPage(w, errPageSandboxNotFound)
		return
	}

	if sbx.Status != "running" {
		writeErrorPage(w, errPageSandboxNotRunning)
		return
	}

	// Route: WebSocket terminal proxy.
	if r.URL.Path == "/ws/terminal" {
		s.handleTerminalWS(w, r, sbx)
		return
	}

	// Everything else: serve the embedded terminal page.
	s.throttledActivity(sbx.ID)
	serveClaudeCodeTerminalPage(w, r)
}

// handleTerminalWS proxies a browser WebSocket to a terminal stream via the tunnel.
func (s *Server) handleTerminalWS(w http.ResponseWriter, r *http.Request, sbx *sbxstore.Sandbox) {
	t, ok := s.TunnelRegistry.Get(sbx.ID)
	if !ok {
		writeErrorPage(w, errPageAgentOffline)
		return
	}

	// Accept browser WebSocket.
	browserWS, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("claudecode terminal ws accept error for %s: %v", sbx.ID, err)
		return
	}
	browserWS.SetReadLimit(-1)

	s.throttledActivity(sbx.ID)

	// Open terminal stream via tunnel.
	termStream, err := t.OpenTerminalStream()
	if err != nil {
		log.Printf("claudecode terminal stream open error for %s: %v", sbx.ID, err)
		browserWS.Close(websocket.StatusInternalError, "tunnel error")
		return
	}

	browserConn := tunnel.NewWSConn(r.Context(), browserWS)

	// Bridge: browser ↔ terminal stream (xray-core style bidirectional copy).
	done := make(chan struct{})
	go func() {
		io.Copy(termStream, browserConn)
		termStream.Close()
		close(done)
	}()
	io.Copy(browserConn, termStream)
	browserConn.Close()
	<-done
}
