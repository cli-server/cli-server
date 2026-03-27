package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"strings"
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
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"

	"github.com/agentserver/agentserver/internal/db"
	"github.com/agentserver/agentserver/internal/process"
)

const (
	labelManagedBy       = "managed-by"
	labelValue           = "agentserver"
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
	namespace   string
}

// Manager manages Sandbox CRs and remotecommand exec sessions.
type Manager struct {
	cfg       Config
	db        *db.DB
	restCfg   *rest.Config
	k8s       client.Client
	clientset kubernetes.Interface
	mu        sync.RWMutex
	sessions  map[string]*sessionEntry
}

// NewManager creates a sandbox Manager using in-cluster or KUBECONFIG config.
func NewManager(cfg Config, database *db.DB) (*Manager, error) {
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
		db:        database,
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

// CleanOrphans deletes Sandbox CRs labelled managed-by=agentserver that are NOT in the known set.
// It iterates all provided workspace namespaces.
func (m *Manager) CleanOrphans(knownSandboxNames []string, namespaces []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	known := make(map[string]bool, len(knownSandboxNames))
	for _, name := range knownSandboxNames {
		known[name] = true
	}

	for _, ns := range namespaces {
		var list sandboxv1alpha1.SandboxList
		if err := m.k8s.List(ctx, &list,
			client.InNamespace(ns),
			client.MatchingLabels{labelManagedBy: labelValue},
		); err != nil {
			log.Printf("failed to list orphan sandboxes in %s: %v", ns, err)
			continue
		}
		for i := range list.Items {
			name := list.Items[i].Name
			if known[name] {
				continue
			}
			log.Printf("cleaning orphan sandbox %s in namespace %s", name, ns)
			if err := m.k8s.Delete(ctx, &list.Items[i]); err != nil {
				log.Printf("failed to delete orphan sandbox %s: %v", name, err)
			}
		}
	}
}

func (m *Manager) Start(id, command string, args, env []string, opts process.StartOptions) (process.Process, error) {
	ctx := context.Background()
	sandboxName := "agent-sandbox-" + shortID(id)
	ns := opts.Namespace

	// Build environment variables for the sandbox pod.
	containerEnv := []corev1.EnvVar{{Name: "TERM", Value: "xterm-256color"}}

	// Inject LLM provider credentials via OPENCODE_CONFIG_CONTENT (provider.anthropic.options).
	if opts.BYOKBaseURL != "" {
		opcodeConfig := BuildOpencodeConfig(m.cfg.OpencodeConfigContent, opts.BYOKAPIKey, opts.BYOKBaseURL)
		containerEnv = append(containerEnv, corev1.EnvVar{Name: "OPENCODE_CONFIG_CONTENT", Value: opcodeConfig})
	} else if opts.ProxyToken != "" {
		opcodeConfig := BuildOpencodeConfig(m.cfg.OpencodeConfigContent, opts.ProxyToken, "")
		containerEnv = append(containerEnv, corev1.EnvVar{Name: "OPENCODE_CONFIG_CONTENT", Value: opcodeConfig})
	} else if m.cfg.OpencodeConfigContent != "" {
		containerEnv = append(containerEnv, corev1.EnvVar{Name: "OPENCODE_CONFIG_CONTENT", Value: m.cfg.OpencodeConfigContent})
	}

	// Volume mounts for the main container.
	volumeMounts := []corev1.VolumeMount{
		{Name: "session-data", MountPath: "/home/agent"},
	}
	var volumes []corev1.Volume

	// Mount workspace drive PVCs if provided.
	for i, vol := range opts.WorkspaceVolumes {
		volName := fmt.Sprintf("ws-vol-%d", i)
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name: volName, MountPath: vol.MountPath,
		})
		volumes = append(volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: vol.PVCName,
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
# Ensure projects directory exists (workspace PVC mount point)
mkdir -p /mnt/session-data/projects
`
	// Add chown for each workspace volume.
	for i := range opts.WorkspaceVolumes {
		initScript += fmt.Sprintf("mkdir -p /mnt/ws-vol-%d\nchown -R 1000:1000 /mnt/ws-vol-%d\n", i, i)
	}

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
	// Also mount workspace drives in init container.
	for i := range opts.WorkspaceVolumes {
		volName := fmt.Sprintf("ws-vol-%d", i)
		initContainers[0].VolumeMounts = append(initContainers[0].VolumeMounts,
			corev1.VolumeMount{Name: volName, MountPath: fmt.Sprintf("/mnt/ws-vol-%d", i)},
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
			Namespace: ns,
			Labels:    map[string]string{labelManagedBy: labelValue},
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			VolumeClaimTemplates: vcts,
			PodTemplate: sandboxv1alpha1.PodTemplate{
				ObjectMeta: sandboxv1alpha1.PodMetadata{
					Labels: map[string]string{labelManagedBy: labelValue},
				},
				Spec: corev1.PodSpec{
					InitContainers: initContainers,
					Containers: []corev1.Container{{
						Name:            sandboxContainerName,
						Image:           m.cfg.Image,
						Command:         []string{"sleep", "infinity"},
						Env:             containerEnv,
						VolumeMounts:    volumeMounts,
						ImagePullPolicy: corev1.PullAlways,
						WorkingDir:      "/home/agent/projects",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: memoryQuantity(opts.Memory),
								corev1.ResourceCPU:    cpuQuantity(opts.CPU),
							},
						},
					}},
					Volumes:          volumes,
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
	podName, _, err := m.waitForReady(ctx, ns, sandboxName)
	if err != nil {
		_ = m.k8s.Delete(ctx, sb)
		return nil, fmt.Errorf("sandbox not ready: %w", err)
	}

	// Build the full command.
	fullCmd := append([]string{command}, args...)

	// Start remotecommand exec into the pod.
	proc, err := startExec(m.restCfg, m.clientset, ns, podName, sandboxContainerName, fullCmd)
	if err != nil {
		_ = m.k8s.Delete(ctx, sb)
		return nil, fmt.Errorf("exec into sandbox: %w", err)
	}

	m.mu.Lock()
	m.sessions[id] = &sessionEntry{proc: proc, sandboxName: sandboxName, namespace: ns}
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
	sandboxName := "agent-sandbox-" + shortID(id)
	ns := opts.Namespace

	// Build environment variables for the sandbox pod.
	containerEnv := []corev1.EnvVar{{Name: "TERM", Value: "xterm-256color"}}

	// Inject LLM provider credentials.
	proxyBaseURL := ExtractProxyBaseURL(m.cfg.OpencodeConfigContent)
	if opts.BYOKBaseURL != "" {
		containerEnv = append(containerEnv,
			corev1.EnvVar{Name: "ANTHROPIC_API_KEY", Value: opts.BYOKAPIKey},
		)
	} else if opts.ProxyToken != "" && proxyBaseURL != "" {
		containerEnv = append(containerEnv,
			corev1.EnvVar{Name: "ANTHROPIC_API_KEY", Value: opts.ProxyToken},
		)
	}

	// Select image, port, and command based on sandbox type.
	sandboxImage := m.cfg.Image
	containerPort := m.cfg.OpencodePort
	if containerPort == 0 {
		containerPort = 4096
	}
	var containerCmd []string

	switch opts.SandboxType {
	case "openclaw":
		if m.cfg.OpenclawImage != "" {
			sandboxImage = m.cfg.OpenclawImage
		}
		containerPort = m.cfg.OpenclawPort
		if containerPort == 0 {
			containerPort = 18789
		}
		// Build openclaw config JSON with gateway settings and LLM provider.
		cfgBaseURL, cfgAPIKey := proxyBaseURL, opts.ProxyToken
		var cfgModels []process.LLMModel
		if opts.BYOKBaseURL != "" {
			cfgBaseURL = opts.BYOKBaseURL
			cfgAPIKey = opts.BYOKAPIKey
			cfgModels = opts.BYOKModels
		}
		openclawCfg := BuildOpenclawConfig(cfgBaseURL, cfgAPIKey, opts.OpenclawToken, m.cfg.OpenclawWeixinEnabled, cfgModels)
		containerCmd = []string{"sh", "-c", `mkdir -p ~/.openclaw && cat > ~/.openclaw/openclaw.json << 'CFGEOF'
` + openclawCfg + `
CFGEOF
exec node openclaw.mjs gateway --allow-unconfigured --bind lan`}
		// Ensure ~ resolves to the PVC mount so credentials and conversation
		// data persist across pause/resume.
		containerEnv = append(containerEnv, corev1.EnvVar{Name: "HOME", Value: "/home/agent"})
		if opts.OpenclawToken != "" {
			containerEnv = append(containerEnv, corev1.EnvVar{Name: "OPENCLAW_GATEWAY_TOKEN", Value: opts.OpenclawToken})
		}
	case "nanoclaw":
		if m.cfg.NanoclawImage == "" {
			return "", fmt.Errorf("NANOCLAW_IMAGE not configured: set the environment variable to the nanoclaw container image (build with Dockerfile.nanoclaw)")
		}
		sandboxImage = m.cfg.NanoclawImage
		containerPort = 3002 // Health/bridge endpoint
		imBridgeURL := ""
		bridgeSecret := ""
		if m.cfg.NanoclawIMBridgeEnabled && opts.NanoclawBridgeSecret != "" {
			bridgeSecret = opts.NanoclawBridgeSecret
			if m.cfg.NanoclawBridgeBaseURL != "" && opts.SandboxID != "" {
				imBridgeURL = m.cfg.NanoclawBridgeBaseURL + "/api/internal/nanoclaw/" + opts.SandboxID + "/im/send"
			}
		}
		// NanoClaw uses the Anthropic SDK (via Claude Code) which appends
		// /v1/messages to ANTHROPIC_BASE_URL. The proxyBaseURL from opencode
		// config already includes /v1 (opencode appends /messages directly).
		// Use the raw LLM proxy URL without path for NanoClaw.
		nanoclawProxyURL := strings.TrimSuffix(proxyBaseURL, "/v1")
		nanoclawCfg := BuildNanoclawConfig(
			nanoclawProxyURL, opts.ProxyToken, "Andy",
			imBridgeURL, bridgeSecret,
			opts.BYOKBaseURL, opts.BYOKAPIKey,
		)
		containerEnv = append(containerEnv, corev1.EnvVar{Name: "NANOCLAW_CONFIG_CONTENT", Value: nanoclawCfg})
		// NANOCLAW_NO_CONTAINER must be a real env var (not just in .env file)
		// because container-runtime.ts reads process.env before NanoClaw's
		// readEnvFile() has a chance to parse the .env file.
		containerEnv = append(containerEnv, corev1.EnvVar{Name: "NANOCLAW_NO_CONTAINER", Value: "true"})
	default: // "opencode"
		if opts.OpencodeToken != "" {
			containerEnv = append(containerEnv, corev1.EnvVar{Name: "OPENCODE_SERVER_PASSWORD", Value: opts.OpencodeToken})
		}
		// Merge LLM provider config into OPENCODE_CONFIG_CONTENT.
		apiKey, overrideURL := opts.ProxyToken, ""
		if opts.BYOKBaseURL != "" {
			apiKey = opts.BYOKAPIKey
			overrideURL = opts.BYOKBaseURL
		}
		opcodeConfig := BuildOpencodeConfig(m.cfg.OpencodeConfigContent, apiKey, overrideURL)
		containerEnv = append(containerEnv, corev1.EnvVar{Name: "OPENCODE_CONFIG_CONTENT", Value: opcodeConfig})
	}

	// Volume mounts for the main container.
	volumeMounts := []corev1.VolumeMount{
		{Name: "session-data", MountPath: "/home/agent"},
	}
	var volumes []corev1.Volume

	// Mount workspace drive PVCs if provided.
	for i, vol := range opts.WorkspaceVolumes {
		volName := fmt.Sprintf("ws-vol-%d", i)
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name: volName, MountPath: vol.MountPath,
		})
		volumes = append(volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: vol.PVCName,
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
# Ensure projects directory exists (workspace PVC mount point)
mkdir -p /mnt/session-data/projects
`
	// Add chown for each workspace volume.
	for i := range opts.WorkspaceVolumes {
		initScript += fmt.Sprintf("mkdir -p /mnt/ws-vol-%d\nchown -R 1000:1000 /mnt/ws-vol-%d\n", i, i)
	}

	initContainers := []corev1.Container{{
		Name:    "fix-perms",
		Image:   sandboxImage,
		Command: []string{"sh", "-c", initScript},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "session-data", MountPath: "/mnt/session-data"},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser: int64Ptr(0),
		},
	}}
	for i := range opts.WorkspaceVolumes {
		volName := fmt.Sprintf("ws-vol-%d", i)
		initContainers[0].VolumeMounts = append(initContainers[0].VolumeMounts,
			corev1.VolumeMount{Name: volName, MountPath: fmt.Sprintf("/mnt/ws-vol-%d", i)},
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

	workingDir := "/home/agent/projects"
	switch opts.SandboxType {
	case "openclaw":
		workingDir = "/app"
	case "nanoclaw":
		workingDir = "/app"
	}

	mainContainer := corev1.Container{
		Name:            sandboxContainerName,
		Image:           sandboxImage,
		Env:             containerEnv,
		VolumeMounts:    volumeMounts,
		ImagePullPolicy: corev1.PullAlways,
		WorkingDir:      workingDir,
		Ports: []corev1.ContainerPort{{
			ContainerPort: int32(containerPort),
			Protocol:      corev1.ProtocolTCP,
		}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt32(int32(containerPort)),
				},
			},
			InitialDelaySeconds: 2,
			PeriodSeconds:       2,
			FailureThreshold:    30,
		},
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: memoryQuantity(opts.Memory),
				corev1.ResourceCPU:    cpuQuantity(opts.CPU),
			},
		},
	}
	if opts.SandboxType == "nanoclaw" {
		mainContainer.ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/health",
					Port: intstr.FromInt32(int32(containerPort)),
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       5,
			FailureThreshold:    30,
		}
	}
	if len(containerCmd) > 0 {
		mainContainer.Command = containerCmd
	}

	sb := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: ns,
			Labels:    map[string]string{labelManagedBy: labelValue},
		},
		Spec: sandboxv1alpha1.SandboxSpec{
			VolumeClaimTemplates: vcts,
			PodTemplate: sandboxv1alpha1.PodTemplate{
				ObjectMeta: sandboxv1alpha1.PodMetadata{
					Labels: map[string]string{labelManagedBy: labelValue},
				},
				Spec: corev1.PodSpec{
					InitContainers:   initContainers,
					Containers:       []corev1.Container{mainContainer},
					Volumes:          volumes,
					RuntimeClassName: m.runtimeClassNameFor(opts.SandboxType),
					RestartPolicy:    corev1.RestartPolicyNever,
				},
			},
		},
	}

	if err := m.k8s.Create(ctx, sb); err != nil {
		return "", fmt.Errorf("create sandbox CR: %w", err)
	}

	_, podIP, err := m.waitForReady(ctx, ns, sandboxName)
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
	sandboxName := "agent-sandbox-" + shortID(id)
	ctx := context.Background()

	ns, err := m.lookupNamespace(id)
	if err != nil {
		return "", fmt.Errorf("resolve namespace for resume: %w", err)
	}

	// Patch sandbox replicas to 1.
	patch := []byte(`{"spec":{"replicas":1}}`)
	sb := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: ns,
		},
	}
	if err := m.k8s.Patch(ctx, sb, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return "", fmt.Errorf("patch sandbox replicas to 1: %w", err)
	}

	// Wait for pod to be ready.
	_, podIP, err := m.waitForReady(ctx, ns, sandboxName)
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
	sandboxName := "agent-sandbox-" + shortID(id)
	var ns string
	if ok {
		sandboxName = entry.sandboxName
		ns = entry.namespace
	}
	if ns == "" {
		var err error
		ns, err = m.lookupNamespace(id)
		if err != nil {
			return fmt.Errorf("resolve namespace for pause: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	patch := []byte(`{"spec":{"replicas":0}}`)
	sb := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: ns,
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

	ns, err := m.lookupNamespace(id)
	if err != nil {
		return nil, fmt.Errorf("resolve namespace for resume: %w", err)
	}

	// Patch sandbox replicas to 1.
	patch := []byte(`{"spec":{"replicas":1}}`)
	sb := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: ns,
		},
	}
	if err := m.k8s.Patch(ctx, sb, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return nil, fmt.Errorf("patch sandbox replicas to 1: %w", err)
	}

	// Wait for pod to be ready.
	podName, _, err := m.waitForReady(ctx, ns, sandboxName)
	if err != nil {
		return nil, fmt.Errorf("sandbox not ready after resume: %w", err)
	}

	// Start remotecommand exec.
	fullCmd := append([]string{command}, args...)
	proc, err := startExec(m.restCfg, m.clientset, ns, podName, sandboxContainerName, fullCmd)
	if err != nil {
		return nil, fmt.Errorf("exec into resumed sandbox: %w", err)
	}

	m.mu.Lock()
	m.sessions[id] = &sessionEntry{proc: proc, sandboxName: sandboxName, namespace: ns}
	m.mu.Unlock()

	return proc, nil
}

