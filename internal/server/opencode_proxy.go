package server

import (
	"encoding/base64"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

const opencodePort = "4096"

// handleSubdomainProxy validates auth and session ownership, then reverse-proxies
// the entire request to the session's pod. Used for subdomain-based opencode routing
// (e.g. oc-{sessionID}.cli.cs.ac.cn → pod:4096).
func (s *Server) handleSubdomainProxy(w http.ResponseWriter, r *http.Request, sessionID string) {
	// Validate auth cookie.
	userID, ok := s.Auth.ValidateRequest(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Validate session ownership.
	sess, found := s.Sessions.Get(sessionID)
	if !found || sess.UserID != userID {
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
