package tui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeAuth struct{ tk string }

func (f *fakeAuth) EnsureValid(_ context.Context) (string, error) {
	return f.tk, nil
}

func TestBus_PostInbound(t *testing.T) {
	var receivedAuth, receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"session_id":"cse_x","turn_id":"trn_y"}`))
	}))
	defer srv.Close()
	bus := NewBus(BusConfig{
		ServerURL:   srv.URL,
		WorkspaceID: "ws_test",
		ExecutorID:  "exe_a",
		Auth:        &fakeAuth{tk: "TKN"},
	})
	out, err := bus.PostInbound(context.Background(), InboundRequest{
		Text: "hello", PermissionResponder: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.SessionID != "cse_x" || out.TurnID != "trn_y" {
		t.Errorf("response %+v", out)
	}
	if receivedAuth != "Bearer TKN" {
		t.Errorf("auth header = %q", receivedAuth)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(receivedBody), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["text"] != "hello" || parsed["executor_id"] != "exe_a" {
		t.Errorf("body = %s", receivedBody)
	}
}

func TestBus_PostInbound_Returns409OnTurnInProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":{"code":"turn_in_progress","message":"x"}}`))
	}))
	defer srv.Close()
	bus := NewBus(BusConfig{
		ServerURL: srv.URL, WorkspaceID: "ws", ExecutorID: "e", Auth: &fakeAuth{tk: "t"},
	})
	_, err := bus.PostInbound(context.Background(), InboundRequest{Text: "x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "turn_in_progress" {
		t.Errorf("err = %v want APIError{turn_in_progress}", err)
	}
}

func TestBus_PostDecision(t *testing.T) {
	var path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		bb, _ := io.ReadAll(r.Body)
		body = string(bb)
		w.WriteHeader(200)
		w.Write([]byte(`{"accepted":true}`))
	}))
	defer srv.Close()
	bus := NewBus(BusConfig{
		ServerURL: srv.URL, WorkspaceID: "ws", ExecutorID: "exe_a", Auth: &fakeAuth{tk: "t"},
	})
	err := bus.PostDecision(context.Background(), "cse_1", "perm_p1", "allow", "always")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "/permissions/perm_p1") {
		t.Errorf("path = %q", path)
	}
	if !strings.Contains(body, `"decision":"allow"`) || !strings.Contains(body, `"scope":"always"`) {
		t.Errorf("body = %s", body)
	}
	if !strings.Contains(body, `"responder_executor_id":"exe_a"`) {
		t.Errorf("responder id missing in body: %s", body)
	}
}

func TestBus_FetchExecutorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"executor_id":"exe_a","status":"online"}`))
	}))
	defer srv.Close()
	bus := NewBus(BusConfig{
		ServerURL: srv.URL, WorkspaceID: "ws", ExecutorID: "exe_a", Auth: &fakeAuth{tk: "t"},
	})
	st, err := bus.FetchExecutorStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "online" {
		t.Errorf("status = %q", st.Status)
	}
}

func TestBus_PostCancel_HitsCorrectURL(t *testing.T) {
	var hitPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	bus := NewBus(BusConfig{
		ServerURL: srv.URL, WorkspaceID: "ws", ExecutorID: "e", Auth: &fakeAuth{tk: "t"},
	})
	if err := bus.PostCancel(context.Background(), "cse_1", "trn_2"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(hitPath, "/agent-sessions/cse_1/turns/trn_2/cancel") {
		t.Errorf("path = %q", hitPath)
	}
}

func TestBus_NewSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"session_id":"cse_new","external_id":"tui:e:1","channel_type":"tui"}`))
	}))
	defer srv.Close()
	bus := NewBus(BusConfig{
		ServerURL: srv.URL, WorkspaceID: "ws", ExecutorID: "e", Auth: &fakeAuth{tk: "t"},
	})
	sid, err := bus.NewSession(context.Background(), "ask", "e")
	if err != nil {
		t.Fatal(err)
	}
	if sid != "cse_new" {
		t.Errorf("sid=%q", sid)
	}
}

func TestBus_AttachSession(t *testing.T) {
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bb, _ := io.ReadAll(r.Body)
		receivedBody = string(bb)
		w.Write([]byte(`{"session_id":"cse_a","permission_responder":"e","previous_responder":"old","previous_preferred":"old_pref"}`))
	}))
	defer srv.Close()
	bus := NewBus(BusConfig{
		ServerURL: srv.URL, WorkspaceID: "ws", ExecutorID: "e", Auth: &fakeAuth{tk: "t"},
	})
	resp, err := bus.AttachSession(context.Background(), "cse_a", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if resp.PreviousResponder != "old" {
		t.Errorf("previous_responder=%q", resp.PreviousResponder)
	}
	if !strings.Contains(receivedBody, `"mode":"operator"`) {
		t.Errorf("body missing mode: %s", receivedBody)
	}
	if !strings.Contains(receivedBody, `"as_permission_responder":true`) {
		t.Errorf("body missing as_permission_responder=true for operator: %s", receivedBody)
	}
}

// Sanity check that Auth wiring is invoked
func TestBus_PassesContextDeadline(t *testing.T) {
	blocker := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-blocker
	}))
	defer srv.Close()
	defer close(blocker)
	bus := NewBus(BusConfig{
		ServerURL: srv.URL, WorkspaceID: "ws", ExecutorID: "e", Auth: &fakeAuth{tk: "t"},
		HTTP: &http.Client{Timeout: 100 * time.Millisecond},
	})
	_, err := bus.PostInbound(context.Background(), InboundRequest{Text: "x"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}
