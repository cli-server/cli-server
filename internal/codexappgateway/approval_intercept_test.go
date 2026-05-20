package codexappgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/auth"
	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
	"github.com/agentserver/agentserver/internal/codexappgateway/supervisor"

	"nhooyr.io/websocket"
)

// TestHandleCodexAppWS_ApprovalIntercept verifies the transparent ws
// path auto-replies to server-pushed approval requests without forwarding
// them to the caller.
func TestHandleCodexAppWS_ApprovalIntercept(t *testing.T) {
	// fakeAppServer: plays the role of `codex app-server`.
	// On any ws connection: push an approval request, read back the response.
	approvalReceived := make(chan json.RawMessage, 1)
	fakeAppServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Logf("fake app-server accept: %v", err)
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "done")

		// Push an approval request immediately.
		req := []byte(`{"jsonrpc":"2.0","id":1,"method":"item/commandExecution/requestApproval","params":{"command":"ls"}}`)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := ws.Write(ctx, websocket.MessageText, req); err != nil {
			t.Logf("fake app-server write approval: %v", err)
			return
		}

		// Expect the gateway's filter to reply.
		_, body, err := ws.Read(ctx)
		if err != nil {
			t.Logf("fake app-server read reply: %v", err)
			return
		}
		approvalReceived <- body
		// Stay open so the gateway proxy doesn't see an unexpected close.
		<-ctx.Done()
	}))
	defer fakeAppServer.Close()

	// Stand up the gateway server whose "subprocess" relays to fakeAppServer.
	srv := newTestServerWithFakeChild(t, fakeAppServer.URL)
	defer srv.Close()

	// Connect a client to the gateway's /codex-app/ws endpoint.
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/codex-app/ws"
	clientWS, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer ast_dummytoken_anything"}},
	})
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer clientWS.Close(websocket.StatusNormalClosure, "done")

	// 1. The gateway must synthesize a response and send it to the fake app-server.
	select {
	case body := <-approvalReceived:
		var resp struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      int             `json:"id"`
			Result  json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("response not JSON: %v\nbody=%s", err, body)
		}
		if resp.ID != 1 {
			t.Errorf("response id: got %d, want 1", resp.ID)
		}
		if string(resp.Result) != `{"decision":"accept"}` {
			t.Errorf("response result: got %s, want {\"decision\":\"accept\"}", resp.Result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not synthesize approval response within 2s")
	}

	// 2. The client MUST NOT see the approval request (filter drops it).
	clientCtx, clientCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer clientCancel()
	_, payload, rerr := clientWS.Read(clientCtx)
	if rerr == nil {
		t.Errorf("client unexpectedly received a frame (approval should be intercepted): %s", payload)
	}
	// Context deadline exceeded is the expected outcome — no frame arrived.
}

// newTestServerWithFakeChild stands up a gateway Server whose subprocess
// (fake codex) relays websocket connections to childHTTPURL (an httptest
// server URL). Authentication accepts any token with the "ast_" prefix.
//
// The caller must call Close() on the returned server when done.
func newTestServerWithFakeChild(t *testing.T, childHTTPURL string) *httptest.Server {
	t.Helper()

	// Build a fake codex binary that relays ws connections to childHTTPURL.
	bin := makeFakeCodexRelay(t, childHTTPURL)

	store := makeFakeStore(t)
	mgr := codexhome.NewManager(t.TempDir())
	sup := supervisor.NewSupervisor(supervisor.SupervisorConfig{
		CodexBin: bin,
		HomeMgr:  mgr,
		Store:    store,
	})
	t.Cleanup(func() { sup.ShutdownAll(context.Background()) })

	// Fake agentserver: any "ast_" token → (u_test, ws_test).
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
		buildConfig: func(_ context.Context, _, _ string) (supervisor.SpawnConfig, error) {
			return supervisor.SpawnConfig{Config: codexhome.ConfigInput{
				ModelProvider:  "p",
				Model:          "m",
				ModelProviders: map[string]codexhome.ModelProvider{"p": {Name: "p", BaseURL: "http://x", EnvKey: "K", WireAPI: "responses"}},
			}}, nil
		},
	}
	return httptest.NewServer(srv.Routes())
}

// makeFakeCodexRelay compiles a tiny Go program that:
//  1. Listens on a random port.
//  2. Prints "ws://127.0.0.1:PORT" to stdout (supervisor scans for this).
//  3. Serves /readyz → 200 (supervisor polls this before use).
//  4. Relays any other request (including WS upgrades) to relayTarget via
//     httputil.ReverseProxy (stdlib; supports WebSocket in Go 1.20+).
func makeFakeCodexRelay(t *testing.T, relayTarget string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")

	// relayTarget is an http:// URL; we replace the scheme with http so
	// httputil.ReverseProxy forwards WS upgrade requests correctly.
	program := `package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
)

func main() {
	relayTo := os.Getenv("WS_RELAY_TARGET")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	addr := l.Addr().(*net.TCPAddr)
	fmt.Printf("ws://%s\n", addr.String())

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	if relayTo != "" {
		target, err := url.Parse(relayTo)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad relay target:", err)
			os.Exit(1)
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		mux.Handle("/", proxy)
	}

	_ = http.Serve(l, mux)
}
`

	if err := os.WriteFile(src, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fake-codex-relay")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	// Build from the module root so the binary can use stdlib packages.
	// The source uses only stdlib so no go.mod is needed in the temp dir.
	out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput()
	if err != nil {
		t.Fatalf("build fake-codex-relay: %v\n%s", err, out)
	}

	// Inject the relay target via SupervisorConfig.ExtraEnv. We can't do
	// that here (we return a binary path, not a supervisor config), so we
	// embed the target URL into the binary's name via a wrapper script.
	// Instead, the caller must ensure the env var is available. Since the
	// supervisor passes os.Environ() to the subprocess, and we set the env
	// var on the current process before spawning, we set it now.
	//
	// Note: setting os.Setenv here is test-scoped because each test runs in
	// its own process. A t.Cleanup restores the previous value.
	prev := os.Getenv("WS_RELAY_TARGET")
	if err := os.Setenv("WS_RELAY_TARGET", relayTarget); err != nil {
		t.Fatalf("setenv WS_RELAY_TARGET: %v", err)
	}
	t.Cleanup(func() { _ = os.Setenv("WS_RELAY_TARGET", prev) })

	return bin
}
