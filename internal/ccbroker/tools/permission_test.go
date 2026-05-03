// internal/ccbroker/tools/permission_test.go
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// eventLog is a race-safe collector for emitted Events. Tests should access
// the slice via Snapshot() rather than dereferencing a raw *[]Event, since
// the notifier closure runs on the goroutine executing Gate.Check while the
// test polls from another goroutine.
type eventLog struct {
	mu     sync.Mutex
	events []Event
}

func (e *eventLog) append(ev Event) {
	e.mu.Lock()
	e.events = append(e.events, ev)
	e.mu.Unlock()
}

func (e *eventLog) Snapshot() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Event, len(e.events))
	copy(out, e.events)
	return out
}

func (e *eventLog) Len() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.events)
}

// captureNotifier returns a Gate plus an eventLog that tests can poll
// race-safely. The historic *[]Event return is preserved only for compat
// with the original test bodies — callers MUST use the eventLog wrapper
// (returned via Log()) when reading concurrently.
func captureNotifier() (*Gate, *eventLog) {
	log := &eventLog{}
	g := NewGate(func(_ string, e Event) {
		log.append(e)
	})
	return g, log
}

func TestGate_BypassMode_AllowsImmediately(t *testing.T) {
	g, _ := captureNotifier()
	err := g.Check(context.Background(), CheckRequest{
		SessionID:            "s1",
		TurnID:               "t1",
		Tool:                 "remote_bash",
		ExecutorID:           "exe_a",
		Args:                 json.RawMessage(`{"command":"ls"}`),
		PermissionMode:       "bypass",
		SessionCreatorUserID: "u",
		ExecutorOwnerUserID:  "u",
		Timeout:              100 * time.Millisecond,
	})
	if err != nil {
		t.Errorf("bypass should allow: %v", err)
	}
}

func TestGate_CrossUser_DeniesImmediately(t *testing.T) {
	g, _ := captureNotifier()
	err := g.Check(context.Background(), CheckRequest{
		SessionID:                 "s1",
		TurnID:                    "t1",
		Tool:                      "remote_bash",
		ExecutorID:                "exe_a",
		Args:                      json.RawMessage(`{"command":"ls"}`),
		PermissionMode:            "ask",
		SessionCreatorUserID:      "u_alice",
		ExecutorOwnerUserID:       "u_bob",
		ExecutorSharedToWorkspace: false,
		Timeout:                   100 * time.Millisecond,
	})
	if err != ErrCrossUserDenied {
		t.Errorf("err=%v want ErrCrossUserDenied", err)
	}
}

func TestGate_AskMode_BlocksUntilResolve(t *testing.T) {
	g, events := captureNotifier()
	done := make(chan error, 1)
	go func() {
		done <- g.Check(context.Background(), CheckRequest{
			SessionID:            "s1",
			TurnID:               "t1",
			Tool:                 "remote_bash",
			ExecutorID:           "exe_a",
			Args:                 json.RawMessage(`{"command":"ls"}`),
			PermissionMode:       "ask",
			SessionCreatorUserID: "u",
			ExecutorOwnerUserID:  "u",
			Timeout:              5 * time.Second,
		})
	}()
	var pid string
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snap := events.Snapshot()
		if len(snap) > 0 {
			pid = snap[0].PermissionID
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pid == "" {
		t.Fatal("no permission_request emitted")
	}
	if err := g.Resolve(pid, Decision{Verdict: "allow", Scope: "once"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Check should allow after resolve, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Check did not return after Resolve")
	}
	var sawResolved bool
	for _, e := range events.Snapshot() {
		if e.Type == "permission_resolved" {
			sawResolved = true
		}
	}
	if !sawResolved {
		t.Errorf("no permission_resolved event")
	}
}

func TestGate_AskMode_TimeoutDenies(t *testing.T) {
	g, _ := captureNotifier()
	err := g.Check(context.Background(), CheckRequest{
		SessionID:            "s1",
		TurnID:               "t1",
		Tool:                 "remote_bash",
		ExecutorID:           "exe_a",
		Args:                 json.RawMessage(`{"command":"ls"}`),
		PermissionMode:       "ask",
		SessionCreatorUserID: "u",
		ExecutorOwnerUserID:  "u",
		Timeout:              50 * time.Millisecond,
	})
	if err != ErrPermissionDenied {
		t.Errorf("err=%v want ErrPermissionDenied (timeout)", err)
	}
}

func TestGate_StickyAlways_HitsWithoutEmit(t *testing.T) {
	g, events := captureNotifier()
	base := CheckRequest{
		SessionID:            "s1",
		TurnID:               "t1",
		Tool:                 "remote_bash",
		ExecutorID:           "exe_a",
		Args:                 json.RawMessage(`{"command":"git status"}`),
		PermissionMode:       "ask",
		SessionCreatorUserID: "u",
		ExecutorOwnerUserID:  "u",
		Timeout:              5 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- g.Check(context.Background(), base) }()
	var pid string
	for i := 0; i < 200 && pid == ""; i++ {
		snap := events.Snapshot()
		if len(snap) > 0 {
			pid = snap[0].PermissionID
		}
		time.Sleep(5 * time.Millisecond)
	}
	g.Resolve(pid, Decision{Verdict: "allow", Scope: "always"})
	if err := <-done; err != nil {
		t.Fatalf("first call: %v", err)
	}
	eventsBefore := events.Len()
	base2 := base
	base2.Args = json.RawMessage(`{"command":"git status -s"}`)
	if err := g.Check(context.Background(), base2); err != nil {
		t.Errorf("sticky should allow: %v", err)
	}
	snapAfter := events.Snapshot()
	eventsAfter := len(snapAfter)
	if eventsAfter <= eventsBefore {
		t.Errorf("expected at least one new event (resolved/sticky)")
	}
	for i := eventsBefore; i < eventsAfter; i++ {
		if snapAfter[i].Type == "permission_request" {
			t.Errorf("sticky path should NOT emit permission_request")
		}
	}
	base3 := base
	base3.Args = json.RawMessage(`{"command":"docker ps"}`)
	base3.Timeout = 50 * time.Millisecond
	err := g.Check(context.Background(), base3)
	if err != ErrPermissionDenied {
		t.Errorf("different head should re-ask (and time out → deny), got %v", err)
	}
}

func TestGate_CancelTurn_ResolvesAllPendingOfThatTurn(t *testing.T) {
	g, _ := captureNotifier()
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			errs[i] = g.Check(context.Background(), CheckRequest{
				SessionID:            "s1",
				TurnID:               "t_cancel",
				Tool:                 "remote_bash",
				ExecutorID:           "exe_a",
				Args:                 json.RawMessage(fmt.Sprintf(`{"command":"cmd%d"}`, i)),
				PermissionMode:       "ask",
				SessionCreatorUserID: "u",
				ExecutorOwnerUserID:  "u",
				Timeout:              5 * time.Second,
			})
		}()
	}
	time.Sleep(50 * time.Millisecond)
	g.CancelTurn("t_cancel")
	wg.Wait()
	for i, err := range errs {
		if err != ErrPermissionDenied {
			t.Errorf("call %d: err=%v want ErrPermissionDenied (cancelled)", i, err)
		}
	}
}

