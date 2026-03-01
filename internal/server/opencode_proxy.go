package server

import (
	"bytes"
	"encoding/base64"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"regexp"
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
	// Step 0: serve real static files from embedded FS without requiring auth.
	// These are public frontend assets (JS, CSS, images, manifest, etc.) that
	// browsers may fetch without cookies (e.g. site.webmanifest, favicons).
	// Only exact file matches are served — SPA fallback still requires auth.
	if s.OpencodeStaticFS != nil {
		upath := path.Clean(r.URL.Path)
		if upath == "/" {
			upath = "/index.html"
		}
		filePath := upath[1:] // strip leading "/"
		if f, err := fs.Stat(s.OpencodeStaticFS, filePath); err == nil && !f.IsDir() {
			s.serveOpencodeFile(w, r, filePath)
			return
		}
	}

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
	sbx, found := s.Sandboxes.Resolve(sandboxID)
	if !found {
		log.Printf("subdomain proxy: sandbox %s not found in store", sandboxID)
		writeErrorPage(w, errPageSandboxNotFound)
		return
	}
	isMember, err := s.DB.IsWorkspaceMember(sbx.WorkspaceID, userID)
	if err != nil || !isMember {
		log.Printf("subdomain proxy: user %s not a member of workspace %s for sandbox %s", userID, sbx.WorkspaceID, sandboxID)
		writeErrorPage(w, errPageSandboxNotFound)
		return
	}

	if sbx.Status != "running" {
		writeErrorPage(w, errPageSandboxNotRunning)
		return
	}

	// Try SPA fallback from embedded opencode frontend before proxying to pod.
	// Real static files were already served above (before auth); here we only
	// handle SPA client-side routes that need index.html.
	if s.OpencodeStaticFS != nil {
		if s.tryServeOpencodeSPAFallback(w, r) {
			return
		}
	}

	// Local agent: proxy via WebSocket tunnel.
	if sbx.IsLocal {
		tunnel, ok := s.TunnelRegistry.Get(sandboxID)
		if !ok {
			writeErrorPage(w, errPageAgentOffline)
			return
		}
		s.proxyViaTunnel(w, r, sbx, tunnel)
		return
	}

	if sbx.PodIP == "" {
		writeErrorPage(w, errPagePodNotReady)
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

// tryServeOpencodeSPAFallback handles SPA client-side routes by serving
// index.html. Real static files are already served before auth (step 0 in
// handleSubdomainProxy). This only handles the fallback case: paths that are
// neither real files nor known API routes.
//
// Returns true if index.html was served, false if the request should be proxied.
func (s *Server) tryServeOpencodeSPAFallback(w http.ResponseWriter, r *http.Request) bool {
	upath := path.Clean(r.URL.Path)

	// If the path starts with a known API prefix, let the proxy handle it.
	for _, prefix := range opencodeAPIPrefixes {
		if upath == prefix || strings.HasPrefix(upath, prefix+"/") {
			return false
		}
	}

	// If the path has a file extension but didn't match a real file, proxy it.
	if ext := path.Ext(upath); ext != "" {
		return false
	}

	// No extension and not an API route — serve index.html as SPA fallback.
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

// handleAssetDomainRequest serves static assets from the shared asset domain
// (e.g. opencodeapp.agentserver.dev). All sandbox subdomains reference this
// domain for JS/CSS/images so browsers can share cached assets across sandboxes.
//
// index.html is blocked (404) — it must be served from each sandbox's subdomain
// so that per-subdomain auth cookies and SPA routing work correctly.
func (s *Server) handleAssetDomainRequest(w http.ResponseWriter, r *http.Request) {
	// Handle CORS preflight.
	if r.Method == http.MethodOptions {
		s.setAssetCORSHeaders(w, r)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if s.OpencodeStaticFS == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	upath := path.Clean(r.URL.Path)
	if upath == "/" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	filePath := upath[1:] // strip leading "/"

	// Block index.html — must be served from sandbox subdomain.
	if filePath == "index.html" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Check file exists in embedded FS.
	fi, err := fs.Stat(s.OpencodeStaticFS, filePath)
	if err != nil || fi.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	s.setAssetCORSHeaders(w, r)
	s.serveOpencodeFile(w, r, filePath)
}

// setAssetCORSHeaders sets CORS headers for the shared asset domain.
// Uses wildcard origin since these are public, cacheable static assets.
func (s *Server) setAssetCORSHeaders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Origin, Accept, Content-Type")
	w.Header().Set("Access-Control-Max-Age", "86400")
}

// crossoriginTagRe matches <script src="..."> and <link ... href="..."> tags
// that don't already have a crossorigin attribute.
var crossoriginTagRe = regexp.MustCompile(`(<(?:script|link)\b[^>]*?)(/?>)`)

// initOpencodeAssetIndex processes the embedded index.html at startup to add
// crossorigin attributes to <script> and <link> tags. This is needed because
// assets are loaded cross-origin from the shared asset domain.
func (s *Server) initOpencodeAssetIndex() {
	if s.OpencodeStaticFS == nil || s.OpencodeAssetDomain == "" {
		return
	}

	f, err := s.OpencodeStaticFS.Open("index.html")
	if err != nil {
		return
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return
	}

	// Add crossorigin="anonymous" to <script> and <link> tags that reference
	// the asset domain and don't already have crossorigin.
	modified := crossoriginTagRe.ReplaceAllFunc(data, func(match []byte) []byte {
		// Skip if already has crossorigin.
		if bytes.Contains(match, []byte("crossorigin")) {
			return match
		}
		// Only process tags that reference the asset domain or have src/href.
		if !bytes.Contains(match, []byte("src=")) && !bytes.Contains(match, []byte("href=")) {
			return match
		}
		// Insert crossorigin="anonymous" before the closing "/>".
		parts := crossoriginTagRe.FindSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		return append(append(parts[1], []byte(` crossorigin="anonymous"`)...), parts[2]...)
	})

	if bytes.Equal(data, modified) {
		return
	}

	log.Printf("opencode: patched index.html with crossorigin attributes for asset domain %s", s.OpencodeAssetDomain)

	// Replace the embedded FS with a patched version that overlays index.html.
	s.OpencodeStaticFS = &patchedFS{
		base:         s.OpencodeStaticFS,
		patchedIndex: modified,
	}
}

// patchedFS wraps an fs.FS and overrides index.html with patched content.
type patchedFS struct {
	base         fs.FS
	patchedIndex []byte
}

func (p *patchedFS) Open(name string) (fs.File, error) {
	if name == "index.html" {
		return &memFile{
			Reader: bytes.NewReader(p.patchedIndex),
			name:   "index.html",
			size:   int64(len(p.patchedIndex)),
		}, nil
	}
	return p.base.Open(name)
}

// memFile implements fs.File for in-memory content.
type memFile struct {
	*bytes.Reader
	name string
	size int64
}

func (f *memFile) Stat() (fs.FileInfo, error) {
	return &memFileInfo{name: f.name, size: f.size}, nil
}

func (f *memFile) Close() error { return nil }

// memFileInfo implements fs.FileInfo for in-memory files.
type memFileInfo struct {
	name string
	size int64
}

func (fi *memFileInfo) Name() string      { return fi.name }
func (fi *memFileInfo) Size() int64       { return fi.size }
func (fi *memFileInfo) Mode() fs.FileMode { return 0444 }
func (fi *memFileInfo) ModTime() time.Time { return time.Time{} }
func (fi *memFileInfo) IsDir() bool       { return false }
func (fi *memFileInfo) Sys() interface{}  { return nil }
