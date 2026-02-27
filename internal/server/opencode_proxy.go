package server

import (
	"encoding/base64"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/imryao/cli-server/internal/auth"
)

const (
	ocSessionCookie = "X-OC-Session"
	opencodePort    = "4096"
)

// opencodeWebProxy returns a reverse proxy to the shared opencode-web pod for static assets.
func (s *Server) opencodeWebProxy() *httputil.ReverseProxy {
	target, err := url.Parse(s.OpencodeWebURL)
	if err != nil {
		log.Fatalf("invalid OPENCODE_WEB_URL: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	return proxy
}

// handleOpencodeEntry handles GET /oc/{sessionID}/ — validates session ownership,
// sets the X-OC-Session cookie, and proxies index.html from the opencode-web pod.
func (s *Server) handleOpencodeEntry(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	sessionID := chi.URLParam(r, "sessionID")

	sess, ok := s.Sessions.Get(sessionID)
	if !ok || sess.UserID != userID {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	// Set session cookie so subsequent requests route to the correct pod.
	http.SetCookie(w, &http.Cookie{
		Name:     ocSessionCookie,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	// Proxy index.html from opencode-web pod.
	r.URL.Path = "/"
	s.ocWebProxy.ServeHTTP(w, r)
}

// handleOpencodeAssets serves static asset requests (/assets/*).
// It first checks the embedded cli-server frontend assets; if the file exists
// there it is served directly. Otherwise the request is proxied to the opencode-web pod.
func (s *Server) handleOpencodeAssets(w http.ResponseWriter, r *http.Request) {
	if s.StaticFS != nil {
		// Strip the leading slash to get the fs path (e.g. "assets/index-xxx.js").
		if _, err := fs.Stat(s.StaticFS, r.URL.Path[1:]); err == nil {
			http.FileServer(http.FS(s.StaticFS)).ServeHTTP(w, r)
			return
		}
	}
	s.ocWebProxy.ServeHTTP(w, r)
}

// handleOpencodeFavicon proxies favicon requests to the opencode-web pod.
func (s *Server) handleOpencodeFavicon(w http.ResponseWriter, r *http.Request) {
	s.ocWebProxy.ServeHTTP(w, r)
}

// opencodeAPIProxy reads the X-OC-Session cookie, validates session ownership,
// injects Basic Auth, and reverse-proxies the request to the session's pod.
func (s *Server) opencodeAPIProxy(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(ocSessionCookie)
	if err != nil {
		http.Error(w, "missing session cookie", http.StatusUnauthorized)
		return
	}

	sessionID := cookie.Value
	sess, ok := s.Sessions.Get(sessionID)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	// Validate user ownership via auth middleware (user ID already in context).
	userID := auth.UserIDFromContext(r.Context())
	if sess.UserID != userID {
		http.Error(w, "forbidden", http.StatusForbidden)
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
	proxy.ServeHTTP(w, r)
}
