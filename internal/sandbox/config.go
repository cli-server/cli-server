package sandbox

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/agentserver/agentserver/internal/process"
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
	OpenclawWeixinEnabled    bool
	NanoclawImage            string
	NanoclawRuntimeClassName string
	NanoclawIMBridgeEnabled  bool
	NanoclawBridgeBaseURL    string // agentserver internal URL for NanoClaw pods to call back (e.g. "http://agentserver:8080")
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
		OpenclawWeixinEnabled:    os.Getenv("OPENCLAW_WEIXIN_ENABLED") == "true",
		NanoclawImage:            os.Getenv("NANOCLAW_IMAGE"),
		NanoclawRuntimeClassName: os.Getenv("NANOCLAW_RUNTIME_CLASS"),
		NanoclawIMBridgeEnabled:  os.Getenv("NANOCLAW_IM_BRIDGE_ENABLED") == "true" || os.Getenv("NANOCLAW_WEIXIN_ENABLED") == "true",
		NanoclawBridgeBaseURL:    os.Getenv("NANOCLAW_BRIDGE_BASE_URL"),
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// BuildOpencodeConfig merges the per-sandbox proxy token into the base opencode
// config JSON. When overrideBaseURL is non-empty (BYOK mode), it also replaces
// provider.anthropic.options.baseURL.
func BuildOpencodeConfig(baseConfig, apiKey, overrideBaseURL string) string {
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
	if apiKey != "" {
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
		options["apiKey"] = apiKey
		if overrideBaseURL != "" {
			options["baseURL"] = overrideBaseURL
		}
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
// and optional Anthropic proxy credentials. The gatewayToken is written into
// gateway.auth.token so that the gateway and Control UI share the same secret;
// without this, OpenClaw v2026.3.12+ auto-generates a random token on startup
// that won't match the token our proxy injects.
func BuildOpenclawConfig(proxyBaseURL, proxyToken, gatewayToken string, weixinEnabled bool, customModels []process.LLMModel) string {
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
	type gatewayAuth struct {
		Token string `json:"token,omitempty"`
	}
	type weixinChannel struct {
		Enabled bool   `json:"enabled,omitempty"`
		BaseURL string `json:"baseUrl,omitempty"`
	}
	type pluginsConfig struct {
		Allow []string `json:"allow,omitempty"`
	}
	type config struct {
		Gateway struct {
			Auth           *gatewayAuth `json:"auth,omitempty"`
			TrustedProxies []string     `json:"trustedProxies,omitempty"`
			ControlUI      struct {
				Enabled             bool `json:"enabled,omitempty"`
				AllowInsecureAuth   bool `json:"allowInsecureAuth,omitempty"`
				AllowOriginFallback bool `json:"dangerouslyAllowHostHeaderOriginFallback,omitempty"`
				DisableDeviceAuth   bool `json:"dangerouslyDisableDeviceAuth,omitempty"`
			} `json:"controlUi"`
		} `json:"gateway"`
		Plugins  *pluginsConfig `json:"plugins,omitempty"`
		Channels *struct {
			Weixin weixinChannel `json:"openclaw-weixin,omitempty"`
		} `json:"channels,omitempty"`
		Models *struct {
			Providers map[string]provider `json:"providers"`
		} `json:"models,omitempty"`
	}

	var c config
	if gatewayToken != "" {
		c.Gateway.Auth = &gatewayAuth{Token: gatewayToken}
	}
	// Trust cluster-internal proxy IPs so the gateway reads our injected
	// Authorization header and X-Forwarded-For on WebSocket upgrades.
	c.Gateway.TrustedProxies = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	c.Gateway.ControlUI.Enabled = true
	c.Gateway.ControlUI.AllowInsecureAuth = true
	c.Gateway.ControlUI.AllowOriginFallback = true
	c.Gateway.ControlUI.DisableDeviceAuth = true

	if weixinEnabled {
		c.Plugins = &pluginsConfig{Allow: []string{"openclaw-weixin"}}
		c.Channels = &struct {
			Weixin weixinChannel `json:"openclaw-weixin,omitempty"`
		}{
			Weixin: weixinChannel{
				Enabled: true,
				BaseURL: "https://ilinkai.weixin.qq.com",
			},
		}
	}

	if proxyBaseURL != "" && proxyToken != "" {
		models := []modelDef{
			{ID: "claude-opus-4-6", Name: "Claude Opus 4.6"},
			{ID: "claude-opus-4-5", Name: "Claude Opus 4.5"},
			{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6"},
			{ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5"},
			{ID: "claude-haiku-4-5", Name: "Claude Haiku 4.5"},
		}
		if len(customModels) > 0 {
			models = make([]modelDef, len(customModels))
			for i, m := range customModels {
				models[i] = modelDef{ID: m.ID, Name: m.Name}
			}
		}
		c.Models = &struct {
			Providers map[string]provider `json:"providers"`
		}{
			Providers: map[string]provider{
				"anthropic": {
					BaseURL: proxyBaseURL,
					APIKey:  proxyToken,
					API:     "anthropic-messages",
					Models:  models,
				},
			},
		}
	}

	b, _ := json.Marshal(c)
	return string(b)
}

// BuildNanoclawConfig returns the environment variable content for a nanoclaw
// container. When byokBaseURL and byokAPIKey are non-empty (BYOK mode), they
// override the default proxy credentials.
func BuildNanoclawConfig(proxyBaseURL, proxyToken, assistantName string, weixinBridgeURL, bridgeSecret string, byokBaseURL, byokAPIKey string) string {
	baseURL := proxyBaseURL
	apiKey := proxyToken
	if byokBaseURL != "" {
		baseURL = byokBaseURL
		apiKey = byokAPIKey
	}
	var lines []string
	lines = append(lines, "ANTHROPIC_BASE_URL="+baseURL)
	lines = append(lines, "ANTHROPIC_API_KEY="+apiKey)
	if assistantName == "" {
		assistantName = "Andy"
	}
	lines = append(lines, "ASSISTANT_NAME="+assistantName)
	lines = append(lines, "NANOCLAW_NO_CONTAINER=true")
	if weixinBridgeURL != "" {
		lines = append(lines, "NANOCLAW_BRIDGE_URL="+weixinBridgeURL)
		// Backwards compat (remove after all pods updated)
		lines = append(lines, "NANOCLAW_WEIXIN_BRIDGE_URL="+weixinBridgeURL)
	}
	if bridgeSecret != "" {
		lines = append(lines, "NANOCLAW_BRIDGE_SECRET="+bridgeSecret)
	}
	return strings.Join(lines, "\n") + "\n"
}
