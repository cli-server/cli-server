package ccbroker

import (
	"context"
	"net/http"
	"strings"
)

type contextKey string

const (
	ctxSessionID   contextKey = "sessionID"
	ctxWorkspaceID contextKey = "workspaceID"
	ctxEpoch       contextKey = "epoch"
)

// SessionIDFromContext extracts the session ID set by workerAuthMiddleware.
func SessionIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxSessionID).(string)
	return v
}

// WorkspaceIDFromContext extracts the workspace ID set by workerAuthMiddleware.
func WorkspaceIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxWorkspaceID).(string)
	return v
}

// EpochFromContext extracts the epoch set by workerAuthMiddleware.
func EpochFromContext(ctx context.Context) int {
	v, _ := ctx.Value(ctxEpoch).(int)
	return v
}

// workerAuthMiddleware validates the Bearer JWT and injects session/workspace/epoch
// into the request context.
func (s *Server) workerAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")

		claims, err := ValidateWorkerJWT(s.config.JWTSecret, token)
		if err != nil {
			http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}

		ctx := r.Context()
		ctx = context.WithValue(ctx, ctxSessionID, claims.SessionID)
		ctx = context.WithValue(ctx, ctxWorkspaceID, claims.WorkspaceID)
		ctx = context.WithValue(ctx, ctxEpoch, claims.Epoch)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
