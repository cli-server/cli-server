package sandboxproxy

import (
	"encoding/json"
	"fmt"

	"github.com/agentserver/agentserver/internal/db"
)

// capabilitiesPayload mirrors agent.AgentCapabilities for JSON unmarshaling.
// Defined locally to avoid importing the agent (CLI) package.
type capabilitiesPayload struct {
	Languages []runtimeEntry `json:"languages"`
	Tools     []runtimeEntry `json:"tools"`
	GPU       *gpuEntry      `json:"gpu,omitempty"`
	ProbedAt  string         `json:"probed_at"`
}

type runtimeEntry struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type gpuEntry struct {
	Name   string `json:"name"`
	Memory string `json:"memory,omitempty"`
}

func buildCardJSON(caps *capabilitiesPayload, info *db.AgentInfo) json.RawMessage {
	card := map[string]any{
		"languages": caps.Languages,
		"tools":     caps.Tools,
		"hardware":  buildHardwareSummary(info, caps.GPU),
		"skills":    buildSkills(caps),
		"tags":      buildTags(caps, info),
	}
	if caps.GPU != nil {
		card["gpu"] = caps.GPU
	}
	data, _ := json.Marshal(card)
	return data
}

func buildHardwareSummary(info *db.AgentInfo, gpu *gpuEntry) map[string]any {
	hw := map[string]any{
		"cpu_summary":  fmt.Sprintf("%s, %d cores", info.CPUModelName, info.CPUCountLogical),
		"memory_gb":    info.MemoryTotal / (1024 * 1024 * 1024),
		"disk_gb":      info.DiskTotal / (1024 * 1024 * 1024),
		"disk_free_gb": info.DiskFree / (1024 * 1024 * 1024),
	}
	if gpu != nil {
		hw["has_gpu"] = true
		hw["gpu_info"] = gpu.Name
	}
	return hw
}

func buildSkills(caps *capabilitiesPayload) []map[string]string {
	skills := []map[string]string{
		{"name": "code-editing", "description": "Read, write, and edit source code"},
		{"name": "code-review", "description": "Review code for bugs and best practices"},
		{"name": "terminal", "description": "Execute shell commands"},
		{"name": "code-search", "description": "Search and navigate codebases"},
	}
	for _, lang := range caps.Languages {
		name := lang.Name
		if name == "python3" {
			name = "python"
		}
		skills = append(skills, map[string]string{
			"name":        name + "-development",
			"description": fmt.Sprintf("%s development (v%s)", name, lang.Version),
		})
	}
	toolSkills := map[string]string{
		"docker":    "container-management",
		"kubectl":   "kubernetes-management",
		"helm":      "kubernetes-management",
		"terraform": "infrastructure-as-code",
		"aws":       "cloud-aws",
		"gcloud":    "cloud-gcp",
	}
	seen := make(map[string]bool)
	for _, tool := range caps.Tools {
		if skillName, ok := toolSkills[tool.Name]; ok && !seen[skillName] {
			seen[skillName] = true
			skills = append(skills, map[string]string{
				"name":        skillName,
				"description": fmt.Sprintf("%s via %s", skillName, tool.Name),
			})
		}
	}
	return skills
}

func buildTags(caps *capabilitiesPayload, info *db.AgentInfo) []string {
	tags := []string{}
	for _, lang := range caps.Languages {
		tag := lang.Name
		if tag == "python3" {
			tag = "python"
		}
		tags = append(tags, tag)
	}
	for _, tool := range caps.Tools {
		tags = append(tags, tool.Name)
	}
	if caps.GPU != nil {
		tags = append(tags, "gpu")
	}
	if info.MemoryTotal >= 32*1024*1024*1024 {
		tags = append(tags, "high-memory")
	}
	return tags
}
