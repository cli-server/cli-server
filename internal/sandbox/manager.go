package sandbox

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"

	"github.com/imryao/cli-server/internal/process"
)

const (
	labelManagedBy        = "managed-by"
	labelValue            = "cli-server"
	sandboxNameHashLabel  = "agents.x-k8s.io/sandbox-name-hash"
	sandboxContainerName  = "agent"
	pollInterval          = 2 * time.Second
	pollTimeout           = 5 * time.Minute
)

// Compile-time interface check.
var _ process.Manager = (*Manager)(nil)

type sessionEntry struct {
	proc        *execProcess
	sandboxName string
}

// Manager manages Sandbox CRs and remotecommand exec sessions.
type Manager struct {
	cfg       Config
	restCfg   *rest.Config
	k8s       client.Client
	clientset kubernetes.Interface
	mu        sync.RWMutex
	sessions  map[string]*sessionEntry
}

// NewManager creates a sandbox Manager using in-cluster or KUBECONFIG config.
func NewManager(cfg Config) (*Manager, error) {
	restCfg, err := buildRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("k8s config: %w", err)
	}

	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(sandboxv1alpha1.AddToScheme(s))

	k8sClient, err := client.New(restCfg, client.Options{Scheme: s})
	if err != nil {
		return nil, fmt.Errorf("controller-runtime client: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes clientset: %w", err)
	}

	m := &Manager{
		cfg:       cfg,
		restCfg:   restCfg,
		k8s:       k8sClient,
		clientset: clientset,
		sessions:  make(map[string]*sessionEntry),
	}

	m.cleanOrphans()
	return m, nil
}

func buildRESTConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.Getenv("HOME") + "/.kube/config"
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

// cleanOrphans deletes Sandbox CRs labelled managed-by=cli-server from previous runs.
func (m *Manager) cleanOrphans() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var list sandboxv1alpha1.SandboxList
	if err := m.k8s.List(ctx, &list,
		client.InNamespace(m.cfg.Namespace),
		client.MatchingLabels{labelManagedBy: labelValue},
	); err != nil {
		log.Printf("failed to list orphan sandboxes: %v", err)
		return
	}
	for i := range list.Items {
		log.Printf("cleaning orphan sandbox %s", list.Items[i].Name)
		if err := m.k8s.Delete(ctx, &list.Items[i]); err != nil {
			log.Printf("failed to delete orphan sandbox %s: %v", list.Items[i].Name, err)
		}
	}
}

func (m *Manager) Start(id, command string, args, env []string) (process.Process, error) {
	ctx := context.Background()
	sandboxName := "cli-session-" + shortID(id)

	// Build environment variables for the sandbox pod.
	containerEnv := []corev1.EnvVar{{Name: "TERM", Value: "xterm-256color"}}
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"} {
		if v := os.Getenv(key); v != "" {
			containerEnv = append(containerEnv, corev1.EnvVar{Name: key, Value: v})
		}
	}

	// Create the Sandbox CR.
	sb := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: m.cfg.Namespace,
			Labels:    map[string]string{labelManagedBy: labelValue},
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    sandboxContainerName,
						Image:   m.cfg.Image,
						Command: []string{"sleep", "infinity"},
						Env:     containerEnv,
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse(m.cfg.MemoryLimit),
								corev1.ResourceCPU:    resource.MustParse(m.cfg.CPULimit),
							},
						},
					}},
					ImagePullSecrets: m.imagePullSecrets(),
					RestartPolicy:    corev1.RestartPolicyNever,
				},
			},
		},
	}

	if err := m.k8s.Create(ctx, sb); err != nil {
		return nil, fmt.Errorf("create sandbox CR: %w", err)
	}

	// Wait for sandbox to become ready.
	podName, err := m.waitForReady(ctx, sandboxName)
	if err != nil {
		// Cleanup on failure.
		_ = m.k8s.Delete(ctx, sb)
		return nil, fmt.Errorf("sandbox not ready: %w", err)
	}

	// Build the full command.
	fullCmd := append([]string{command}, args...)

	// Start remotecommand exec into the pod.
	proc, err := startExec(m.restCfg, m.clientset, m.cfg.Namespace, podName, sandboxContainerName, fullCmd)
	if err != nil {
		_ = m.k8s.Delete(ctx, sb)
		return nil, fmt.Errorf("exec into sandbox: %w", err)
	}

	m.mu.Lock()
	m.sessions[id] = &sessionEntry{proc: proc, sandboxName: sandboxName}
	m.mu.Unlock()

	return proc, nil
}

// waitForReady polls until the Sandbox has Ready=True and returns the backing pod name.
func (m *Manager) waitForReady(ctx context.Context, sandboxName string) (string, error) {
	deadline := time.Now().Add(pollTimeout)
	nameHash := nameHash(sandboxName)

	for time.Now().Before(deadline) {
		var sb sandboxv1alpha1.Sandbox
		key := client.ObjectKey{Namespace: m.cfg.Namespace, Name: sandboxName}
		if err := m.k8s.Get(ctx, key, &sb); err != nil {
			time.Sleep(pollInterval)
			continue
		}

		if isSandboxReady(&sb) {
			// Resolve the pod via label selector.
			podList, err := m.clientset.CoreV1().Pods(m.cfg.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: sandboxNameHashLabel + "=" + nameHash,
			})
			if err != nil {
				time.Sleep(pollInterval)
				continue
			}
			for _, pod := range podList.Items {
				if pod.Status.Phase == corev1.PodRunning {
					return pod.Name, nil
				}
			}
		}
		time.Sleep(pollInterval)
	}
	return "", fmt.Errorf("timed out waiting for sandbox %s", sandboxName)
}

func isSandboxReady(sb *sandboxv1alpha1.Sandbox) bool {
	for _, c := range sb.Status.Conditions {
		if c.Type == string(sandboxv1alpha1.SandboxConditionReady) && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// nameHash replicates the agent-sandbox controller's FNV-1a hash for label selectors.
func nameHash(name string) string {
	h := fnv.New32a()
	h.Write([]byte(name))
	return fmt.Sprintf("%08x", h.Sum32())
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (m *Manager) imagePullSecrets() []corev1.LocalObjectReference {
	if m.cfg.ImagePullSecret == "" {
		return nil
	}
	return []corev1.LocalObjectReference{{Name: m.cfg.ImagePullSecret}}
}

func (m *Manager) Get(id string) (process.Process, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.sessions[id]
	if !ok {
		return nil, false
	}
	return entry.proc, true
}

func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	entry, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}

	// Terminate the exec stream.
	entry.proc.close()

	// Delete the Sandbox CR (controller cascade-deletes the pod).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sb := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      entry.sandboxName,
			Namespace: m.cfg.Namespace,
		},
	}
	if err := m.k8s.Delete(ctx, sb); err != nil {
		log.Printf("failed to delete sandbox %s: %v", entry.sandboxName, err)
	}
	return nil
}

func (m *Manager) Close() error {
	m.mu.RLock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.RUnlock()

	for _, id := range ids {
		m.Stop(id)
	}
	return nil
}
