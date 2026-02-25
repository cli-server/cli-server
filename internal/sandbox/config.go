package sandbox

import "os"

// Config holds configuration for the K8s sandbox backend.
type Config struct {
	Namespace       string
	Image           string
	MemoryLimit     string
	CPULimit        string
	ImagePullSecret string
}

// DefaultConfig returns a Config populated from environment variables with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Namespace:       envOrDefault("SANDBOX_NAMESPACE", "default"),
		Image:           envOrDefault("AGENT_IMAGE", "cli-server-agent:latest"),
		MemoryLimit:     envOrDefault("AGENT_MEMORY_LIMIT", "2Gi"),
		CPULimit:        envOrDefault("AGENT_CPU_LIMIT", "2"),
		ImagePullSecret: os.Getenv("IMAGE_PULL_SECRET"),
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
