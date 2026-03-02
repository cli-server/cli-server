package server

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	settingKeyMaxWorkspaces        = "quota_max_workspaces_per_user"
	settingKeyMaxSandboxes         = "quota_max_sandboxes_per_workspace"
	settingKeyMaxWorkspaceDriveSize = "default_max_workspace_drive_size"
	settingKeyMaxSandboxCPU        = "default_max_sandbox_cpu"
	settingKeyMaxSandboxMemory     = "default_max_sandbox_memory"
	settingKeyMaxIdleTimeout       = "default_max_idle_timeout"
	settingKeyWsMaxTotalCPU        = "default_ws_max_total_cpu"
	settingKeyWsMaxTotalMemory     = "default_ws_max_total_memory"
	settingKeyWsMaxIdleTimeout     = "default_ws_max_idle_timeout"

	defaultMaxWorkspaces = 10
	defaultMaxSandboxes  = 20
)

// ResourceDefaults holds all resolved system-wide defaults.
type ResourceDefaults struct {
	MaxWorkspacesPerUser     int
	MaxSandboxesPerWorkspace int
	MaxWorkspaceDriveSize    int64 // bytes
	MaxSandboxCPU            int   // millicores
	MaxSandboxMemory         int64 // bytes
	MaxIdleTimeout           int   // seconds
	WsMaxTotalCPU            int   // millicores
	WsMaxTotalMemory         int64 // bytes
	WsMaxIdleTimeout         int   // seconds
}

