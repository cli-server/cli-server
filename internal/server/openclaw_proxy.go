package server

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

const (
	openclawPort       = "18789"
	clawCookieKey      = "claw-token"
)

// handleOpenclawSubdomainProxy handles all requests on claw-{sandboxID}.{baseDomain}.
//
// Auth flow:
//  1. GET /auth?token=xxx — validates the main-site token, sets a per-subdomain
//     cookie, and redirects to /.
//  2. All other requests — validated via the per-subdomain cookie, then proxied
//     to the sandbox pod.
func (s *Server) handleOpenclawSubdomainProxy(w http.ResponseWriter, r *http.Request, sandboxID string) {
	// Step 1: handle /auth?token=xxx — exchange main-site token for subdomain cookie.
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
		// Verify workspace membership.
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
		// Set a per-subdomain auth cookie (no Domain attr — scoped to this subdomain only).
		http.SetCookie(w, &http.Cookie{
			Name:     clawCookieKey,
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int((7 * 24 * time.Hour).Seconds()),
		})
		// Redirect to Control UI with the gateway token so the frontend JS
		// can authenticate the WebSocket connection automatically.
		redirectURL := "/"
		if sbx.GatewayToken != "" {
			redirectURL = "/?token=" + url.QueryEscape(sbx.GatewayToken)
		}
		http.Redirect(w, r, redirectURL, http.StatusFound)
		return
	}

	// Step 2: validate per-subdomain cookie for all other requests.
	cookie, err := r.Cookie(clawCookieKey)
	if err != nil {
		// No subdomain cookie — redirect to main site login.
		loginURL := s.BaseScheme + "://" + s.BaseDomain + "/"
		http.Redirect(w, r, loginURL, http.StatusFound)
		return
	}
	userID, ok := s.Auth.ValidateToken(cookie.Value)
	if !ok {
		loginURL := s.BaseScheme + "://" + s.BaseDomain + "/"
		http.Redirect(w, r, loginURL, http.StatusFound)
		return
	}

	// Validate workspace membership.
	sbx, found := s.Sandboxes.Resolve(sandboxID)
	if !found {
		log.Printf("openclaw proxy: sandbox %s not found in store", sandboxID)
		writeErrorPage(w, errPageSandboxNotFound)
		return
	}
	isMember, err := s.DB.IsWorkspaceMember(sbx.WorkspaceID, userID)
	if err != nil || !isMember {
		log.Printf("openclaw proxy: user %s not a member of workspace %s for sandbox %s", userID, sbx.WorkspaceID, sandboxID)
		writeErrorPage(w, errPageSandboxNotFound)
		return
	}

	if sbx.Status != "running" {
		writeErrorPage(w, errPageSandboxNotRunning)
		return
	}

	if sbx.PodIP == "" {
		writeErrorPage(w, errPagePodNotReady)
		return
	}

	// Inject Bearer token for openclaw gateway authentication.
	if sbx.GatewayToken != "" {
		r.Header.Set("Authorization", "Bearer "+sbx.GatewayToken)
	}

	// Track activity for idle watcher.
	s.throttledActivity(sandboxID)

	// Reverse proxy to the sandbox pod.
	target := &url.URL{
		Scheme: "http",
		Host:   sbx.PodIP + ":" + openclawPort,
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1 // Enable SSE streaming.
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("openclaw proxy error for sandbox %s: %v", sandboxID, err)
		http.Error(w, "proxy error", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}