// waitForReady polls until the Sandbox has Ready=True and returns the backing pod name and IP.
func (m *Manager) waitForReady(ctx context.Context, namespace, sandboxName string) (podName string, podIP string, err error) {
	deadline := time.Now().Add(pollTimeout)
	nameHash := nameHash(sandboxName)

	for time.Now().Before(deadline) {
		var sb sandboxv1alpha1.Sandbox
		key := client.ObjectKey{Namespace: namespace, Name: sandboxName}
		if err := m.k8s.Get(ctx, key, &sb); err != nil {
			time.Sleep(pollInterval)
			continue
		}

		if isSandboxReady(&sb) {
			podList, err := m.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
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

func strPtr(s string) *string { return &s }
func int64Ptr(i int64) *int64 { return &i }

// cpuQuantity converts millicores to a K8s resource.Quantity.
// Falls back to 2000m (2 cores) if zero.
func cpuQuantity(millis int) resource.Quantity {
	if millis == 0 {
		millis = 2000
	}
	return *resource.NewMilliQuantity(int64(millis), resource.DecimalSI)
}

// memoryQuantity converts bytes to a K8s resource.Quantity.
// Falls back to 2Gi if zero.
func memoryQuantity(bytes int64) resource.Quantity {
	if bytes == 0 {
		bytes = 2 * 1024 * 1024 * 1024
	}
	return *resource.NewQuantity(bytes, resource.BinarySI)
}

func (m *Manager) runtimeClassName() *string {
	if m.cfg.RuntimeClassName == "" {
		return nil
	}
	return strPtr(m.cfg.RuntimeClassName)
}

func (m *Manager) runtimeClassNameFor(sandboxType string) *string {
	switch sandboxType {
	case "openclaw":
		if m.cfg.OpenclawRuntimeClassName != "" {
			return strPtr(m.cfg.OpenclawRuntimeClassName)
		}
	case "nanoclaw":
		if m.cfg.NanoclawRuntimeClassName != "" {
			return strPtr(m.cfg.NanoclawRuntimeClassName)
		}
	}
	return m.runtimeClassName()
}

// lookupNamespace resolves the K8s namespace for a sandbox by looking up
// sandbox → workspace → k8s_namespace in the database.
func (m *Manager) lookupNamespace(sandboxID string) (string, error) {
	if m.db == nil {
		return "", fmt.Errorf("no database reference for namespace lookup")
	}
	sbx, err := m.db.GetSandbox(sandboxID)
	if err != nil {
		return "", fmt.Errorf("get sandbox %s: %w", sandboxID, err)
	}
	if sbx == nil {
		return "", fmt.Errorf("sandbox %s not found", sandboxID)
	}
	ws, err := m.db.GetWorkspace(sbx.WorkspaceID)
	if err != nil {
		return "", fmt.Errorf("get workspace %s: %w", sbx.WorkspaceID, err)
	}
	if ws == nil {
		return "", fmt.Errorf("workspace %s not found", sbx.WorkspaceID)
	}
	if !ws.K8sNamespace.Valid || ws.K8sNamespace.String == "" {
		return "", fmt.Errorf("workspace %s has no k8s namespace", ws.ID)
	}
	return ws.K8sNamespace.String, nil
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

	sandboxName := "agent-sandbox-" + shortID(id)
	var ns string
	if ok {
		sandboxName = entry.sandboxName
		ns = entry.namespace
	}
	if ns == "" {
		var err error
		ns, err = m.lookupNamespace(id)
		if err != nil {
			log.Printf("failed to resolve namespace for stop %s: %v", id, err)
			return nil
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sb := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: ns,
		},
	}
	if err := m.k8s.Delete(ctx, sb); err != nil {
		log.Printf("failed to delete sandbox %s: %v", sandboxName, err)
	}
	return nil
}

// StopBySandboxName deletes a Sandbox CR by its name in the given namespace.
func (m *Manager) StopBySandboxName(namespace, sandboxName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sb := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: namespace,
		},
	}
	return m.k8s.Delete(ctx, sb)
}

// ExecSimple runs a command in a sandbox pod and returns its stdout.
// It is a one-shot exec (no stdin/TTY) intended for short-lived commands
// like writing config files or restarting a gateway.
func (m *Manager) ExecSimple(ctx context.Context, sandboxID string, command []string) (string, error) {
	// Resolve pod namespace and name.
	ns, err := m.lookupNamespace(sandboxID)
	if err != nil {
		return "", err
	}
	sandboxName := "agent-sandbox-" + shortID(sandboxID)
	podName, _, err := m.waitForReady(ctx, ns, sandboxName)
	if err != nil {
		return "", fmt.Errorf("pod not ready: %w", err)
	}

	req := m.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(ns).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: sandboxContainerName,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	wsExec, err := remotecommand.NewWebSocketExecutor(m.restCfg, "POST", req.URL().String())
	if err != nil {
		return "", err
	}
	spdyExec, err := remotecommand.NewSPDYExecutor(m.restCfg, "POST", req.URL())
	if err != nil {
		return "", err
	}
	executor, err := remotecommand.NewFallbackExecutor(wsExec, spdyExec, func(error) bool { return true })
	if err != nil {
		return "", err
	}

	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return "", fmt.Errorf("exec: %w (stderr: %s)", err, stderr.String())
	}
	return stdout.String(), nil
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
