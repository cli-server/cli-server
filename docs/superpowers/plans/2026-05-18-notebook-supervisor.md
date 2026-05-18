# Notebook Supervisor Implementation Plan (Plan 3a)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** New `internal/notebooksupervisor/` package that creates and tracks one Jupyter Server `Deployment` + `Service` per workspace in k8s, with idle-reap.

**Architecture:** Same pattern as `internal/codexappgateway/supervisor` (per-workspace lifecycle map keyed by `Key{WorkspaceID}`), but the managed unit is a k8s Deployment/Service pair instead of a subprocess. Uses `k8s.io/client-go/kubernetes` (already a dep via `internal/sandbox/`). Spawn is idempotent via `EnsureRunning`. Idle reaper deletes the Deployment/Service when `lastActive` exceeds TTL — PV (workspace volume) survives, so the next EnsureRunning brings the user back into the same files.

**Tech Stack:** Go 1.26 · `k8s.io/client-go` (apps/v1 Deployments, core/v1 Services) · `k8s.io/client-go/kubernetes/fake` for tests · existing helm chart with new RBAC rules.

**Out of scope (separate plans):**
- Jupyter proxy handler (`/api/notebooks/{ws}/*`) — **Plan 3b**
- Per-kernel user_id injection (custom IdentityProvider + KernelProvisioner Python) — **Plan 3b**
- Web UI panels — **Plan 3c**
- Real driver / consumer of `EnsureRunning` — Plan 3b's handler is the first caller; this plan provides API + tests only

---

## File Structure

```
internal/notebooksupervisor/                    # NEW package
├── doc.go                                       # package summary
├── types.go                                     # Key, Config, Handle
├── spawn.go                                     # builds Deployment + Service (pure)
├── spawn_test.go
├── supervisor.go                                # EnsureRunning + Touch + Stop + Stats
├── supervisor_test.go                           # uses k8s fake clientset
├── reaper.go                                    # background idle reap loop
└── reaper_test.go

deploy/helm/agentserver/values.yaml              # MODIFIED — notebook: block
deploy/helm/agentserver/templates/rbac.yaml      # MODIFIED — Deployment + Service RBAC

cmd/serve.go                                     # MODIFIED — construct + run supervisor
```

The supervisor is constructed in `cmd/serve.go` and made available on `*server.Server` for Plan 3b to consume, but no HTTP route exists yet — this plan's deliverable is just "package builds, tests pass, deploys with the right RBAC".

---

## Design Decisions Locked Before Tasks

**1. Per-workspace Deployment + Service in the workspace namespace.** Matches sandbox manager's per-workspace ns pattern (`internal/sandbox/manager.go`). Naming: Deployment `notebook-{workspaceID}`, Service `notebook-{workspaceID}`. Sanitised workspaceID is k8s-safe (lowercase alphanumeric + `-`, ≤ 63 chars after the `notebook-` prefix — error out if doesn't fit).

**2. Workspace ns lookup is the caller's responsibility.** Supervisor receives the ns string in `Key`. Resolving workspaceID → ns lives in agentserver's existing workspace layer; supervisor stays stateless about that mapping.

**3. Image / resources are config, not hardcoded.** `Config` has `Image string`, `ImagePullPolicy string`, `CPURequest/Limit`, `MemoryRequest/Limit`, `EphemeralStorage` — defaulted from helm values, never from inside the supervisor.

**4. EnsureRunning is idempotent on Deployment + Service identity.** Calls `Create()`; on `AlreadyExists`, fetches existing and verifies pod-template-hash matches `Config.Image`. If image drifted (upgraded), reports it but does NOT auto-reconcile — drift triggers a Touch+Reap+respawn through the normal flow.

**5. Readiness signal.** EnsureRunning blocks up to `Config.ReadyTimeout` (default 60s) waiting for the Deployment to have `Status.ReadyReplicas >= 1`. Returns `*Handle{ServiceURL: "http://notebook-{ws}.{ns}.svc.cluster.local:8888"}` once ready. Timeout returns a typed error.

**6. Idle reap is `lastActive + TTL < now`.** `Touch(Key)` updates `lastActive`. Plan 3b's proxy will call Touch on every request. TTL default 4h. Reaper runs every 5 min. Deletion is best-effort: 404 = already gone (no-op).

