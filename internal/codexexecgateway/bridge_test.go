package codexexecgateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"nhooyr.io/websocket"
)

func mintBridgeToken(secret []byte, p CapPayload) string {
	header := []byte(`{"alg":"HS256","typ":"CXG"}`)
	pj, _ := json.Marshal(p)
	enc := base64.RawURLEncoding
	si := enc.EncodeToString(header) + "." + enc.EncodeToString(pj)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(si))
	return si + "." + enc.EncodeToString(mac.Sum(nil))
}

// connectInbound registers an executor (db row + bcrypt hash), dials the
// inbound endpoint, and waits until the registry shows a live conn for exeID.
func connectInbound(t *testing.T, srv *Server, baseURL, exeID string) *websocket.Conn {
	t.Helper()
	hash, _ := bcrypt.GenerateFromPassword([]byte("rt"), bcrypt.DefaultCost)
	srv.store.CreateExecutor(context.Background(), Executor{
		ExeID: exeID, UserID: "u", RegisteredAt: time.Now().UTC(),
	}, string(hash))
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
	cfg := Config{CapTokenHMACSecret: []byte("k"), InternalSharedSecret: "s",
		PingInterval: time.Second, IdleTimeout: 10 * time.Second}
	// NewServer accepts a nil store; the bridge auth paths don't call it.
	srv := NewServer(cfg, nil)
	hs := httptest.NewServer(srv.Routes())
	t.Cleanup(hs.Close)
	return hs, srv
}

func TestBridge_Rejects401OnBadToken(t *testing.T) {
	hs, _ := newBridgeNoDBServer(t)
	url := "ws" + hs.URL[len("http"):] + "/bridge/exe_x?token=garbage"
	_, resp, err := websocket.Dial(context.Background(), url, nil)
	if err == nil {
		t.Fatal("dial should fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", resp)
	}
}

func TestBridge_Rejects403WhenExeIDNotInAllowList(t *testing.T) {
	hs, srv := newBridgeNoDBServer(t)
	now := time.Now().Unix()
	tok := mintBridgeToken(srv.config.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_1", WorkspaceID: "ws_1", ExeIDs: []string{"exe_other"},
		IAT: now, EXP: now + 60,
	})
	url := "ws" + hs.URL[len("http"):] + "/bridge/exe_target?token=" + tok
	_, resp, err := websocket.Dial(context.Background(), url, nil)
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
		TurnID: "trn_1", WorkspaceID: "ws_1", ExeIDs: []string{"exe_offline"},
		IAT: now, EXP: now + 60,
	})
	// exe_offline is not in the registry → 503
	url := "ws" + hs.URL[len("http"):] + "/bridge/exe_offline?token=" + tok
	_, resp, err := websocket.Dial(context.Background(), url, nil)
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
		TurnID: "trn_revoked", WorkspaceID: "ws_1", ExeIDs: []string{"exe_rev"},
		IAT: now, EXP: now + 60,
	})
	url := "ws" + hs.URL[len("http"):] + "/bridge/exe_rev?token=" + tok
	_, resp, err := websocket.Dial(context.Background(), url, nil)
	if err == nil {
		t.Fatal("dial should fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", resp)
	}
}

func TestBridge_PairsAndForwardsBidirectional(t *testing.T) {
	hs, srv := newInboundTestServer(t)
	inbound := connectInbound(t, srv, hs.URL, "exe_pair")
	defer inbound.Close(websocket.StatusNormalClosure, "")

	now := time.Now().Unix()
	tok := mintBridgeToken(srv.config.CapTokenHMACSecret, CapPayload{
		TurnID: "trn_1", WorkspaceID: "ws_1", ExeIDs: []string{"exe_pair"},
		IAT: now, EXP: now + 60,
	})
	url := "ws" + hs.URL[len("http"):] + "/bridge/exe_pair?token=" + tok
	bridge, _, err := websocket.Dial(context.Background(), url, nil)
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
