package server

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
)

// handleAnthropicProxy proxies Anthropic API requests from sandbox containers,
// injecting the real API key server-side so sandboxes never see it.
//
// Auth: the sandbox sends its per-sandbox proxy token via the x-api-key header
// (the standard Anthropic SDK authentication header). The proxy validates this
// token against the database, replaces it with the real API key, and forwards
// the request to the real Anthropic API.
func (s *Server) handleAnthropicProxy(w http.ResponseWriter, r *http.Request) {
	// Extract proxy token from x-api-key header (standard Anthropic SDK auth).
	proxyToken := r.Header.Get("x-api-key")
	if proxyToken == "" {
		http.Error(w, "missing api key", http.StatusUnauthorized)
		return
	}

	// Validate proxy token against the database.
	sbx, err := s.DB.GetSandboxByProxyToken(proxyToken)
	if err != nil {
		log.Printf("anthropic proxy: db error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if sbx == nil {
		http.Error(w, "invalid api key", http.StatusUnauthorized)
		return
	}
	if sbx.Status != "running" && sbx.Status != "creating" {
		http.Error(w, "sandbox not active", http.StatusForbidden)
		return
	}

	// Determine the upstream Anthropic API URL.
	baseURL := os.Getenv("ANTHROPIC_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	authToken := os.Getenv("ANTHROPIC_AUTH_TOKEN")
	if apiKey == "" && authToken == "" {
		log.Printf("anthropic proxy: neither ANTHROPIC_API_KEY nor ANTHROPIC_AUTH_TOKEN configured")
		http.Error(w, "API credentials not configured", http.StatusInternalServerError)
		return
	}

	// Strip the /proxy/anthropic prefix to get the upstream path.
	upstreamPath := strings.TrimPrefix(r.URL.Path, "/proxy/anthropic")
	if upstreamPath == "" {
		upstreamPath = "/"
	}

	target, err := url.Parse(baseURL)
	if err != nil {
		log.Printf("anthropic proxy: invalid base URL %q: %v", baseURL, err)
		http.Error(w, "invalid upstream URL", http.StatusInternalServerError)
		return
	}

	// Limit request body size to 10MB.
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = upstreamPath
			req.URL.RawQuery = r.URL.RawQuery
			req.Host = target.Host

			// Inject real credentials for the upstream API.
			if apiKey != "" {
				req.Header.Set("x-api-key", apiKey)
			}
			if authToken != "" {
				req.Header.Set("Authorization", "Bearer "+authToken)
			}

			// Ensure anthropic-version header is set.
			if req.Header.Get("anthropic-version") == "" {
				req.Header.Set("anthropic-version", "2023-06-01")
			}
		},
		FlushInterval: -1, // Enable SSE streaming.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("anthropic proxy error for sandbox %s: %v", sbx.ID, err)
			http.Error(w, "proxy error", http.StatusBadGateway)
		},
	}

	proxy.ServeHTTP(w, r)
}
