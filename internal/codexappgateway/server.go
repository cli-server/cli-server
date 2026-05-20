package codexappgateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/approvalfilter"
	"github.com/agentserver/agentserver/internal/codexappgateway/auth"
	"github.com/agentserver/agentserver/internal/codexappgateway/broker"
	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
	"github.com/agentserver/agentserver/internal/codexappgateway/oplog"
	"github.com/agentserver/agentserver/internal/codexappgateway/supervisor"
	"github.com/agentserver/agentserver/internal/codexexecgateway/execmodel"
	"github.com/agentserver/agentserver/internal/clientmeta"
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
	cfg  ServeConfig
	auth auth.Authenticator
	sup  *supervisor.Supervisor
	homeMgr      *codexhome.Manager
	logger       *slog.Logger

	// buildConfig produces the per-spawn config + env vars (e.g. a
	// workspace-scoped LLM API key). Receives the per-spawn loopback
	// token so the agentserver MCP entry in config.toml can embed it.
	// Allowed to hit the network. Errors abort the spawn.
	buildConfig func(ctx context.Context, workspaceID, loopbackToken string) (supervisor.SpawnConfig, error)

	execClient connectedClient // exposed for the loopback /internal/connected handler

	// oplogClient is nil when OperationLogURL/Secret are empty.
	oplogClient *oplog.Client
	oplogList   *oplog.ListClient

	// brokerPool caches per-workspace broker.Conn instances (max idle 5 min).
	// Initialized in NewServer; nil in lightweight test Server literals.
	brokerPool *broker.Pool
}

// workspaceTokenFetcher is the subset of *WorkspaceTokenClient buildConfig
// needs. Defined here so tests can stub.
type workspaceTokenFetcher interface {
	GetOrCreate(ctx context.Context, workspaceID string) (string, error)
}

// maxWSFrameBytes bounds each ws read on the user-facing and
// app-server-facing connections. 64 MiB is well above any legitimate
// codex frame (conversation history + tool output) while still
// preventing a runaway or hostile client from pinning gateway memory.
const maxWSFrameBytes int64 = 64 << 20

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
	// Static fallback env: only used if the per-spawn ModelServer token
	// fetch returns empty (e.g. workspace hasn't connected ModelServer yet).
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
	wsTokenClient := NewWorkspaceTokenClient(cfg.AgentserverInternalURL, cfg.AgentserverInternalSecret)
	s := &Server{
		cfg:  cfg,
		auth: auth.NewRemoteVerifier(cfg.AgentserverInternalURL, cfg.AgentserverInternalSecret),
		sup:  sup,
		homeMgr:      mgr,
		logger:       logger,
		execClient:   execClient,
	}
	s.buildConfig = makeBuildConfig(cfg, execClient, wsTokenClient, selfBin, logger)
	s.brokerPool = broker.NewPool(
		makeSupervisorResolver(s.sup, s.buildConfig),
		5*time.Minute,
	)
	if cfg.OperationLogURL != "" && cfg.OperationLogSecret != "" {
		s.oplogClient = oplog.NewClient(cfg.OperationLogURL, cfg.OperationLogSecret, cfg.OperationLogChan)
	}
	return s, nil
}

// loopbackInternalURL turns a listen address like ":8086" or
// "0.0.0.0:8086" into the loopback URL env-mcp should call. Empty
// listenAddr yields empty result (codexhome will omit the agentserver
// MCP entry, useful for tests).
func loopbackInternalURL(listenAddr string) string {
	if listenAddr == "" {
		return ""
	}
	addr := listenAddr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	} else if strings.HasPrefix(addr, "0.0.0.0:") {
		addr = "127.0.0.1:" + strings.TrimPrefix(addr, "0.0.0.0:")
	}
	return "http://" + addr
}

