package oplog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHandleOperationsList_ForwardsAndReturns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("workspace_id") != "ws-1" {
			t.Fatalf("ws=%q", q.Get("workspace_id"))
		}
		if q.Get("limit") != "10" {
			t.Fatalf("limit=%q", q.Get("limit"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"operations": []map[string]any{
				{"id": "op_1", "env_id": "a", "tool": "shell", "is_error": false},
			},
		})
	}))
	defer srv.Close()

	lc := NewListClient(srv.URL+"/", "s")
	frame, ok := TryHandleOperationsList(context.Background(), lc, "ws-1",
		[]byte(`{"jsonrpc":"2.0","id":42,"method":"operations/list","params":{"limit":10}}`))
	if !ok {
		t.Fatal("not handled")
	}
	var resp struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(frame, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != 42 {
		t.Fatalf("id=%d", resp.ID)
	}
	if !strings.Contains(string(resp.Result), `"op_1"`) {
		t.Fatalf("result=%s", resp.Result)
	}
}

func TestTryHandleOperationsList_OtherMethodsIgnored(t *testing.T) {
	frame, ok := TryHandleOperationsList(context.Background(), nil, "ws",
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"mcpServer/tool/call"}`))
	if ok || frame != nil {
		t.Fatalf("should not handle")
	}
}

func TestTryHandleOperationsList_NotificationIgnored(t *testing.T) {
	frame, ok := TryHandleOperationsList(context.Background(), nil, "ws",
		[]byte(`{"jsonrpc":"2.0","method":"operations/list"}`))
	if ok || frame != nil {
		t.Fatalf("notifications (no id) should not be handled")
	}
}

func TestTryHandleOperationsList_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	lc := NewListClient(srv.URL+"/", "s")

	frame, ok := TryHandleOperationsList(context.Background(), lc, "ws",
		[]byte(`{"jsonrpc":"2.0","id":7,"method":"operations/list","params":{}}`))
	if !ok {
		t.Fatal("should handle, but produce error response")
	}
	var resp struct {
		ID    int `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(frame, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != 7 || resp.Error.Code == 0 {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestTryHandleOperationsList_ForwardsAllFilterParams(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = w.Write([]byte(`{"operations":[]}`))
	}))
	defer srv.Close()
	lc := NewListClient(srv.URL+"/", "s")

	body := `{"jsonrpc":"2.0","id":1,"method":"operations/list","params":{
		"limit":50,"env_id":"alpha","tool":"shell","source":"sdk",
		"is_error":true,"since":"2026-01-01T00:00:00Z","id":"op_99"}}`
	_, ok := TryHandleOperationsList(context.Background(), lc, "ws-A", []byte(body))
	if !ok {
		t.Fatal("not handled")
	}
	for k, want := range map[string]string{
		"workspace_id": "ws-A", "limit": "50",
		"env_id": "alpha", "tool": "shell", "source": "sdk",
		"is_error": "true", "since": "2026-01-01T00:00:00Z", "id": "op_99",
	} {
		if v := got.Get(k); v != want {
			t.Errorf("%s=%q want %q", k, v, want)
		}
	}
}
