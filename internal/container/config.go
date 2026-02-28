package container

import "os"

type Config struct {
	Image        string
	OpenclawImage string
	MemoryLimit  int64
	NanoCPUs     int64
	PidsLimit    int64
	NetworkMode  string
}

func DefaultConfig() Config {
	return Config{
		Image:        envOrDefault("AGENT_IMAGE", "cli-server-agent:latest"),
		OpenclawImage: os.Getenv("OPENCLAW_IMAGE"),
		MemoryLimit:  envInt64OrDefault("AGENT_MEMORY_LIMIT", 2*1024*1024*1024), // 2GB
		NanoCPUs:     envInt64OrDefault("AGENT_NANO_CPUS", 2_000_000_000),       // 2 CPUs
		PidsLimit:    envInt64OrDefault("AGENT_PIDS_LIMIT", 256),
		NetworkMode:  envOrDefault("AGENT_NETWORK_MODE", "bridge"),
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt64OrDefault(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var n int64
	for _, c := range v {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int64(c-'0')
	}
	return n
}
