//go:build integration

// internal/agent/tui/e2e_test.go
package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestE2E_TUIHappyPath drives the full end-to-end loop:
//  1. Inbound POST creates a turn.
//  2. Fake agentserver streams tool_use + permission_request via SSE.
//  3. Test simulates the user pressing 'y' in the permission panel.
//  4. Permission decision POST releases the gate; tool_result + turn_done arrive.
//
// All HTTP traffic goes through a single fake server. Bubble Tea's program
// loop is bypassed: we drive Update directly with synthetic Msgs because
// the real Program reads from stdin which is not available in tests.
func TestE2E_TUIHappyPath(t *testing.T) {
	var sseTriggered atomic.Bool
	var (
		sseStarted = make(chan struct{})
		sseW       http.ResponseWriter
		sseFlush   http.Flusher
		decideRcv  = make(chan map[string]string, 1)
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/inbound"):
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"session_id":"cse_e","turn_id":"trn_e"}`))
			sseTriggered.Store(true)
		case strings.Contains(r.URL.Path, "/api/agents/sessions/cse_e/events"):
			w.Header().Set("Content-Type", "text/event-stream")
			sseW = w
			sseFlush = w.(http.Flusher)
			close(sseStarted)
			for !sseTriggered.Load() {
				time.Sleep(10 * time.Millisecond)
			}
			fmt.Fprint(sseW, "id: 1\nevent: tool_use\ndata: {\"tool\":\"remote_bash\",\"executor_id\":\"exe_a\"}\n\n")
			sseFlush.Flush()
			fmt.Fprint(sseW, "id: 2\nevent: permission_request\ndata: {\"permission_id\":\"p1\",\"tool\":\"remote_bash\",\"executor_id\":\"exe_a\",\"args\":{}}\n\n")
			sseFlush.Flush()
			select {
			case <-decideRcv:
			case <-time.After(2 * time.Second):
				t.Errorf("decide timeout")
			}
			fmt.Fprint(sseW, "id: 3\nevent: tool_result\ndata: {\"output\":\"ok\",\"exit_code\":0}\n\n")
			sseFlush.Flush()
			fmt.Fprint(sseW, "id: 4\nevent: turn_done\ndata: {}\n\n")
			sseFlush.Flush()
		case strings.Contains(r.URL.Path, "/permissions/p1"):
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			decideRcv <- b
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/api/agents/sessions/cse_e/attach"):
			_, _ = w.Write([]byte(`{"session_id":"cse_e","permission_responder":"exe_a"}`))
		case strings.Contains(r.URL.Path, "/api/agents/executors/exe_a/status"):
			_, _ = w.Write([]byte(`{"executor_id":"exe_a","status":"online"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	bus := NewBus(BusConfig{
		ServerURL: srv.URL, WorkspaceID: "ws", ExecutorID: "exe_a",
		Auth: &fakeAuth{tk: "t"},
	})
	model := NewModel(ModelConfig{
		ServerURL: srv.URL, WorkspaceID: "ws", ExecutorID: "exe_a",
		Bus: bus, Resume: "cse_e",
	})
	model.SetAuthState(AuthLoggedIn)

	var captured []tea.Msg
	var captureMu sync.Mutex
	capture := func(msg tea.Msg) {
		captureMu.Lock()
		defer captureMu.Unlock()
		captured = append(captured, msg)
	}

	cmd := model.Init()
	if cmd != nil {
		_ = cmd()
	}

	go func() {
		sub := NewSSEConsumer(bus, SSEConfig{SessionID: "cse_e"})
		for ev := range sub.Run(t.Context()) {
			capture(EventArrivedMsg{Event: ev})
		}
	}()
	select {
	case <-sseStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("sse not started")
	}

	model.input.SetValue("hello")
	handled, sendCmd := model.handleNormalKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled || sendCmd == nil {
		t.Fatal("send cmd not produced")
	}
	_ = sendCmd()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for events")
		default:
		}
		captureMu.Lock()
		if len(captured) == 0 {
			captureMu.Unlock()
			time.Sleep(10 * time.Millisecond)
			continue
		}
		msg := captured[0]
		captured = captured[1:]
		captureMu.Unlock()
		next, _ := model.Update(msg)
		model = next.(*Model)
		if model.activePanel != nil && model.activePanel.ID() == "p1" {
			_, decideCmd, _ := model.activePanel.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
			if decideCmd == nil {
				t.Fatal("decide cmd nil")
			}
			decideMsg := decideCmd()
			next2, postCmd := model.Update(decideMsg)
			model = next2.(*Model)
			if postCmd == nil {
				t.Fatal("post-decision cmd nil")
			}
			go postCmd()
		}
		if model.timeline.Len() > 0 {
			tail := model.timeline.items[len(model.timeline.items)-1]
			if tail.EventType == "turn_done" {
				break
			}
		}
	}
	_ = io.Discard
}
