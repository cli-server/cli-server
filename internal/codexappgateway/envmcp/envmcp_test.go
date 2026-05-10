package envmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// fakeBridgeServer answers initialize, then a single process/start +
// process/read with canned stdout, then closes.
func fakeBridgeServer(t *testing.T, wantAuth string, sawAuth *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*sawAuth = r.Header.Get("Authorization")
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		exit := 0
		read := ProcessReadResult{
			Chunks: []ProcessOutputChunk{
				{Seq: 1, Stream: "stdout", Chunk: "ZmFrZS1vdXQ="}, // "fake-out"
			},
			NextSeq:  2,
			Exited:   true,
			ExitCode: &exit,
			Closed:   true,
		}
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var msg JSONRPCMessage
			_ = json.Unmarshal(data, &msg)
			if msg.ID == nil {
				continue
			}
			var resp JSONRPCMessage
			resp.JSONRPC = "2.0"
			resp.ID = msg.ID
			switch msg.Method {
			case ExecMethodInitialize:
				out, _ := json.Marshal(ExecInitializeResult{SessionID: "fake-session"})
				resp.Result = out
			case ExecMethodProcessStart:
				out, _ := json.Marshal(ProcessStartResult{ProcessID: "p1"})
				resp.Result = out
			case ExecMethodProcessRead:
				out, _ := json.Marshal(read)
				resp.Result = out
			default:
				resp.Error = &JSONRPCError{Code: -32601, Message: "no"}
			}
			payload, _ := json.Marshal(&resp)
			_ = c.Write(ctx, websocket.MessageText, payload)
		}
	}))
}

func TestRun_EndToEnd(t *testing.T) {
	var sawAuth string
	srv := fakeBridgeServer(t, "Bearer fake-tok", &sawAuth)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	t.Setenv("CXG_TEST_TOKEN", "fake-tok")

	in := bytes.NewBufferString(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"shell","arguments":{"command":["echo","x"]}}}`,
		"",
	}, "\n"))
	out := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(stderr, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := Run(ctx, RunArgs{
		ExeID:     "exe_test",
		BridgeURL: wsURL,
		TokenEnv:  "CXG_TEST_TOKEN",
		ExeDesc:   "Test executor",
	}, in, out, stderr, logger)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	if sawAuth != "Bearer fake-tok" {
		t.Errorf("Authorization seen by bridge = %q", sawAuth)
	}
	if !strings.Contains(out.String(), "fake-out") {
		t.Errorf("MCP stdout missing translated output: %q", out.String())
	}
}

func TestRun_EmptyToken_Errors(t *testing.T) {
	t.Setenv("CXG_TEST_TOKEN", "")
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	err := Run(context.Background(), RunArgs{
		ExeID:     "x",
		BridgeURL: "ws://127.0.0.1:1",
		TokenEnv:  "CXG_TEST_TOKEN",
		ExeDesc:   "x",
	}, &bytes.Buffer{}, &bytes.Buffer{}, &bytes.Buffer{}, logger)
	if err == nil || !strings.Contains(err.Error(), "CXG_TEST_TOKEN") {
		t.Fatalf("want empty-token error, got %v", err)
	}
}
