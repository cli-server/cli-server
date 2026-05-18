package notebooksupervisor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Supervisor owns the per-workspace lifecycle map. Concurrent
// EnsureRunning calls for the same Key are serialised; a single
// in-memory map is the source of truth for lastActive.
type Supervisor struct {
	k8s    kubernetes.Interface
	cfg    Config
	logger *slog.Logger

	mu       sync.Mutex
	children map[Key]*entry
}

type entry struct {
	handle     *Handle
	lastActive time.Time
}

// New constructs a Supervisor. logger may be nil (defaults to slog.Default).
func New(k8s kubernetes.Interface, cfg Config, logger *slog.Logger) *Supervisor {
	if logger == nil {
		logger = slog.Default().With("component", "notebooksupervisor")
	}
	return &Supervisor{
		k8s:      k8s,
		cfg:      cfg.WithDefaults(),
		logger:   logger,
		children: map[Key]*entry{},
	}
}

// EnsureRunning creates the Deployment + Service if absent, then blocks
// up to Config.ReadyTimeout waiting for ReadyReplicas >= 1. Returns a
// cached Handle on subsequent calls.
func (s *Supervisor) EnsureRunning(ctx context.Context, k Key) (*Handle, error) {
	if _, err := k.SafeDeploymentName(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	if e, ok := s.children[k]; ok {
		e.lastActive = time.Now()
		s.mu.Unlock()
		return e.handle, nil
	}
	s.mu.Unlock()

	dep, err := BuildDeployment(k, s.cfg)
	if err != nil {
		return nil, err
	}
	if _, err := s.k8s.AppsV1().Deployments(k.Namespace).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create deployment: %w", err)
		}
	}
	svc, err := BuildService(k)
	if err != nil {
		return nil, err
	}
	if _, err := s.k8s.CoreV1().Services(k.Namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create service: %w", err)
		}
	}

	if err := s.waitReady(ctx, k); err != nil {
		return nil, err
	}

	handle := &Handle{ServiceURL: ServiceURL(k)}
	s.mu.Lock()
	if existing, ok := s.children[k]; ok {
		existing.lastActive = time.Now()
		s.mu.Unlock()
		return existing.handle, nil
	}
	s.children[k] = &entry{handle: handle, lastActive: time.Now()}
	s.mu.Unlock()
	return handle, nil
}

func (s *Supervisor) waitReady(ctx context.Context, k Key) error {
	deadline := time.Now().Add(s.cfg.ReadyTimeout)
	name, _ := k.SafeDeploymentName()
	for time.Now().Before(deadline) {
		d, err := s.k8s.AppsV1().Deployments(k.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get deployment for ready check: %w", err)
		}
		if d.Status.ReadyReplicas >= 1 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("deployment %s/%s did not become ready within %v", k.Namespace, name, s.cfg.ReadyTimeout)
}

// Stop deletes the Deployment + Service. 404 is treated as success.
func (s *Supervisor) Stop(ctx context.Context, k Key) error {
	name, err := k.SafeDeploymentName()
	if err != nil {
		return err
	}
	delErr := s.k8s.AppsV1().Deployments(k.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if delErr != nil && !apierrors.IsNotFound(delErr) {
		return fmt.Errorf("delete deployment: %w", delErr)
	}
	delErr = s.k8s.CoreV1().Services(k.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if delErr != nil && !apierrors.IsNotFound(delErr) {
		return fmt.Errorf("delete service: %w", delErr)
	}
	s.mu.Lock()
	delete(s.children, k)
	s.mu.Unlock()
	return nil
}

// Touch updates lastActive for k. No-op if the workspace isn't tracked.
func (s *Supervisor) Touch(k Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.children[k]; ok {
		e.lastActive = time.Now()
	}
}

// LastActive returns the last activity timestamp for k, or zero time if
// the workspace isn't tracked.
func (s *Supervisor) LastActive(k Key) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.children[k]; ok {
		return e.lastActive
	}
	return time.Time{}
}

// idleKeys returns Keys whose lastActive is older than the cutoff.
// Caller holds the lock.
func (s *Supervisor) idleKeys(cutoff time.Time) []Key {
	out := []Key{}
	for k, e := range s.children {
		if e.lastActive.Before(cutoff) {
			out = append(out, k)
		}
	}
	return out
}
