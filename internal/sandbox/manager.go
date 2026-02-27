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
	"k8s.io/apimachinery/pkg/types"
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
	labelManagedBy       = "managed-by"
	labelValue           = "cli-server"
	sandboxNameHashLabel = "agents.x-k8s.io/sandbox-name-hash"
	sandboxContainerName = "agent"
	pollInterval         = 2 * time.Second
	pollTimeout          = 5 * time.Minute
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

// CleanOrphans deletes Sandbox CRs labelled managed-by=cli-server that are NOT in the known set.
func (m *Manager) CleanOrphans(knownSandboxNames []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	known := make(map[string]bool, len(knownSandboxNames))
	for _, name := range knownSandboxNames {
		known[name] = true
	}

	var list sandboxv1alpha1.SandboxList
	if err := m.k8s.List(ctx, &list,
		client.InNamespace(m.cfg.Namespace),
		client.MatchingLabels{labelManagedBy: labelValue},
	); err != nil {
		log.Printf("failed to list orphan sandboxes: %v", err)
		return
	}
	for i := range list.Items {
		name := list.Items[i].Name
		if known[name] {
			continue
		}
		log.Printf("cleaning orphan sandbox %s", name)
		if err := m.k8s.Delete(ctx, &list.Items[i]); err != nil {
			log.Printf("failed to delete orphan sandbox %s: %v", name, err)
		}
	}
}

