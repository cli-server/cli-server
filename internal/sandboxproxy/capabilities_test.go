package sandboxproxy

import (
	"encoding/json"
	"testing"

	"github.com/agentserver/agentserver/internal/db"
)

func TestBuildCardJSON(t *testing.T) {
	caps := &capabilitiesPayload{
		Languages: []runtimeEntry{
			{Name: "go", Version: "1.22.0"},
			{Name: "python3", Version: "3.12.1"},
		},
		Tools: []runtimeEntry{
			{Name: "docker", Version: "25.0.3"},
			{Name: "git", Version: "2.44.0"},
		},
		GPU: &gpuEntry{Name: "Apple M3 Pro GPU"},
	}
	info := &db.AgentInfo{
		CPUModelName:    "Apple M3 Pro",
		CPUCountLogical: 12,
		MemoryTotal:     38654705664, // 36 GB
		DiskTotal:       1000000000000,
		DiskFree:        500000000000,
	}

	raw := buildCardJSON(caps, info)
	var card map[string]json.RawMessage
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if _, ok := card["languages"]; !ok {
		t.Error("missing languages field")
	}
	if _, ok := card["tools"]; !ok {
		t.Error("missing tools field")
	}
	if _, ok := card["hardware"]; !ok {
		t.Error("missing hardware field")
	}

	var tags []string
	json.Unmarshal(card["tags"], &tags)
	tagSet := make(map[string]bool)
	for _, tag := range tags {
		tagSet[tag] = true
	}
	for _, expected := range []string{"go", "python", "docker", "git", "gpu", "high-memory"} {
		if !tagSet[expected] {
			t.Errorf("missing tag %q in %v", expected, tags)
		}
	}

	var skills []map[string]string
	json.Unmarshal(card["skills"], &skills)
	skillNames := make(map[string]bool)
	for _, s := range skills {
		skillNames[s["name"]] = true
	}
	for _, expected := range []string{"code-editing", "code-review", "terminal", "code-search", "go-development", "python-development"} {
		if !skillNames[expected] {
			t.Errorf("missing skill %q in %v", expected, skills)
		}
	}

	var hw map[string]any
	json.Unmarshal(card["hardware"], &hw)
	if hw["has_gpu"] != true {
		t.Error("hardware.has_gpu should be true")
	}
}

func TestBuildCardJSON_NoGPU_LowMemory(t *testing.T) {
	caps := &capabilitiesPayload{
		Languages: []runtimeEntry{{Name: "go", Version: "1.22.0"}},
	}
	info := &db.AgentInfo{
		CPUModelName:    "Intel Core i5",
		CPUCountLogical: 4,
		MemoryTotal:     8589934592, // 8 GB
		DiskTotal:       256000000000,
		DiskFree:        128000000000,
	}

	raw := buildCardJSON(caps, info)
	var card map[string]json.RawMessage
	json.Unmarshal(raw, &card)

	var tags []string
	json.Unmarshal(card["tags"], &tags)
	for _, tag := range tags {
		if tag == "gpu" || tag == "high-memory" {
			t.Errorf("unexpected tag %q for low-spec machine", tag)
		}
	}

	var hw map[string]any
	json.Unmarshal(card["hardware"], &hw)
	if _, ok := hw["has_gpu"]; ok {
		t.Error("hardware should not have has_gpu for no GPU")
	}
}
