package codexexecgateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"nhooyr.io/websocket"
)

// dialBridge dials the /bridge endpoint with the cap-token in the
// Authorization: Bearer header (matching the env-mcp child's wire shape).
func dialBridge(ctx context.Context, baseURL, exeID, tok string) (*websocket.Conn, *http.Response, error) {
	wsURL := "ws" + baseURL[len("http"):] + "/bridge/" + exeID
	return websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + tok}},
	})
}

func mintBridgeToken(secret []byte, p CapPayload) string {
	header := []byte(`{"alg":"HS256","typ":"CXG"}`)
	pj, _ := json.Marshal(p)
	enc := base64.RawURLEncoding
	si := enc.EncodeToString(header) + "." + enc.EncodeToString(pj)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(si))
	return si + "." + enc.EncodeToString(mac.Sum(nil))
}

// connectInbound registers an executor (db row + bcrypt hash + workspace
// binding to "ws_1" so bridge ownership checks pass), dials the inbound
// endpoint, and waits until the registry shows a live conn for exeID.
func connectInbound(t *testing.T, srv *Server, baseURL, exeID string) *websocket.Conn {
	t.Helper()
	hash, _ := bcrypt.GenerateFromPassword([]byte("rt"), bcrypt.DefaultCost)
	srv.store.CreateExecutor(context.Background(), Executor{
		ExeID: exeID, UserID: "u", RegisteredAt: time.Now().UTC(),
	}, string(hash))
	// Bind to ws_1 — all bridge tests mint tokens with WorkspaceID="ws_1".
	if err := srv.store.BindWorkspaceExecutor(context.Background(), "ws_1", exeID, false); err != nil {
		t.Fatalf("BindWorkspaceExecutor: %v", err)
	}
	url := "ws" + baseURL[len("http"):] + "/codex-exec/" + exeID + "?token=rt"
	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("inbound dial: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := srv.registry.Lookup(exeID); ok {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("inbound not registered for %s", exeID)
	return nil
}

// newBridgeNoDBServer returns a test server with no store — only routes that
// don't touch the DB can be exercised. Used for bridge auth-rejection tests
// that fail before any store lookup, allowing them to run without TEST_DATABASE_URL.
func newBridgeNoDBServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	cfg := Config{CapTokenHMACSecret: []byte("k"), InternalSharedSecret: "s"}
	srv, err := newServerNoStoreForTesting(cfg)
	if err != nil {
		t.Fatalf("newServerNoStoreForTesting: %v", err)
	}
	hs := httptest.NewServer(srv.Routes())
	t.Cleanup(hs.Close)
	return hs, srv
}

func TestBridge_Rejects401OnBadToken(t *testing.T) {
	hs, _ := newBridgeNoDBServer(t)
	_, resp, err := dialBridge(context.Background(), hs.URL, "exe_x", "garbage")
	if err == nil {
		t.Fatal("dial should fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", resp)
	}
}

func TestBridge_Rejects403WhenExeIDNotInWorkspace(t *testing.T) {
	// DB-backed: token's workspace_id has no binding to the URL's exe_id
	// → /bridge returns 403 via the workspace_executors ownership check.
	hs, srv := newInboundTestServer(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("rt"), bcrypt.DefaultCost)
	srv.store.CreateExecutor(context.Background(), Executor{
		ExeID: "exe_target", UserID: "u", RegisteredAt: time.Now().UTC(),
	}, string(hash))
	// Intentionally no BindWorkspaceExecutor — ws_1 does not own exe_target.
	now := time.Now().Unix()
	tok := mintBridgeToken(srv.config.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_1", WorkspaceID: "ws_1",
		IAT: now, EXP: now + 60,
	})
	_, resp, err := dialBridge(context.Background(), hs.URL, "exe_target", tok)
	if err == nil {
		t.Fatal("dial should fail")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %v", resp)
	}
}

func TestBridge_Rejects503WhenExecutorOffline(t *testing.T) {
	hs, srv := newBridgeNoDBServer(t)
	now := time.Now().Unix()
	tok := mintBridgeToken(srv.config.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_1", WorkspaceID: "ws_1",
		IAT: now, EXP: now + 60,
	})
	// exe_offline is not in the registry → 503
	_, resp, err := dialBridge(context.Background(), hs.URL, "exe_offline", tok)
	if err == nil {
		t.Fatal("dial should fail")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %v", resp)
	}
}

