package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
)

func TestReaper_RetiresIdleSubprocess(t *testing.T) {
	bin := buildFakeCodex(t)
	root := t.TempDir()
	store := newFakeStore()
	mgr := codexhome.NewManager(root)
	sup := NewSupervisor(SupervisorConfig{CodexBin: bin, HomeMgr: mgr, Store: store})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	build := func() (codexhome.ConfigInput, error) { return defaultConfigInput(), nil }
	if _, err := sup.EnsureSubprocess(ctx, Key{WorkspaceID: "ws_a", ThreadID: "thr_1"}, build); err != nil {
		t.Fatal(err)
	}

	r := NewIdleReaper(sup, 50*time.Millisecond, 100*time.Millisecond, nil)
	rctx, rcancel := context.WithCancel(context.Background())
	defer rcancel()
	go r.Run(rctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sup.snapshot()) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := sup.snapshot(); len(got) != 0 {
		t.Fatalf("expected empty after reap, got %v", got)
	}
}

func TestReaper_KeepsActiveSubprocess(t *testing.T) {
	bin := buildFakeCodex(t)
	root := t.TempDir()
	store := newFakeStore()
	mgr := codexhome.NewManager(root)
	sup := NewSupervisor(SupervisorConfig{CodexBin: bin, HomeMgr: mgr, Store: store})
	defer sup.ShutdownAll(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	build := func() (codexhome.ConfigInput, error) { return defaultConfigInput(), nil }
	key := Key{WorkspaceID: "ws_a", ThreadID: "thr_keep"}
	if _, err := sup.EnsureSubprocess(ctx, key, build); err != nil {
		t.Fatal(err)
	}

	r := NewIdleReaper(sup, 30*time.Millisecond, 200*time.Millisecond, nil)
	rctx, rcancel := context.WithCancel(context.Background())
	defer rcancel()
	go r.Run(rctx)

	end := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(end) {
		sup.Touch(key)
		time.Sleep(50 * time.Millisecond)
	}
	if got := sup.snapshot(); len(got) != 1 {
		t.Fatalf("expected entry to survive, got %v", got)
	}
}
