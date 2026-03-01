package sandbox

import (
	"encoding/json"
	"os"
)

// Config holds configuration for the K8s sandbox backend.
type Config struct {
	AgentserverNamespace    string
	Image                 string
	MemoryLimit           string
	CPULimit              string
	SessionStorageSize    string
	StorageClassName      string
	RuntimeClassName      string
	OpencodePort          int
	OpencodeConfigContent string // JSON config injected via OPENCODE_CONFIG_CONTENT
	OpenclawImage         string
	OpenclawPort          int
}

// DefaultConfig returns a Config populated from environment variables with sensible defaults.
func DefaultConfig() Config {
	return Config{
		AgentserverNamespace:    envOrDefault("AGENTSERVER_NAMESPACE", "default"),
		Image:                 envOrDefault("AGENT_IMAGE", "agentserver-agent:latest"),
		MemoryLimit:           envOrDefault("AGENT_MEMORY_LIMIT", "2Gi"),
		CPULimit:              envOrDefault("AGENT_CPU_LIMIT", "2"),
		SessionStorageSize:    envOrDefault("SESSION_STORAGE_SIZE", "5Gi"),
		StorageClassName:      os.Getenv("STORAGE_CLASS"),
		RuntimeClassName:      os.Getenv("RUNTIME_CLASS"),
		OpencodePort:          4096,
		OpencodeConfigContent: os.Getenv("OPENCODE_CONFIG_CONTENT"),
		OpenclawImage:         os.Getenv("OPENCLAW_IMAGE"),
		OpenclawPort:          18789,
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// BuildOpenclawConfig returns the openclaw.json content with gateway settings
// and optional Anthropic proxy credentials.
func BuildOpenclawConfig(proxyBaseURL, proxyToken string) string {
	type modelDef struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	type provider struct {
		BaseURL string     `json:"baseUrl"`
		APIKey  string     `json:"apiKey"`
		API     string     `json:"api"`
		Models  []modelDef `json:"models"`
	}
	type config struct {
		Gateway struct {
			ControlUI struct {
				AllowOriginFallback bool `json:"dangerouslyAllowHostHeaderOriginFallback,omitempty"`
				DisableDeviceAuth   bool `json:"dangerouslyDisableDeviceAuth,omitempty"`
			} `json:"controlUi"`
		} `json:"gateway"`
		Models *struct {
			Providers map[string]provider `json:"providers"`
		} `json:"models,omitempty"`
	}

	var cfg config
	cfg.Gateway.ControlUI.AllowOriginFallback = true
	cfg.Gateway.ControlUI.DisableDeviceAuth = true

	if proxyBaseURL != "" && proxyToken != "" {
		cfg.Models = &struct {
			Providers map[string]provider `json:"providers"`
		}{
			Providers: map[string]provider{
				"anthropic": {
					BaseURL: proxyBaseURL,
					APIKey:  proxyToken,
					API:     "anthropic-messages",
					Models: []modelDef{
						{ID: "claude-opus-4-6", Name: "Claude Opus 4.6"},
						{ID: "claude-opus-4-5", Name: "Claude Opus 4.5"},
						{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6"},
						{ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5"},
						{ID: "claude-haiku-4-5", Name: "Claude Haiku 4.5"},
					},
				},
			},
		}
	}

	b, _ := json.Marshal(cfg)
	return string(b)
}
