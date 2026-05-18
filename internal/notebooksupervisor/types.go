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
