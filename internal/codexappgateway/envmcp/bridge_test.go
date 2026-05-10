package envmcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// fakeExecServer accepts one ws connection, echoes each JSON-RPC
// request as a result whose body is the request's params, and exposes
// the last Authorization header it saw.
type fakeExecServer struct {
	srv        *httptest.Server
	gotAuth    string
	connectErr error
}

func newFakeExecServer(t *testing.T) *fakeExecServer {
	t.Helper()
	f := &fakeExecServer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotAuth = r.Header.Get("Authorization")
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			f.connectErr = err
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var msg JSONRPCMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			if msg.ID == nil {
				continue // notification, no reply
			}
			resp := JSONRPCMessage{JSONRPC: "2.0", ID: msg.ID, Result: msg.Params}
			out, _ := json.Marshal(&resp)
			_ = c.Write(ctx, websocket.MessageText, out)
		}
	}))
	return f
}

func (f *fakeExecServer) wsURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http")
}

func (f *fakeExecServer) Close() { f.srv.Close() }

func TestBridgeClient_DialAndCall(t *testing.T) {
	f := newFakeExecServer(t)
	defer f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bc, err := DialBridge(ctx, f.wsURL(), "tok-123")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer bc.Close()

	if f.gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want %q", f.gotAuth, "Bearer tok-123")
	}

	res, err := bc.Call(ctx, "ping", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(res) != `{"x":1}` {
		t.Errorf("result = %s", res)
	}
}

func TestBridgeClient_Notify_NoReply(t *testing.T) {
	f := newFakeExecServer(t)
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bc, err := DialBridge(ctx, f.wsURL(), "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer bc.Close()
	if err := bc.Notify(ctx, "initialized", nil); err != nil {
		t.Fatalf("notify: %v", err)
	}
}

func TestBridgeClient_Call_AfterClose_Errors(t *testing.T) {
	f := newFakeExecServer(t)
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bc, err := DialBridge(ctx, f.wsURL(), "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	bc.Close()
	if _, err := bc.Call(ctx, "ping", nil); err == nil {
		t.Fatal("expected error after Close")
	}
}

// TestBridgeClient_Call_ServerClosesMidCall: server closes the ws connection
// while Call is blocked. Expected: Call returns with an error (no hang).
func TestBridgeClient_Call_ServerClosesMidCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// Read one frame, then close the ws server-side without replying.
		_, _, _ = c.Read(r.Context())
		_ = c.Close(websocket.StatusGoingAway, "server closing")
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bc, err := DialBridge(ctx, wsURL, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer bc.Close()

	if _, err := bc.Call(ctx, "ping", nil); err == nil {
		t.Fatal("expected error when server closes mid-call")
	}
}

// TestBridgeClient_Call_CtxCancel: caller cancels ctx while Call is blocked.
// Expected: Call returns context.Canceled (no hang).
func TestBridgeClient_Call_CtxCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// Hold the connection open without replying until the caller goes away.
		<-r.Context().Done()
		_ = c.Close(websocket.StatusGoingAway, "client gone")
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	bc, err := DialBridge(dialCtx, wsURL, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer bc.Close()

	callCtx, callCancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		callCancel()
	}()
	_, err = bc.Call(callCtx, "slow", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// TestBridgeClient_Call_ServerErrorResponse: server returns a JSON-RPC error response.
// Expected: Call returns an error whose message contains the server's message.
func TestBridgeClient_Call_ServerErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var req JSONRPCMessage
		_ = json.Unmarshal(data, &req)
		resp := JSONRPCMessage{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &JSONRPCError{Code: -32601, Message: "method not found"},
		}
		out, _ := json.Marshal(&resp)
		_ = c.Write(ctx, websocket.MessageText, out)
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bc, err := DialBridge(ctx, wsURL, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer bc.Close()

	_, err = bc.Call(ctx, "bogus", nil)
	if err == nil || !strings.Contains(err.Error(), "method not found") {
		t.Fatalf("want method-not-found error, got %v", err)
	}
}

// TestBridgeClient_Close_Idempotent: Close called twice in a row is safe and
// a no-op the second time.
func TestBridgeClient_Close_Idempotent(t *testing.T) {
	f := newFakeExecServer(t)
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bc, err := DialBridge(ctx, f.wsURL(), "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	bc.Close()
	bc.Close() // must not panic, must not block
}
