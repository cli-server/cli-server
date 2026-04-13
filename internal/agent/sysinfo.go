package agent

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// cpuInfoDetail aggregates CPU information.
type cpuInfoDetail struct {
	CPUs          []cpu.InfoStat `json:"cpus"`
	CountPhysical int            `json:"count_physical"`
	CountLogical  int            `json:"count_logical"`
}

// AgentInfoData is the system info payload sent from agent to server.
type AgentInfoData struct {
	Hostname        string                `json:"hostname"`
	OS              string                `json:"os"`
	Platform        string                `json:"platform"`
	PlatformVersion string                `json:"platform_version"`
	KernelArch      string                `json:"kernel_arch"`
	CPUModelName    string                `json:"cpu_model_name"`
	CPUCountLogical int                   `json:"cpu_count_logical"`
	MemoryTotal     uint64                `json:"memory_total"`
	DiskTotal       uint64                `json:"disk_total"`
	DiskFree        uint64                `json:"disk_free"`
	AgentVersion    string                `json:"agent_version"`
	OpencodeVersion string                `json:"opencode_version"`
	Workdir         string                `json:"workdir"`
	HostInfo        *host.InfoStat        `json:"host_info,omitempty"`
	CPUInfo         *cpuInfoDetail        `json:"cpu_info,omitempty"`
	MemoryInfo      *mem.VirtualMemoryStat `json:"memory_info,omitempty"`
	DiskInfo        *disk.UsageStat       `json:"disk_info,omitempty"`
	Capabilities    *AgentCapabilities     `json:"capabilities,omitempty"`
}

func collectAgentInfo(opencodeURL string, workdir string) *AgentInfoData {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info := &AgentInfoData{
		AgentVersion:    Version,
		OpencodeVersion: fetchOpencodeVersion(opencodeURL),
		Workdir:         workdir,
	}

	// Host info
	if hi, err := host.InfoWithContext(ctx); err == nil {
		info.HostInfo = hi
		info.Hostname = hi.Hostname
		info.OS = hi.OS
		info.Platform = hi.Platform
		info.PlatformVersion = hi.PlatformVersion
		info.KernelArch = hi.KernelArch
	} else {
		log.Printf("agent info: failed to get host info: %v", err)
	}

	// CPU info
	if cpus, err := cpu.InfoWithContext(ctx); err == nil {
		cpuDetail := &cpuInfoDetail{CPUs: cpus}
		if physical, err := cpu.CountsWithContext(ctx, false); err == nil {
			cpuDetail.CountPhysical = physical
		}
		if logical, err := cpu.CountsWithContext(ctx, true); err == nil {
			cpuDetail.CountLogical = logical
			info.CPUCountLogical = logical
		}
		if len(cpus) > 0 {
			info.CPUModelName = cpus[0].ModelName
		}
		info.CPUInfo = cpuDetail
	} else {
		log.Printf("agent info: failed to get cpu info: %v", err)
	}

	// Memory info
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		info.MemoryInfo = vm
		info.MemoryTotal = vm.Total
	} else {
		log.Printf("agent info: failed to get memory info: %v", err)
	}

	// Disk info
	diskPath := "/"
	if runtime.GOOS == "windows" {
		diskPath = "C:"
	}
	if du, err := disk.UsageWithContext(ctx, diskPath); err == nil {
		info.DiskInfo = du
		info.DiskTotal = du.Total
		info.DiskFree = du.Free
	} else {
		log.Printf("agent info: failed to get disk info: %v", err)
	}

	return info
}

func fetchOpencodeVersion(opencodeURL string) string {
	if opencodeURL == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opencodeURL+"/global/health", nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var result struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}
	return result.Version
}
