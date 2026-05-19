package codexappgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/broker"
)

// fakeBroker implements turnRunner for handler unit tests.
type fakeBroker struct {
	startThreadFn func(ctx context.Context, workspaceID string) (string, error)
	turnFn        func(ctx context.Context, workspaceID, threadID string, params json.RawMessage, timeout time.Duration) (json.RawMessage, error)
}

func (f *fakeBroker) StartThread(ctx context.Context, workspaceID string) (string, error) {
	return f.startThreadFn(ctx, workspaceID)
}
func (f *fakeBroker) Turn(ctx context.Context, workspaceID, threadID string, params json.RawMessage, timeout time.Duration) (json.RawMessage, error) {
	return f.turnFn(ctx, workspaceID, threadID, params, timeout)
}

func TestTurnAPISuccess(t *testing.T) {
	h := &turnAPIHandler{
		runner: &fakeBroker{
			startThreadFn: func(_ context.Context, _ string) (string, error) {
				return "thr-new", nil
			},
			turnFn: func(_ context.Context, ws, tid string, _ json.RawMessage, _ time.Duration) (json.RawMessage, error) {
				if ws != "ws-1" || tid != "thr-new" {
					t.Errorf("ws=%s tid=%s", ws, tid)
				}
				return json.RawMessage(`{"id":"trn-1","status":"completed","items":[{"type":"agentMessage","id":"m","text":"hi"}],"itemsView":"full","error":null}`), nil
			},
		},
	}
	body, _ := json.Marshal(map[string]any{
		"workspaceId": "ws-1",
		"threadId":    nil,
		"params":      map[string]any{"input": []any{map[string]any{"type": "text", "text": "hi"}}},
		"timeoutMs":   30000,
	})
	r := httptest.NewRequest("POST", "/api/turns", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp turnAPIResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ThreadID != "thr-new" {
		t.Errorf("threadId=%q", resp.ThreadID)
	}
	if resp.Transport != nil {
		t.Errorf("transport=%+v want nil", resp.Transport)
	}
	if resp.Turn == nil {
		t.Fatal("turn missing")
	}
	var turn map[string]any
	_ = json.Unmarshal(resp.Turn, &turn)
	if turn["status"] != "completed" {
		t.Errorf("turn.status=%v", turn["status"])
	}
}

func TestTurnAPITimeout(t *testing.T) {
	h := &turnAPIHandler{
		runner: &fakeBroker{
			startThreadFn: func(_ context.Context, _ string) (string, error) { return "thr-x", nil },
			turnFn: func(_ context.Context, _, _ string, _ json.RawMessage, _ time.Duration) (json.RawMessage, error) {
				return nil, &broker.TimeoutError{ThreadID: "thr-x", TurnID: "trn-x"}
			},
		},
	}
	body, _ := json.Marshal(map[string]any{
		"workspaceId": "ws-1",
		"params":      map[string]any{"input": []any{}},
	})
	r := httptest.NewRequest("POST", "/api/turns", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	var resp turnAPIResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Turn != nil {
		t.Errorf("turn must be nil on timeout, got %s", resp.Turn)
	}
	if resp.Transport == nil || resp.Transport.Code != "brokerTimeout" {
		t.Errorf("transport=%+v want brokerTimeout", resp.Transport)
	}
}

func TestTurnAPIMissingWorkspace(t *testing.T) {
	h := &turnAPIHandler{runner: &fakeBroker{}}
	body, _ := json.Marshal(map[string]any{
		"params": map[string]any{"input": []any{}},
	})
	r := httptest.NewRequest("POST", "/api/turns", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Errorf("status=%d want 400", w.Code)
	}
}

func TestTurnAPISubprocessCrash(t *testing.T) {
	h := &turnAPIHandler{
		runner: &fakeBroker{
			startThreadFn: func(_ context.Context, _ string) (string, error) { return "thr-x", nil },
			turnFn: func(_ context.Context, _, _ string, _ json.RawMessage, _ time.Duration) (json.RawMessage, error) {
				return nil, errors.New("dial: connection refused")
			},
		},
	}
	body, _ := json.Marshal(map[string]any{"workspaceId": "ws-1", "params": map[string]any{"input": []any{}}})
	r := httptest.NewRequest("POST", "/api/turns", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp turnAPIResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Transport == nil || resp.Transport.Code != "subprocessCrash" {
		t.Errorf("transport=%+v want subprocessCrash", resp.Transport)
	}
}
