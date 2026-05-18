package notebooksupervisor

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func testConfig() Config {
	return Config{
		Image:            "img:tag",
		WorkspacePVCName: "pvc",
	}.WithDefaults()
}

// markReady spawns a goroutine that patches the Deployment status to
// ReadyReplicas=1 after a brief delay, so the waitReady polling
// succeeds inside fake-clientset tests.
func markReady(c *fake.Clientset, name, ns string) {
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = c.AppsV1().Deployments(ns).UpdateStatus(
			context.Background(),
			&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
			},
			metav1.UpdateOptions{},
		)
	}()
}

func TestEnsureRunning_CreatesDeploymentAndService(t *testing.T) {
	c := fake.NewClientset()
	sup := New(c, testConfig(), nil)
	k := Key{WorkspaceID: "alpha", Namespace: "ns-alpha"}
	markReady(c, "notebook-alpha", "ns-alpha")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h, err := sup.EnsureRunning(ctx, k)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if h.ServiceURL != "http://notebook-alpha.ns-alpha.svc.cluster.local:8888" {
		t.Errorf("svc url=%q", h.ServiceURL)
	}

	if _, err := c.AppsV1().Deployments("ns-alpha").Get(ctx, "notebook-alpha", metav1.GetOptions{}); err != nil {
		t.Errorf("deployment not created: %v", err)
	}
	if _, err := c.CoreV1().Services("ns-alpha").Get(ctx, "notebook-alpha", metav1.GetOptions{}); err != nil {
		t.Errorf("service not created: %v", err)
	}
}

func TestEnsureRunning_IdempotentOnAlreadyExists(t *testing.T) {
	c := fake.NewClientset()
	sup := New(c, testConfig(), nil)
	k := Key{WorkspaceID: "alpha", Namespace: "ns-alpha"}
	markReady(c, "notebook-alpha", "ns-alpha")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := sup.EnsureRunning(ctx, k); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := sup.EnsureRunning(ctx, k); err != nil {
		t.Fatalf("second: %v", err)
	}
}

func TestStop_DeletesDeploymentAndService(t *testing.T) {
	c := fake.NewClientset()
	sup := New(c, testConfig(), nil)
	k := Key{WorkspaceID: "alpha", Namespace: "ns-alpha"}
	markReady(c, "notebook-alpha", "ns-alpha")
	ctx := context.Background()
	if _, err := sup.EnsureRunning(ctx, k); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := sup.Stop(ctx, k); err != nil {
		t.Fatalf("stop: %v", err)
	}
	_, err := c.AppsV1().Deployments("ns-alpha").Get(ctx, "notebook-alpha", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("deployment still exists: %v", err)
	}
	_, err = c.CoreV1().Services("ns-alpha").Get(ctx, "notebook-alpha", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("service still exists: %v", err)
	}
}

func TestStop_OnMissingIsNoOp(t *testing.T) {
	c := fake.NewClientset()
	sup := New(c, testConfig(), nil)
	if err := sup.Stop(context.Background(), Key{WorkspaceID: "nope", Namespace: "ns"}); err != nil {
		t.Errorf("stop missing should be nil, got %v", err)
	}
}

func TestTouch_UpdatesLastActive(t *testing.T) {
	c := fake.NewClientset()
	sup := New(c, testConfig(), nil)
	k := Key{WorkspaceID: "alpha", Namespace: "ns-alpha"}
	markReady(c, "notebook-alpha", "ns-alpha")
	if _, err := sup.EnsureRunning(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	before := sup.LastActive(k)
	time.Sleep(5 * time.Millisecond)
	sup.Touch(k)
	after := sup.LastActive(k)
	if !after.After(before) {
		t.Errorf("touch did not advance lastActive (before=%v after=%v)", before, after)
	}
}

func TestEnsureRunning_ReadyTimeout(t *testing.T) {
	c := fake.NewClientset() // no markReady → ReadyReplicas stays 0
	cfg := testConfig()
	cfg.ReadyTimeout = 200 * time.Millisecond
	sup := New(c, cfg, nil)

	_, err := sup.EnsureRunning(context.Background(),
		Key{WorkspaceID: "alpha", Namespace: "ns-alpha"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestSafeDeploymentName_RejectsAtSupervisor(t *testing.T) {
	c := fake.NewClientset()
	sup := New(c, testConfig(), nil)
	_, err := sup.EnsureRunning(context.Background(),
		Key{WorkspaceID: "INVALID", Namespace: "ns"})
	if err == nil {
		t.Fatal("expected validation error")
	}
}
