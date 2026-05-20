package codexexecgateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if err := srv.store.BindWorkspaceExecutor(context.Background(), "ws_1", exeID, "test-"+exeID, "", false); err != nil {
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
	// Register a fake inbound so the revocation check is reached.
	srv.registry.Register("exe_rev", newInboundConn("exe_rev", nil, nil, 0))
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

// 409 Conflict is gone in v0.53.0: bridges multiplex by stream_id on
// one inbound, no longer serialised by an exe-wide mutex. The old
// TestBridge_Returns409WhenAnotherSessionActive test is removed;
// multi-bridge concurrency is covered by
// TestBridge_TwoConcurrentBridgesShareInbound below.

// v0.53.0: the bridge handler is no longer a transparent text-frame
// proxy. It parses incoming frames as RelayMessageFrame protobuf, the
// first frame must be Resume, and forwarding is gated on stream_id
// matching the session's. The tests below (PairsAndForwards,
// CloseFromBridge/Inbound, E2EByteFidelity) were written against the
// old transparent-forwarding model and have been removed. Coverage of
// the new behavior lives in:
//   - TestBridge_RejectsFirstFrameNonResume
//   - TestBridge_TwoConcurrentBridgesShareInbound (in multiplex_e2e_test.go)
//   - TestBridge_StreamIdCollisionEvictsFirst (in multiplex_e2e_test.go)

// ---------- stream-cap test helpers ----------

// dialBridgeWithStream opens a ws to /bridge/{exeID}, sends a Resume frame
// with the given streamID, and returns the open ws (caller must close).
// tok must be a valid cap-token for the server.
func dialBridgeWithStream(t *testing.T, baseURL, exeID, streamID, tok string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + "/bridge/" + exeID
	c, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + tok}},
	})
	if err != nil {
		t.Fatalf("dialBridgeWithStream(%s): %v", streamID, err)
	}
	// Send Resume frame with the given streamID (reuse sendResume from multiplex_e2e_test.go).
	sendResume(t, c, streamID)
	return c
}

// dialBridgeHTTPOnly sends a WebSocket upgrade request to /bridge/{exeID}
// but reads back the HTTP response instead of completing the handshake.
// This lets the test inspect the status code when the server rejects the
// request before upgrading (e.g. 503 for cap-exceeded).
func dialBridgeHTTPOnly(t *testing.T, baseURL, exeID, tok string) *http.Response {
	t.Helper()
	url := baseURL + "/bridge/" + exeID
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

	// Use a plain http.Client — it will NOT follow the 101 upgrade and
	// will return the raw response (including 4xx/5xx before upgrade).
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("dialBridgeHTTPOnly: %v", err)
	}
	return resp
}

// TestBridge_StreamCapEnforced verifies that when an executor already has
// MaxStreamsPerExecutor concurrent bridge sessions, a new dial is rejected
// with HTTP 503 + Retry-After header (Strategy A: pre-upgrade cap check).
//
// This test uses no database: it wires a fake inboundConn directly into
// the registry and pre-fills its routes map to simulate "cap reached".
// Because Strategy A rejects before websocket.Accept, the nil ws on the
// fake inbound is never touched.
//
// TDD trace:
//   - RED:  streamCount() doesn't exist → compile error; or cap check absent → 3rd dial succeeds (101).
//   - GREEN: after adding streamCount() + pre-upgrade check in bridge.go → 503 returned.
func TestBridge_StreamCapEnforced(t *testing.T) {
	cfg := Config{
		CapTokenHMACSecret:   []byte("k"),
		InternalSharedSecret: "s",
		MaxStreamsPerExecutor: 2,
	}
	srv, err := newServerNoStoreForTesting(cfg)
	if err != nil {
		t.Fatalf("newServerNoStoreForTesting: %v", err)
	}
	hs := httptest.NewServer(srv.Routes())
	t.Cleanup(hs.Close)

	exeID := "exe_cap"
	// Build a fake inboundConn with 2 pre-registered routes (simulates cap reached).
	fakeInbound := newInboundConn(exeID, nil, nil, 0)
	fakeInbound.addRoute("stream-0", newBridgeSession("stream-0", fakeInbound, nil))
	fakeInbound.addRoute("stream-1", newBridgeSession("stream-1", fakeInbound, nil))
	srv.registry.Register(exeID, fakeInbound)

	now := time.Now().Unix()
	tok := mintBridgeToken(cfg.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_cap", WorkspaceID: "ws_1", IAT: now, EXP: now + 60,
	})

	// The dial must be rejected pre-upgrade: Strategy A → HTTP 503 + Retry-After.
	resp := dialBridgeHTTPOnly(t, hs.URL, exeID, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("got status %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Errorf("missing Retry-After header on 503 response")
	}
}

// TestBridge_StreamCapDisabledWhenZero verifies that MaxStreamsPerExecutor=0
// means "disabled" — no cap is enforced even when the inbound already has routes.
// Uses no database; the 503 path (cap check) is what we're verifying is absent.
func TestBridge_StreamCapDisabledWhenZero(t *testing.T) {
	cfg := Config{
		CapTokenHMACSecret:   []byte("k"),
		InternalSharedSecret: "s",
		MaxStreamsPerExecutor: 0, // disabled
	}
	srv, err := newServerNoStoreForTesting(cfg)
	if err != nil {
		t.Fatalf("newServerNoStoreForTesting: %v", err)
	}
	hs := httptest.NewServer(srv.Routes())
	t.Cleanup(hs.Close)

	exeID := "exe_nocap"
	// Build a fake inboundConn with routes already present.
	fakeInbound := newInboundConn(exeID, nil, nil, 0)
	fakeInbound.addRoute("stream-0", newBridgeSession("stream-0", fakeInbound, nil))
	fakeInbound.addRoute("stream-1", newBridgeSession("stream-1", fakeInbound, nil))
	srv.registry.Register(exeID, fakeInbound)

	now := time.Now().Unix()
	tok := mintBridgeToken(cfg.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_nocap", WorkspaceID: "ws_1", IAT: now, EXP: now + 60,
	})

	// With cap=0 (disabled), the request must NOT be rejected with 503.
	// It will proceed to websocket.Accept — we send a proper ws dial so the
	// server can upgrade, then read the response. Since the fake inbound ws
	// is nil, the bridge will error out after upgrade, but we should NOT see
	// a 503. A 101 (upgrade) or any non-503 error confirms the cap is off.
	resp := dialBridgeHTTPOnly(t, hs.URL, exeID, tok)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		t.Errorf("got 503 with cap=0 (disabled); cap should not be enforced")
	}
}

