package notebooksupervisor

import (
	"fmt"
	"strings"

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
		"app":            name,
		managedByLabel:   managedByValue,
		workspaceIDLabel: k.WorkspaceID,
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
	envVars := []corev1.EnvVar{}
	for k2, v := range c.ExtraEnvVars {
		envVars = append(envVars, corev1.EnvVar{
			Name:  k2,
			Value: substituteWorkspaceID(v, k.WorkspaceID),
		})
	}
	container := corev1.Container{
		Name:            "notebook",
		Image:           c.Image,
		ImagePullPolicy: corev1.PullPolicy(c.ImagePullPolicy),
		Env:             envVars,
		Ports: []corev1.ContainerPort{
			{ContainerPort: notebookPort, Name: "http", Protocol: corev1.ProtocolTCP},
		},
		Resources: resources,
		VolumeMounts: []corev1.VolumeMount{
			{Name: workspaceVolumeName, MountPath: workspaceMountPath},
		},
	}
	pvcName := substituteWorkspaceID(c.WorkspacePVCName, k.WorkspaceID)
	pod := corev1.PodSpec{
		Containers: []corev1.Container{container},
		Volumes: []corev1.Volume{
			{
				Name: workspaceVolumeName,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: pvcName,
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

// substituteWorkspaceID replaces both {workspace_id} (full UUID) and
// {workspace_id_short} (first 8 chars, matching the agent-ws-<short>
// convention used by internal/storage/workspacedrive.go) in s.
func substituteWorkspaceID(s, workspaceID string) string {
	short := workspaceID
	if len(short) > 8 {
		short = short[:8]
	}
	s = strings.ReplaceAll(s, "{workspace_id_short}", short)
	return strings.ReplaceAll(s, "{workspace_id}", workspaceID)
}

// ServiceURL returns the cluster-internal HTTP url for the Service.
func ServiceURL(k Key) string {
	name, err := k.SafeDeploymentName()
	if err != nil {
		return ""
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", name, k.Namespace, notebookPort)
}
