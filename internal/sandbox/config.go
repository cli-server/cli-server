package sandbox

import (
	"encoding/json"
	"os"
)

// Config holds configuration for the K8s sandbox backend.
type Config struct {
	AgentserverNamespace     string
	Image                    string
	SessionStorageSize       string
	StorageClassName         string
	RuntimeClassName         string
	OpencodePort             int
	OpencodeConfigContent    string // JSON config injected via OPENCODE_CONFIG_CONTENT
	OpenclawImage            string
	OpenclawPort             int
	OpenclawRuntimeClassName string
}

// DefaultConfig returns a Config populated from environment variables with sensible defaults.
func DefaultConfig() Config {
	return Config{
		AgentserverNamespace:     envOrDefault("AGENTSERVER_NAMESPACE", "default"),
		Image:                    envOrDefault("AGENT_IMAGE", "agentserver-agent:latest"),
		SessionStorageSize:       envOrDefault("SESSION_STORAGE_SIZE", "5Gi"),
		StorageClassName:         os.Getenv("STORAGE_CLASS"),
		RuntimeClassName:         os.Getenv("RUNTIME_CLASS"),
		OpencodePort:             4096,
		OpencodeConfigContent:    os.Getenv("OPENCODE_CONFIG_CONTENT"),
		OpenclawImage:            os.Getenv("OPENCLAW_IMAGE"),
		OpenclawPort:             18789,
		OpenclawRuntimeClassName: os.Getenv("OPENCLAW_RUNTIME_CLASS"),
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// BuildOpencodeConfig merges the per-sandbox proxy token into the base opencode
// config JSON. The proxyBaseURL is already expected to be in the base config
// (provider.anthropic.options.baseURL). This function only injects the
// per-sandbox apiKey.
func BuildOpencodeConfig(baseConfig, proxyToken string) string {
	// Parse the user-provided base config (from OPENCODE_CONFIG_CONTENT / values.yaml).
	var cfg map[string]interface{}
	if baseConfig != "" {
		if err := json.Unmarshal([]byte(baseConfig), &cfg); err != nil {
			cfg = make(map[string]interface{})
		}
	} else {
		cfg = make(map[string]interface{})
	}

	// Inject provider.anthropic.options.apiKey with per-sandbox token.
	if proxyToken != "" {
		provider, _ := cfg["provider"].(map[string]interface{})
		if provider == nil {
			provider = make(map[string]interface{})
		}
		anthropic, _ := provider["anthropic"].(map[string]interface{})
		if anthropic == nil {
			anthropic = make(map[string]interface{})
		}
		options, _ := anthropic["options"].(map[string]interface{})
		if options == nil {
			options = make(map[string]interface{})
		}
		options["apiKey"] = proxyToken
		anthropic["options"] = options
		provider["anthropic"] = anthropic
		cfg["provider"] = provider
	}

	b, _ := json.Marshal(cfg)
	return string(b)
}

// ExtractProxyBaseURL extracts provider.anthropic.options.baseURL from the
// opencode config JSON. Used by sandbox managers that need the proxy URL
// (e.g. for openclaw config).
func ExtractProxyBaseURL(configJSON string) string {
	if configJSON == "" {
		return ""
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return ""
	}
	provider, _ := cfg["provider"].(map[string]interface{})
	if provider == nil {
		return ""
	}
	anthropic, _ := provider["anthropic"].(map[string]interface{})
	if anthropic == nil {
		return ""
	}
	options, _ := anthropic["options"].(map[string]interface{})
	if options == nil {
		return ""
	}
	baseURL, _ := options["baseURL"].(string)
	return baseURL
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

	var c config
	c.Gateway.ControlUI.AllowOriginFallback = true
	c.Gateway.ControlUI.DisableDeviceAuth = true

	if proxyBaseURL != "" && proxyToken != "" {
		c.Models = &struct {
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

	b, _ := json.Marshal(c)
	return string(b)
}
