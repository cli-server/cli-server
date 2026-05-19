package sdk

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/agentserver/agentserver/internal/envtools/bridge"
	"github.com/agentserver/agentserver/internal/envtools/nameresolver"
	"github.com/agentserver/agentserver/internal/envtools/processes"
	"github.com/agentserver/agentserver/internal/envtools/tools"
)

// ConnectedExecutor mirrors the fields codex-exec-gateway's existing
// /api/exec-gateway/connected handler returns. Defined here to avoid
// importing the handler package from sdk.
type ConnectedExecutor struct {
	ExeID      string `json:"exe_id,omitempty"`
	Name       string `json:"name"`
	IsDefault  bool   `json:"is_default,omitempty"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
}

// ConnectedLister is the subset of the gateway's executor registry the
// sdk package needs. The B6 wiring step provides an adapter that
// satisfies this interface from the existing store + registry types.
type ConnectedLister interface {
	Connected(ctx context.Context, workspaceID string) ([]ConnectedExecutor, error)
}

// Server holds the SDK REST surface. Construct in cmd/codex-exec-gateway/main.go
// and call Mount(r chi.Router) once at startup.
type Server struct {
	Auth     *ProxyTokenAuth
	Pool     *bridge.Pool
	Resolver *nameresolver.Resolver
	Sessions *processes.Manager
	Registry ConnectedLister
	Tools    map[string]tools.Tool
}

// Mount registers every SDK route under /api/sdk/*. Each handler runs
// through authMiddleware which extracts and validates the Bearer token.
func (s *Server) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.authMiddleware)
		r.Post("/api/sdk/envs/list", s.handleEnvsList)
		r.Post("/api/sdk/envs/{name}/tool/call", s.handleToolCall)
		r.Post("/api/sdk/processes/{sid}/stdin", s.handleStdin)
		r.Get("/api/sdk/processes/{sid}/output", s.handleOutput)
		r.Post("/api/sdk/processes/{sid}/terminate", s.handleTerminate)
	})
}

type ctxKey int

const (
	ctxWorkspaceID ctxKey = iota
	ctxUserID
)

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			writeErr(w, http.StatusUnauthorized, "missing_bearer", "Authorization: Bearer <token> required")
			return
		}
		tok := strings.TrimPrefix(h, "Bearer ")
		wsID, userID, err := s.Auth.Verify(r.Context(), tok)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid_token", err.Error())
			return
		}
		ctx := context.WithValue(r.Context(), ctxWorkspaceID, wsID)
		ctx = context.WithValue(ctx, ctxUserID, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func workspaceFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(ctxWorkspaceID).(string); ok {
		return v
	}
	return ""
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": msg},
	})
}
