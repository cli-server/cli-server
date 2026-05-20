package codexexecgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/relaypb"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/protobuf/proto"
	"nhooyr.io/websocket"
)

func newInboundTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	store := newTestStore(t)
	cfg := Config{CapTokenHMACSecret: []byte("k"), InternalSharedSecret: "s"}
	srv, err := NewServer(cfg, store)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	hs := httptest.NewServer(srv.Routes())
	t.Cleanup(hs.Close)
	return hs, srv
}

// newTestServerWithConfig mirrors newInboundTestServer but accepts a
// fully-constructed Config. The caller can override any field (e.g.
// MaxFrameBytes) while still getting a real DB-backed store.
func newTestServerWithConfig(t *testing.T, cfg Config) (*httptest.Server, *Server) {
	t.Helper()
	store := newTestStore(t)
	srv, err := NewServer(cfg, store)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	hs := httptest.NewServer(srv.Routes())
	t.Cleanup(hs.Close)
	return hs, srv
}

func TestInbound_RejectsBadToken(t *testing.T) {
	hs, srv := newInboundTestServer(t)
	ctx := context.Background()
	hash, _ := bcrypt.GenerateFromPassword([]byte("right_token"), bcrypt.DefaultCost)
	srv.store.CreateExecutor(ctx, Executor{
		ExeID: "exe_inb1", UserID: "u", RegisteredAt: time.Now().UTC(),
	}, string(hash))

	wsURL := "ws" + hs.URL[len("http"):] + "/codex-exec/exe_inb1?token=wrong"
	_, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil {
		t.Fatal("expected dial to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", resp)
	}
}

func TestInbound_AcceptsAndRegisters(t *testing.T) {
	hs, srv := newInboundTestServer(t)
	ctx := context.Background()
	hash, _ := bcrypt.GenerateFromPassword([]byte("good"), bcrypt.DefaultCost)
	srv.store.CreateExecutor(ctx, Executor{
		ExeID: "exe_inb2", UserID: "u", RegisteredAt: time.Now().UTC(),
	}, string(hash))

	wsURL := "ws" + hs.URL[len("http"):] + "/codex-exec/exe_inb2?token=good"
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// Wait for the handler to register; poll briefly.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := srv.registry.Lookup("exe_inb2"); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := srv.registry.Lookup("exe_inb2"); !ok {
		t.Fatal("registry should hold exe_inb2 after accept")
	}
}

func TestInbound_EvictsOldConn(t *testing.T) {
	hs, srv := newInboundTestServer(t)
	ctx := context.Background()
	hash, _ := bcrypt.GenerateFromPassword([]byte("tok"), bcrypt.DefaultCost)
	srv.store.CreateExecutor(ctx, Executor{
		ExeID: "exe_inb3", UserID: "u", RegisteredAt: time.Now().UTC(),
	}, string(hash))

	wsURL := "ws" + hs.URL[len("http"):] + "/codex-exec/exe_inb3?token=tok"

	// First connection.
	c1, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial c1: %v", err)
	}
	defer c1.Close(websocket.StatusNormalClosure, "")

	// Wait for first conn to register.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := srv.registry.Lookup("exe_inb3"); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Second connection should evict first.
	c2, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial c2: %v", err)
	}
	defer c2.Close(websocket.StatusNormalClosure, "")

	// Wait for eviction to complete — c1 should receive a close frame.
	// We detect this by trying to read from c1; it should get an error.
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, _, readErr := c1.Read(readCtx)
	if readErr == nil {
		t.Fatal("c1 should have been closed after eviction")
	}

	// c2 should be registered.
	if _, ok := srv.registry.Lookup("exe_inb3"); !ok {
		t.Fatal("registry should hold c2 for exe_inb3 after eviction")
	}
}

// TestInbound_RejectsOversizedFrame verifies that newInboundConn enforces
// maxFrameBytes so that a frame larger than the cap is rejected with close
// code 1009 (MessageTooBig).
//
// The test wires up a real ws pair via httptest so it exercises
// nhooyr/websocket's SetReadLimit enforcement without a database.
//
// TDD trace:
//   - RED:  compile fails because newInboundConn has no maxFrameBytes param.
//   - RED:  after adding the param but before adding ws.SetReadLimit, the
//           server accepts the oversized frame and Read succeeds → FAIL.
//   - GREEN: after ws.SetReadLimit(maxFrameBytes) is called inside
//            newInboundConn, Read returns close 1009 → PASS.
func TestInbound_RejectsOversizedFrame(t *testing.T) {
	const limitBytes int64 = 1024

	// serverIC will hold the inboundConn created by the server handler so
	// the test can drain it cleanly.
	var serverIC *inboundConn

	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		// newInboundConn must call ws.SetReadLimit(maxFrameBytes).
		ic := newInboundConn("exe_bigframe", ws, nil, limitBytes)
		serverIC = ic
		// Run the reader so the ws actually processes frames.
		for {
			if _, _, err := ic.ws.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	t.Cleanup(hs.Close)

	ctx := context.Background()
	wsURL := "ws" + hs.URL[len("http"):]
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "test done")

	// Build a valid RelayMessageFrame with a payload well above the 1 KiB cap.
	big := make([]byte, 2048)
	frame := &relaypb.RelayMessageFrame{
		Version:  1,
		StreamId: "test-stream",
		Body: &relaypb.RelayMessageFrame_Data{Data: &relaypb.RelayData{
			Seq:          1,
			SegmentIndex: 0,
			SegmentCount: 1,
			Payload:      big,
		}},
	}
	b, err := proto.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	if int64(len(b)) <= limitBytes {
		t.Fatalf("test frame is not large enough: %d bytes (need > %d)", len(b), limitBytes)
	}

	// Write may return an error on its own, or the close arrives on Read.
	_ = c.Write(ctx, websocket.MessageBinary, b)

	// Read should return a close with code 1009 (MessageTooBig).
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, _, rerr := c.Read(readCtx)
	if rerr == nil {
		t.Fatal("expected ws.Read to fail after oversized frame")
	}
	if got := websocket.CloseStatus(rerr); got != websocket.StatusMessageTooBig {
		t.Errorf("close code: got %d, want %d (MessageTooBig); err=%v",
			got, websocket.StatusMessageTooBig, rerr)
	}
	_ = serverIC // referenced to avoid "declared and not used"
}