func (m *Manager) Start(id, command string, args, env []string, opts process.StartOptions) (process.Process, error) {
	ctx := context.Background()
	sandboxName := "cli-session-" + shortID(id)

	// Build environment variables for the sandbox pod.
	containerEnv := []corev1.EnvVar{{Name: "TERM", Value: "xterm-256color"}}

	// Inject proxy URL and token so the sandbox uses the cli-server proxy
	// instead of the real Anthropic API key.
	proxyBaseURL := os.Getenv("ANTHROPIC_PROXY_URL")
	if proxyBaseURL == "" {
		proxyBaseURL = "http://cli-server." + m.cfg.Namespace + ".svc.cluster.local:8080/proxy/anthropic/v1"
	}
	if opts.ProxyToken != "" {
		containerEnv = append(containerEnv,
			corev1.EnvVar{Name: "ANTHROPIC_BASE_URL", Value: proxyBaseURL},
			corev1.EnvVar{Name: "ANTHROPIC_API_KEY", Value: opts.ProxyToken},
		)
	}

	// Volume mounts for the main container.
	volumeMounts := []corev1.VolumeMount{
		{Name: "session-data", MountPath: "/home/agent"},
	}
	var volumes []corev1.Volume

	// Mount user drive PVC if provided.
	if opts.UserDrivePVC != "" {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name: "user-drive", MountPath: "/data/disk0",
		})
		volumes = append(volumes, corev1.Volume{
			Name: "user-drive",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: opts.UserDrivePVC,
				},
			},
		})
	}

	// Init container: mount PVC at a temp path, seed it from the original home dir on first use, then fix ownership.
	// This avoids the empty PVC overwriting the agent image's /home/agent (which has claude CLI, dotfiles, etc.).
	initScript := `
set -e
# If the PVC is fresh (only has lost+found or is empty), seed it from the image's home dir.
if [ ! -f /mnt/session-data/.initialized ]; then
  echo "Seeding session PVC from /home/agent..."
  cp -a /home/agent/. /mnt/session-data/ 2>/dev/null || true
  touch /mnt/session-data/.initialized
fi
chown -R 1000:1000 /mnt/session-data
mkdir -p /mnt/user-drive
chown -R 1000:1000 /mnt/user-drive
`
	initContainers := []corev1.Container{{
		Name:    "fix-perms",
		Image:   m.cfg.Image,
		Command: []string{"sh", "-c", initScript},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "session-data", MountPath: "/mnt/session-data"},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser: int64Ptr(0),
		},
	}}
	// Also mount user drive in init container if present.
	if opts.UserDrivePVC != "" {
		initContainers[0].VolumeMounts = append(initContainers[0].VolumeMounts,
			corev1.VolumeMount{Name: "user-drive", MountPath: "/mnt/user-drive"},
		)
	}

	// Build VolumeClaimTemplates for session data.
	storageSize := resource.MustParse(m.cfg.SessionStorageSize)
	vctMeta := sandboxv1alpha1.EmbeddedObjectMetadata{Name: "session-data"}
	vcts := []sandboxv1alpha1.PersistentVolumeClaimTemplate{{
		EmbeddedObjectMetadata: vctMeta,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: storageSize},
			},
		},
	}}

	// Set storage class if configured.
	if m.cfg.StorageClassName != "" {
		vcts[0].Spec.StorageClassName = &m.cfg.StorageClassName
	}

	// Create the Sandbox CR.
	sb := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: m.cfg.Namespace,
			Labels:    map[string]string{labelManagedBy: labelValue},
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			VolumeClaimTemplates: vcts,
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					InitContainers: initContainers,
					Containers: []corev1.Container{{
						Name:            sandboxContainerName,
						Image:           m.cfg.Image,
						Command:         []string{"sleep", "infinity"},
						Env:             containerEnv,
						VolumeMounts:    volumeMounts,
						ImagePullPolicy: corev1.PullAlways,
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse(m.cfg.MemoryLimit),
								corev1.ResourceCPU:    resource.MustParse(m.cfg.CPULimit),
							},
						},
					}},
					Volumes:          volumes,
					ImagePullSecrets: m.imagePullSecrets(),
					RuntimeClassName: m.runtimeClassName(),
					RestartPolicy:    corev1.RestartPolicyNever,
				},
			},
		},
	}

	if err := m.k8s.Create(ctx, sb); err != nil {
		return nil, fmt.Errorf("create sandbox CR: %w", err)
	}

	// Wait for sandbox to become ready.
	podName, _, err := m.waitForReady(ctx, sandboxName)
	if err != nil {
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

// StartContainer for K8s sandbox creates the Sandbox CR and waits for it to be ready.
// Returns the pod IP for agent server communication.
func (m *Manager) StartContainer(id string, opts process.StartOptions) error {
	_, err := m.Start(id, "sleep", []string{"infinity"}, nil, opts)
	return err
}

// StartContainerWithIP creates/starts the sandbox and returns the pod IP.
func (m *Manager) StartContainerWithIP(id string, opts process.StartOptions) (string, error) {
	ctx := context.Background()
	sandboxName := "cli-session-" + shortID(id)

	// Build environment variables for the sandbox pod.
	containerEnv := []corev1.EnvVar{{Name: "TERM", Value: "xterm-256color"}}

	// Inject proxy URL and token so the sandbox uses the cli-server proxy
	// instead of the real Anthropic API key.
	proxyBaseURL := os.Getenv("ANTHROPIC_PROXY_URL")
	if proxyBaseURL == "" {
		proxyBaseURL = "http://cli-server." + m.cfg.Namespace + ".svc.cluster.local:8080/proxy/anthropic/v1"
	}
	if opts.ProxyToken != "" {
		containerEnv = append(containerEnv,
			corev1.EnvVar{Name: "ANTHROPIC_BASE_URL", Value: proxyBaseURL},
			corev1.EnvVar{Name: "ANTHROPIC_API_KEY", Value: opts.ProxyToken},
		)
	}

	// Set opencode server password for per-session auth.
	if opts.OpencodePassword != "" {
		containerEnv = append(containerEnv, corev1.EnvVar{Name: "OPENCODE_SERVER_PASSWORD", Value: opts.OpencodePassword})
	}

	// Volume mounts for the main container.
	volumeMounts := []corev1.VolumeMount{
		{Name: "session-data", MountPath: "/home/agent"},
	}
	var volumes []corev1.Volume

	// Mount user drive PVC if provided.
	if opts.UserDrivePVC != "" {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name: "user-drive", MountPath: "/data/disk0",
		})
		volumes = append(volumes, corev1.Volume{
			Name: "user-drive",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: opts.UserDrivePVC,
				},
			},
		})
	}

	initScript := `
set -e
if [ ! -f /mnt/session-data/.initialized ]; then
  echo "Seeding session PVC from /home/agent..."
  cp -a /home/agent/. /mnt/session-data/ 2>/dev/null || true
  touch /mnt/session-data/.initialized
fi
chown -R 1000:1000 /mnt/session-data
mkdir -p /mnt/user-drive
chown -R 1000:1000 /mnt/user-drive
`
	initContainers := []corev1.Container{{
		Name:    "fix-perms",
		Image:   m.cfg.Image,
		Command: []string{"sh", "-c", initScript},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "session-data", MountPath: "/mnt/session-data"},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser: int64Ptr(0),
		},
	}}
	if opts.UserDrivePVC != "" {
		initContainers[0].VolumeMounts = append(initContainers[0].VolumeMounts,
			corev1.VolumeMount{Name: "user-drive", MountPath: "/mnt/user-drive"},
		)
	}

	storageSize := resource.MustParse(m.cfg.SessionStorageSize)
	vctMeta := sandboxv1alpha1.EmbeddedObjectMetadata{Name: "session-data"}
	vcts := []sandboxv1alpha1.PersistentVolumeClaimTemplate{{
		EmbeddedObjectMetadata: vctMeta,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: storageSize},
			},
		},
	}}
	if m.cfg.StorageClassName != "" {
		vcts[0].Spec.StorageClassName = &m.cfg.StorageClassName
	}

	// Use opencode serve as entrypoint: container runs opencode serve on port 4096.
	opencodePort := m.cfg.OpencodePort
	if opencodePort == 0 {
		opencodePort = 4096
	}

	sb := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: m.cfg.Namespace,
			Labels:    map[string]string{labelManagedBy: labelValue},
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			VolumeClaimTemplates: vcts,
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					InitContainers: initContainers,
					Containers: []corev1.Container{{
						Name:            sandboxContainerName,
						Image:           m.cfg.Image,
						Env:             containerEnv,
						VolumeMounts:    volumeMounts,
						ImagePullPolicy: corev1.PullAlways,
						Ports: []corev1.ContainerPort{{
							ContainerPort: int32(opencodePort),
							Protocol:      corev1.ProtocolTCP,
						}},
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse(m.cfg.MemoryLimit),
								corev1.ResourceCPU:    resource.MustParse(m.cfg.CPULimit),
							},
						},
					}},
					Volumes:          volumes,
					ImagePullSecrets: m.imagePullSecrets(),
					RuntimeClassName: m.runtimeClassName(),
					RestartPolicy:    corev1.RestartPolicyNever,
				},
			},
		},
	}

	if err := m.k8s.Create(ctx, sb); err != nil {
		return "", fmt.Errorf("create sandbox CR: %w", err)
	}

	_, podIP, err := m.waitForReady(ctx, sandboxName)
	if err != nil {
		_ = m.k8s.Delete(ctx, sb)
		return "", fmt.Errorf("sandbox not ready: %w", err)
	}

	return podIP, nil
}

