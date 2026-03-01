package server

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	settingKeyMaxWorkspaces       = "quota_max_workspaces_per_user"
	settingKeyMaxSandboxes        = "quota_max_sandboxes_per_workspace"
	settingKeyWorkspaceDriveSize  = "default_workspace_drive_size"
	settingKeySandboxCPU          = "default_sandbox_cpu"
	settingKeySandboxMemory       = "default_sandbox_memory"
	settingKeyIdleTimeout         = "default_idle_timeout"
	settingKeyWsMaxTotalCPU       = "default_ws_max_total_cpu"
	settingKeyWsMaxTotalMemory    = "default_ws_max_total_memory"
	settingKeyWsMaxIdleTimeout    = "default_ws_max_idle_timeout"

	defaultMaxWorkspaces = 10
	defaultMaxSandboxes  = 20
)

// ResourceDefaults holds all resolved system-wide defaults.
type ResourceDefaults struct {
	MaxWorkspacesPerUser     int
	MaxSandboxesPerWorkspace int
	WorkspaceDriveSize       string
	SandboxCPU               string
	SandboxMemory            string
	IdleTimeout              string
	WsMaxTotalCPU            string
	WsMaxTotalMemory         string
	WsMaxIdleTimeout         string
}

// getResourceDefaults resolves all defaults via the 3-layer priority chain:
// 1. DB system_settings (highest)
// 2. Environment variables
// 3. Hardcoded fallback (lowest)
func (s *Server) getResourceDefaults() ResourceDefaults {
	rd := ResourceDefaults{
		MaxWorkspacesPerUser:     defaultMaxWorkspaces,
		MaxSandboxesPerWorkspace: defaultMaxSandboxes,
		WorkspaceDriveSize:       "10Gi",
		SandboxCPU:               "2",
		SandboxMemory:            "2Gi",
		IdleTimeout:              "30m",
		WsMaxTotalCPU:            "0",
		WsMaxTotalMemory:         "0",
		WsMaxIdleTimeout:         "0",
	}

	// Layer 2: environment variables override hardcoded defaults.
	if v := os.Getenv("QUOTA_MAX_WORKSPACES_PER_USER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rd.MaxWorkspacesPerUser = n
		}
	}
	if v := os.Getenv("QUOTA_MAX_SANDBOXES_PER_WORKSPACE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rd.MaxSandboxesPerWorkspace = n
		}
	}
	if v := os.Getenv("USER_DRIVE_SIZE"); v != "" {
		rd.WorkspaceDriveSize = v
	}
	if v := os.Getenv("QUOTA_DEFAULT_SANDBOX_CPU"); v != "" {
		rd.SandboxCPU = v
	}
	if v := os.Getenv("QUOTA_DEFAULT_SANDBOX_MEMORY"); v != "" {
		rd.SandboxMemory = v
	}
	if v := os.Getenv("IDLE_TIMEOUT"); v != "" {
		rd.IdleTimeout = v
	}
	if v := os.Getenv("QUOTA_WS_MAX_TOTAL_CPU"); v != "" {
		rd.WsMaxTotalCPU = v
	}
	if v := os.Getenv("QUOTA_WS_MAX_TOTAL_MEMORY"); v != "" {
		rd.WsMaxTotalMemory = v
	}
	if v := os.Getenv("QUOTA_WS_MAX_IDLE_TIMEOUT"); v != "" {
		rd.WsMaxIdleTimeout = v
	}

	// Layer 1: DB system_settings take highest priority.
	if v, err := s.DB.GetSystemSetting(settingKeyMaxWorkspaces); err == nil && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rd.MaxWorkspacesPerUser = n
		}
	}
	if v, err := s.DB.GetSystemSetting(settingKeyMaxSandboxes); err == nil && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rd.MaxSandboxesPerWorkspace = n
		}
	}
	if v, err := s.DB.GetSystemSetting(settingKeyWorkspaceDriveSize); err == nil && v != "" {
		rd.WorkspaceDriveSize = v
	}
	if v, err := s.DB.GetSystemSetting(settingKeySandboxCPU); err == nil && v != "" {
		rd.SandboxCPU = v
	}
	if v, err := s.DB.GetSystemSetting(settingKeySandboxMemory); err == nil && v != "" {
		rd.SandboxMemory = v
	}
	if v, err := s.DB.GetSystemSetting(settingKeyIdleTimeout); err == nil && v != "" {
		rd.IdleTimeout = v
	}
	if v, err := s.DB.GetSystemSetting(settingKeyWsMaxTotalCPU); err == nil && v != "" {
		rd.WsMaxTotalCPU = v
	}
	if v, err := s.DB.GetSystemSetting(settingKeyWsMaxTotalMemory); err == nil && v != "" {
		rd.WsMaxTotalMemory = v
	}
	if v, err := s.DB.GetSystemSetting(settingKeyWsMaxIdleTimeout); err == nil && v != "" {
		rd.WsMaxIdleTimeout = v
	}

	return rd
}

