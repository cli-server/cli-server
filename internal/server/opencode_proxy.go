package server

import (
	"encoding/base64"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"time"
)

const (
	opencodePort       = "4096"
	subdomainCookieKey = "oc-token"
)

// handleSubdomainProxy handles all requests on oc-{sandboxID}.{baseDomain}.
//
// Auth flow:
//  1. GET /auth?token=xxx — validates the main-site token, sets a per-subdomain
//     cookie, and redirects to /.
//  2. All other requests — validated via the per-subdomain cookie, then proxied
//     to the sandbox pod.
func (s *Server) handleSubdomainProxy(w http.ResponseWriter, r *http.Request, sandboxID string) {
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
		sbx, found := s.Sandboxes.Get(sandboxID)
		if !found {
			http.Error(w, "sandbox not found", http.StatusNotFound)
			return
		}
		isMember, err := s.DB.IsWorkspaceMember(sbx.WorkspaceID, userID)
		if err != nil || !isMember {
			http.Error(w, "sandbox not found", http.StatusNotFound)
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

	// Validate workspace membership.
	sbx, found := s.Sandboxes.Get(sandboxID)
	if !found {
		log.Printf("subdomain proxy: sandbox %s not found in store", sandboxID)
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	isMember, err := s.DB.IsWorkspaceMember(sbx.WorkspaceID, userID)
	if err != nil || !isMember {
		log.Printf("subdomain proxy: user %s not a member of workspace %s for sandbox %s", userID, sbx.WorkspaceID, sandboxID)
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	if sbx.Status != "running" {
		http.Error(w, "sandbox is not running", http.StatusServiceUnavailable)
		return
	}

	// Try serving from embedded opencode frontend before proxying to pod.
	if s.OpencodeStaticFS != nil {
		if s.tryServeOpencodeStatic(w, r) {
			return
		}
	}

	// Local agent: proxy via WebSocket tunnel.
	if sbx.IsLocal {
		tunnel, ok := s.TunnelRegistry.Get(sandboxID)
		if !ok {
			http.Error(w, "agent offline", http.StatusServiceUnavailable)
			return
		}
		s.proxyViaTunnel(w, r, sbx, tunnel)
		return
	}

	if sbx.PodIP == "" {
		http.Error(w, "sandbox pod not ready", http.StatusServiceUnavailable)
		return
	}

	// Inject Basic Auth header for opencode server authentication.
	if sbx.OpencodePassword != "" {
		cred := base64.StdEncoding.EncodeToString([]byte("opencode:" + sbx.OpencodePassword))
		r.Header.Set("Authorization", "Basic "+cred)
	}

	// Track activity for idle watcher.
	s.throttledActivity(sandboxID)

	// Reverse proxy to the sandbox pod.
	target := &url.URL{
		Scheme: "http",
		Host:   sbx.PodIP + ":" + opencodePort,
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1 // Enable SSE streaming.
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("subdomain proxy error for sandbox %s: %v", sandboxID, err)
		http.Error(w, "proxy error", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}

// opencodeAPIPrefixes lists path segments that should always be proxied to
// the opencode pod rather than served from the embedded frontend. A request
// matches if its path equals the prefix exactly (e.g. "/project") or starts
// with the prefix followed by "/" (e.g. "/project/current").
var opencodeAPIPrefixes = []string{
	"/global", "/auth", "/project", "/session", "/pty",
	"/file", "/find", "/config", "/mcp", "/provider",
	"/question", "/permission", "/tui", "/experimental",
	"/doc", "/path", "/vcs", "/command", "/log",
	"/agent", "/skill", "/lsp", "/formatter", "/event",
	"/instance",
}

// tryServeOpencodeStatic attempts to serve a request from the embedded opencode
// frontend (SPA). Returns true if the response was handled, false if the request
// should be proxied to the pod.
//
// Decision flow:
//  1. If the cleaned path matches a real file in OpencodeStaticFS → serve it.
//  2. If the path starts with a known API prefix → return false (proxy to pod).
//  3. Otherwise → serve index.html (SPA client-side route fallback).
func (s *Server) tryServeOpencodeStatic(w http.ResponseWriter, r *http.Request) bool {
	upath := path.Clean(r.URL.Path)
	if upath == "/" {
		upath = "/index.html"
	}

	// 1. Check if the path matches a real file in the embedded FS.
	filePath := upath[1:] // strip leading "/"
	if f, err := fs.Stat(s.OpencodeStaticFS, filePath); err == nil && !f.IsDir() {
		s.serveOpencodeFile(w, r, filePath)
		return true
	}

	// 2. If the path starts with a known API prefix, let the proxy handle it.
	for _, prefix := range opencodeAPIPrefixes {
		if upath == prefix || strings.HasPrefix(upath, prefix+"/") {
			return false
		}
	}

	// 3. If the path has a file extension but didn't match a real file, proxy it.
	// This handles cases like sourcemaps or other assets not in the embedded FS.
	if ext := path.Ext(upath); ext != "" {
		return false
	}

	// 4. No extension and not an API route — serve index.html as SPA fallback.
	if _, err := fs.Stat(s.OpencodeStaticFS, "index.html"); err != nil {
		return false
	}
	s.serveOpencodeFile(w, r, "index.html")
	return true
}

// serveOpencodeFile serves a single file from the embedded opencode frontend FS
// with appropriate cache headers.
func (s *Server) serveOpencodeFile(w http.ResponseWriter, r *http.Request, filePath string) {
	f, err := s.OpencodeStaticFS.Open(filePath)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Set cache headers: long cache for hashed assets, no-cache for index.html.
	if filePath == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
	} else if strings.HasPrefix(filePath, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}

	// http.ServeContent handles Content-Type detection, range requests, and If-Modified-Since.
	rs, ok := f.(readSeeker)
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, filePath, stat.ModTime(), rs)
}

// readSeeker combines io.Reader and io.Seeker (fs.File may implement this).
type readSeeker interface {
	Read([]byte) (int, error)
	Seek(int64, int) (int64, error)
}
