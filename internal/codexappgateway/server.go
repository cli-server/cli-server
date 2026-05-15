package codexappgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/auth"
	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
	"github.com/agentserver/agentserver/internal/codexappgateway/supervisor"
	"github.com/agentserver/agentserver/internal/codexexecgateway/execmodel"
	"github.com/agentserver/agentserver/internal/shortid"
	"github.com/agentserver/agentserver/internal/wsbridge"

	"github.com/go-chi/chi/v5"
	"nhooyr.io/websocket"
)

// connectedClient is the subset of *ExecGatewayClient buildConfig needs.
// Defined here so tests can stub it without spinning up an HTTP server.
type connectedClient interface {
	Connected(ctx context.Context, workspaceID string) ([]execmodel.ConnectedExecutor, error)
}

// Server is the codex-app-gateway HTTP/WS server.
type Server struct {
	cfg     ServeConfig
	auth    auth.Authenticator
	sup     *supervisor.Supervisor
	homeMgr *codexhome.Manager
	logger  *slog.Logger

	// buildConfig produces the per-session config.toml input. Allowed to
	// hit the network. Errors abort the spawn.
	buildConfig func(ctx context.Context, workspaceID string) (codexhome.ConfigInput, error)
}

// NewServer wires up the production server. selfBin is the absolute path
// to the codex-app-gateway binary itself, used as the `command =` for
// each per-executor `[mcp_servers.exe_*]` entry (codex spawns it as the
// env-mcp child).
func NewServer(cfg ServeConfig, codexBin, selfBin string, logger *slog.Logger) (*Server, error) {
	store, err := newS3Store(cfg.S3)
	if err != nil {
		return nil, fmt.Errorf("s3 store: %w", err)
	}
	mgr := codexhome.NewManager(cfg.TmpRoot)
	supEnv := []string{}
	if cfg.CodexAPIKey != "" && cfg.ModelProviderEnvKey != "" {
		supEnv = append(supEnv, cfg.ModelProviderEnvKey+"="+cfg.CodexAPIKey)
	}
	sup := supervisor.NewSupervisor(supervisor.SupervisorConfig{
		CodexBin: codexBin,
		HomeMgr:  mgr,
		Store:    store,
		ExtraEnv: supEnv,
		Logger:   logger,
	})
	execClient := NewExecGatewayClient(cfg.ExecGatewayInternalURL, cfg.ExecGatewayInternalSecret)
	s := &Server{
		cfg:     cfg,
		auth:    auth.NewRemoteVerifier(cfg.AgentserverInternalURL, cfg.AgentserverInternalSecret),
		sup:     sup,
		homeMgr: mgr,
		logger:  logger,
	}
	s.buildConfig = makeBuildConfig(cfg, execClient, selfBin, logger)
	return s, nil
}

