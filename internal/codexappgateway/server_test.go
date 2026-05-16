package codexappgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/auth"
	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
	"github.com/agentserver/agentserver/internal/codexappgateway/supervisor"

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
