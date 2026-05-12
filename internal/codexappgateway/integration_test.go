//go:build integration

package codexappgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/auth"
	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
	"github.com/agentserver/agentserver/internal/codexappgateway/supervisor"

	"nhooyr.io/websocket"
)

// TestServer_RealCodexAppServer_FullRPCRoundtrip is opt-in: build with
// `-tags integration`. Requires `codex` (>= 0.130.0) on PATH.
func TestServer_RealCodexAppServer_FullRPCRoundtrip(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not on PATH")
	}

	root := t.TempDir()
	store := makeFakeStore(t) // from server_testhelper_test.go
	mgr := codexhome.NewManager(root)
	sup := supervisor.NewSupervisor(supervisor.SupervisorConfig{
		CodexBin: "codex",
		HomeMgr:  mgr,
		Store:    store,
	})
	t.Cleanup(func() { sup.ShutdownAll(context.Background()) })

	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	s := &Server{
		cfg:     ServeConfig{InboundHMACSecret: []byte("int")},
		auth:    auth.NewHMAC([]byte("int")),
		sup:     sup,
		homeMgr: mgr,
		logger:  logger,
		buildConfig: func(_ context.Context, ws, thr string) (codexhome.ConfigInput, error) {
			return codexhome.ConfigInput{
				ModelProvider: "modelserver",
				Model:         "gpt-5.5",
				ModelProviders: map[string]codexhome.ModelProvider{
					"modelserver": {Name: "modelserver", BaseURL: "https://code.ai.cs.ac.cn/v1", EnvKey: "OPENAI_API_KEY", WireAPI: "responses"},
				},
				ProjectTrustedPaths: []string{"/tmp"},
			}, nil
		},
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	tok := auth.NewHMAC([]byte("int")).Mint("ws_int", "thr_1")
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/codex-app/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + tok}},
		// codex app-server doesn't accept permessage-deflate.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	send := func(payload string) {
		if err := c.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	expectReply := func(label string) map[string]any {
		_, raw, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("%s read: %v", label, err)
		}
		var resp map[string]any
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("%s decode: %v", label, err)
		}
		if resp["error"] != nil {
			t.Fatalf("%s errored: %v", label, resp["error"])
		}
		return resp
	}

	send(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"int","title":"int","version":"0"},"capabilities":{"experimentalApi":true,"requestAttestation":false,"optOutNotificationMethods":[]}}}`)
	resp := expectReply("initialize")
	if resp["id"] == nil {
		t.Fatalf("no id in initialize reply: %v", resp)
	}
	t.Logf("initialize reply ok: %v", resp["result"])

	send(`{"method":"initialized"}`)
	send(`{"id":2,"method":"thread/start","params":{}}`)
	resp = expectReply("thread/start")
	t.Logf("thread/start ok: %v", resp["result"])
}