// makeBuildConfig returns the per-spawn SpawnConfig producer. Split out
// so server_test.go can construct a Server with stub clients.
func makeBuildConfig(cfg ServeConfig, _ connectedClient, wsTokenClient workspaceTokenFetcher, selfBin string, logger *slog.Logger) func(context.Context, string, string) (supervisor.SpawnConfig, error) {
	return func(ctx context.Context, workspaceID, loopbackToken string) (supervisor.SpawnConfig, error) {
		// Per 2026-05-16 redesign, the executor list is no longer
		// fixed at spawn time — env-mcp reads it live via
		// /internal/connected. We still mint a per-spawn turn so
		// /api/exec-gateway/revoke-turn semantics survive.
		turnID := "trn_" + shortid.Generate()
		ttl := cfg.CapTokenTTL
		if ttl <= 0 {
			ttl = time.Hour
		}
		workspaceTok, err := MintCapToken(cfg.CapTokenHMACSecret, turnID, workspaceID, ttl)
		if err != nil {
			return supervisor.SpawnConfig{}, fmt.Errorf("mint workspace cap token: %w", err)
		}
		trusted := cfg.ProjectTrustedPaths
		if len(trusted) == 0 {
			trusted = []string{"/tmp"}
		}

		// Per-spawn env: fetch a workspace-scoped proxy token (long
		// lived, cached server-side). codex sends this as Bearer to
		// llmproxy, which validates and swaps it for a fresh
		// modelserver JWT per request — meaning OAuth refreshes
		// server-side reach the running pod without a respawn.
		var spawnEnv []string
		if cfg.ModelProviderEnvKey != "" {
			tok, err := wsTokenClient.GetOrCreate(ctx, workspaceID)
			if err != nil {
				logger.Warn("workspace-token: fetch failed; falling back to static CodexAPIKey",
					"workspace_id", workspaceID, "err", err)
			} else {
				spawnEnv = append(spawnEnv, cfg.ModelProviderEnvKey+"="+tok)
			}
		}

		return supervisor.SpawnConfig{
			Config: codexhome.ConfigInput{
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
				AgentServer: codexhome.AgentServerMCP{
					CodexBin:                  selfBin,
					WorkspaceID:               workspaceID,
					ExecGatewayURL:            strings.TrimRight(cfg.ExecGatewayWSURL, "/") + "/bridge",
					AppGatewayInternalURL:     loopbackInternalURL(cfg.ListenAddr),
					WorkspaceToken:            workspaceTok,
					LoopbackToken:             loopbackToken,
					ExecGatewayInternalURL:    cfg.ExecGatewayInternalURL,
					ExecGatewayInternalSecret: cfg.ExecGatewayInternalSecret,
				},
				ProjectTrustedPaths: trusted,
			},
			Env: spawnEnv,
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

	ln, err := wsbridge.ListenWithKeepAlive(ctx, "tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		s.sup.ShutdownAll(shutdownCtx)
		if s.oplogClient != nil {
			s.oplogClient.Close()
		}
		if s.brokerPool != nil {
			s.brokerPool.Close()
		}
		return nil
	case err := <-errCh:
		s.sup.ShutdownAll(context.Background())
		if s.oplogClient != nil {
			s.oplogClient.Close()
		}
		if s.brokerPool != nil {
			s.brokerPool.Close()
		}
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
	r.Get("/internal/connected", s.handleInternalConnected)
	turnHandler := &turnAPIHandler{
		runner: newPoolRunner(s.brokerPool),
	}
	r.With(s.requireInternalSecret).Post("/api/turns", turnHandler.ServeHTTP)
	return r
}

// requireInternalSecret is chi middleware that validates the
// X-Internal-Secret header against cfg.AgentserverInternalSecret.
func (s *Server) requireInternalSecret(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AgentserverInternalSecret == "" {
			http.Error(w, "internal secret not configured", http.StatusInternalServerError)
			return
		}
		if r.Header.Get("X-Internal-Secret") != s.cfg.AgentserverInternalSecret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Close releases per-server resources. Must be called on shutdown.
func (s *Server) Close() {
	if s.brokerPool != nil {
		s.brokerPool.Close()
	}
}

func (s *Server) handleCodexAppWS(w http.ResponseWriter, r *http.Request) {
	tok, ok := auth.ExtractBearer(r)
	if !ok {
		http.Error(w, "missing Bearer", http.StatusUnauthorized)
		return
	}

	// Prefer OpenSession when the authenticator supports it (RemoteVerifier
	// in production). HMAC (local-test only) falls through to plain Verify
	// and leaves sessionID empty so the deferred close is a no-op.
	clientIP := clientmeta.ClientIP(r)
	clientUA := r.Header.Get("User-Agent")
	codexVersion, osStr := clientmeta.ParseCodexUA(clientUA)
	var (
		id        auth.Identity
		sessionID string
		err       error
	)
	if tracker, ok := s.auth.(auth.SessionTracker); ok {
		id, sessionID, err = tracker.OpenSession(r.Context(), tok, clientIP, clientUA, codexVersion, osStr)
	} else {
		id, err = s.auth.Verify(r.Context(), tok)
	}
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if sessionID != "" {
		// Close session in the background — must not block ws shutdown.
		defer func() {
			go func(sid string) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if tracker, ok := s.auth.(auth.SessionTracker); ok {
					if cerr := tracker.CloseSession(ctx, sid); cerr != nil {
						s.logger.Warn("close session", "err", cerr, "session", sid)
					}
				}
			}(sessionID)
		}()
	}

	userWS, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.logger.Warn("ws accept failed", "err", err)
		return
	}
	// codex client streams large frames (tool listings, prompts, file
	// contents); nhooyr's 32 KiB default would slam the connection shut
	// mid-session with "read limited at 32769 bytes". 64 MiB is well
	// above any legitimate codex frame and still bounds a runaway
	// client.
	userWS.SetReadLimit(maxWSFrameBytes)
	defer userWS.Close(websocket.StatusNormalClosure, "client closing")

	key := supervisor.Key{WorkspaceID: id.WorkspaceID}
	ctx := r.Context()
	handle, err := s.sup.EnsureSubprocess(ctx, key, func(loopbackToken string) (supervisor.SpawnConfig, error) {
		return s.buildConfig(ctx, id.WorkspaceID, loopbackToken)
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
	childWS.SetReadLimit(maxWSFrameBytes)
	defer childWS.Close(websocket.StatusNormalClosure, "gateway closing")

	s.sup.Touch(key)
	intc := wsbridge.Interceptor{
		OnServerFrame: func(frame []byte) []byte {
			if resp, ok := approvalfilter.TryReply(frame); ok {
				// Auto-accept: write the synthesized response back to upstream
				// and drop the request so the caller never sees it. Codex
				// expects server-to-client requests to be answered on the same
				// ws connection.
				if werr := childWS.Write(ctx, websocket.MessageText, resp); werr != nil {
					s.logger.Warn("approval-filter: write reply", "err", werr, "key", key)
				}
				return wsbridge.DropFrame
			}
			return nil
		},
	}

	if err := wsbridge.RunProxyWithInterceptor(ctx, userWS, childWS, intc, func() { s.sup.Touch(key) }); err != nil {
		s.logger.Info("proxy ended", "err", err, "key", key)
	}
}