// getDefaultQuotas resolves system-wide defaults with priority:
// 1. DB system_settings table (admin UI overrides)
// 2. Environment variables (Helm)
// 3. Hardcoded fallback (10/20)
func (s *Server) getDefaultQuotas() (maxWs, maxSbx int) {
	rd := s.getResourceDefaults()
	return rd.MaxWorkspacesPerUser, rd.MaxSandboxesPerWorkspace
}

// effectiveResourceDefaults returns per-user overrides applied on top of system defaults.
func (s *Server) effectiveResourceDefaults(userID string) (ResourceDefaults, error) {
	rd := s.getResourceDefaults()

	uq, err := s.DB.GetUserQuota(userID)
	if err != nil {
		return rd, err
	}
	if uq == nil {
		return rd, nil
	}

	if uq.MaxWorkspaces != nil {
		rd.MaxWorkspacesPerUser = *uq.MaxWorkspaces
	}
	if uq.MaxSandboxesPerWorkspace != nil {
		rd.MaxSandboxesPerWorkspace = *uq.MaxSandboxesPerWorkspace
	}
	if uq.WorkspaceDriveSize != nil {
		rd.WorkspaceDriveSize = *uq.WorkspaceDriveSize
	}
	if uq.SandboxCPU != nil {
		rd.SandboxCPU = *uq.SandboxCPU
	}
	if uq.SandboxMemory != nil {
		rd.SandboxMemory = *uq.SandboxMemory
	}
	if uq.IdleTimeout != nil {
		rd.IdleTimeout = *uq.IdleTimeout
	}
	if uq.WsMaxTotalCPU != nil {
		rd.WsMaxTotalCPU = *uq.WsMaxTotalCPU
	}
	if uq.WsMaxTotalMemory != nil {
		rd.WsMaxTotalMemory = *uq.WsMaxTotalMemory
	}
	if uq.WsMaxIdleTimeout != nil {
		rd.WsMaxIdleTimeout = *uq.WsMaxIdleTimeout
	}

	return rd, nil
}

// effectiveQuota returns the effective quota for a user.
// Per-user overrides take precedence over system defaults.
func (s *Server) effectiveQuota(userID string) (maxWs, maxSbx int, err error) {
	rd, err := s.effectiveResourceDefaults(userID)
	if err != nil {
		return 0, 0, err
	}
	return rd.MaxWorkspacesPerUser, rd.MaxSandboxesPerWorkspace, nil
}

// checkWorkspaceQuota checks if a user can create another workspace.
// Returns whether creation is allowed, the current count, and the max.
// max=0 means unlimited.
func (s *Server) checkWorkspaceQuota(userID string) (allowed bool, current, max int, err error) {
	maxWs, _, err := s.effectiveQuota(userID)
	if err != nil {
		return false, 0, 0, err
	}

	current, err = s.DB.CountWorkspacesOwnedByUser(userID)
	if err != nil {
		return false, 0, 0, err
	}

	if maxWs == 0 {
		return true, current, 0, nil
	}

	return current < maxWs, current, maxWs, nil
}

