package codexexecgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/relaypb"
	"google.golang.org/protobuf/proto"
	"nhooyr.io/websocket"
)

// wsPair stands up a tiny httptest server that ws-accepts a single
// connection and returns the server-side *websocket.Conn plus a
// client-side *websocket.Conn (dialed by the helper).
//
// The accepted server-side ws is exposed via a channel so the test can
// inject it into newInboundConn / newBridgeSession before driving
// reaper behavior.
//
// Cleanup uses CloseNow (no close handshake) so test teardown doesn't
// block on the peer reading the close frame.
func wsPair(t *testing.T) (server *websocket.Conn, client *websocket.Conn) {
	t.Helper()
	srvCh := make(chan *websocket.Conn, 1)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("ws accept: %v", err)
			return
		}
		srvCh <- ws
		// Park here so the http handler doesn't return (which would close
		// the ws). The test cancels via ws.Close from either side OR via
		// httptest.Server's shutdown (which cancels r.Context()).
		<-r.Context().Done()
	}))
	t.Cleanup(hs.Close)

	wsURL := "ws" + hs.URL[len("http"):]
	c, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })

	select {
	case s := <-srvCh:
		t.Cleanup(func() { _ = s.CloseNow() })
		return s, c
	case <-time.After(2 * time.Second):
		t.Fatal("server-side ws never arrived")
		return nil, nil
	}
}

// TestIdleReaper_ClosesIdleBridgeAndSendsReset verifies that when a bridge
// session has no activity for longer than BridgeIdleTimeout, the reaper:
//  1. Sends a RelayMessageFrame{body: Reset{reason:"idle-timeout"}} on the
//     inbound ws (so the executor's relay layer tears down its per-stream
//     JSON-RPC session), AND
//  2. Closes the bridge ws (so the env-mcp child sees connection closed).
//
// TDD trace:
//   - RED:  bridgeSession.lastActivity / touch() / startIdleReaper don't
//     exist → compile error. Or, once helpers exist but reaper isn't
//     wired, the bridge ws stays open forever and the inbound side never
//     receives a Reset frame → test times out / fails.
//   - GREEN: after wiring lastActivity + touch() + startIdleReaper + the
//     "spawn reaper" hook in handleInbound, both assertions pass.
func TestIdleReaper_ClosesIdleBridgeAndSendsReset(t *testing.T) {
	// Set up a real ws pair for the inbound so we can read the Reset
	// frame the reaper writes. Set up a separate real ws pair for the
	// bridge so we can observe it closing.
	inboundServerWS, inboundClientWS := wsPair(t)
	bridgeServerWS, bridgeClientWS := wsPair(t)

	exeID := "exe_idle"
	streamID := "stream_idle"
	ic := newInboundConn(exeID, inboundServerWS, nil, 16*1024*1024)
	session := newBridgeSession(streamID, ic, bridgeServerWS)
	if evicted := ic.addRoute(streamID, session); evicted != nil {
		t.Fatalf("unexpected eviction: %v", evicted)
	}

	// Start the reaper with a short timeout. The reaper runs in its own
	// goroutine; we feed it a fresh ctx tied to this test.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	idle := 150 * time.Millisecond
	go ic.startIdleReaper(ctx, idle)

	// Read on the inbound client side. Should get a Reset frame within
	// timeout*~2 (one tick interval = timeout/4, then on next tick the
	// session is idle past the cutoff).
	readCtx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	mt, data, err := inboundClientWS.Read(readCtx)
	if err != nil {
		t.Fatalf("inbound read failed: %v", err)
	}
	if mt != websocket.MessageBinary {
		t.Fatalf("inbound got non-binary frame: %v", mt)
	}
	var frame relaypb.RelayMessageFrame
	if err := proto.Unmarshal(data, &frame); err != nil {
		t.Fatalf("unmarshal reset frame: %v", err)
	}
	if frame.StreamId != streamID {
		t.Errorf("reset frame stream_id = %q, want %q", frame.StreamId, streamID)
	}
	reset, ok := frame.Body.(*relaypb.RelayMessageFrame_Reset_)
	if !ok {
		t.Fatalf("frame body is not Reset: %T", frame.Body)
	}
	if reset.Reset_.Reason != "idle-timeout" {
		t.Errorf("reset reason = %q, want %q", reset.Reset_.Reason, "idle-timeout")
	}

	// The bridge ws should have been closed by the reaper. Wait for the
	// closed channel on the session OR a read error on bridgeClientWS.
	select {
	case <-session.closed:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("session.closed never fired after idle reap")
	}
	// And on the bridge client side, a Read should return an error
	// (since the server-side ws is closed).
	readCtx2, readCancel2 := context.WithTimeout(context.Background(), 1*time.Second)
	defer readCancel2()
	if _, _, err := bridgeClientWS.Read(readCtx2); err == nil {
		t.Error("bridge client Read should have errored after reaper closed bridge ws")
	}

	// And the route should be unregistered.
	if _, ok := ic.lookup(streamID); ok {
		t.Errorf("route for %q still present after reap", streamID)
	}
}

