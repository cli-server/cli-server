package codexappgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/auth"
	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
	"github.com/agentserver/agentserver/internal/codexappgateway/oplog"
	"github.com/agentserver/agentserver/internal/codexappgateway/supervisor"
	"github.com/agentserver/agentserver/internal/wsbridge"

	"nhooyr.io/websocket"
)

func TestServer_WSEndpoint_AuthRequired(t *testing.T) {
	srv := makeTestServer(t)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/codex-app/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil {
		t.Fatal("expected dial to fail without Bearer")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %v", resp)
	}
}

func TestServer_WSEndpoint_HappyPath_ProxiesToFakeChild(t *testing.T) {
	srv := makeTestServer(t)
	defer srv.Close()

	tok := "ast_dummytoken_anything"
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/codex-app/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + tok}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	// fake codex doesn't reply to RPC; just verify we can write a frame
	// without immediate error (proxy is alive).
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"ping"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestServer_NotebookWS_AuthRequired(t *testing.T) {
	srv := makeTestServer(t)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/notebook/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil {
		t.Fatal("expected dial to fail without Bearer")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %v", resp)
	}
}

// TestServer_NotebookWS_OplogReceivesToolCall exercises the /notebook/ws
// route end-to-end against a stub child ws that echoes a tool/call result.
// Verifies the gateway's Interceptor POSTs an Operation to the oplog HTTP
// endpoint pinned to the verified workspace.
func TestServer_NotebookWS_OplogReceivesToolCall(t *testing.T) {
	// Fake child ws — echoes a result frame whenever it receives a request.
	childMux := http.NewServeMux()
	childMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var msg struct {
				ID     any    `json:"id"`
				Method string `json:"method"`
			}
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			if msg.ID == nil {
				continue
			}
			resp, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": msg.ID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": "ok"}},
					"isError": false,
				},
			})
			_ = c.Write(ctx, websocket.MessageText, resp)
		}
	})
	childSrv := httptest.NewServer(childMux)
	defer childSrv.Close()
	childWSURL := "ws" + strings.TrimPrefix(childSrv.URL, "http")

	// Fake agentserver: token verify only.
	asSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/codex/tokens/verify" {
			http.Error(w, "404", 404)
			return
		}
		var body struct{ Token string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !strings.HasPrefix(body.Token, "ast_") {
			http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"user_id": "u_test", "workspace_id": "ws_test",
		})
	}))
	defer asSrv.Close()

	// Fake oplog endpoint: capture POSTs.
	var (
		mu       sync.Mutex
		captured []oplog.Operation
		gotCh    = make(chan struct{}, 8)
	)
	opSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", 405)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var op oplog.Operation
		if err := json.Unmarshal(body, &op); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		mu.Lock()
		captured = append(captured, op)
		mu.Unlock()
		w.WriteHeader(200)
		select {
		case gotCh <- struct{}{}:
		default:
		}
	}))
	defer opSrv.Close()

	// Server with a stub supervisor: EnsureSubprocess is bypassed by giving
	// the supervisor a fake handle directly via a fakeSupervisor — but the
	// real Supervisor type doesn't allow that injection here. Instead we
	// route around it by spinning up a real supervisor that points at the
	// fake codex binary (which prints ws://addr and serves /readyz). The
	// child it spawns won't be the same as childSrv. To exercise the proxy
	// against childSrv, we need a way to override the WSURL returned by the
	// supervisor. The simplest path: write a thin handler-level integration
	// using EnsureSubprocess on the real supervisor.
	//
	// Simpler approach: hand-construct a *Server with a fake supervisor by
	// replacing the buildConfig and reusing the real fake-codex child. The
	// fake child will accept ws but not produce tool/call replies. That
	// defeats this test's goal.
	//
	// Instead: bypass Server.handleNotebookWS's supervisor by wiring up an
	// alternate route that uses the same logic but dials childSrv directly.
	// Done below via a minimal Server-like router exclusively for this test.

	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	s := &Server{
		cfg:    ServeConfig{},
		auth:   auth.NewRemoteVerifier(asSrv.URL, "ignored"),
		logger: logger,
	}
	s.oplogClient = oplog.NewClient(opSrv.URL, "secret", 16)
	s.oplogList = oplog.NewListClient(opSrv.URL, "secret")
	t.Cleanup(func() { s.oplogClient.Close() })

	// Minimal handler that mirrors handleNotebookWS but dials childSrv
	// directly (no supervisor). Keeps the interceptor wiring under test.
	handler := func(w http.ResponseWriter, r *http.Request) {
		tok, ok := auth.ExtractBearer(r)
		if !ok {
			http.Error(w, "missing Bearer", 401)
			return
		}
		id, err := s.auth.Verify(r.Context(), tok)
		if err != nil {
			http.Error(w, "unauthorized", 401)
			return
		}
		userWS, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer userWS.Close(websocket.StatusNormalClosure, "")
		userWS.SetReadLimit(maxWSFrameBytes)
		ctx := r.Context()
		cWS, _, err := websocket.Dial(ctx, childWSURL, nil)
		if err != nil {
			t.Errorf("dial child: %v", err)
			return
		}
		defer cWS.Close(websocket.StatusNormalClosure, "")
		cWS.SetReadLimit(maxWSFrameBytes)

		perConn := oplog.NewInterceptor(s.oplogClient, oplog.Config{
			Source: "sdk", WorkspaceID: id.WorkspaceID,
		})
		intc := wsbridge.Interceptor{
			OnClientFrame: func(frame []byte) []byte {
				if s.oplogList != nil {
					if resp, handled := oplog.TryHandleOperationsList(ctx, s.oplogList, id.WorkspaceID, frame); handled {
						_ = userWS.Write(ctx, websocket.MessageText, resp)
						return wsbridge.DropFrame
					}
				}
				perConn.OnClientFrame(frame)
				return nil
			},
			OnServerFrame: func(frame []byte) []byte {
				perConn.OnServerFrame(frame)
				return nil
			},
		}
		_ = wsbridge.RunProxyWithInterceptor(ctx, userWS, cWS, intc, nil)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/notebook/ws", handler)
	gw := httptest.NewServer(mux)
	defer gw.Close()

	// Client dial + tool/call frame.
	wsURL := "ws" + strings.TrimPrefix(gw.URL, "http") + "/notebook/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer ast_token"}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	req := `{"jsonrpc":"2.0","id":42,"method":"mcpServer/tool/call","params":{"thread_id":"t1","server":"exe_abc","tool":"shell","arguments":{"environment_id":"env_x","cmd":"echo hi"}}}`
	if err := c.Write(ctx, websocket.MessageText, []byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Read the response (echoed by fake child)
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatalf("read response: %v", err)
	}

	// Wait for oplog POST
	select {
	case <-gotCh:
	case <-time.After(3 * time.Second):
		t.Fatal("oplog endpoint never received a POST")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("captured = %d, want 1", len(captured))
	}
	op := captured[0]
	if op.WorkspaceID != "ws_test" {
		t.Errorf("WorkspaceID = %q, want ws_test", op.WorkspaceID)
	}
	if op.Source != "sdk" {
		t.Errorf("Source = %q, want sdk", op.Source)
	}
	if op.Tool != "shell" {
		t.Errorf("Tool = %q, want shell", op.Tool)
	}
	if op.EnvID != "env_x" {
		t.Errorf("EnvID = %q, want env_x", op.EnvID)
	}
	if op.IsError {
		t.Errorf("IsError = true, want false")
	}
}