// ResumeContainer scales a paused sandbox back to 1 replica and waits for it
// to be ready, without starting an exec session (the sidecar handles exec).
// Returns the pod IP.
func (m *Manager) ResumeContainer(id string) error {
	_, err := m.ResumeContainerWithIP(id)
	return err
}

// ResumeContainerWithIP scales a paused sandbox back to 1 replica and returns the pod IP.
func (m *Manager) ResumeContainerWithIP(id string) (string, error) {
	sandboxName := "cli-session-" + shortID(id)
	ctx := context.Background()

	// Patch sandbox replicas to 1.
	patch := []byte(`{"spec":{"replicas":1}}`)
	sb := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: m.cfg.Namespace,
		},
	}
	if err := m.k8s.Patch(ctx, sb, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return "", fmt.Errorf("patch sandbox replicas to 1: %w", err)
	}

	// Wait for pod to be ready.
	_, podIP, err := m.waitForReady(ctx, sandboxName)
	if err != nil {
		return "", fmt.Errorf("sandbox not ready after resume: %w", err)
	}
	return podIP, nil
}

// Pause scales the sandbox to 0 replicas. Pod goes away, PVC stays.
func (m *Manager) Pause(id string) error {
	m.mu.Lock()
	entry, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	if ok {
		// Close exec stream if one exists.
		entry.proc.close()
	}

	// Patch sandbox replicas to 0.
	sandboxName := "cli-session-" + shortID(id)
	if ok {
		sandboxName = entry.sandboxName
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	patch := []byte(`{"spec":{"replicas":0}}`)
	sb := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: m.cfg.Namespace,
		},
	}
	if err := m.k8s.Patch(ctx, sb, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("patch sandbox replicas to 0: %w", err)
	}
	return nil
}

