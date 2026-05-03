// internal/ccbroker/tools/permission_test.go
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func captureNotifier() (*Gate, *[]Event) {
	var mu sync.Mutex
	events := []Event{}
	g := NewGate(func(_ string, e Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})
	return g, &events
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
		if len(*events) > 0 {
			pid = (*events)[0].PermissionID
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
	for _, e := range *events {
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
		if len(*events) > 0 {
			pid = (*events)[0].PermissionID
		}
		time.Sleep(5 * time.Millisecond)
	}
	g.Resolve(pid, Decision{Verdict: "allow", Scope: "always"})
	if err := <-done; err != nil {
		t.Fatalf("first call: %v", err)
	}
	eventsBefore := len(*events)
	base2 := base
	base2.Args = json.RawMessage(`{"command":"git status -s"}`)
	if err := g.Check(context.Background(), base2); err != nil {
		t.Errorf("sticky should allow: %v", err)
	}
	eventsAfter := len(*events)
	if eventsAfter <= eventsBefore {
		t.Errorf("expected at least one new event (resolved/sticky)")
	}
	for i := eventsBefore; i < eventsAfter; i++ {
		if (*events)[i].Type == "permission_request" {
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