func TestEvent_JSONShape_UsesSnakeCase(t *testing.T) {
	e := Event{
		Type:         "permission_request",
		PermissionID: "perm_xyz",
		Tool:         "remote_bash",
		ExecutorID:   "exe_a",
		Decision:     &Decision{Verdict: "allow", Scope: "once"},
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		`"event_type":"permission_request"`,
		`"permission_id":"perm_xyz"`,
		`"tool":"remote_bash"`,
		`"executor_id":"exe_a"`,
		`"decision":{"verdict":"allow","scope":"once"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in marshalled event:\n%s", want, s)
		}
	}
	// Make sure the old PascalCase keys are gone.
	for _, bad := range []string{`"Type":`, `"PermissionID":`, `"Tool":`} {
		if strings.Contains(s, bad) {
			t.Errorf("unexpected PascalCase key %q in:\n%s", bad, s)
		}
	}
}

func TestMakeRuleKey_BashHeadIsTwoTokens(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		{"git status", "remote_bash|exe|cmd:git status"},
		{"git status -s", "remote_bash|exe|cmd:git status"},
		{"git push", "remote_bash|exe|cmd:git push"},
		{"ls", "remote_bash|exe|cmd:ls"},
		{"", "remote_bash|exe|cmd:"},
	}
	for _, c := range cases {
		args := []byte(fmt.Sprintf(`{"command":%q}`, c.cmd))
		got := makeRuleKey("remote_bash", "exe", args)
		if got != c.want {
			t.Errorf("makeRuleKey(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

func TestGate_CrossUser_AllowsWhenSharedToWorkspace(t *testing.T) {
	g, _ := captureNotifier()
	err := g.Check(context.Background(), CheckRequest{
		SessionID:                 "s1",
		TurnID:                    "t1",
		Tool:                      "remote_bash",
		ExecutorID:                "exe_a",
		Args:                      json.RawMessage(`{"command":"ls"}`),
		PermissionMode:            "bypass",
		SessionCreatorUserID:      "u_alice",
		ExecutorOwnerUserID:       "u_bob",
		ExecutorSharedToWorkspace: true,
		Timeout:                   100 * time.Millisecond,
	})
	if err != nil {
		t.Errorf("shared sandbox should allow cross-user: %v", err)
	}
}

func TestGate_CrossUser_DeniesEvenWhenOwnerEmpty(t *testing.T) {
	// Defense in depth: empty ExecutorOwnerUserID against a real session
	// creator must still trigger denial. (The store layer normally projects
	// NULL → 'unknown' so this case shouldn't occur in production, but the
	// gate must not silently allow it if a future caller forgets to populate.)
	g, _ := captureNotifier()
	err := g.Check(context.Background(), CheckRequest{
		SessionID:            "s1",
		TurnID:               "t1",
		Tool:                 "remote_bash",
		ExecutorID:           "exe_a",
		Args:                 json.RawMessage(`{}`),
		PermissionMode:       "ask",
		SessionCreatorUserID: "u_alice",
		ExecutorOwnerUserID:  "", // simulates caller-side bug
		Timeout:              100 * time.Millisecond,
	})
	if err != ErrCrossUserDenied {
		t.Errorf("err=%v want ErrCrossUserDenied", err)
	}
}
