package sandbox

import "os"

// Config holds configuration for the K8s sandbox backend.
type Config struct {
	Namespace          string
	Image              string
	MemoryLimit        string
	CPULimit           string
	SessionStorageSize string
	StorageClassName   string
	RuntimeClassName   string
	OpencodePort       int
	OpenclawImage      string
	OpenclawPort       int
}

// DefaultConfig returns a Config populated from environment variables with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Namespace:          envOrDefault("SANDBOX_NAMESPACE", "default"),
		Image:              envOrDefault("AGENT_IMAGE", "cli-server-agent:latest"),
		MemoryLimit:        envOrDefault("AGENT_MEMORY_LIMIT", "2Gi"),
		CPULimit:           envOrDefault("AGENT_CPU_LIMIT", "2"),
		SessionStorageSize: envOrDefault("SESSION_STORAGE_SIZE", "5Gi"),
		StorageClassName:   os.Getenv("STORAGE_CLASS"),
		RuntimeClassName:   os.Getenv("RUNTIME_CLASS"),
		OpencodePort:       4096,
		OpenclawImage:      os.Getenv("OPENCLAW_IMAGE"),
		OpenclawPort:       18789,
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
