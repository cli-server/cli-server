package codexexecgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWorkspaceBinding_PostListDelete(t *testing.T) {
	store := newTestStore(t)
	srv, err := NewServer(Config{CapTokenHMACSecret: []byte("k"), InternalSharedSecret: "s"}, store)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// Pre-seed an executor.
	store.CreateExecutor(context.Background(), Executor{
		ExeID: "exe_w1", UserID: "u", Description: "alpha", RegisteredAt: time.Now().UTC(),
	}, "h")

	// POST
	body := bytes.NewReader([]byte(`{"exe_id":"exe_w1","is_default":true}`))
	req := httptest.NewRequest(http.MethodPost, "/api/codex-exec/workspaces/ws_a/executors", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST: want 201, got %d body=%s", rr.Code, rr.Body.String())
	}

	// GET
	req = httptest.NewRequest(http.MethodGet, "/api/codex-exec/workspaces/ws_a/executors", nil)
	rr = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET: want 200, got %d", rr.Code)
	}
	var got []ConnectedExecutor
	json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got) != 1 || got[0].ExeID != "exe_w1" || !got[0].IsDefault {
		t.Fatalf("GET body: %+v", got)
	}

	// DELETE
	req = httptest.NewRequest(http.MethodDelete, "/api/codex-exec/workspaces/ws_a/executors/exe_w1", nil)
	rr = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE: want 204, got %d", rr.Code)
	}

	// GET again — should be empty
	req = httptest.NewRequest(http.MethodGet, "/api/codex-exec/workspaces/ws_a/executors", nil)
	rr = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got) != 0 {
		t.Fatalf("after delete: %+v", got)
	}
}

func TestWorkspaceBinding_PostBadJSON(t *testing.T) {
	srv, err := NewServer(Config{CapTokenHMACSecret: []byte("test-hmac"), InternalSharedSecret: "test-internal"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/codex-exec/workspaces/ws_a/executors", bytes.NewReader([]byte(`!`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}
