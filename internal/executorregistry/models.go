package executorregistry

import (
	"encoding/json"
	"time"
)

type Executor struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type ExecutorCapability struct {
	ExecutorID  string            `json:"executor_id"`
	Tools       []string          `json:"tools"`
	Environment map[string]string `json:"environment"`
	Resources   ResourceInfo      `json:"resources"`
	Description string            `json:"description"`
	WorkingDir  string            `json:"working_dir"`
	ProbedAt    *time.Time        `json:"probed_at,omitempty"`
}

type ResourceInfo struct {
	CPUCores int    `json:"cpu_cores,omitempty"`
	MemoryGB int    `json:"memory_gb,omitempty"`
	DiskGB   int    `json:"disk_gb,omitempty"`
	GPU      string `json:"gpu,omitempty"`
}

type ExecutorInfo struct {
	Executor
	Capabilities ExecutorCapability `json:"capabilities"`
	LastSeen     *time.Time         `json:"last_seen,omitempty"`
}

type ExecuteRequest struct {
	ExecutorID string          `json:"executor_id"`
	Tool       string          `json:"tool"`
	Arguments  json.RawMessage `json:"arguments"`
	TimeoutMs  int             `json:"timeout_ms,omitempty"`
}

type ExecuteResponse struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}
