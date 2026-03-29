package sandboxproxy

import (
	"context"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/agentserver/agentserver/internal/auth"
	"github.com/agentserver/agentserver/internal/db"
	"github.com/agentserver/agentserver/internal/sbxstore"
	"github.com/agentserver/agentserver/internal/tunnel"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type contextKey string

const matchedDomainKey contextKey = "matchedBaseDomain"

// matchedBaseDomain returns the base domain that matched the current request,
// falling back to the first configured domain.
func (s *Server) matchedBaseDomain(r *http.Request) string {
	if d, ok := r.Context().Value(matchedDomainKey).(string); ok {
		return d
	}
	if len(s.BaseDomains) > 0 {
		return s.BaseDomains[0]
	}
	return ""
}

// Server is the sandbox-proxy HTTP server that handles subdomain traffic
// proxying and WebSocket tunnel connections.
type Server struct {
	Auth                    *auth.Auth
	DB                      *db.DB
	Sandboxes               *sbxstore.Store
	TunnelRegistry          *tunnel.Registry
	OpencodeStaticFS        fs.FS
	BaseDomains             []string // all configured base domains (first is primary)
	OpencodeAssetDomain     string
	OpencodeSubdomainPrefix   string
	OpenclawSubdomainPrefix   string
	ClaudeCodeSubdomainPrefix string

	activityMu   sync.Mutex
	activityLast map[string]time.Time
}

// New creates a new sandbox-proxy server.
func New(cfg Config, authSvc *auth.Auth, database *db.DB, sandboxStore *sbxstore.Store, tunnelReg *tunnel.Registry, opcodeStaticFS fs.FS) *Server {
	s := &Server{
		Auth:                    authSvc,
		DB:                      database,
		Sandboxes:               sandboxStore,
		TunnelRegistry:          tunnelReg,
		OpencodeStaticFS:        opcodeStaticFS,
		BaseDomains:             cfg.BaseDomains,
		OpencodeAssetDomain:     cfg.OpencodeAssetDomain,
		OpencodeSubdomainPrefix: cfg.OpencodeSubdomainPrefix,
		OpenclawSubdomainPrefix:   cfg.OpenclawSubdomainPrefix,
		ClaudeCodeSubdomainPrefix: cfg.ClaudeCodeSubdomainPrefix,
		activityLast:            make(map[string]time.Time),
	}
	s.initOpencodeAssetIndex()
	return s
}

// throttledActivity updates activity at most once per 30 seconds per sandbox.
func (s *Server) throttledActivity(sandboxID string) {
	s.activityMu.Lock()
	last, ok := s.activityLast[sandboxID]
	now := time.Now()
	if ok && now.Sub(last) < 30*time.Second {
		s.activityMu.Unlock()
		return
	}
	s.activityLast[sandboxID] = now
	s.activityMu.Unlock()
	s.Sandboxes.UpdateActivity(sandboxID)
}

// Router returns the HTTP handler for the sandbox-proxy service.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Subdomain middleware: if the Host matches {prefix}-{sandboxID}.{baseDomain},
	// proxy the entire request to the sandbox and skip all other routes.
	// Supports multiple base domains.
	if len(s.BaseDomains) > 0 {
		r.Use(func(next http.Handler) http.Handler {
			type domainEntry struct {
				suffix string
				domain string
			}
			entries := make([]domainEntry, len(s.BaseDomains))
			for i, d := range s.BaseDomains {
				entries[i] = domainEntry{suffix: "." + d, domain: d}
			}
			opcodePrefix := s.OpencodeSubdomainPrefix + "-"
			clawPrefix := s.OpenclawSubdomainPrefix + "-"
			claudePrefix := s.ClaudeCodeSubdomainPrefix + "-"
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				host := r.Host
				if idx := strings.LastIndex(host, ":"); idx != -1 {
					host = host[:idx]
				}
				for _, e := range entries {
					if !strings.HasSuffix(host, e.suffix) {
						continue
					}
					sub := strings.TrimSuffix(host, e.suffix)
					// Store matched domain in context for login redirects.
					ctx := context.WithValue(r.Context(), matchedDomainKey, e.domain)
					r = r.WithContext(ctx)

					if s.OpencodeAssetDomain != "" && host == s.OpencodeAssetDomain {
						s.handleAssetDomainRequest(w, r)
						return
					}
					if strings.HasPrefix(sub, opcodePrefix) {
						sandboxID := sub[len(opcodePrefix):]
						s.handleSubdomainProxy(w, r, sandboxID)
						return
					}
					if strings.HasPrefix(sub, clawPrefix) {
						sandboxID := sub[len(clawPrefix):]
						s.handleOpenclawSubdomainProxy(w, r, sandboxID)
						return
					}
					if strings.HasPrefix(sub, claudePrefix) {
						sandboxID := sub[len(claudePrefix):]
						s.handleClaudeCodeSubdomainProxy(w, r, sandboxID)
						return
					}
				}
				next.ServeHTTP(w, r)
			})
		})
	}

	// Health check.
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Tunnel endpoint (auth via tunnel token, no cookie auth needed).
	r.HandleFunc("/api/tunnel/{sandboxId}", s.handleTunnel)

	return r
}
