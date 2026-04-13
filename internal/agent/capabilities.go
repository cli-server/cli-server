package agent

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AgentCapabilities describes what runtimes, tools, and hardware are available
// in the agent's environment.
type AgentCapabilities struct {
	Languages []RuntimeInfo `json:"languages,omitempty"`
	Tools     []RuntimeInfo `json:"tools,omitempty"`
	GPU       *GPUInfo      `json:"gpu,omitempty"`
	ProbedAt  time.Time     `json:"probed_at"`
}

// RuntimeInfo describes a single detected runtime or tool.
type RuntimeInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Path    string `json:"path,omitempty"`
}

// GPUInfo describes a detected GPU.
type GPUInfo struct {
	Name     string `json:"name"`
	MemoryMB int    `json:"memory_mb,omitempty"`
}

// probe defines how to detect a single runtime or tool.
type probe struct {
	name     string
	binary   string
	args     []string
	parser   func(string) string
	category string // "language" or "tool"
}

// versionRe matches a semver-like version string, optionally prefixed with 'v'.
var versionRe = regexp.MustCompile(`v?(\d+\.\d+(?:\.\d+)?)`)

// javaVersionRe matches a quoted version in java -version output.
var javaVersionRe = regexp.MustCompile(`"(\d+\.\d+(?:\.\d+)?)"`)

// probes is the registry of all runtimes and tools to detect.
var probes = []probe{
	// Languages
	{name: "go", binary: "go", args: []string{"version"}, parser: parseGoVersion, category: "language"},
	{name: "python", binary: "python3", args: []string{"--version"}, parser: parsePythonVersion, category: "language"},
	{name: "node", binary: "node", args: []string{"--version"}, parser: parseNodeVersion, category: "language"},
	{name: "rust", binary: "rustc", args: []string{"--version"}, parser: parseRustVersion, category: "language"},
	{name: "java", binary: "java", args: []string{"-version"}, parser: parseJavaVersion, category: "language"},
	{name: "ruby", binary: "ruby", args: []string{"--version"}, parser: parseRubyVersion, category: "language"},
	{name: "php", binary: "php", args: []string{"--version"}, parser: parseGenericVersion, category: "language"},

	// Tools
	{name: "docker", binary: "docker", args: []string{"--version"}, parser: parseDockerVersion, category: "tool"},
	{name: "kubectl", binary: "kubectl", args: []string{"version", "--client", "--short"}, parser: parseGenericVersion, category: "tool"},
	{name: "git", binary: "git", args: []string{"--version"}, parser: parseGitVersion, category: "tool"},
	{name: "make", binary: "make", args: []string{"--version"}, parser: parseGenericVersion, category: "tool"},
	{name: "helm", binary: "helm", args: []string{"version", "--short"}, parser: parseGenericVersion, category: "tool"},
	{name: "cmake", binary: "cmake", args: []string{"--version"}, parser: parseGenericVersion, category: "tool"},
	{name: "terraform", binary: "terraform", args: []string{"--version"}, parser: parseGenericVersion, category: "tool"},
	{name: "aws", binary: "aws", args: []string{"--version"}, parser: parseGenericVersion, category: "tool"},
	{name: "gcloud", binary: "gcloud", args: []string{"--version"}, parser: parseGenericVersion, category: "tool"},
	{name: "ffmpeg", binary: "ffmpeg", args: []string{"-version"}, parser: parseGenericVersion, category: "tool"},
}

// ProbeCapabilities detects installed languages, tools, and GPU hardware
// concurrently. It always returns a non-nil result even if the context is
// cancelled.
func ProbeCapabilities(ctx context.Context) *AgentCapabilities {
	caps := &AgentCapabilities{
		ProbedAt: time.Now().UTC(),
	}

	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, p := range probes {
		wg.Add(1)
		go func(p probe) {
			defer wg.Done()
			ri := runProbe(ctx, p)
			if ri == nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			switch p.category {
			case "language":
				caps.Languages = append(caps.Languages, *ri)
			case "tool":
				caps.Tools = append(caps.Tools, *ri)
			}
		}(p)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		gpu := detectGPU(ctx)
		if gpu != nil {
			mu.Lock()
			caps.GPU = gpu
			mu.Unlock()
		}
	}()

	wg.Wait()
	return caps
}