func TestBridge_RejectsRevokedTurn(t *testing.T) {
	hs, srv := newBridgeNoDBServer(t)
	// Register a fake inbound conn so the revocation check is reached.
	fakeConn := new(websocket.Conn)
	srv.registry.Register("exe_rev", fakeConn)
	now := time.Now().Unix()
	srv.revoked.Add("trn_revoked", now+60)
	tok := mintBridgeToken(srv.config.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_revoked", WorkspaceID: "ws_1",
		IAT: now, EXP: now + 60,
	})
	_, resp, err := dialBridge(context.Background(), hs.URL, "exe_rev", tok)
	if err == nil {
		t.Fatal("dial should fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", resp)
	}
}

func TestBridge_Returns409WhenAnotherSessionActive(t *testing.T) {
	hs, srv := newBridgeNoDBServer(t)
	// Register a fake inbound conn so the bridge mutex check is reached.
	fakeInbound := new(websocket.Conn)
	srv.registry.Register("exe_409", fakeInbound)
	// Manually acquire the bridge lock to simulate an active session.
	if !srv.registry.AcquireBridge("exe_409") {
		t.Fatal("setup: AcquireBridge should succeed on first call")
	}
	t.Cleanup(func() { srv.registry.ReleaseBridge("exe_409") })

	now := time.Now().Unix()
	tok := mintBridgeToken(srv.config.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_2", WorkspaceID: "ws_1",
		IAT: now, EXP: now + 60,
	})
	_, resp, err := dialBridge(context.Background(), hs.URL, "exe_409", tok)
	if err == nil {
		t.Fatal("dial should fail when another bridge session is active")
	}
	if resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409 Conflict, got %v", resp)
	}
}

