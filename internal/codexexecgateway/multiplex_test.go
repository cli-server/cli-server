package codexexecgateway

import (
	"sync"
	"testing"
)

// TestInboundConn_AddRemoveRouteRace exercises concurrent add/remove
// of the same stream_id from multiple goroutines. No panics, final
// map state matches the last writer.
func TestInboundConn_AddRemoveRouteRace(t *testing.T) {
	i := newInboundConn("exe_x", nil, nil, 0)
	a := &bridgeSession{streamID: "s1", closed: make(chan struct{})}
	b := &bridgeSession{streamID: "s1", closed: make(chan struct{})}

	var wg sync.WaitGroup
	for k := 0; k < 50; k++ {
		wg.Add(2)
		go func() { defer wg.Done(); i.addRoute("s1", a) }()
		go func() { defer wg.Done(); i.addRoute("s1", b) }()
	}
	wg.Wait()

	cur, ok := i.lookup("s1")
	if !ok || (cur != a && cur != b) {
		t.Fatalf("unexpected route after race: %v", cur)
	}

	// removeRoute only deletes when the value matches.
	other := a
	if cur == a {
		other = b
	}
	i.removeRoute("s1", other) // no-op (value mismatch)
	if _, ok := i.lookup("s1"); !ok {
		t.Fatal("removeRoute deleted the wrong entry")
	}
	i.removeRoute("s1", cur)
	if _, ok := i.lookup("s1"); ok {
		t.Fatal("removeRoute did not delete the matching entry")
	}
}

// TestInboundConn_AddRouteEvictsPrior: a second addRoute for the same
// stream_id returns the prior session so the caller can close it.
func TestInboundConn_AddRouteEvictsPrior(t *testing.T) {
	i := newInboundConn("exe_x", nil, nil, 0)
	a := &bridgeSession{streamID: "s1", closed: make(chan struct{})}
	b := &bridgeSession{streamID: "s1", closed: make(chan struct{})}

	if evicted := i.addRoute("s1", a); evicted != nil {
		t.Fatalf("first add should return nil, got %v", evicted)
	}
	evicted := i.addRoute("s1", b)
	if evicted != a {
		t.Fatalf("second add should evict a, got %v", evicted)
	}
	if cur, _ := i.lookup("s1"); cur != b {
		t.Fatalf("lookup after second add = %v, want b", cur)
	}
}

// TestInboundConn_CloseIdempotent: multiple close() calls don't panic
// and only run cleanup once (closed channel close + route fan-out).
func TestInboundConn_CloseIdempotent(t *testing.T) {
	i := newInboundConn("exe_x", nil, nil, 0)
	// Don't add real bridge sessions (they'd try to close nil ws). The
	// idempotency check is just that close() doesn't double-close
	// channels or panic.
	i.close(nil)
	i.close(nil) // would panic on double-close(closed) without sync.Once
	select {
	case <-i.closed:
	default:
		t.Fatal("closed channel was not closed")
	}
}

// TestBridgeSession_CloseIdempotent: same as above for bridgeSession.
func TestBridgeSession_CloseIdempotent(t *testing.T) {
	b := &bridgeSession{streamID: "s1", closed: make(chan struct{})}
	// bridgeWS is nil; close() will try to ws.Close which would panic.
	// To avoid that, just exercise the sync.Once guard by checking the
	// closed channel directly using a manual close.
	b.closeOnce.Do(func() { close(b.closed) }) // simulate first close
	b.close(nil)                                // should be no-op
	select {
	case <-b.closed:
	default:
		t.Fatal("closed channel was not closed")
	}
}