// TestIdleReaper_PreservesActiveBridge: a bridge that touches activity
// periodically (faster than the idle timeout) is NOT reaped, even after
// waiting longer than the idle timeout.
func TestIdleReaper_PreservesActiveBridge(t *testing.T) {
	inboundServerWS, inboundClientWS := wsPair(t)
	bridgeServerWS, _ := wsPair(t)

	exeID := "exe_active"
	streamID := "stream_active"
	ic := newInboundConn(exeID, inboundServerWS, nil, 16*1024*1024)
	session := newBridgeSession(streamID, ic, bridgeServerWS)
	if evicted := ic.addRoute(streamID, session); evicted != nil {
		t.Fatalf("unexpected eviction: %v", evicted)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	idle := 150 * time.Millisecond
	go ic.startIdleReaper(ctx, idle)

	// Touch the session every 50ms for ~400ms (well past idle timeout).
	// Reaper must NOT reap it.
	touchCtx, touchCancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer touchCancel()
	for {
		session.touch()
		select {
		case <-touchCtx.Done():
			goto done
		case <-time.After(50 * time.Millisecond):
		}
	}
done:

	// Bridge should still be alive.
	select {
	case <-session.closed:
		t.Fatal("active session was reaped despite touches")
	default:
	}

	// And the route should still be registered.
	if _, ok := ic.lookup(streamID); !ok {
		t.Error("route was removed despite active touches")
	}

	// And no Reset frame should have been written to the inbound. Use a
	// short read timeout — Read should NOT succeed.
	noReadCtx, noReadCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer noReadCancel()
	if _, _, err := inboundClientWS.Read(noReadCtx); err == nil {
		t.Error("inbound should not have received a Reset frame for an active session")
	}
}

// TestIdleReaper_DisabledWhenTimeoutZero: BridgeIdleTimeout <= 0 means
// reaper returns immediately and does not periodically scan.
func TestIdleReaper_DisabledWhenTimeoutZero(t *testing.T) {
	inboundServerWS, _ := wsPair(t)
	bridgeServerWS, _ := wsPair(t)

	exeID := "exe_disabled"
	streamID := "stream_disabled"
	ic := newInboundConn(exeID, inboundServerWS, nil, 16*1024*1024)
	session := newBridgeSession(streamID, ic, bridgeServerWS)
	ic.addRoute(streamID, session)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Disabled: zero timeout. startIdleReaper must return early without
	// scanning. We verify by waiting some time, then checking the session
	// is still alive (even though its lastActivity is unset).
	done := make(chan struct{})
	go func() {
		ic.startIdleReaper(ctx, 0)
		close(done)
	}()
	select {
	case <-done:
		// reaper exited immediately — good
	case <-time.After(500 * time.Millisecond):
		t.Fatal("startIdleReaper(0) should return immediately")
	}

	// Session must still be alive.
	select {
	case <-session.closed:
		t.Fatal("session was closed despite reaper being disabled")
	default:
	}
}

// TestIdleReaper_ExitsOnContextCancel: a running reaper exits cleanly
// when its ctx is cancelled. No goroutine leak.
func TestIdleReaper_ExitsOnContextCancel(t *testing.T) {
	inboundServerWS, _ := wsPair(t)

	ic := newInboundConn("exe_ctx", inboundServerWS, nil, 16*1024*1024)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ic.startIdleReaper(ctx, 100*time.Millisecond)
		close(done)
	}()

	// Let it spin once.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// exited — good
	case <-time.After(1 * time.Second):
		t.Fatal("reaper did not exit after context cancel")
	}
}

// TestIdleReaper_ExitsOnInboundClose: a running reaper exits cleanly
// when the inbound's closed channel fires.
func TestIdleReaper_ExitsOnInboundClose(t *testing.T) {
	inboundServerWS, inboundClientWS := wsPair(t)

	ic := newInboundConn("exe_close", inboundServerWS, nil, 16*1024*1024)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		ic.startIdleReaper(ctx, 100*time.Millisecond)
		close(done)
	}()

	// Drain the client side so ic.close()'s ws.Close handshake can
	// complete (the close handshake requires the peer to read the close
	// frame and respond). Without this drain, ic.close() blocks on the
	// handshake forever.
	clientDrainCtx, clientDrainCancel := context.WithCancel(context.Background())
	t.Cleanup(clientDrainCancel)
	go func() {
		for {
			if _, _, err := inboundClientWS.Read(clientDrainCtx); err != nil {
				return
			}
		}
	}()

	// Let the reaper spin once.
	time.Sleep(50 * time.Millisecond)
	ic.close(nil)

	select {
	case <-done:
		// exited — good
	case <-time.After(2 * time.Second):
		t.Fatal("reaper did not exit after inbound.close()")
	}
}