func TestBridge_PairsAndForwardsBidirectional(t *testing.T) {
	hs, srv := newInboundTestServer(t)
	inbound := connectInbound(t, srv, hs.URL, "exe_pair")
	defer inbound.Close(websocket.StatusNormalClosure, "")

	now := time.Now().Unix()
	tok := mintBridgeToken(srv.config.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_1", WorkspaceID: "ws_1",
		IAT: now, EXP: now + 60,
	})
	bridge, _, err := dialBridge(context.Background(), hs.URL, "exe_pair", tok)
	if err != nil {
		t.Fatalf("bridge dial: %v", err)
	}
	defer bridge.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// bridge → inbound
	if err := bridge.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"initialize"}`)); err != nil {
		t.Fatalf("bridge.Write: %v", err)
	}
	mt, data, err := inbound.Read(ctx)
	if err != nil {
		t.Fatalf("inbound.Read: %v", err)
	}
	if mt != websocket.MessageText || string(data) != `{"id":1,"method":"initialize"}` {
		t.Fatalf("got mt=%v data=%q", mt, data)
	}

	// inbound → bridge
	if err := inbound.Write(ctx, websocket.MessageText, []byte(`{"id":1,"result":{}}`)); err != nil {
		t.Fatalf("inbound.Write: %v", err)
	}
	mt, data, err = bridge.Read(ctx)
	if err != nil {
		t.Fatalf("bridge.Read: %v", err)
	}
	if mt != websocket.MessageText || string(data) != `{"id":1,"result":{}}` {
		t.Fatalf("got mt=%v data=%q", mt, data)
	}
}

// Task 13: Frame-pump close & error propagation tests

func TestBridge_CloseFromBridgeSidePropagates(t *testing.T) {
	hs, srv := newInboundTestServer(t)
	inbound := connectInbound(t, srv, hs.URL, "exe_close1")
	defer inbound.Close(websocket.StatusInternalError, "test cleanup")

	now := time.Now().Unix()
	tok := mintBridgeToken(srv.config.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_1", WorkspaceID: "ws_1",
		IAT: now, EXP: now + 60,
	})
	beforeG := runtime.NumGoroutine()
	bridge, _, err := dialBridge(context.Background(), hs.URL, "exe_close1", tok)
	if err != nil {
		t.Fatalf("bridge dial: %v", err)
	}
	// Active pair: 2 pump goroutines + 1 handler goroutine on the server side.
	bridge.Close(websocket.StatusNormalClosure, "client done")

	// Give the pumps time to wind down.
	time.Sleep(200 * time.Millisecond)
	afterG := runtime.NumGoroutine()
	// Allow some scheduler slack: assert no NET growth of more than 1.
	if afterG > beforeG+1 {
		t.Fatalf("possible goroutine leak: before=%d after=%d", beforeG, afterG)
	}

	// Inbound conn must still be registered.
	if _, ok := srv.registry.Lookup("exe_close1"); !ok {
		t.Fatal("inbound should still be registered after bridge close")
	}
}

func TestBridge_CloseFromInboundSidePropagates(t *testing.T) {
	hs, srv := newInboundTestServer(t)
	inbound := connectInbound(t, srv, hs.URL, "exe_close2")

	now := time.Now().Unix()
	tok := mintBridgeToken(srv.config.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_1", WorkspaceID: "ws_1",
		IAT: now, EXP: now + 60,
	})
	bridge, _, err := dialBridge(context.Background(), hs.URL, "exe_close2", tok)
	if err != nil {
		t.Fatalf("bridge dial: %v", err)
	}
	defer bridge.Close(websocket.StatusInternalError, "test cleanup")

	// Close inbound; the bridge pump should observe and return.
	inbound.Close(websocket.StatusNormalClosure, "executor offline")

	// The bridge client should observe close within a short window.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err = bridge.Read(ctx)
	if err == nil {
		t.Fatal("bridge.Read should have errored after inbound close")
	}
}

// Task 14: End-to-end byte-fidelity test
//
// Verifies that the gateway preserves frame boundaries and byte contents
// across multiple frame types and sizes in both directions.

func TestBridge_E2EByteFidelity(t *testing.T) {
	hs, srv := newInboundTestServer(t)
	inbound := connectInbound(t, srv, hs.URL, "exe_e2e")
	defer inbound.Close(websocket.StatusNormalClosure, "")

	now := time.Now().Unix()
	tok := mintBridgeToken(srv.config.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_1", WorkspaceID: "ws_1",
		IAT: now, EXP: now + 60,
	})
	bridge, _, err := dialBridge(context.Background(), hs.URL, "exe_e2e", tok)
	if err != nil {
		t.Fatalf("bridge dial: %v", err)
	}
	defer bridge.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Bridge -> inbound: 5 distinct JSON-RPC text frames, each must arrive
	// as ONE frame (boundary preserved) in order.
	sent := []string{
		`{"id":1,"method":"initialize","params":{"clientName":"x"}}`,
		`{"id":1,"result":{}}`,
		`{"method":"initialized","params":{}}`,
		`{"id":2,"method":"process/start","params":{"processId":"p1","argv":["bash","-lc","echo hi"]}}`,
		`{"id":2,"result":{"processId":"p1"}}`,
	}
	for _, s := range sent {
		if err := bridge.Write(ctx, websocket.MessageText, []byte(s)); err != nil {
			t.Fatalf("bridge.Write %q: %v", s, err)
		}
	}
	for _, want := range sent {
		mt, data, err := inbound.Read(ctx)
		if err != nil {
			t.Fatalf("inbound.Read: %v", err)
		}
		if mt != websocket.MessageText {
			t.Fatalf("frame type drift: got %v want text", mt)
		}
		if string(data) != want {
			t.Fatalf("frame contents drift: got %q want %q", data, want)
		}
	}

	// Inbound -> bridge: large binary frame (64 KiB, well past nhooyr's 32 KiB
	// default read limit) round-trips intact. Without SetReadLimit(-1) on the
	// bridge conn this would close with status 1009 (message too large).
	big := make([]byte, 64*1024)
	for i := range big {
		big[i] = byte(i % 251)
	}
	if err := inbound.Write(ctx, websocket.MessageBinary, big); err != nil {
		t.Fatalf("inbound.Write big: %v", err)
	}
	mt, data, err := bridge.Read(ctx)
	if err != nil {
		t.Fatalf("bridge.Read big (want SetReadLimit(-1) in effect): %v", err)
	}
	if mt != websocket.MessageBinary || len(data) != len(big) {
		t.Fatalf("big frame: mt=%v len=%d", mt, len(data))
	}
	for i := range big {
		if data[i] != big[i] {
			t.Fatalf("byte %d: got %x want %x", i, data[i], big[i])
		}
	}
}
