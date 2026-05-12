package codexappgateway

import (
	"bytes"
	"context"
	"io"
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

	authHelper := auth.NewHMAC([]byte("test-secret"))
	tok := authHelper.Mint("ws_a", "thr_1")
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

func TestServer_AdminRestart_KillsSubprocess(t *testing.T) {
	srv := makeTestServer(t)
	defer srv.Close()

	authHelper := auth.NewHMAC([]byte("test-secret"))
	tok := authHelper.Mint("ws_b", "thr_42")
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/codex-app/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + tok}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.Close(websocket.StatusNormalClosure, "")

	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/admin/threads/restart",
		strings.NewReader(`{"workspaceId":"ws_b","threadId":"thr_42"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, body = %s", resp.StatusCode, body)
	}
}

func makeTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	bin := makeFakeCodex(t)
	store := makeFakeStore(t)
	mgr := codexhome.NewManager(t.TempDir())
	sup := supervisor.NewSupervisor(supervisor.SupervisorConfig{CodexBin: bin, HomeMgr: mgr, Store: store})
	t.Cleanup(func() { sup.ShutdownAll(context.Background()) })

	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	srv := &Server{
		cfg:     ServeConfig{InboundHMACSecret: []byte("test-secret")},
		auth:    auth.NewHMAC([]byte("test-secret")),
		sup:     sup,
		homeMgr: mgr,
		logger:  logger,
		buildConfig: func(_ context.Context, ws, thr string) (codexhome.ConfigInput, error) {
			return codexhome.ConfigInput{
				ModelProvider:  "p", Model: "m",
				ModelProviders: map[string]codexhome.ModelProvider{"p": {Name: "p", BaseURL: "http://x", EnvKey: "K", WireAPI: "responses"}},
			}, nil
		},
	}
	return httptest.NewServer(srv.Routes())
}