// runProbe attempts to detect a single runtime or tool. Returns nil if the
// binary is not found or the command fails.
func runProbe(ctx context.Context, p probe) *RuntimeInfo {
	path, err := exec.LookPath(p.binary)
	if err != nil {
		return nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, p.binary, p.args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil
	}

	version := p.parser(string(out))
	if version == "" {
		return nil
	}

	return &RuntimeInfo{
		Name:    p.name,
		Version: version,
		Path:    path,
	}
}

// detectGPU tries nvidia-smi first, then macOS system_profiler.
func detectGPU(ctx context.Context) *GPUInfo {
	gpuCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	// Try NVIDIA first.
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		cmd := exec.CommandContext(gpuCtx, "nvidia-smi",
			"--query-gpu=name,memory.total",
			"--format=csv,noheader,nounits")
		out, err := cmd.CombinedOutput()
		if err == nil {
			line := strings.TrimSpace(string(out))
			// Format: "NAME, MEMORY_MB"
			parts := strings.SplitN(line, ",", 2)
			if len(parts) == 2 {
				name := strings.TrimSpace(parts[0])
				memStr := strings.TrimSpace(parts[1])
				return &GPUInfo{
					Name:     name,
					MemoryMB: parseInt(memStr),
				}
			}
			if len(parts) == 1 && parts[0] != "" {
				return &GPUInfo{Name: strings.TrimSpace(parts[0])}
			}
		}
	}

	// Try macOS system_profiler.
	if _, err := exec.LookPath("system_profiler"); err == nil {
		cmd := exec.CommandContext(gpuCtx, "system_profiler", "SPDisplaysDataType")
		out, err := cmd.CombinedOutput()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "Chipset Model:") {
					name := strings.TrimSpace(strings.TrimPrefix(trimmed, "Chipset Model:"))
					if name != "" {
						return &GPUInfo{Name: name}
					}
				}
			}
		}
	}

	return nil
}

// parseInt parses a string as an integer, returning 0 on failure.
func parseInt(s string) int {
	s = strings.TrimSpace(s)
	n, err := strconv.Atoi(s)
	if err != nil {
		// Try parsing as float and truncating (nvidia-smi sometimes returns "1234.0").
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int(f)
		}
		return 0
	}
	return n
}

// --- Version parsers ---

// parseGoVersion extracts the version from "go version go1.22.0 darwin/arm64".
func parseGoVersion(s string) string {
	if s == "" {
		return ""
	}
	idx := strings.Index(s, "go1")
	if idx < 0 {
		// Try go2+ in case it ever exists.
		idx = strings.Index(s, "go2")
		if idx < 0 {
			return ""
		}
	}
	// Skip the "go" prefix and parse the remaining version.
	return parseGenericVersion(s[idx+2:])
}

// parsePythonVersion extracts the version from "Python 3.12.1".
func parsePythonVersion(s string) string {
	return parseGenericVersion(s)
}

// parseNodeVersion extracts the version from "v20.11.0".
func parseNodeVersion(s string) string {
	return parseGenericVersion(s)
}

// parseRustVersion extracts the version from "rustc 1.77.0 (...)".
func parseRustVersion(s string) string {
	return parseGenericVersion(s)
}

// parseJavaVersion extracts the quoted version from java -version output.
// Example: `openjdk version "21.0.1" 2023-10-17`
func parseJavaVersion(s string) string {
	if s == "" {
		return ""
	}
	m := javaVersionRe.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// parseRubyVersion extracts the version from "ruby 3.3.0 (...)".
func parseRubyVersion(s string) string {
	return parseGenericVersion(s)
}

// parseDockerVersion extracts the version from "Docker version 25.0.3, build ...".
func parseDockerVersion(s string) string {
	return parseGenericVersion(s)
}

// parseGitVersion extracts the version from "git version 2.44.0".
func parseGitVersion(s string) string {
	return parseGenericVersion(s)
}

// parseGenericVersion extracts the first semver-like version from the first
// line of input. Handles optional 'v' prefix and trailing suffixes like
// "+gf56ede7".
func parseGenericVersion(s string) string {
	if s == "" {
		return ""
	}
	// Only look at the first line.
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	m := versionRe.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}