**7. Tests use `k8s.io/client-go/kubernetes/fake`.** No real cluster needed in CI. Fake supports `Watch` for the readiness wait, but we use `Get` + polling for simplicity (matches what sandbox manager does).

**8. ServiceAccount on the spawned pod.** Notebook pods get their own SA `notebook` in the workspace ns, with no extra permissions (Jupyter doesn't need k8s access). Plan 3a's RBAC just lets agentserver CREATE/DELETE the SA; the SA itself binds to nothing.

**9. No PV provisioning in this plan.** Workspace PV mounted at `/workspace` is assumed pre-existing (created by agentserver's workspace provisioner — same dependency the sandbox manager has). Volume mount config goes into the Deployment template; spec is the PV name pattern.

**10. No e2e against real k8s in this plan.** Validation = `go test ./internal/notebooksupervisor` with fake clientset. Real-cluster smoke comes in Plan 3b when the proxy is the consumer.

---

## Task 1: Package skeleton + types

**Files:**
- Create: `internal/notebooksupervisor/doc.go`
- Create: `internal/notebooksupervisor/types.go`
- Create: `internal/notebooksupervisor/types_test.go`

- [ ] **Step 1: Write failing test for Key/Config validation**

Create `internal/notebooksupervisor/types_test.go`:

```go
package notebooksupervisor

import (
	"strings"
	"testing"
	"time"
)

func TestKey_DeploymentName(t *testing.T) {
	k := Key{WorkspaceID: "ws-abc-123", Namespace: "agentserver-ws-abc-123"}
	if got := k.DeploymentName(); got != "notebook-ws-abc-123" {
		t.Fatalf("got %q", got)
	}
}

func TestKey_DeploymentName_TooLong(t *testing.T) {
	k := Key{WorkspaceID: strings.Repeat("x", 100), Namespace: "ns"}
	if _, err := k.SafeDeploymentName(); err == nil {
		t.Fatal("expected error for too-long name")
	}
}

func TestKey_DeploymentName_InvalidChars(t *testing.T) {
	k := Key{WorkspaceID: "Has_Caps_AndUnderscores", Namespace: "ns"}
	if _, err := k.SafeDeploymentName(); err == nil {
		t.Fatal("expected error for invalid chars")
	}
}

func TestConfig_Defaults(t *testing.T) {
	c := Config{}.WithDefaults()
	if c.ReadyTimeout != 60*time.Second {
		t.Errorf("ready timeout = %v", c.ReadyTimeout)
	}
	if c.IdleTTL != 4*time.Hour {
		t.Errorf("idle ttl = %v", c.IdleTTL)
	}
	if c.ReapInterval != 5*time.Minute {
		t.Errorf("reap interval = %v", c.ReapInterval)
	}
	if c.Image == "" {
		t.Error("image must have a default")
	}
}
```

- [ ] **Step 2: Run, confirm failure**

```bash
cd /root/agentserver
go test ./internal/notebooksupervisor -v
```
Expected: FAIL — package missing.

- [ ] **Step 3: Implement doc + types**

Create `internal/notebooksupervisor/doc.go`:

```go
// Package notebooksupervisor manages per-workspace Jupyter Server
// Deployments + Services in Kubernetes. EnsureRunning spawns on first
// call, returns a cached handle on subsequent calls. Touch refreshes
// lastActive so idle workspaces get reaped after Config.IdleTTL.
package notebooksupervisor
```

Create `internal/notebooksupervisor/types.go`:

```go
package notebooksupervisor

import (
	"fmt"
	"regexp"
	"time"
)

// Key identifies one workspace's notebook Deployment. Namespace is the
// k8s namespace the Deployment + Service live in (typically per-workspace).
type Key struct {
	WorkspaceID string
	Namespace   string
}

// DeploymentName returns the Deployment + Service name (they share one).
// Assumes the workspace id is already k8s-safe; use SafeDeploymentName
// when you need explicit validation.
func (k Key) DeploymentName() string {
	return "notebook-" + k.WorkspaceID
}

var workspaceIDPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// SafeDeploymentName validates the workspaceID against k8s DNS-1123 rules
// AND the 63-char Deployment name limit. Returns the prefixed name on
// success.
func (k Key) SafeDeploymentName() (string, error) {
	if !workspaceIDPattern.MatchString(k.WorkspaceID) {
		return "", fmt.Errorf("workspace id %q is not a valid k8s DNS-1123 label", k.WorkspaceID)
	}
	name := k.DeploymentName()
	if len(name) > 63 {
		return "", fmt.Errorf("deployment name %q exceeds 63 chars", name)
	}
	return name, nil
}

// Handle is what callers get back from EnsureRunning. ServiceURL is the
// cluster-internal HTTP url to reach the Jupyter Server.
type Handle struct {
	ServiceURL string
}

// Config tunes Supervisor lifetime + per-Deployment template.
type Config struct {
	Image            string        // e.g. "ghcr.io/agentserver/agentserver-notebook:dev"
	ImagePullPolicy  string        // e.g. "Always" or "IfNotPresent"
	CPURequest       string        // k8s quantity, e.g. "500m"
	CPULimit         string        // e.g. "2"
	MemoryRequest    string        // e.g. "1Gi"
	MemoryLimit      string        // e.g. "4Gi"
	EphemeralStorage string        // e.g. "5Gi"
	WorkspacePVCName string        // mounted at /workspace inside the pod
	ReadyTimeout     time.Duration // how long EnsureRunning blocks waiting for ReadyReplicas >= 1
	IdleTTL          time.Duration // idle threshold for reaper
	ReapInterval     time.Duration // how often the reaper loop ticks
}

// WithDefaults returns a copy of c with zero-valued fields replaced by
// safe defaults. Always returns a Config; never errors.
func (c Config) WithDefaults() Config {
	if c.Image == "" {
		c.Image = "ghcr.io/agentserver/agentserver-notebook:dev"
	}
	if c.ImagePullPolicy == "" {
		c.ImagePullPolicy = "IfNotPresent"
	}
	if c.CPURequest == "" {
		c.CPURequest = "500m"
	}
	if c.CPULimit == "" {
		c.CPULimit = "2"
	}
	if c.MemoryRequest == "" {
		c.MemoryRequest = "1Gi"
	}
	if c.MemoryLimit == "" {
		c.MemoryLimit = "4Gi"
	}
	if c.EphemeralStorage == "" {
		c.EphemeralStorage = "5Gi"
	}
	if c.ReadyTimeout == 0 {
		c.ReadyTimeout = 60 * time.Second
	}
	if c.IdleTTL == 0 {
		c.IdleTTL = 4 * time.Hour
	}
	if c.ReapInterval == 0 {
		c.ReapInterval = 5 * time.Minute
	}
	return c
}
```

- [ ] **Step 4: Run, confirm pass**

```bash
cd /root/agentserver
go vet ./internal/notebooksupervisor
go test ./internal/notebooksupervisor -v
```
Expected: 4 passed.

- [ ] **Step 5: Commit**

```bash
cd /root/agentserver
git add internal/notebooksupervisor/doc.go \
        internal/notebooksupervisor/types.go \
        internal/notebooksupervisor/types_test.go
git commit -m "feat(notebooksupervisor): types + Key validation

Key, Handle, Config with WithDefaults. Key.SafeDeploymentName enforces
DNS-1123 label + 63-char limit before any k8s call."
```

---

## Task 2: `spawn.go` — pure Deployment + Service builders

**Files:**
- Create: `internal/notebooksupervisor/spawn.go`
- Create: `internal/notebooksupervisor/spawn_test.go`

These are pure functions: given a Key + Config, return Deployment / Service objects. No k8s I/O. Easy to TDD.

- [ ] **Step 1: Write failing test**

Create `internal/notebooksupervisor/spawn_test.go`:

```go
package notebooksupervisor

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestBuildDeployment_BasicShape(t *testing.T) {
	c := Config{
		Image:            "img:tag",
		CPURequest:       "500m",
		CPULimit:         "2",
		MemoryRequest:    "1Gi",
		MemoryLimit:      "4Gi",
		EphemeralStorage: "5Gi",
		WorkspacePVCName: "ws-pvc",
	}.WithDefaults()
	k := Key{WorkspaceID: "alpha", Namespace: "ns-alpha"}

	d, err := BuildDeployment(k, c)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if d.Name != "notebook-alpha" {
		t.Errorf("Name=%q", d.Name)
	}
	if d.Namespace != "ns-alpha" {
		t.Errorf("Namespace=%q", d.Namespace)
	}
	if *d.Spec.Replicas != 1 {
		t.Errorf("Replicas=%d", *d.Spec.Replicas)
	}
	pod := d.Spec.Template.Spec
	if len(pod.Containers) != 1 {
		t.Fatalf("containers=%d", len(pod.Containers))
	}
	cn := pod.Containers[0]
	if cn.Image != "img:tag" {
		t.Errorf("image=%q", cn.Image)
	}
	if string(cn.ImagePullPolicy) != "IfNotPresent" {
		t.Errorf("pull policy=%q", cn.ImagePullPolicy)
	}
	if cn.Resources.Requests[corev1.ResourceCPU] != resource.MustParse("500m") {
		t.Errorf("cpu request=%v", cn.Resources.Requests[corev1.ResourceCPU])
	}
	if cn.Resources.Limits[corev1.ResourceMemory] != resource.MustParse("4Gi") {
		t.Errorf("mem limit=%v", cn.Resources.Limits[corev1.ResourceMemory])
	}
	// Workspace PVC mounted at /workspace
	var foundMount bool
	for _, m := range cn.VolumeMounts {
		if m.MountPath == "/workspace" && m.Name == "workspace" {
			foundMount = true
		}
	}
	if !foundMount {
		t.Error("no /workspace volume mount")
	}
	var foundVol bool
	for _, v := range pod.Volumes {
		if v.Name == "workspace" && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "ws-pvc" {
			foundVol = true
		}
	}
	if !foundVol {
		t.Error("no workspace PVC volume")
	}
	// 8888 container port
	var foundPort bool
	for _, p := range cn.Ports {
		if p.ContainerPort == 8888 {
			foundPort = true
		}
	}
	if !foundPort {
		t.Error("no port 8888")
	}
}

func TestBuildDeployment_LabelsAndSelector(t *testing.T) {
	c := Config{Image: "x"}.WithDefaults()
	k := Key{WorkspaceID: "alpha", Namespace: "ns"}
	d, err := BuildDeployment(k, c)
	if err != nil {
		t.Fatal(err)
	}
	want := "notebook-alpha"
	if d.Labels["app"] != want {
		t.Errorf("d.Labels[app]=%q", d.Labels["app"])
	}
	if d.Spec.Selector.MatchLabels["app"] != want {
		t.Errorf("selector=%q", d.Spec.Selector.MatchLabels["app"])
	}
	if d.Spec.Template.Labels["app"] != want {
		t.Errorf("pod labels=%q", d.Spec.Template.Labels["app"])
	}
	if d.Labels["managed-by"] != "agentserver" {
		t.Errorf("managed-by=%q", d.Labels["managed-by"])
	}
	if d.Labels["workspace-id"] != "alpha" {
		t.Errorf("workspace-id=%q", d.Labels["workspace-id"])
	}
}

func TestBuildDeployment_InvalidWorkspaceID(t *testing.T) {
	_, err := BuildDeployment(Key{WorkspaceID: "INVALID", Namespace: "ns"},
		Config{Image: "x"}.WithDefaults())
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestBuildService_PointsAtDeployment(t *testing.T) {
	k := Key{WorkspaceID: "alpha", Namespace: "ns-alpha"}
	s, err := BuildService(k)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "notebook-alpha" || s.Namespace != "ns-alpha" {
		t.Errorf("name/ns=%q/%q", s.Name, s.Namespace)
	}
	if s.Spec.Selector["app"] != "notebook-alpha" {
		t.Errorf("selector=%q", s.Spec.Selector["app"])
	}
	if len(s.Spec.Ports) != 1 || s.Spec.Ports[0].Port != 8888 {
		t.Errorf("ports=%+v", s.Spec.Ports)
	}
}

func TestServiceURL(t *testing.T) {
	got := ServiceURL(Key{WorkspaceID: "alpha", Namespace: "ns-alpha"})
	want := "http://notebook-alpha.ns-alpha.svc.cluster.local:8888"
	if got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}
```

- [ ] **Step 2: Run, confirm failure**

```bash
cd /root/agentserver
go test ./internal/notebooksupervisor -run Build -v
```
Expected: FAIL — `BuildDeployment`/`BuildService`/`ServiceURL` undefined.

- [ ] **Step 3: Implement spawn.go**

Create `internal/notebooksupervisor/spawn.go`:

```go
package notebooksupervisor

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	notebookPort        int32 = 8888
	managedByLabel            = "managed-by"
	managedByValue            = "agentserver"
	workspaceIDLabel          = "workspace-id"
	workspaceVolumeName       = "workspace"
	workspaceMountPath        = "/workspace"
)

// BuildDeployment produces a fresh Deployment spec for a notebook pod.
// Pure function — no I/O. Returns an error if the Key has an invalid
// workspace id.
func BuildDeployment(k Key, c Config) (*appsv1.Deployment, error) {
	name, err := k.SafeDeploymentName()
	if err != nil {
		return nil, err
	}
	labels := map[string]string{
		"app":             name,
		managedByLabel:    managedByValue,
		workspaceIDLabel:  k.WorkspaceID,
	}
	replicas := int32(1)
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(c.CPURequest),
			corev1.ResourceMemory: resource.MustParse(c.MemoryRequest),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse(c.CPULimit),
			corev1.ResourceMemory:           resource.MustParse(c.MemoryLimit),
			corev1.ResourceEphemeralStorage: resource.MustParse(c.EphemeralStorage),
		},
	}
	container := corev1.Container{
		Name:            "notebook",
		Image:           c.Image,
		ImagePullPolicy: corev1.PullPolicy(c.ImagePullPolicy),
		Ports: []corev1.ContainerPort{
			{ContainerPort: notebookPort, Name: "http", Protocol: corev1.ProtocolTCP},
		},
		Resources: resources,
		VolumeMounts: []corev1.VolumeMount{
			{Name: workspaceVolumeName, MountPath: workspaceMountPath},
		},
	}
	pod := corev1.PodSpec{
		Containers: []corev1.Container{container},
		Volumes: []corev1.Volume{
			{
				Name: workspaceVolumeName,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: c.WorkspacePVCName,
					},
				},
			},
		},
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: k.Namespace, Labels: labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       pod,
			},
		},
	}, nil
}

// BuildService returns a ClusterIP Service that fronts the Deployment.
func BuildService(k Key) (*corev1.Service, error) {
	name, err := k.SafeDeploymentName()
	if err != nil {
		return nil, err
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: k.Namespace,
			Labels: map[string]string{
				"app":          name,
				managedByLabel: managedByValue,
			},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": name},
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       notebookPort,
				TargetPort: intstr.FromInt32(notebookPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}, nil
}

// ServiceURL returns the cluster-internal HTTP url for the Service.
func ServiceURL(k Key) string {
	name, err := k.SafeDeploymentName()
	if err != nil {
		return ""
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", name, k.Namespace, notebookPort)
}
```

- [ ] **Step 4: Run, confirm pass**

```bash
cd /root/agentserver
go vet ./internal/notebooksupervisor
go test ./internal/notebooksupervisor -v
```
Expected: 9 passed (4 types + 5 spawn).

- [ ] **Step 5: Commit**

```bash
cd /root/agentserver
git add internal/notebooksupervisor/spawn.go \
        internal/notebooksupervisor/spawn_test.go
git commit -m "feat(notebooksupervisor): pure Deployment + Service builders

BuildDeployment / BuildService / ServiceURL. No k8s I/O — easy to unit
test the resource shapes. WithDefaults from Task 1 covers config fills."
```

---

## Task 3: `Supervisor` — EnsureRunning / Touch / Stop

**Files:**
- Create: `internal/notebooksupervisor/supervisor.go`
- Create: `internal/notebooksupervisor/supervisor_test.go`

The Supervisor wraps a `kubernetes.Interface` so tests can pass `fake.NewClientset()`.

- [ ] **Step 1: Write failing tests**

Create `internal/notebooksupervisor/supervisor_test.go`:

```go
package notebooksupervisor

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func testConfig() Config {
	return Config{
		Image:            "img:tag",
		WorkspacePVCName: "pvc",
	}.WithDefaults()
}

// markReady patches the Deployment to ReadyReplicas=1 after a tiny delay
// so EnsureRunning's wait loop succeeds. Used by tests.
func markReady(c *fake.Clientset, name, ns string) {
	c.PrependReactor("get", "deployments",
		func(a k8stesting.Action) (bool, k8s_runtime_Object, error) {
			ga := a.(k8stesting.GetAction)
			if ga.GetName() != name || ga.GetNamespace() != ns {
				return false, nil, nil
			}
			d := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
			}
			return true, d, nil
		})
}

// Aliases to avoid importing k8s_runtime everywhere.
type k8s_runtime_Object = interface {
	GetObjectKind() schema_ObjectKind
	DeepCopyObject() any
}
type schema_ObjectKind interface{}

func TestEnsureRunning_CreatesDeploymentAndService(t *testing.T) {
	c := fake.NewClientset()
	sup := New(c, testConfig(), nil)
	k := Key{WorkspaceID: "alpha", Namespace: "ns-alpha"}

	// Seed: simulate the Deployment becoming Ready immediately
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

	// Verify both Deployment + Service were created
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
```

Note: the `k8s_runtime_Object`/`schema_ObjectKind` aliases in the test are fragile. Replace with the proper imports:

```go
import (
	"k8s.io/apimachinery/pkg/runtime"
)
// then use runtime.Object in place of k8s_runtime_Object
```

I left the alias as a defensive fallback; the implementer should switch to the real import once they confirm `k8s.io/apimachinery/pkg/runtime` is available (it is — it's already used in `internal/sandbox/manager.go`).

- [ ] **Step 2: Run, confirm failure**

```bash
cd /root/agentserver
go test ./internal/notebooksupervisor -v
```
Expected: FAIL — `Supervisor`, `New` undefined.

- [ ] **Step 3: Implement supervisor.go**

Create `internal/notebooksupervisor/supervisor.go`:

```go
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
	// Validate first; refuses k8s I/O for bad ids.
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
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("deployment %s/%s did not become ready within %v", k.Namespace, name, s.cfg.ReadyTimeout)
}

// Stop deletes the Deployment + Service. 404 is treated as success.
// Removes the entry from the in-memory map.
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

// Touch updates lastActive for k. No-op if the workspace isn't tracked
// (e.g. EnsureRunning was never called or Stop already cleared it).
func (s *Supervisor) Touch(k Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.children[k]; ok {
		e.lastActive = time.Now()
	}
}

// LastActive returns the last activity timestamp for k, or zero time if
// the workspace isn't tracked. Useful for the reaper + tests.
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
```

- [ ] **Step 4: Run, confirm pass**

```bash
cd /root/agentserver
go vet ./internal/notebooksupervisor
go test ./internal/notebooksupervisor -v
```
Expected: all tests pass (4 types + 5 spawn + 7 supervisor = 16).

If the test's `markReady` reactor approach doesn't work cleanly with the fake clientset, alternative is to seed the Deployment directly with `c.Tracker().Add(...)` after the test calls `EnsureRunning` and before `waitReady` polls. Replace `markReady` with:

```go
func markReady(c *fake.Clientset, name, ns string) {
	go func() {
		// Wait briefly for the Create call, then patch Status.ReadyReplicas
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
```

Pick whichever works with the fake clientset version installed (run a single test to verify).

- [ ] **Step 5: Commit**

```bash
cd /root/agentserver
git add internal/notebooksupervisor/supervisor.go \
        internal/notebooksupervisor/supervisor_test.go
git commit -m "feat(notebooksupervisor): EnsureRunning + Touch + Stop

per-workspace lifecycle map with k8s.io/client-go. EnsureRunning is
idempotent on AlreadyExists; Stop is best-effort (404 = success).
Tests use fake.NewClientset."
```

---

## Task 4: `reaper.go` — idle reap loop

**Files:**
- Create: `internal/notebooksupervisor/reaper.go`
- Create: `internal/notebooksupervisor/reaper_test.go`

- [ ] **Step 1: Write failing test**

Create `internal/notebooksupervisor/reaper_test.go`:

```go
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

	// Touch every 30ms for 300ms; entry must survive
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
```

- [ ] **Step 2: Run, confirm failure**

```bash
cd /root/agentserver
go test ./internal/notebooksupervisor -run TestReaper -v
```
Expected: FAIL — `StartReaper` undefined.

- [ ] **Step 3: Implement reaper.go**

Create `internal/notebooksupervisor/reaper.go`:

```go
package notebooksupervisor

import (
	"context"
	"time"
)

// StartReaper runs the idle reap loop. Returns when ctx is cancelled.
// Each tick: snapshot idle Keys (lastActive + IdleTTL < now), call
// Stop on each. Errors are logged, never propagated.
func (s *Supervisor) StartReaper(ctx context.Context) {
	t := time.NewTicker(s.cfg.ReapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reapOnce(ctx)
		}
	}
}

func (s *Supervisor) reapOnce(ctx context.Context) {
	cutoff := time.Now().Add(-s.cfg.IdleTTL)
	s.mu.Lock()
	idle := s.idleKeys(cutoff)
	s.mu.Unlock()
	for _, k := range idle {
		if err := s.Stop(ctx, k); err != nil {
			s.logger.Warn("notebooksupervisor: reap stop failed", "key", k, "err", err)
		} else {
			s.logger.Info("notebooksupervisor: reaped idle deployment", "key", k)
		}
	}
}
```

- [ ] **Step 4: Run, confirm pass**

```bash
cd /root/agentserver
go vet ./internal/notebooksupervisor
go test ./internal/notebooksupervisor -v
```
Expected: 19 passed (16 + 3 reaper).

- [ ] **Step 5: Commit**

```bash
cd /root/agentserver
git add internal/notebooksupervisor/reaper.go \
        internal/notebooksupervisor/reaper_test.go
git commit -m "feat(notebooksupervisor): idle reap loop

StartReaper ticks every Config.ReapInterval; Stops any Key whose
lastActive is older than Config.IdleTTL. Exits cleanly on ctx cancel."
```

---

## Task 5: Helm RBAC + values

**Files:**
- Modify: `deploy/helm/agentserver/values.yaml`
- Modify: `deploy/helm/agentserver/templates/rbac.yaml`

- [ ] **Step 1: Add values block**

Append to `deploy/helm/agentserver/values.yaml`:

```yaml
notebook:
  # Image for the per-workspace Jupyter Server. Built by
  # Dockerfile.notebook (Plan 1).
  image:
    repository: ghcr.io/agentserver/agentserver-notebook
    tag: dev
    pullPolicy: IfNotPresent
  # Per-workspace resource limits. Apply to the notebook container.
  resources:
    cpuRequest: "500m"
    cpuLimit: "2"
    memoryRequest: "1Gi"
    memoryLimit: "4Gi"
    ephemeralStorage: "5Gi"
  # Lifecycle.
  idleTTLSeconds: 14400      # 4h
  reapIntervalSeconds: 300   # 5min
  readyTimeoutSeconds: 60
  # PVC name pattern. Must match the existing workspace-volume provisioner.
  # The literal "{workspace_id}" is substituted at runtime by the supervisor.
  workspacePVCName: "ws-{workspace_id}"
```

- [ ] **Step 2: Extend ClusterRole with Deployment + Service perms**

Edit `deploy/helm/agentserver/templates/rbac.yaml`. Find the `{{ .Release.Name }}-sandbox` ClusterRole (or whichever role the agentserver SA binds to) and add rules. If you'd rather not pollute the sandbox role, add a SEPARATE ClusterRole `{{ .Release.Name }}-notebooksupervisor` + ClusterRoleBinding to the same ServiceAccount. Recommended: separate role for clean scoping. Append:

```yaml
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ .Release.Name }}-notebooksupervisor
  labels:
    app: {{ .Release.Name }}
rules:
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["get", "list", "create", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: ["services"]
    verbs: ["get", "list", "create", "update", "patch", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{ .Release.Name }}-notebooksupervisor
  labels:
    app: {{ .Release.Name }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: {{ .Release.Name }}-notebooksupervisor
subjects:
  - kind: ServiceAccount
    name: {{ .Release.Name }}
    namespace: {{ .Release.Namespace }}
```

- [ ] **Step 3: Lint helm**

```bash
cd /root/agentserver
helm lint deploy/helm/agentserver
helm template deploy/helm/agentserver | grep -A 5 "notebooksupervisor" | head -30
```
Expected: lint clean; rendered ClusterRole + Binding visible.

- [ ] **Step 4: Commit**

```bash
cd /root/agentserver
git add deploy/helm/agentserver/values.yaml \
        deploy/helm/agentserver/templates/rbac.yaml
git commit -m "feat(helm): notebook supervisor RBAC + values

new notebook: values block (image/resources/lifecycle/pvc); separate
ClusterRole giving agentserver SA Deployment + Service perms for
per-workspace notebook pods."
```

---

## Task 6: Wire into agentserver `cmd/serve.go`

**Files:**
- Modify: `cmd/serve.go`
- Modify: `internal/server/server.go` — add field for the supervisor

The supervisor is constructed at boot and stored on `*server.Server` so Plan 3b's handler can access it. The reaper goroutine starts at boot. No HTTP handler yet — that's Plan 3b.

- [ ] **Step 1: Add field on *Server**

In `internal/server/server.go`, add to the struct:

```go
NotebookSupervisor *notebooksupervisor.Supervisor // nil if notebook feature disabled
```

- [ ] **Step 2: Construct in cmd/serve.go**

Find where the existing `*server.Server` is built (likely near the existing retention-loop launch from Plan 2). Add:

```go
// Notebook supervisor (Plan 3a).
// Gated on having a k8s client available (sandbox manager already
// requires one for production). Pull image + resource config from env.
if k8sClient != nil {  // reuse the same k8s client the sandbox manager uses
    nbCfg := notebooksupervisor.Config{
        Image:            envOr("NOTEBOOK_IMAGE", "ghcr.io/agentserver/agentserver-notebook:dev"),
        ImagePullPolicy:  envOr("NOTEBOOK_IMAGE_PULL_POLICY", "IfNotPresent"),
        CPURequest:       envOr("NOTEBOOK_CPU_REQUEST", "500m"),
        CPULimit:         envOr("NOTEBOOK_CPU_LIMIT", "2"),
        MemoryRequest:    envOr("NOTEBOOK_MEM_REQUEST", "1Gi"),
        MemoryLimit:      envOr("NOTEBOOK_MEM_LIMIT", "4Gi"),
        EphemeralStorage: envOr("NOTEBOOK_EPHEMERAL_STORAGE", "5Gi"),
        WorkspacePVCName: envOr("NOTEBOOK_WORKSPACE_PVC", "ws-{workspace_id}"),
    }
    if v := os.Getenv("NOTEBOOK_IDLE_TTL_SECONDS"); v != "" {
        if n, err := strconv.Atoi(v); err == nil && n > 0 {
            nbCfg.IdleTTL = time.Duration(n) * time.Second
        }
    }
    if v := os.Getenv("NOTEBOOK_REAP_INTERVAL_SECONDS"); v != "" {
        if n, err := strconv.Atoi(v); err == nil && n > 0 {
            nbCfg.ReapInterval = time.Duration(n) * time.Second
        }
    }
    if v := os.Getenv("NOTEBOOK_READY_TIMEOUT_SECONDS"); v != "" {
        if n, err := strconv.Atoi(v); err == nil && n > 0 {
            nbCfg.ReadyTimeout = time.Duration(n) * time.Second
        }
    }
    srv.NotebookSupervisor = notebooksupervisor.New(k8sClient, nbCfg, nil)
    go srv.NotebookSupervisor.StartReaper(healthCtx)
}
```

Adjust `k8sClient` to whatever variable name `cmd/serve.go` actually uses for the kubernetes.Interface (look at the existing sandbox manager construction nearby).

If no `envOr` helper exists, inline `os.Getenv("X") || default` pattern that's already used elsewhere in serve.go.

- [ ] **Step 3: Build + vet**

```bash
cd /root/agentserver
go vet ./...
go build ./...
```
Expected: clean.

- [ ] **Step 4: Commit**

```bash
cd /root/agentserver
git add cmd/serve.go internal/server/server.go
git commit -m "feat(server): wire notebook supervisor into boot

constructs Supervisor when k8s client is available; starts reaper on
healthCtx (cancels on shutdown). Exposed on *Server for Plan 3b's
handler to consume. No HTTP route added in this plan."
```

---

## Self-review checklist (for the implementer)

After all tasks done:

- [ ] `go test ./internal/notebooksupervisor -v` — all 19 tests pass
- [ ] `go vet ./...` clean
- [ ] `go build ./...` clean
- [ ] `helm lint deploy/helm/agentserver` clean
- [ ] `helm template deploy/helm/agentserver` renders the new ClusterRole + Binding
- [ ] Supervisor wired in cmd/serve.go; reaper launched on healthCtx
- [ ] No HTTP handler added (that's Plan 3b's job)
- [ ] No jupyter image changes (that's Plan 3b's job)
- [ ] No Web UI changes (that's Plan 3c's job)

## After this plan

When Plan 3a is merged:
- **Plan 3b** can begin: jupyter proxy handler `/api/notebooks/{ws}/*` that calls `srv.NotebookSupervisor.EnsureRunning()` + reverse proxies HTTP/WS to the returned ServiceURL, with JWT minting + custom IdentityProvider/KernelProvisioner Python in Dockerfile.notebook to consume the JWT
- **Plan 3c**: React `<NotebooksPanel />` iframe + `<OperationsPanel />` against Plan 2's `operations/list` data