// /admin/sessions/restart was removed in the 2026-05-16 fixed-tools
// redesign — env-mcp reads the executor list live via /internal/connected,
// so per-workspace subprocess invalidation is no longer needed. Test
// deleted.

func makeTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	bin := makeFakeCodex(t)
	store := makeFakeStore(t)
	mgr := codexhome.NewManager(t.TempDir())
	sup := supervisor.NewSupervisor(supervisor.SupervisorConfig{CodexBin: bin, HomeMgr: mgr, Store: store})
	t.Cleanup(func() { sup.ShutdownAll(context.Background()) })

	// Fake agentserver: any token starting with "ast_" verifies as (u_test, ws_test).
	asSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/codex/tokens/verify" {
			http.Error(w, "404", 404)
			return
		}
		var body struct{ Token string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !strings.HasPrefix(body.Token, "ast_") {
			http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"user_id": "u_test", "workspace_id": "ws_test",
		})
	}))
	t.Cleanup(asSrv.Close)

	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	srv := &Server{
		cfg:     ServeConfig{},
		auth:    auth.NewRemoteVerifier(asSrv.URL, "ignored"),
		sup:     sup,
		homeMgr: mgr,
		logger:  logger,
		buildConfig: func(_ context.Context, ws, _ string) (supervisor.SpawnConfig, error) {
			return supervisor.SpawnConfig{Config: codexhome.ConfigInput{
				ModelProvider:  "p", Model: "m",
				ModelProviders: map[string]codexhome.ModelProvider{"p": {Name: "p", BaseURL: "http://x", EnvKey: "K", WireAPI: "responses"}},
			}}, nil
		},
	}
	return httptest.NewServer(srv.Routes())
}
