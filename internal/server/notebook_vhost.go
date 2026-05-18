package server

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/agentserver/agentserver/internal/notebookjwt"
)

const (
	notebookCookieName  = "nb-token"
	notebookCookieMaxTTL = 24 * time.Hour
)

// notebookVhostHostMatch returns the workspace-id short (lowercase hex,
// first 8 chars) embedded in the Host header, or "" if the host does
// not match the configured nb-<short>.<baseDomain> pattern.
//
// Comparison is suffix-based against s.NotebookHostBaseDomain, prefix-
// based against s.NotebookSubdomainPrefix + "-". The middle segment is
// returned unvalidated; the caller cross-checks it against the JWT
// workspace id.
func (s *Server) notebookVhostHostMatch(r *http.Request) string {
	if s.NotebookHostBaseDomain == "" {
		return ""
	}
	host := r.Host
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}
	suffix := "." + s.NotebookHostBaseDomain
	if !strings.HasSuffix(host, suffix) {
		return ""
	}
	sub := strings.TrimSuffix(host, suffix)
	prefix := s.notebookSubdomainPrefixOrDefault() + "-"
	if !strings.HasPrefix(sub, prefix) {
		return ""
	}
	return sub[len(prefix):]
}

func (s *Server) notebookSubdomainPrefixOrDefault() string {
	if s.NotebookSubdomainPrefix == "" {
		return "nb"
	}
	return s.NotebookSubdomainPrefix
}

// notebookVhost handles every request that lands on
// {prefix}-{ws_short}.{baseDomain}. Auth flow:
//
//  1. GET /auth?token=<JWT> — verify the HMAC token minted by
//     postNotebookSession, confirm host-short matches the JWT's
//     workspace id, set a per-subdomain HttpOnly cookie, 302 to "/lab".
//  2. Every other request — read the cookie, verify it, reverse-proxy
//     to the in-cluster Jupyter Service with the full path intact.
//
// Cookie scope: no Domain attr — locked to this exact subdomain so a
// leaked cookie for one workspace cannot reach another.
func (s *Server) notebookVhost(w http.ResponseWriter, r *http.Request, hostShort string) {
	if len(s.NotebookJWTSecret) == 0 || s.NotebookSupervisor == nil {
		http.Error(w, "notebook feature disabled", http.StatusServiceUnavailable)
		return
	}

	// Token-from-query exchange.
	if r.URL.Path == "/auth" && r.Method == http.MethodGet {
		s.notebookVhostExchangeToken(w, r, hostShort)
		return
	}

	cookie, err := r.Cookie(notebookCookieName)
	if err != nil {
		http.Error(w, "missing session cookie", http.StatusUnauthorized)
		return
	}
	claims, err := notebookjwt.Verify(s.NotebookJWTSecret, cookie.Value)
	if err != nil {
		http.Error(w, "invalid session cookie: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if !workspaceIDStartsWith(claims.WorkspaceID, hostShort) {
		http.Error(w, "host/workspace mismatch", http.StatusForbidden)
		return
	}

	s.proxyNotebookVhost(w, r, claims.WorkspaceID, claims.UserID)
}

// notebookVhostExchangeToken validates the URL-supplied JWT, sets the
// session cookie, and redirects to "/lab" (stripping the token from
// the visible URL).
func (s *Server) notebookVhostExchangeToken(w http.ResponseWriter, r *http.Request, hostShort string) {
	tok := r.URL.Query().Get("token")
	if tok == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}
	claims, err := notebookjwt.Verify(s.NotebookJWTSecret, tok)
	if err != nil {
		http.Error(w, "invalid token: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if !workspaceIDStartsWith(claims.WorkspaceID, hostShort) {
		http.Error(w, "host/workspace mismatch", http.StatusForbidden)
		return
	}
	maxAge := time.Until(time.Unix(claims.Exp, 0))
	if maxAge <= 0 || maxAge > notebookCookieMaxTTL {
		maxAge = notebookCookieMaxTTL
	}
	http.SetCookie(w, &http.Cookie{
		Name:     notebookCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(maxAge.Seconds()),
	})
	dest := r.URL.Query().Get("next")
	if dest == "" || !strings.HasPrefix(dest, "/") {
		dest = "/lab"
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// proxyNotebookVhost reverse-proxies the request to the in-cluster
// Jupyter Service. Path is forwarded as-is — Jupyter is started with
// base_url=/ on the vhost so absolute URLs in its HTML/JS work
// without any rewriting.
func (s *Server) proxyNotebookVhost(w http.ResponseWriter, r *http.Request, wsID, userID string) {
	upstreamURLStr, err := s.resolveNotebookUpstream(r.Context(), wsID)
	if err != nil {
		http.Error(w, "notebook resolve: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	upstreamURL, err := url.Parse(upstreamURLStr)
	if err != nil {
		http.Error(w, "bad upstream url", http.StatusInternalServerError)
		return
	}
	rp := httputil.NewSingleHostReverseProxy(upstreamURL)
	rp.FlushInterval = -1
	originalDirector := rp.Director
	rp.Director = func(req *http.Request) {
		originalDirector(req)
		// Strip any leftover ?token=… from the auth exchange before
		// forwarding so it never reaches the upstream logs.
		q := req.URL.Query()
		if q.Get("token") != "" {
			q.Del("token")
			req.URL.RawQuery = q.Encode()
		}
		req.Header.Set("X-Forwarded-User", userID)
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("notebook vhost proxy error for ws %s: %v", wsID, err)
		http.Error(w, "proxy error", http.StatusBadGateway)
	}
	rp.ServeHTTP(w, r)
}

// workspaceIDStartsWith reports whether the workspace id begins with
// the given lowercase short. Matches the convention in
// internal/storage/workspacedrive.go (first 8 chars of the UUID).
func workspaceIDStartsWith(wsID, short string) bool {
	if short == "" || len(short) > len(wsID) {
		return false
	}
	return strings.EqualFold(wsID[:len(short)], short)
}
