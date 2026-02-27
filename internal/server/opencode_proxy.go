package server

import (
	"encoding/base64"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

const (
	opencodePort       = "4096"
	subdomainCookieKey = "oc-token"
)

// handleSubdomainProxy handles all requests on oc-{sessionID}.{baseDomain}.
//
// Auth flow:
//  1. GET /auth?token=xxx — validates the main-site token, sets a per-subdomain
//     cookie, and redirects to /.
//  2. All other requests — validated via the per-subdomain cookie, then proxied
//     to the session pod.
func (s *Server) handleSubdomainProxy(w http.ResponseWriter, r *http.Request, sessionID string) {
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
		// Verify session ownership.
		sess, found := s.Sessions.Get(sessionID)
		if !found || sess.UserID != userID {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		// Set a per-subdomain auth cookie (no Domain attr — scoped to this subdomain only).
		http.SetCookie(w, &http.Cookie{
			Name:     subdomainCookieKey,
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int((7 * 24 * time.Hour).Seconds()),
		})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// Step 2: validate per-subdomain cookie for all other requests.
	cookie, err := r.Cookie(subdomainCookieKey)
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

	// Validate session ownership.
	sess, found := s.Sessions.Get(sessionID)
	if !found {
		log.Printf("subdomain proxy: session %s not found in store", sessionID)
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if sess.UserID != userID {
		log.Printf("subdomain proxy: session %s owned by %s, but request from %s", sessionID, sess.UserID, userID)
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	if sess.PodIP == "" {
		http.Error(w, "session pod not ready", http.StatusServiceUnavailable)
		return
	}

	// Inject Basic Auth header for opencode server authentication.
	if sess.OpencodePassword != "" {
		cred := base64.StdEncoding.EncodeToString([]byte("opencode:" + sess.OpencodePassword))
		r.Header.Set("Authorization", "Basic "+cred)
	}

	// Reverse proxy to the session pod.
	target := &url.URL{
		Scheme: "http",
		Host:   sess.PodIP + ":" + opencodePort,
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1 // Enable SSE streaming.
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("subdomain proxy error for session %s: %v", sessionID, err)
		http.Error(w, "proxy error", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}
