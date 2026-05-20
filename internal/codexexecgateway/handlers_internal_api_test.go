package codexexecgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

)

func TestInternalConnected_RequiresSharedSecret(t *testing.T) {
	srv, err := newServerNoStoreForTesting(Config{CapTokenHMACSecret: []byte("test-hmac"), InternalSharedSecret: "test-internal"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/exec-gateway/connected?workspace_id=ws_a", nil)
	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
}

func TestInternalConnected_ReturnsIntersection(t *testing.T) {
	store := newTestStore(t)
	srv, err := NewServer(Config{CapTokenHMACSecret: []byte("k"), InternalSharedSecret: "s3cret"}, store)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	hs := httptest.NewServer(srv.Routes())
	t.Cleanup(hs.Close)

	// Seed: two executors bound to workspace, one connected.
	for _, e := range []Executor{
		{ExeID: "exe_on", UserID: "u", Description: "online", DefaultCwd: "/x", RegisteredAt: time.Now().UTC()},
		{ExeID: "exe_off", UserID: "u", Description: "offline", DefaultCwd: "/y", RegisteredAt: time.Now().UTC()},
	} {
		store.CreateExecutor(context.Background(), e, "h")
		store.BindWorkspaceExecutor(context.Background(), "ws_a", e.ExeID, e.ExeID, "", e.ExeID == "exe_on")
	}
	srv.registry.Register("exe_on", newInboundConn("exe_on", nil, nil, 0))

	req, _ := http.NewRequest(http.MethodGet,
		hs.URL+"/api/exec-gateway/connected?workspace_id=ws_a", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var got []ConnectedExecutor
	json.NewDecoder(resp.Body).Decode(&got)
	if len(got) != 1 || got[0].ExeID != "exe_on" {
		t.Fatalf("intersection: %+v", got)
	}
}
