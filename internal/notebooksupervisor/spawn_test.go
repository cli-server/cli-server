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

func TestBuildDeployment_PVCNameTemplateSubstitution(t *testing.T) {
	c := Config{
		Image:            "img:tag",
		WorkspacePVCName: "ws-{workspace_id}-data",
	}.WithDefaults()
	k := Key{WorkspaceID: "alpha", Namespace: "ns-alpha"}

	d, err := BuildDeployment(k, c)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var pvcName string
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			pvcName = v.PersistentVolumeClaim.ClaimName
		}
	}
	if pvcName != "ws-alpha-data" {
		t.Errorf("pvc name = %q, want ws-alpha-data", pvcName)
	}
}

func TestBuildDeployment_PVCNameNoTemplate(t *testing.T) {
	c := Config{
		Image:            "img:tag",
		WorkspacePVCName: "literal-pvc-name",
	}.WithDefaults()
	k := Key{WorkspaceID: "alpha", Namespace: "ns"}
	d, err := BuildDeployment(k, c)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var pvcName string
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			pvcName = v.PersistentVolumeClaim.ClaimName
		}
	}
	if pvcName != "literal-pvc-name" {
		t.Errorf("pvc name = %q (no template should pass through literally)", pvcName)
	}
}

func TestBuildDeployment_ExtraEnvVarsWithSubstitution(t *testing.T) {
	c := Config{
		Image:            "img:tag",
		WorkspacePVCName: "pvc",
		ExtraEnvVars: map[string]string{
			"NOTEBOOK_BASE_URL": "/api/notebooks/{workspace_id}/",
			"STATIC_VAR":        "literal",
		},
	}.WithDefaults()
	k := Key{WorkspaceID: "alpha", Namespace: "ns"}

	d, err := BuildDeployment(k, c)
	if err != nil {
		t.Fatal(err)
	}
	env := d.Spec.Template.Spec.Containers[0].Env
	got := map[string]string{}
	for _, e := range env {
		got[e.Name] = e.Value
	}
	if got["NOTEBOOK_BASE_URL"] != "/api/notebooks/alpha/" {
		t.Errorf("base_url=%q", got["NOTEBOOK_BASE_URL"])
	}
	if got["STATIC_VAR"] != "literal" {
		t.Errorf("static=%q", got["STATIC_VAR"])
	}
}
