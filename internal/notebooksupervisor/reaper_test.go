package notebooksupervisor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestReaper_StopsIdle(t *testing.T) {
	c := fake.NewClientset()
	cfg := testConfig()
	cfg.IdleTTL = 50 * time.Millisecond
	cfg.ReapInterval = 20 * time.Millisecond
	sup := New(c, cfg, nil)
	k := Key{WorkspaceID: "alpha", Namespace: "ns-alpha"}
	markReady(c, "notebook-alpha", "ns-alpha")
	if _, err := sup.EnsureRunning(context.Background(), k); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.StartReaper(ctx)

	// Wait long enough for the entry to age out + at least one tick
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, err := c.AppsV1().Deployments("ns-alpha").Get(context.Background(), "notebook-alpha", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return // success
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("deployment was not reaped within deadline")
}

func TestReaper_KeepsActive(t *testing.T) {
	c := fake.NewClientset()
	cfg := testConfig()
	cfg.IdleTTL = 100 * time.Millisecond
	cfg.ReapInterval = 20 * time.Millisecond
	sup := New(c, cfg, nil)
	k := Key{WorkspaceID: "alpha", Namespace: "ns-alpha"}
	markReady(c, "notebook-alpha", "ns-alpha")
	if _, err := sup.EnsureRunning(context.Background(), k); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.StartReaper(ctx)

	done := make(chan struct{})
	var touches int32
	go func() {
		defer close(done)
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(deadline) {
			select {
			case <-ticker.C:
				sup.Touch(k)
				atomic.AddInt32(&touches, 1)
			case <-ctx.Done():
				return
			}
		}
	}()
	<-done
	if atomic.LoadInt32(&touches) < 5 {
		t.Fatalf("expected >=5 touches, got %d", touches)
	}
	if _, err := c.AppsV1().Deployments("ns-alpha").Get(context.Background(), "notebook-alpha", metav1.GetOptions{}); err != nil {
		t.Fatalf("active deployment was reaped: %v", err)
	}
}

func TestReaper_ExitsOnContext(t *testing.T) {
	c := fake.NewClientset()
	cfg := testConfig()
	cfg.ReapInterval = 10 * time.Millisecond
	sup := New(c, cfg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sup.StartReaper(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reaper did not exit on ctx cancel")
	}
}
