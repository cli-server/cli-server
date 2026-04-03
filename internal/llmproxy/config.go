package llmproxy

import (
	"os"
	"strconv"
)

// Config holds all configuration for the LLM proxy.
type Config struct {
	ListenAddr         string // HTTP listen address, e.g. ":8081"
	DatabaseURL        string // proxy's own PostgreSQL connection URL
	AgentserverURL     string // agentserver internal API URL for token validation
	AnthropicBaseURL   string // upstream Anthropic API URL
	AnthropicAPIKey    string // real Anthropic API key
	AnthropicAuthToken string // alternative: Bearer token auth
	GeminiBaseURL      string // upstream Gemini API URL
	GeminiAPIKey       string // real Google API key for Gemini
	TraceHeader        string // custom trace header name
	DefaultMaxRPD      int    // default max requests per day per workspace (0 = unlimited)
}

// LoadConfigFromEnv reads configuration from environment variables.
func LoadConfigFromEnv() Config {
	cfg := Config{
		ListenAddr:         envOr("LLMPROXY_LISTEN_ADDR", ":8081"),
		DatabaseURL:        os.Getenv("LLMPROXY_DATABASE_URL"),
		AgentserverURL:     os.Getenv("LLMPROXY_AGENTSERVER_URL"),
		AnthropicBaseURL:   envOr("ANTHROPIC_BASE_URL", "https://api.anthropic.com"),
		AnthropicAPIKey:    os.Getenv("ANTHROPIC_API_KEY"),
		AnthropicAuthToken: os.Getenv("ANTHROPIC_AUTH_TOKEN"),
		GeminiBaseURL:      envOr("GEMINI_BASE_URL", "https://generativelanguage.googleapis.com"),
		GeminiAPIKey:       os.Getenv("GEMINI_API_KEY"),
		TraceHeader:        envOr("LLMPROXY_TRACE_HEADER", "X-Trace-Id"),
	}
	if v := os.Getenv("LLMPROXY_DEFAULT_MAX_RPD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.DefaultMaxRPD = n
		}
	}
	return cfg
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
