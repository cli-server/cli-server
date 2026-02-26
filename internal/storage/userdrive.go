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

// UserDriveManager handles user persistent volume creation/deletion.
type UserDriveManager struct {
	db               *db.DB
	clientset        kubernetes.Interface
	namespace        string
	storageSize      string
	storageClassName string
}

// NewUserDriveManager creates a K8s-backed user drive manager.
func NewUserDriveManager(database *db.DB, clientset kubernetes.Interface, namespace, storageSize, storageClassName string) *UserDriveManager {
	return &UserDriveManager{
		db:               database,
		clientset:        clientset,
		namespace:        namespace,
		storageSize:      storageSize,
		storageClassName: storageClassName,
	}
}

// EnsurePVC creates a PVC for the user if it doesn't exist and records it in the DB.
func (m *UserDriveManager) EnsurePVC(ctx context.Context, userID string) (string, error) {
	// Check DB first.
	pvcName, err := m.db.GetUserDrive(userID)
	if err != nil {
		return "", err
	}
	if pvcName != "" {
		return pvcName, nil
	}

	pvcName = "cli-user-" + shortID(userID) + "-drive"

	// Check if PVC already exists in K8s.
	_, err = m.clientset.CoreV1().PersistentVolumeClaims(m.namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err == nil {
		// PVC exists, just record in DB.
		if err := m.db.SetUserDrive(userID, pvcName); err != nil {
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
				"managed-by": "cli-server",
				"user-id":    userID,
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
		return "", fmt.Errorf("create user drive PVC: %w", err)
	}
	log.Printf("Created user drive PVC %s for user %s", pvcName, userID)

	if err := m.db.SetUserDrive(userID, pvcName); err != nil {
		return "", err
	}
	return pvcName, nil
}

// DeletePVC removes the user's PVC.
func (m *UserDriveManager) DeletePVC(ctx context.Context, userID string) error {
	pvcName, err := m.db.GetUserDrive(userID)
	if err != nil || pvcName == "" {
		return err
	}

	if err := m.clientset.CoreV1().PersistentVolumeClaims(m.namespace).Delete(ctx, pvcName, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete user drive PVC: %w", err)
	}
	return nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// DockerUserDriveManager handles user Docker volume creation.
type DockerUserDriveManager struct {
	db *db.DB
}

// NewDockerUserDriveManager creates a Docker-backed user drive manager.
func NewDockerUserDriveManager(database *db.DB) *DockerUserDriveManager {
	return &DockerUserDriveManager{db: database}
}

// EnsureVolume ensures a Docker named volume exists for the user.
func (m *DockerUserDriveManager) EnsureVolume(userID string) (string, error) {
	// Check DB first.
	volumeName, err := m.db.GetUserDrive(userID)
	if err != nil {
		return "", err
	}
	if volumeName != "" {
		return volumeName, nil
	}

	volumeName = "cli-user-" + shortID(userID) + "-drive"
	if err := m.db.SetUserDrive(userID, volumeName); err != nil {
		return "", err
	}
	// Docker volumes are created automatically when first referenced in a mount.
	return volumeName, nil
}

// DriveManager is a backend-agnostic interface for user drive management.
type DriveManager interface {
	EnsureDrive(ctx context.Context, userID string) (string, error)
}

// K8sDriveAdapter adapts UserDriveManager to the DriveManager interface.
type K8sDriveAdapter struct {
	mgr *UserDriveManager
}

func NewK8sDriveAdapter(mgr *UserDriveManager) DriveManager {
	return &K8sDriveAdapter{mgr: mgr}
}

func (a *K8sDriveAdapter) EnsureDrive(ctx context.Context, userID string) (string, error) {
	return a.mgr.EnsurePVC(ctx, userID)
}

// DockerDriveAdapter adapts DockerUserDriveManager to the DriveManager interface.
type DockerDriveAdapter struct {
	mgr *DockerUserDriveManager
}

func NewDockerDriveAdapter(mgr *DockerUserDriveManager) DriveManager {
	return &DockerDriveAdapter{mgr: mgr}
}

func (a *DockerDriveAdapter) EnsureDrive(ctx context.Context, userID string) (string, error) {
	_ = ctx
	return a.mgr.EnsureVolume(userID)
}

// NilDriveManager is a no-op drive manager for when storage is not configured.
type NilDriveManager struct{}

func (NilDriveManager) EnsureDrive(ctx context.Context, userID string) (string, error) {
	_ = ctx
	_ = userID
	return "", nil
}

// Ensure compile-time interface compliance.
var (
	_ DriveManager = (*K8sDriveAdapter)(nil)
	_ DriveManager = (*DockerDriveAdapter)(nil)
	_ DriveManager = NilDriveManager{}
)

// EnsureDriveWithTimeout wraps EnsureDrive with a default timeout.
func EnsureDriveWithTimeout(dm DriveManager, userID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return dm.EnsureDrive(ctx, userID)
}
