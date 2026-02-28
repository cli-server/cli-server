package storage

import (
	"context"
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/imryao/cli-server/internal/db"
)

// WorkspaceDriveManager handles workspace persistent volume creation.
type WorkspaceDriveManager struct {
	db               *db.DB
	clientset        kubernetes.Interface
	namespace        string
	storageSize      string
	storageClassName string
}

// NewWorkspaceDriveManager creates a K8s-backed workspace drive manager.
func NewWorkspaceDriveManager(database *db.DB, clientset kubernetes.Interface, namespace, storageSize, storageClassName string) *WorkspaceDriveManager {
	return &WorkspaceDriveManager{
		db:               database,
		clientset:        clientset,
		namespace:        namespace,
		storageSize:      storageSize,
		storageClassName: storageClassName,
	}
}

// EnsurePVC creates a PVC for the workspace if it doesn't exist and records it in the DB.
func (m *WorkspaceDriveManager) EnsurePVC(ctx context.Context, workspaceID string) (string, error) {
	// Check DB first.
	ws, err := m.db.GetWorkspace(workspaceID)
	if err != nil {
		return "", err
	}
	if ws == nil {
		return "", fmt.Errorf("workspace %s not found", workspaceID)
	}
	if ws.DiskPVCName.Valid && ws.DiskPVCName.String != "" {
		return ws.DiskPVCName.String, nil
	}

	pvcName := "cli-ws-" + shortID(workspaceID) + "-disk"

	// Check if PVC already exists in K8s.
	_, err = m.clientset.CoreV1().PersistentVolumeClaims(m.namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err == nil {
		// PVC exists, just record in DB.
		if err := m.db.UpdateWorkspaceDiskPVC(workspaceID, pvcName); err != nil {
			return "", err
		}
		return pvcName, nil
	}
	if !errors.IsNotFound(err) {
		return "", fmt.Errorf("check existing PVC: %w", err)
	}

	// Create the PVC.
	storageSize := resource.MustParse(m.storageSize)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: m.namespace,
			Labels: map[string]string{
				"managed-by":   "cli-server",
				"workspace-id": workspaceID,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: storageSize},
			},
		},
	}
	if m.storageClassName != "" {
		pvc.Spec.StorageClassName = &m.storageClassName
	}

	if _, err := m.clientset.CoreV1().PersistentVolumeClaims(m.namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("create workspace drive PVC: %w", err)
	}
	log.Printf("Created workspace drive PVC %s for workspace %s", pvcName, workspaceID)

	if err := m.db.UpdateWorkspaceDiskPVC(workspaceID, pvcName); err != nil {
		return "", err
	}
	return pvcName, nil
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
func (m *DockerWorkspaceDriveManager) EnsureVolume(workspaceID string) (string, error) {
	// Check DB first.
	ws, err := m.db.GetWorkspace(workspaceID)
	if err != nil {
		return "", err
	}
	if ws == nil {
		return "", fmt.Errorf("workspace %s not found", workspaceID)
	}
	if ws.DiskPVCName.Valid && ws.DiskPVCName.String != "" {
		return ws.DiskPVCName.String, nil
	}

	volumeName := "cli-ws-" + shortID(workspaceID) + "-disk"
	if err := m.db.UpdateWorkspaceDiskPVC(workspaceID, volumeName); err != nil {
		return "", err
	}
	// Docker volumes are created automatically when first referenced in a mount.
	return volumeName, nil
}

// DriveManager is a backend-agnostic interface for workspace drive management.
type DriveManager interface {
	EnsureDrive(ctx context.Context, workspaceID string) (string, error)
}

// K8sDriveAdapter adapts WorkspaceDriveManager to the DriveManager interface.
type K8sDriveAdapter struct {
	mgr *WorkspaceDriveManager
}

func NewK8sDriveAdapter(mgr *WorkspaceDriveManager) DriveManager {
	return &K8sDriveAdapter{mgr: mgr}
}

func (a *K8sDriveAdapter) EnsureDrive(ctx context.Context, workspaceID string) (string, error) {
	return a.mgr.EnsurePVC(ctx, workspaceID)
}

// DockerDriveAdapter adapts DockerWorkspaceDriveManager to the DriveManager interface.
type DockerDriveAdapter struct {
	mgr *DockerWorkspaceDriveManager
}

func NewDockerDriveAdapter(mgr *DockerWorkspaceDriveManager) DriveManager {
	return &DockerDriveAdapter{mgr: mgr}
}

func (a *DockerDriveAdapter) EnsureDrive(ctx context.Context, workspaceID string) (string, error) {
	_ = ctx
	return a.mgr.EnsureVolume(workspaceID)
}

// NilDriveManager is a no-op drive manager for when storage is not configured.
type NilDriveManager struct{}

func (NilDriveManager) EnsureDrive(ctx context.Context, workspaceID string) (string, error) {
	_ = ctx
	_ = workspaceID
	return "", nil
}

// Ensure compile-time interface compliance.
var (
	_ DriveManager = (*K8sDriveAdapter)(nil)
	_ DriveManager = (*DockerDriveAdapter)(nil)
	_ DriveManager = NilDriveManager{}
)

// EnsureDriveWithTimeout wraps EnsureDrive with a default timeout.
func EnsureDriveWithTimeout(dm DriveManager, workspaceID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return dm.EnsureDrive(ctx, workspaceID)
}
