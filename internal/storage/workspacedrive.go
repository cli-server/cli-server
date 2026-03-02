package storage

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/agentserver/agentserver/internal/db"
	"github.com/agentserver/agentserver/internal/process"
)

// WorkspaceDriveManager handles workspace persistent volume creation.
type WorkspaceDriveManager struct {
	db               *db.DB
	clientset        kubernetes.Interface
	storageSize      int64 // bytes
	storageClassName string
}

// NewWorkspaceDriveManager creates a K8s-backed workspace drive manager.
func NewWorkspaceDriveManager(database *db.DB, clientset kubernetes.Interface, storageSize int64, storageClassName string) *WorkspaceDriveManager {
	return &WorkspaceDriveManager{
		db:               database,
		clientset:        clientset,
		storageSize:      storageSize,
		storageClassName: storageClassName,
	}
}

// EnsurePVC creates a PVC for the workspace in the given namespace if it doesn't exist and records it in the DB.
func (m *WorkspaceDriveManager) EnsurePVC(ctx context.Context, workspaceID, namespace string) ([]process.VolumeMount, error) {
	ws, err := m.db.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	if ws == nil {
		return nil, fmt.Errorf("workspace %s not found", workspaceID)
	}

	// Check for existing volumes.
	volumes, err := m.db.ListWorkspaceVolumes(workspaceID)
	if err != nil {
		return nil, err
	}
	if len(volumes) > 0 {
		// Ensure PVCs exist in the target namespace, then return mounts.
		var mounts []process.VolumeMount
		for _, v := range volumes {
			// Verify PVC exists.
			_, err := m.clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, v.PVCName, metav1.GetOptions{})
			if err != nil && !errors.IsNotFound(err) {
				return nil, fmt.Errorf("check existing PVC %s: %w", v.PVCName, err)
			}
			mounts = append(mounts, process.VolumeMount{PVCName: v.PVCName, MountPath: v.MountPath})
		}
		return mounts, nil
	}

	// No volumes yet — create PVC and record it.
	pvcName := "agent-ws-" + shortID(workspaceID) + "-disk"
	mountPath := "/home/agent/projects"

	// Check if PVC already exists in the target namespace.
	_, err = m.clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err == nil {
		// PVC exists, just record in DB.
		if err := m.db.AddWorkspaceVolume(uuid.New().String(), workspaceID, pvcName, mountPath); err != nil {
			return nil, err
		}
		return []process.VolumeMount{{PVCName: pvcName, MountPath: mountPath}}, nil
	}
	if !errors.IsNotFound(err) {
		return nil, fmt.Errorf("check existing PVC: %w", err)
	}

	// Create the PVC.
	storageQty := *resource.NewQuantity(m.storageSize, resource.BinarySI)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: namespace,
			Labels: map[string]string{
				"managed-by":   "agentserver",
				"workspace-id": workspaceID,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: storageQty},
			},
		},
	}
	if m.storageClassName != "" {
		pvc.Spec.StorageClassName = &m.storageClassName
	}

	if _, err := m.clientset.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("create workspace drive PVC: %w", err)
	}
	log.Printf("Created workspace drive PVC %s for workspace %s", pvcName, workspaceID)

	if err := m.db.AddWorkspaceVolume(uuid.New().String(), workspaceID, pvcName, mountPath); err != nil {
		return nil, err
	}
	return []process.VolumeMount{{PVCName: pvcName, MountPath: mountPath}}, nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// DockerWorkspaceDriveManager handles workspace Docker volume creation.
type DockerWorkspaceDriveManager struct {
	db *db.DB
}

// NewDockerWorkspaceDriveManager creates a Docker-backed workspace drive manager.
func NewDockerWorkspaceDriveManager(database *db.DB) *DockerWorkspaceDriveManager {
	return &DockerWorkspaceDriveManager{db: database}
}

// EnsureVolume ensures a Docker named volume exists for the workspace.
func (m *DockerWorkspaceDriveManager) EnsureVolume(workspaceID string) ([]process.VolumeMount, error) {
	ws, err := m.db.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	if ws == nil {
		return nil, fmt.Errorf("workspace %s not found", workspaceID)
	}

	// Check for existing volumes.
	volumes, err := m.db.ListWorkspaceVolumes(workspaceID)
	if err != nil {
		return nil, err
	}
	if len(volumes) > 0 {
		var mounts []process.VolumeMount
		for _, v := range volumes {
			mounts = append(mounts, process.VolumeMount{PVCName: v.PVCName, MountPath: v.MountPath})
		}
		return mounts, nil
	}

	volumeName := "cli-ws-" + shortID(workspaceID) + "-disk"
	mountPath := "/home/agent/projects"
	if err := m.db.AddWorkspaceVolume(uuid.New().String(), workspaceID, volumeName, mountPath); err != nil {
		return nil, err
	}
	// Docker volumes are created automatically when first referenced in a mount.
	return []process.VolumeMount{{PVCName: volumeName, MountPath: mountPath}}, nil
}

// DriveManager is a backend-agnostic interface for workspace drive management.
type DriveManager interface {
	EnsureDrive(ctx context.Context, workspaceID, namespace string) ([]process.VolumeMount, error)
}

// K8sDriveAdapter adapts WorkspaceDriveManager to the DriveManager interface.
type K8sDriveAdapter struct {
	mgr *WorkspaceDriveManager
}

func NewK8sDriveAdapter(mgr *WorkspaceDriveManager) DriveManager {
	return &K8sDriveAdapter{mgr: mgr}
}

func (a *K8sDriveAdapter) EnsureDrive(ctx context.Context, workspaceID, namespace string) ([]process.VolumeMount, error) {
	return a.mgr.EnsurePVC(ctx, workspaceID, namespace)
}

// DockerDriveAdapter adapts DockerWorkspaceDriveManager to the DriveManager interface.
type DockerDriveAdapter struct {
	mgr *DockerWorkspaceDriveManager
}

func NewDockerDriveAdapter(mgr *DockerWorkspaceDriveManager) DriveManager {
	return &DockerDriveAdapter{mgr: mgr}
}

func (a *DockerDriveAdapter) EnsureDrive(ctx context.Context, workspaceID, namespace string) ([]process.VolumeMount, error) {
	_ = ctx
	_ = namespace
	return a.mgr.EnsureVolume(workspaceID)
}

// NilDriveManager is a no-op drive manager for when storage is not configured.
type NilDriveManager struct{}

func (NilDriveManager) EnsureDrive(ctx context.Context, workspaceID, namespace string) ([]process.VolumeMount, error) {
	_ = ctx
	_ = workspaceID
	_ = namespace
	return nil, nil
}

// Ensure compile-time interface compliance.
var (
	_ DriveManager = (*K8sDriveAdapter)(nil)
	_ DriveManager = (*DockerDriveAdapter)(nil)
	_ DriveManager = NilDriveManager{}
)

// EnsureDriveWithTimeout wraps EnsureDrive with a default timeout.
func EnsureDriveWithTimeout(dm DriveManager, workspaceID, namespace string) ([]process.VolumeMount, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return dm.EnsureDrive(ctx, workspaceID, namespace)
}