// getResourceDefaults resolves all defaults via the 3-layer priority chain:
// 1. DB system_settings (highest)
// 2. Environment variables
// 3. Hardcoded fallback (lowest)
func (s *Server) getResourceDefaults() ResourceDefaults {
	rd := ResourceDefaults{
		MaxWorkspacesPerUser:     defaultMaxWorkspaces,
		MaxSandboxesPerWorkspace: defaultMaxSandboxes,
		MaxWorkspaceDriveSize:    10 * 1024 * 1024 * 1024, // 10Gi
		MaxSandboxCPU:            2000,                     // 2 cores
		MaxSandboxMemory:         2 * 1024 * 1024 * 1024,  // 2Gi
		MaxIdleTimeout:           1800,                     // 30m
		WsMaxTotalCPU:            0,
		WsMaxTotalMemory:         0,
		WsMaxIdleTimeout:         0,
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
		rd.MaxWorkspaceDriveSize = parseResourceInt64(v, rd.MaxWorkspaceDriveSize, parseMemoryBytes)
	}
	if v := os.Getenv("QUOTA_DEFAULT_SANDBOX_CPU"); v != "" {
		rd.MaxSandboxCPU = parseResourceInt(v, rd.MaxSandboxCPU, parseCPUMillicores)
	}
	if v := os.Getenv("QUOTA_DEFAULT_SANDBOX_MEMORY"); v != "" {
		rd.MaxSandboxMemory = parseResourceInt64(v, rd.MaxSandboxMemory, parseMemoryBytes)
	}
	if v := os.Getenv("IDLE_TIMEOUT"); v != "" {
		rd.MaxIdleTimeout = parseResourceInt(v, rd.MaxIdleTimeout, parseDurationSeconds)
	}
	if v := os.Getenv("QUOTA_WS_MAX_TOTAL_CPU"); v != "" {
		rd.WsMaxTotalCPU = parseResourceInt(v, rd.WsMaxTotalCPU, parseCPUMillicores)
	}
	if v := os.Getenv("QUOTA_WS_MAX_TOTAL_MEMORY"); v != "" {
		rd.WsMaxTotalMemory = parseResourceInt64(v, rd.WsMaxTotalMemory, parseMemoryBytes)
	}
	if v := os.Getenv("QUOTA_WS_MAX_IDLE_TIMEOUT"); v != "" {
		rd.WsMaxIdleTimeout = parseResourceInt(v, rd.WsMaxIdleTimeout, parseDurationSeconds)
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
	if v, err := s.DB.GetSystemSetting(settingKeyMaxWorkspaceDriveSize); err == nil && v != "" {
		rd.MaxWorkspaceDriveSize = parseResourceInt64(v, rd.MaxWorkspaceDriveSize, parseMemoryBytes)
	}
	if v, err := s.DB.GetSystemSetting(settingKeyMaxSandboxCPU); err == nil && v != "" {
		rd.MaxSandboxCPU = parseResourceInt(v, rd.MaxSandboxCPU, parseCPUMillicores)
	}
	if v, err := s.DB.GetSystemSetting(settingKeyMaxSandboxMemory); err == nil && v != "" {
		rd.MaxSandboxMemory = parseResourceInt64(v, rd.MaxSandboxMemory, parseMemoryBytes)
	}
	if v, err := s.DB.GetSystemSetting(settingKeyMaxIdleTimeout); err == nil && v != "" {
		rd.MaxIdleTimeout = parseResourceInt(v, rd.MaxIdleTimeout, parseDurationSeconds)
	}
	if v, err := s.DB.GetSystemSetting(settingKeyWsMaxTotalCPU); err == nil && v != "" {
		rd.WsMaxTotalCPU = parseResourceInt(v, rd.WsMaxTotalCPU, parseCPUMillicores)
	}
	if v, err := s.DB.GetSystemSetting(settingKeyWsMaxTotalMemory); err == nil && v != "" {
		rd.WsMaxTotalMemory = parseResourceInt64(v, rd.WsMaxTotalMemory, parseMemoryBytes)
	}
	if v, err := s.DB.GetSystemSetting(settingKeyWsMaxIdleTimeout); err == nil && v != "" {
		rd.WsMaxIdleTimeout = parseResourceInt(v, rd.WsMaxIdleTimeout, parseDurationSeconds)
	}

	return rd
}

// WorkspaceDefaults holds workspace-level resolved defaults (system defaults <- workspace_quotas override).
type WorkspaceDefaults struct {
	MaxSandboxes     int
	MaxSandboxCPU    int   // millicores
	MaxSandboxMemory int64 // bytes
	MaxIdleTimeout   int   // seconds
	MaxTotalCPU      int   // millicores
	MaxTotalMemory   int64 // bytes
	MaxDriveSize     int64 // bytes
}

// effectiveWorkspaceDefaults merges system defaults with workspace_quotas overrides.
func (s *Server) effectiveWorkspaceDefaults(workspaceID string) (WorkspaceDefaults, error) {
	rd := s.getResourceDefaults()
	wd := WorkspaceDefaults{
		MaxSandboxes:     rd.MaxSandboxesPerWorkspace,
		MaxSandboxCPU:    rd.MaxSandboxCPU,
		MaxSandboxMemory: rd.MaxSandboxMemory,
		MaxIdleTimeout:   rd.MaxIdleTimeout,
		MaxTotalCPU:      rd.WsMaxTotalCPU,
		MaxTotalMemory:   rd.WsMaxTotalMemory,
		MaxDriveSize:     rd.MaxWorkspaceDriveSize,
	}

	wq, err := s.DB.GetWorkspaceQuota(workspaceID)
	if err != nil {
		return wd, err
	}
	if wq == nil {
		return wd, nil
	}

	if wq.MaxSandboxes != nil {
		wd.MaxSandboxes = *wq.MaxSandboxes
	}
	if wq.MaxSandboxCPU != nil {
		wd.MaxSandboxCPU = *wq.MaxSandboxCPU
	}
	if wq.MaxSandboxMemory != nil {
		wd.MaxSandboxMemory = *wq.MaxSandboxMemory
	}
	if wq.MaxIdleTimeout != nil {
		wd.MaxIdleTimeout = *wq.MaxIdleTimeout
	}
	if wq.MaxTotalCPU != nil {
		wd.MaxTotalCPU = *wq.MaxTotalCPU
	}
	if wq.MaxTotalMemory != nil {
		wd.MaxTotalMemory = *wq.MaxTotalMemory
	}
	if wq.MaxDriveSize != nil {
		wd.MaxDriveSize = *wq.MaxDriveSize
	}

	return wd, nil
}

// effectiveQuota returns the effective max-workspaces quota for a user.
// Per-user overrides take precedence over system defaults.
func (s *Server) effectiveQuota(userID string) (maxWs int, err error) {
	rd := s.getResourceDefaults()
	maxWs = rd.MaxWorkspacesPerUser

	uq, err := s.DB.GetUserQuota(userID)
	if err != nil {
		return 0, err
	}
	if uq != nil && uq.MaxWorkspaces != nil {
		maxWs = *uq.MaxWorkspaces
	}

	return maxWs, nil
}

// checkWorkspaceQuota checks if a user can create another workspace.
// Returns whether creation is allowed, the current count, and the max.
// max=0 means unlimited.
func (s *Server) checkWorkspaceQuota(userID string) (allowed bool, current, max int, err error) {
	maxWs, err := s.effectiveQuota(userID)
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
// Uses workspace-level quotas.
// max=0 means unlimited.
func (s *Server) checkSandboxQuota(workspaceID string) (allowed bool, current, max int, err error) {
	wd, err := s.effectiveWorkspaceDefaults(workspaceID)
	if err != nil {
		return false, 0, 0, err
	}

	current, err = s.DB.CountSandboxesByWorkspace(workspaceID)
	if err != nil {
		return false, 0, 0, err
	}

	if wd.MaxSandboxes == 0 {
		return true, current, 0, nil
	}

	return current < wd.MaxSandboxes, current, wd.MaxSandboxes, nil
}

// checkWorkspaceResourceBudget checks if adding a sandbox with the given resources
// would exceed the workspace's total CPU/memory budget.
// Uses workspace-level quotas.
// Returns allowed=true if within budget or budget is unlimited (0).
func (s *Server) checkWorkspaceResourceBudget(workspaceID string, cpuMillis int, memBytes int64) (bool, error) {
	wd, err := s.effectiveWorkspaceDefaults(workspaceID)
	if err != nil {
		return false, err
	}

	// 0 means unlimited
	if wd.MaxTotalCPU == 0 && wd.MaxTotalMemory == 0 {
		return true, nil
	}

	currentCPU, currentMem, err := s.DB.SumWorkspaceSandboxResources(workspaceID)
	if err != nil {
		return false, err
	}

	if wd.MaxTotalCPU > 0 && currentCPU+int64(cpuMillis) > int64(wd.MaxTotalCPU) {
		return false, nil
	}
	if wd.MaxTotalMemory > 0 && currentMem+memBytes > wd.MaxTotalMemory {
		return false, nil
	}

	return true, nil
}

// getEffectiveIdleTimeout resolves the idle timeout from the settings chain.
// Returns 0 if disabled.
func (s *Server) getEffectiveIdleTimeout() time.Duration {
	rd := s.getResourceDefaults()
	if rd.MaxIdleTimeout == 0 {
		return 0
	}
	return time.Duration(rd.MaxIdleTimeout) * time.Second
}

// GetEffectiveIdleTimeout is the exported version for use by cmd/serve.go.
func (s *Server) GetEffectiveIdleTimeout() time.Duration {
	return s.getEffectiveIdleTimeout()
}

// parseResourceInt tries strconv.Atoi first (new integer format),
// then falls back to the legacy K8s format parser for backward compatibility.
func parseResourceInt(s string, fallback int, legacyParser func(string) int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	if n := legacyParser(s); n != 0 {
		return n
	}
	return fallback
}

// parseResourceInt64 tries strconv.ParseInt first (new integer format),
// then falls back to the legacy K8s format parser for backward compatibility.
func parseResourceInt64(s string, fallback int64, legacyParser func(string) int64) int64 {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if n := legacyParser(s); n != 0 {
		return n
	}
	return fallback
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

// parseDurationSeconds converts a Go-style duration string to seconds.
// Examples: "30m" -> 1800, "1h" -> 3600, "0" -> 0
func parseDurationSeconds(s string) int {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return int(d.Seconds())
}
