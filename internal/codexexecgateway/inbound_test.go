package codexexecgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
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
