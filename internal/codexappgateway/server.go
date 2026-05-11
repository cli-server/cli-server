package codexappgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/auth"
	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
	"github.com/agentserver/agentserver/internal/codexappgateway/proxy"
	"github.com/agentserver/agentserver/internal/codexappgateway/supervisor"

	"github.com/go-chi/chi/v5"
	"nhooyr.io/websocket"
)

// Server is the codex-app-gateway HTTP/WS server.
type Server struct {
	cfg     ServeConfig
	auth    auth.Authenticator
	sup     *supervisor.Supervisor
	homeMgr *codexhome.Manager
	logger  *slog.Logger

	// buildConfig produces the per-thread config.toml input. Allowed to
	// hit the network. Errors abort the spawn.
	buildConfig func(workspaceID, threadID string) (codexhome.ConfigInput, error)
}

// NewServer wires up the production server.
func NewServer(cfg ServeConfig, codexBin string, logger *slog.Logger) (*Server, error) {
	store, err := newS3Store(cfg.S3)
	if err != nil {
		return nil, fmt.Errorf("s3 store: %w", err)
	}
	mgr := codexhome.NewManager(cfg.TmpRoot)
	sup := supervisor.NewSupervisor(supervisor.SupervisorConfig{
		CodexBin: codexBin,
		HomeMgr:  mgr,
		Store:    store,
	})
	return &Server{
		cfg:     cfg,
		auth:    auth.NewHMAC(cfg.InboundHMACSecret),
		sup:     sup,
		homeMgr: mgr,
		logger:  logger,
		buildConfig: func(workspaceID, threadID string) (codexhome.ConfigInput, error) {
			// Phase-1 default: minimal config from env. Real exec-gw fetch
			// is wired in a follow-up task that reads CXG_EXEC_GATEWAY_*
			// and mints per-turn cap tokens; until then, no executors.
			return codexhome.ConfigInput{
				ModelProvider: "modelserver",
				Model:         "gpt-5.5",
				ModelProviders: map[string]codexhome.ModelProvider{
					"modelserver": {Name: "modelserver", BaseURL: "http://llmproxy:8085/v1", EnvKey: "CODEX_API_KEY", WireAPI: "responses"},
				},
			}, nil
		},
	}, nil
}

// Run serves HTTP until ctx is done.
func (s *Server) Run(ctx context.Context, listenAddr string) error {
	httpSrv := &http.Server{Addr: listenAddr, Handler: s.Routes()}
	reaper := supervisor.NewIdleReaper(s.sup, 1*time.Minute, s.cfg.IdleShutdown, s.logger)
	reaperCtx, reaperCancel := context.WithCancel(context.Background())
	defer reaperCancel()
	go reaper.Run(reaperCtx)

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		s.sup.ShutdownAll(shutdownCtx)
		return nil
	case err := <-errCh:
		s.sup.ShutdownAll(context.Background())
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Routes builds the chi router. Public for tests.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	r.Get("/codex-app/ws", s.handleCodexAppWS)
	r.Post("/admin/threads/restart", s.handleAdminRestart)
	return r
}

func (s *Server) handleCodexAppWS(w http.ResponseWriter, r *http.Request) {
	tok, ok := auth.ExtractBearer(r)
	if !ok {
		http.Error(w, "missing Bearer", http.StatusUnauthorized)
		return
	}
	id, err := s.auth.Verify(tok)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	userWS, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.logger.Warn("ws accept failed", "err", err)
		return
	}
	defer userWS.Close(websocket.StatusNormalClosure, "client closing")

	key := supervisor.Key{WorkspaceID: id.WorkspaceID, ThreadID: id.ThreadID}
	ctx := r.Context()
	handle, err := s.sup.EnsureSubprocess(ctx, key, func() (codexhome.ConfigInput, error) {
		return s.buildConfig(id.WorkspaceID, id.ThreadID)
	})
	if err != nil {
		s.logger.Error("ensure subprocess", "err", err, "key", key)
		_ = userWS.Close(websocket.StatusInternalError, "subprocess unavailable")
		return
	}

	childWS, _, err := websocket.Dial(ctx, handle.WSURL, &websocket.DialOptions{
		// codex app-server rejects connections that request permessage-deflate.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.logger.Error("dial child", "err", err, "url", handle.WSURL)
		_ = userWS.Close(websocket.StatusInternalError, "subprocess dial failed")
		return
	}
	defer childWS.Close(websocket.StatusNormalClosure, "gateway closing")

	s.sup.Touch(key)
	if err := proxy.RunProxy(ctx, userWS, childWS, func() { s.sup.Touch(key) }); err != nil {
		s.logger.Info("proxy ended", "err", err, "key", key)
	}
}

// handleAdminRestart shuts down the codex app-server subprocess for a
// given (workspaceId, threadId), forcing a fresh spawn (and S3 reload)
// on the next ws connect. Used by operators after executor-binding
// changes; see spec § Subsystem 2 "Per-turn config refresh".
//
// AUTHORIZATION (phase 1): the bearer token's identity is checked only
// to authenticate the caller as a valid token holder. The (workspaceId,
// threadId) to restart is taken from the request body, allowing
// cross-thread restarts by any authenticated caller. This matches the
// operator-scoped intent of an admin endpoint. Phase 2 may tighten to
// require token-identity == body-identity for self-service restarts.
func (s *Server) handleAdminRestart(w http.ResponseWriter, r *http.Request) {
	tok, ok := auth.ExtractBearer(r)
	if !ok {
		http.Error(w, "missing Bearer", http.StatusUnauthorized)
		return
	}
	if _, err := s.auth.Verify(tok); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var body struct {
		WorkspaceID string `json:"workspaceId"`
		ThreadID    string `json:"threadId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if body.WorkspaceID == "" || body.ThreadID == "" {
		http.Error(w, "workspaceId and threadId required", http.StatusBadRequest)
		return
	}
	if err := s.sup.Shutdown(r.Context(), supervisor.Key{WorkspaceID: body.WorkspaceID, ThreadID: body.ThreadID}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
