package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveCodexGatewayRESTURL(t *testing.T) {
	cases := []struct {
		name     string
		restEnv  string
		urlEnv   string
		want     string
	}{
		// Explicit REST var wins.
		{"rest var set", "http://cxg.svc:8086", "ws://cxg.svc:8086/notebook/ws", "http://cxg.svc:8086"},
		{"rest var trims trailing slash", "http://cxg.svc:8086/", "", "http://cxg.svc:8086"},
		// Fallback derive from the legacy ws URL the chart emits.
		{"ws fallback strips notebook path + swaps scheme", "", "ws://cxg.svc:8086/notebook/ws", "http://cxg.svc:8086"},
		{"wss fallback", "", "wss://cxg.example.com/notebook/ws", "https://cxg.example.com"},
		{"ws fallback no path", "", "ws://cxg.svc:8086", "http://cxg.svc:8086"},
		// Already an http URL — pass through.
		{"http passthrough", "", "http://cxg.svc:8086", "http://cxg.svc:8086"},
		{"https passthrough", "", "https://cxg.example.com", "https://cxg.example.com"},
		// Unusable / empty.
		{"both empty", "", "", ""},
		{"unknown scheme", "", "tcp://cxg:8086", ""},
		{"whitespace only", "   ", "  ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CODEX_APP_GATEWAY_REST_URL", c.restEnv)
			t.Setenv("CODEX_APP_GATEWAY_URL", c.urlEnv)
			if got := resolveCodexGatewayRESTURL(); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestCodexClientPostsExpectedBody(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/turns" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.Header.Get("X-Internal-Secret") != "s3cret" {
			t.Errorf("missing secret")
		}
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"threadId":"thr-1","turn":{"id":"trn-1","status":"completed","items":[],"itemsView":"full","error":null}}`))
	}))
	defer srv.Close()

	c := NewCodexClient(srv.URL, "s3cret")
	resp, err := c.RunTurn(context.Background(), CodexTurnRequest{
		WorkspaceID: "ws-x",
		ThreadID:    nil,
		Params:      json.RawMessage(`{"input":[{"type":"text","text":"hi"}]}`),
		TimeoutMs:   30000,
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if resp.ThreadID != "thr-1" {
		t.Errorf("threadID=%q", resp.ThreadID)
	}

	var sent map[string]any
	_ = json.Unmarshal(gotBody, &sent)
	if sent["workspaceId"] != "ws-x" {
		t.Errorf("body workspaceId=%v", sent["workspaceId"])
	}
}

func TestCodexClientReturnsErrorOnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(502)
	}))
	defer srv.Close()
	c := NewCodexClient(srv.URL, "")
	_, err := c.RunTurn(context.Background(), CodexTurnRequest{
		WorkspaceID: "ws", Params: json.RawMessage(`{"input":[]}`),
	})
	if err == nil {
		t.Fatal("expected error on 502")
	}
}

func TestCodexClientPassesThroughTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"threadId":"thr-1","transport":{"code":"brokerTimeout","message":"..."}}`))
	}))
	defer srv.Close()
	c := NewCodexClient(srv.URL, "")
	resp, err := c.RunTurn(context.Background(), CodexTurnRequest{WorkspaceID: "ws", Params: json.RawMessage(`{"input":[]}`)})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if resp.Transport == nil || resp.Transport.Code != "brokerTimeout" {
		t.Errorf("transport=%+v", resp.Transport)
	}
	if resp.Turn != nil {
		t.Errorf("turn should be nil when transport set, got %s", resp.Turn)
	}
}