// makeBuildConfig returns the per-spawn ConfigInput producer. Split out
// so server_test.go can construct a Server with a stub connectedClient.
func makeBuildConfig(cfg ServeConfig, client connectedClient, selfBin string, logger *slog.Logger) func(context.Context, string) (codexhome.ConfigInput, error) {
	return func(ctx context.Context, workspaceID string) (codexhome.ConfigInput, error) {
		executors, err := client.Connected(ctx, workspaceID)
		if err != nil {
			// Fail-soft: a spawn with no executors still gives the user a
			// working chat — the model just can't trigger remote tools.
			// Production should alert on this rather than silently degrade,
			// hence the warn-level log; we still proceed.
			logger.Warn("execgw: connected fetch failed; spawning with no executors",
				"workspace_id", workspaceID, "err", err)
			executors = nil
		}
		entries := make([]codexhome.ExecutorEntry, 0, len(executors))
		// One token per executor per turn. turn_id ties them together so
		// /api/exec-gateway/revoke-turn cancels them as a unit.
		turnID := "trn_" + shortid.Generate()
		ttl := cfg.CapTokenTTL
		if ttl <= 0 {
			ttl = time.Hour
		}
		for _, e := range executors {
			tok, err := MintCapToken(cfg.CapTokenHMACSecret, turnID, workspaceID, e.ExeID, ttl)
			if err != nil {
				return codexhome.ConfigInput{}, fmt.Errorf("mint cap token for %s: %w", e.ExeID, err)
			}
			entries = append(entries, codexhome.ExecutorEntry{
				ID:        e.ExeID,
				BridgeURL: strings.TrimRight(cfg.ExecGatewayWSURL, "/") + "/bridge/" + e.ExeID,
				TokenEnv:  "CXG_BRIDGE_TOKEN_" + strings.ToUpper(strings.ReplaceAll(e.ExeID, "-", "_")),
				TokenVal:  tok,
				Desc:      e.Description,
				CodexBin:  selfBin,
				TurnID:    turnID,
			})
		}
		trusted := cfg.ProjectTrustedPaths
		if len(trusted) == 0 {
			trusted = []string{"/tmp"}
		}
		return codexhome.ConfigInput{
			ModelProvider: cfg.ModelProvider,
			Model:         cfg.Model,
			ModelProviders: map[string]codexhome.ModelProvider{
				cfg.ModelProvider: {
					Name:    cfg.ModelProvider,
					BaseURL: cfg.ModelProviderBaseURL,
					EnvKey:  cfg.ModelProviderEnvKey,
					WireAPI: cfg.ModelProviderWireAPI,
				},
			},
			Executors:           entries,
			ProjectTrustedPaths: trusted,
		}, nil
	}
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
//
// Two paths serve the same handler for the inbound TUI ws upgrade:
//   - "/"             — required by upstream codex's --remote URL parser,
//                       which only accepts ws[s]://host:port and connects
//                       to "/" (no path component).
//   - "/codex-app/ws" — kept for direct in-cluster testing (curl, kubectl
//                       port-forward) and path-based ingress setups.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	r.Get("/", s.handleCodexAppWS)
	r.Get("/codex-app/ws", s.handleCodexAppWS)
	r.Post("/admin/sessions/restart", s.handleAdminRestart)
	return r
}

func (s *Server) handleCodexAppWS(w http.ResponseWriter, r *http.Request) {
	tok, ok := auth.ExtractBearer(r)
	if !ok {
		http.Error(w, "missing Bearer", http.StatusUnauthorized)
		return
	}
	id, err := s.auth.Verify(r.Context(), tok)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	userWS, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.logger.Warn("ws accept failed", "err", err)
		return
	}
	defer userWS.Close(websocket.StatusNormalClosure, "client closing")

	key := supervisor.Key{WorkspaceID: id.WorkspaceID}
	ctx := r.Context()
	handle, err := s.sup.EnsureSubprocess(ctx, key, func() (codexhome.ConfigInput, error) {
		return s.buildConfig(ctx, id.WorkspaceID)
	})
	if err != nil {
		s.logger.Error("ensure subprocess", "err", err, "key", key)
		_ = userWS.Close(websocket.StatusInternalError, "subprocess unavailable")
		return
	}

	childWS, _, err := websocket.Dial(ctx, handle.WSURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.logger.Error("dial child", "err", err, "url", handle.WSURL)
		_ = userWS.Close(websocket.StatusInternalError, "subprocess dial failed")
		return
	}
	defer childWS.Close(websocket.StatusNormalClosure, "gateway closing")

	s.sup.Touch(key)
	if err := wsbridge.RunProxy(ctx, userWS, childWS, func() { s.sup.Touch(key) }); err != nil {
		s.logger.Info("proxy ended", "err", err, "key", key)
	}
}

// handleAdminRestart shuts down the codex app-server subprocess for a
// given workspaceId, forcing a fresh spawn (and S3 reload) on the next
// ws connect. Used by operators after executor-binding changes; see spec
// § Subsystem 2 "Per-session config refresh".
func (s *Server) handleAdminRestart(w http.ResponseWriter, r *http.Request) {
	tok, ok := auth.ExtractBearer(r)
	if !ok {
		http.Error(w, "missing Bearer", http.StatusUnauthorized)
		return
	}
	if _, err := s.auth.Verify(r.Context(), tok); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		WorkspaceID string `json:"workspaceId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if body.WorkspaceID == "" {
		http.Error(w, "workspaceId required", http.StatusBadRequest)
		return
	}
	if err := s.sup.Shutdown(r.Context(), supervisor.Key{WorkspaceID: body.WorkspaceID}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
