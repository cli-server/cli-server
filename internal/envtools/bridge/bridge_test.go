package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/relaypb"
	"google.golang.org/protobuf/proto"
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
		runFakeRelayLoop(r.Context(), c, echoJSONRPC)
	}))
	return f
}

// echoJSONRPC turns each request into a reply whose result == params.
// Notifications (id==nil) get no reply.
func echoJSONRPC(req JSONRPCMessage) *JSONRPCMessage {
	if req.ID == nil {
		return nil
	}
	return &JSONRPCMessage{JSONRPC: "2.0", ID: req.ID, Result: req.Params}
}

// runFakeRelayLoop reads binary relay frames, decodes Data payloads as
// JSON-RPC, hands them to `handle`, and wraps each non-nil reply back
// in a Data frame on the same stream_id. Used by all envmcp tests as
// the server side of the relay-wrapped exec-server protocol.
func runFakeRelayLoop(ctx context.Context, c *websocket.Conn, handle func(JSONRPCMessage) *JSONRPCMessage) {
	var streamID string
	var nextSeq uint32
	for {
		mt, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if mt != websocket.MessageBinary {
			continue
		}
		var frame relaypb.RelayMessageFrame
		if err := proto.Unmarshal(data, &frame); err != nil {
			continue
		}
		if streamID == "" {
			streamID = frame.StreamId
		}
		dataBody, ok := frame.Body.(*relaypb.RelayMessageFrame_Data)
		if !ok || dataBody.Data == nil {
			continue
		}
		var req JSONRPCMessage
		if err := json.Unmarshal(dataBody.Data.Payload, &req); err != nil {
			continue
		}
		resp := handle(req)
		if resp == nil {
			continue
		}
		respBytes, _ := json.Marshal(resp)
		outFrame := &relaypb.RelayMessageFrame{
			Version:  1,
			StreamId: streamID,
			Body: &relaypb.RelayMessageFrame_Data{
				Data: &relaypb.RelayData{
					Seq: nextSeq, SegmentIndex: 0, SegmentCount: 1, Payload: respBytes,
				},
			},
		}
		nextSeq++
		outBytes, _ := proto.Marshal(outFrame)
		_ = c.Write(ctx, websocket.MessageBinary, outBytes)
	}
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

	bc, err := DialBridge(ctx, f.wsURL(), "tok-123", nil)
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
	bc, err := DialBridge(ctx, f.wsURL(), "", nil)
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
	bc, err := DialBridge(ctx, f.wsURL(), "", nil)
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
		// Drain the Resume frame (sent by DialBridge) + the Data frame
		// carrying the actual Call request, then close without replying.
		_, _, _ = c.Read(r.Context()) // Resume
		_, _, _ = c.Read(r.Context()) // Data (Call request)
		_ = c.Close(websocket.StatusGoingAway, "server closing")
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bc, err := DialBridge(ctx, wsURL, "", nil)
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
	bc, err := DialBridge(dialCtx, wsURL, "", nil)
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
		runFakeRelayLoop(r.Context(), c, func(req JSONRPCMessage) *JSONRPCMessage {
			if req.ID == nil {
				return nil
			}
			return &JSONRPCMessage{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &JSONRPCError{Code: -32601, Message: "method not found"},
			}
		})
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bc, err := DialBridge(ctx, wsURL, "", nil)
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
	bc, err := DialBridge(ctx, f.wsURL(), "", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	bc.Close()
	bc.Close() // must not panic, must not block
}