// Resume scales the sandbox back to 1, waits for ready, and starts a new exec.
func (m *Manager) Resume(id, sandboxName, command string, args []string) (process.Process, error) {
	ctx := context.Background()

	// Patch sandbox replicas to 1.
	patch := []byte(`{"spec":{"replicas":1}}`)
	sb := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: m.cfg.Namespace,
		},
	}
	if err := m.k8s.Patch(ctx, sb, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return nil, fmt.Errorf("patch sandbox replicas to 1: %w", err)
	}

	// Wait for pod to be ready.
	podName, _, err := m.waitForReady(ctx, sandboxName)
	if err != nil {
		return nil, fmt.Errorf("sandbox not ready after resume: %w", err)
	}

	// Start remotecommand exec.
	fullCmd := append([]string{command}, args...)
	proc, err := startExec(m.restCfg, m.clientset, m.cfg.Namespace, podName, sandboxContainerName, fullCmd)
	if err != nil {
		return nil, fmt.Errorf("exec into resumed sandbox: %w", err)
	}

	m.mu.Lock()
	m.sessions[id] = &sessionEntry{proc: proc, sandboxName: sandboxName}
	m.mu.Unlock()

	return proc, nil
}

// waitForReady polls until the Sandbox has Ready=True and returns the backing pod name and IP.
func (m *Manager) waitForReady(ctx context.Context, sandboxName string) (podName string, podIP string, err error) {
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
			podList, err := m.clientset.CoreV1().Pods(m.cfg.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: sandboxNameHashLabel + "=" + nameHash,
			})
			if err != nil {
				time.Sleep(pollInterval)
				continue
			}
			for _, pod := range podList.Items {
				if pod.Status.Phase == corev1.PodRunning {
					return pod.Name, pod.Status.PodIP, nil
				}
			}
		}
		time.Sleep(pollInterval)
	}
	return "", "", fmt.Errorf("timed out waiting for sandbox %s", sandboxName)
}

func isSandboxReady(sb *sandboxv1alpha1.Sandbox) bool {
	for _, c := range sb.Status.Conditions {
		if c.Type == string(sandboxv1alpha1.SandboxConditionReady) && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

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

func strPtr(s string) *string   { return &s }
func int64Ptr(i int64) *int64   { return &i }

func (m *Manager) runtimeClassName() *string {
	if m.cfg.RuntimeClassName == "" {
		return nil
	}
	return strPtr(m.cfg.RuntimeClassName)
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

	if ok {
		entry.proc.close()
	}

	sandboxName := "cli-session-" + shortID(id)
	if ok {
		sandboxName = entry.sandboxName
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sb := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: m.cfg.Namespace,
		},
	}
	if err := m.k8s.Delete(ctx, sb); err != nil {
		log.Printf("failed to delete sandbox %s: %v", sandboxName, err)
	}
	return nil
}

// StopBySandboxName deletes a Sandbox CR by its name (for paused sessions with no active process).
func (m *Manager) StopBySandboxName(sandboxName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sb := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: m.cfg.Namespace,
		},
	}
	return m.k8s.Delete(ctx, sb)
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