// checkSandboxQuota checks if a workspace can have another sandbox.
// Returns whether creation is allowed, the current count, and the max.
// max=0 means unlimited.
func (s *Server) checkSandboxQuota(userID, workspaceID string) (allowed bool, current, max int, err error) {
	_, maxSbx, err := s.effectiveQuota(userID)
	if err != nil {
		return false, 0, 0, err
	}

	current, err = s.DB.CountSandboxesByWorkspace(workspaceID)
	if err != nil {
		return false, 0, 0, err
	}

	if maxSbx == 0 {
		return true, current, 0, nil
	}

	return current < maxSbx, current, maxSbx, nil
}

// checkWorkspaceResourceBudget checks if adding a sandbox with the given resources
// would exceed the workspace's total CPU/memory budget.
// Returns allowed=true if within budget or budget is unlimited (0).
func (s *Server) checkWorkspaceResourceBudget(userID, workspaceID string, cpuMillis int, memBytes int64) (bool, error) {
	rd, err := s.effectiveResourceDefaults(userID)
	if err != nil {
		return false, err
	}

	maxCPU := parseCPUMillicores(rd.WsMaxTotalCPU)
	maxMem := parseMemoryBytes(rd.WsMaxTotalMemory)

	// 0 means unlimited
	if maxCPU == 0 && maxMem == 0 {
		return true, nil
	}

	currentCPU, currentMem, err := s.DB.SumWorkspaceSandboxResources(workspaceID)
	if err != nil {
		return false, err
	}

	if maxCPU > 0 && currentCPU+int64(cpuMillis) > int64(maxCPU) {
		return false, nil
	}
	if maxMem > 0 && currentMem+memBytes > maxMem {
		return false, nil
	}

	return true, nil
}

// getEffectiveIdleTimeout resolves the idle timeout from the settings chain.
// Returns 0 if disabled.
func (s *Server) getEffectiveIdleTimeout() time.Duration {
	rd := s.getResourceDefaults()
	d, err := time.ParseDuration(rd.IdleTimeout)
	if err != nil {
		return 30 * time.Minute
	}
	return d
}

// GetEffectiveIdleTimeout is the exported version for use by cmd/serve.go.
func (s *Server) GetEffectiveIdleTimeout() time.Duration {
	return s.getEffectiveIdleTimeout()
}

// parseCPUMillicores converts a K8s CPU string to millicores.
// Examples: "2" -> 2000, "500m" -> 500, "1.5" -> 1500, "0" -> 0
func parseCPUMillicores(s string) int {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0
	}
	if strings.HasSuffix(s, "m") {
		v := strings.TrimSuffix(s, "m")
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0
		}
		return n
	}
	// Parse as float to handle "1.5"
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int(f * 1000)
}

// parseMemoryBytes converts a K8s memory string to bytes.
// Examples: "2Gi" -> 2*1024^3, "512Mi" -> 512*1024^2, "1073741824" -> 1073741824, "0" -> 0
func parseMemoryBytes(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0
	}

	multiplier := int64(1)
	numStr := s

	switch {
	case strings.HasSuffix(s, "Gi"):
		multiplier = 1024 * 1024 * 1024
		numStr = strings.TrimSuffix(s, "Gi")
	case strings.HasSuffix(s, "Mi"):
		multiplier = 1024 * 1024
		numStr = strings.TrimSuffix(s, "Mi")
	case strings.HasSuffix(s, "Ki"):
		multiplier = 1024
		numStr = strings.TrimSuffix(s, "Ki")
	case strings.HasSuffix(s, "G"):
		multiplier = 1000 * 1000 * 1000
		numStr = strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "M"):
		multiplier = 1000 * 1000
		numStr = strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "K"):
		multiplier = 1000
		numStr = strings.TrimSuffix(s, "K")
	}

	f, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0
	}
	return int64(f * float64(multiplier))
}
