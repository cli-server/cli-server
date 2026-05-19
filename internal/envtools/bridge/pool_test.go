package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// newFakePoolServer returns an http.Handler that accepts a relay-speaking
// fake exec-server per /<exe_id> path and counts dials per exe_id.
type fakePoolServer struct {
	srv       *httptest.Server
	dialsByID sync.Map // exe_id → *atomic.Int64
}

func (f *fakePoolServer) dialsFor(exeID string) int64 {
	if v, ok := f.dialsByID.Load(exeID); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}

func newFakePoolServer(t *testing.T) *fakePoolServer {
	t.Helper()
	f := &fakePoolServer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// path = "/<exe_id>"
		exeID := strings.TrimPrefix(r.URL.Path, "/")
		ctr, _ := f.dialsByID.LoadOrStore(exeID, new(atomic.Int64))
		ctr.(*atomic.Int64).Add(1)
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		runFakeRelayLoop(r.Context(), c, func(req JSONRPCMessage) *JSONRPCMessage {
			if req.ID == nil {
				return nil
			}
			// Echo initialize → ExecInitializeResult{}
			if req.Method == ExecMethodInitialize {
				body, _ := json.Marshal(ExecInitializeResult{SessionID: "fake"})
				return &JSONRPCMessage{JSONRPC: "2.0", ID: req.ID, Result: body}
			}
			return &JSONRPCMessage{JSONRPC: "2.0", ID: req.ID, Result: req.Params}
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePoolServer) wsBase() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http")
}

func TestBridgePool_DialsOncePerExeID(t *testing.T) {
	f := newFakePoolServer(t)
	pool := NewPool(f.wsBase(), "tok", nil)
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		if _, err := pool.Get(ctx, "exe_a"); err != nil {
			t.Fatalf("Get(exe_a)[%d]: %v", i, err)
		}
	}
	if _, err := pool.Get(ctx, "exe_b"); err != nil {
		t.Fatalf("Get(exe_b): %v", err)
	}
	if f.dialsFor("exe_a") != 1 {
		t.Errorf("exe_a dials = %d, want 1", f.dialsFor("exe_a"))
	}
	if f.dialsFor("exe_b") != 1 {
		t.Errorf("exe_b dials = %d, want 1", f.dialsFor("exe_b"))
	}
}

func TestBridgePool_RedialsAfterClose(t *testing.T) {
	f := newFakePoolServer(t)
	pool := NewPool(f.wsBase(), "tok", nil)
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c1, err := pool.Get(ctx, "exe_a")
	if err != nil {
		t.Fatalf("Get 1: %v", err)
	}
	c1.Close()
	// Give the readLoop a moment to observe close.
	time.Sleep(50 * time.Millisecond)

	c2, err := pool.Get(ctx, "exe_a")
	if err != nil {
		t.Fatalf("Get 2: %v", err)
	}
	if c2 == c1 {
		t.Error("Get returned the dead connection")
	}
	if f.dialsFor("exe_a") != 2 {
		t.Errorf("dials = %d, want 2 (redial after close)", f.dialsFor("exe_a"))
	}
}

func TestBridgePool_ParallelGetSameID(t *testing.T) {
	f := newFakePoolServer(t)
	pool := NewPool(f.wsBase(), "tok", nil)
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := pool.Get(ctx, "exe_p"); err != nil {
				t.Errorf("parallel Get: %v", err)
			}
		}()
	}
	wg.Wait()
	// One dial winner is the floor; race-losers may also dial then drop,
	// but we hold the lock for the de-dupe so at most 10 dials happen
	// (and typically just 1–3). The strong invariant is "only one entry
	// in the pool", which redials in subsequent Gets would expose.
	dials := f.dialsFor("exe_p")
	if dials < 1 || dials > 10 {
		t.Errorf("dials = %d, want 1..10", dials)
	}
	// After races settle, the pool has exactly one entry that's still alive.
	pool.mu.Lock()
	n := len(pool.conns)
	pool.mu.Unlock()
	if n != 1 {
		t.Errorf("pool entries = %d, want 1", n)
	}
}

func TestBridgePool_EmptyExeIDErrors(t *testing.T) {
	pool := NewPool("ws://example.invalid/bridge", "tok", nil)
	if _, err := pool.Get(context.Background(), ""); err == nil {
		t.Fatal("Get(\"\") should error")
	}
}
