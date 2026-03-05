package db

import (
	"database/sql"
	"encoding/json"
	"time"
)

// AgentInfo holds system information reported by a local agent.
type AgentInfo struct {
	SandboxID       string          `json:"sandbox_id"`
	Hostname        string          `json:"hostname"`
	OS              string          `json:"os"`
	Platform        string          `json:"platform"`
	PlatformVersion string          `json:"platform_version"`
	KernelArch      string          `json:"kernel_arch"`
	CPUModelName    string          `json:"cpu_model_name"`
	CPUCountLogical int             `json:"cpu_count_logical"`
	MemoryTotal     int64           `json:"memory_total"`
	DiskTotal       int64           `json:"disk_total"`
	DiskFree        int64           `json:"disk_free"`
	AgentVersion    string          `json:"agent_version"`
	OpencodeVersion string          `json:"opencode_version"`
	HostInfo        json.RawMessage `json:"host_info"`
	CPUInfo         json.RawMessage `json:"cpu_info"`
	MemoryInfo      json.RawMessage `json:"memory_info"`
	DiskInfo        json.RawMessage `json:"disk_info"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// UpsertAgentInfo inserts or updates agent info for a sandbox.
func (db *DB) UpsertAgentInfo(info *AgentInfo) error {
	_, err := db.Exec(`
		INSERT INTO agent_info (
			sandbox_id, hostname, os, platform, platform_version, kernel_arch,
			cpu_model_name, cpu_count_logical, memory_total, disk_total, disk_free,
			agent_version, opencode_version, host_info, cpu_info, memory_info, disk_info,
			updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,NOW())
		ON CONFLICT (sandbox_id) DO UPDATE SET
			hostname = EXCLUDED.hostname,
			os = EXCLUDED.os,
			platform = EXCLUDED.platform,
			platform_version = EXCLUDED.platform_version,
			kernel_arch = EXCLUDED.kernel_arch,
			cpu_model_name = EXCLUDED.cpu_model_name,
			cpu_count_logical = EXCLUDED.cpu_count_logical,
			memory_total = EXCLUDED.memory_total,
			disk_total = EXCLUDED.disk_total,
			disk_free = EXCLUDED.disk_free,
			agent_version = EXCLUDED.agent_version,
			opencode_version = EXCLUDED.opencode_version,
			host_info = EXCLUDED.host_info,
			cpu_info = EXCLUDED.cpu_info,
			memory_info = EXCLUDED.memory_info,
			disk_info = EXCLUDED.disk_info,
			updated_at = NOW()
	`,
		info.SandboxID, info.Hostname, info.OS, info.Platform, info.PlatformVersion, info.KernelArch,
		info.CPUModelName, info.CPUCountLogical, info.MemoryTotal, info.DiskTotal, info.DiskFree,
		info.AgentVersion, info.OpencodeVersion, info.HostInfo, info.CPUInfo, info.MemoryInfo, info.DiskInfo,
	)
	return err
}

// GetAgentInfo returns agent info for a sandbox, or nil,nil if not found.
func (db *DB) GetAgentInfo(sandboxID string) (*AgentInfo, error) {
	info := &AgentInfo{}
	err := db.QueryRow(`
		SELECT sandbox_id, hostname, os, platform, platform_version, kernel_arch,
			cpu_model_name, cpu_count_logical, memory_total, disk_total, disk_free,
			agent_version, opencode_version, host_info, cpu_info, memory_info, disk_info,
			updated_at
		FROM agent_info WHERE sandbox_id = $1
	`, sandboxID).Scan(
		&info.SandboxID, &info.Hostname, &info.OS, &info.Platform, &info.PlatformVersion, &info.KernelArch,
		&info.CPUModelName, &info.CPUCountLogical, &info.MemoryTotal, &info.DiskTotal, &info.DiskFree,
		&info.AgentVersion, &info.OpencodeVersion, &info.HostInfo, &info.CPUInfo, &info.MemoryInfo, &info.DiskInfo,
		&info.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return info, nil
}
