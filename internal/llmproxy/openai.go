package llmproxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// handleOpenAIProxy fronts an OpenAI-compatible upstream (currently
// the modelserver at code.ai.cs.ac.cn) for codex's /v1/responses (and
// future /v1/chat/completions) traffic. It mirrors the anthropic
// proxy's modelserver branch but drops anthropic-specific trace/usage
// extraction since OpenAI's response shape is different and codex
// usage tracking isn't wired up yet.
//
// Routes (in server.go): /v1/responses, /v1/responses/*,
// /v1/chat/completions, /v1/embeddings, /v1/models[/*]. The path is
// forwarded as-is to the upstream.
//
// Auth: the caller (codex app-server subprocess) sends Bearer
// <workspace-proxy-token>. We validate that against agentserver,
// then exchange it for a fresh modelserver JWT and inject the JWT
// into the upstream request. This means the codex pod never holds a
// modelserver-validated credential — its workspace token is
// long-lived, but the actual upstream-bound token rotates per
// request and survives OAuth refreshes server-side.
func (s *Server) handleOpenAIProxy(w http.ResponseWriter, r *http.Request) {
	proxyToken := extractProxyToken(r.Header)
	if proxyToken == "" {
		http.Error(w, "missing api key", http.StatusUnauthorized)
		return
	}

	sbx, err := s.ValidateProxyToken(r.Context(), proxyToken)
	if err != nil {
		s.logger.Error("openai: token validation failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if sbx == nil {
		http.Error(w, "invalid api key", http.StatusUnauthorized)
		return
	}
	if sbx.TokenType == "sandbox" && sbx.Status != "running" && sbx.Status != "creating" {
		http.Error(w, "sandbox not active", http.StatusForbidden)
		return
	}
	if sbx.ModelserverUpstreamURL == "" {
		http.Error(w, "workspace has no modelserver connection", http.StatusForbidden)
		return
	}

	msToken, err := s.fetchModelserverToken(sbx.WorkspaceID)
	if err != nil {
		s.logger.Error("openai: failed to get modelserver token",
			"error", err, "workspace_id", sbx.WorkspaceID)
		http.Error(w, "modelserver token unavailable", http.StatusBadGateway)
		return
	}

	target, err := url.Parse(sbx.ModelserverUpstreamURL)
	if err != nil {
		s.logger.Error("openai: invalid upstream URL", "error", err, "url", sbx.ModelserverUpstreamURL)
		http.Error(w, "invalid upstream URL", http.StatusInternalServerError)
		return
	}

	// Forward the path as-is. The upstream (modelserver) advertises
	// OpenAI-shape URLs at /v1/responses etc., so /v1/responses on us
	// maps directly to /v1/responses on it.
	reqPath := r.URL.Path
	rawQuery := r.URL.RawQuery

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = joinPaths(target.Path, reqPath)
			req.URL.RawQuery = rawQuery
			req.Host = target.Host
			req.Header.Del("x-api-key")
			req.Header.Set("Authorization", "Bearer "+msToken)
		},
		FlushInterval: -1, // SSE streaming for /v1/responses
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			s.logger.Error("openai: proxy error", "error", err)
			http.Error(w, "proxy error", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// joinPaths concatenates a base path (from the upstream URL) with the
// per-request path. ModelserverUpstreamURL typically has no path
// component (e.g. "https://code.ai.cs.ac.cn"), but if it ever has a
// prefix like "/api" we want to honor it.
func joinPaths(base, reqPath string) string {
	switch {
	case base == "" || base == "/":
		return reqPath
	case strings.HasSuffix(base, "/") && strings.HasPrefix(reqPath, "/"):
		return base + strings.TrimPrefix(reqPath, "/")
	case !strings.HasSuffix(base, "/") && !strings.HasPrefix(reqPath, "/"):
		return base + "/" + reqPath
	default:
		return base + reqPath
	}
}
