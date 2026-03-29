package sandboxproxy

import (
	"os"
	"strings"
)

// Config holds sandbox-proxy configuration loaded from environment variables.
type Config struct {
	DatabaseURL             string
	ListenAddr              string
	BaseDomains             []string // all base domains (first is primary)
	OpencodeAssetDomain     string
	OpencodeSubdomainPrefix   string
	OpenclawSubdomainPrefix   string
	ClaudeCodeSubdomainPrefix string
}

// LoadConfigFromEnv reads configuration from environment variables.
// BASE_DOMAIN supports comma-separated values for multiple domains
// (e.g. "agentserver.dev,agent.cs.ac.cn").
func LoadConfigFromEnv() Config {
	cfg := Config{
		DatabaseURL:             os.Getenv("DATABASE_URL"),
		ListenAddr:              os.Getenv("LISTEN_ADDR"),
		OpencodeAssetDomain:     os.Getenv("OPENCODE_ASSET_DOMAIN"),
		OpencodeSubdomainPrefix: os.Getenv("OPENCODE_SUBDOMAIN_PREFIX"),
		OpenclawSubdomainPrefix:   os.Getenv("OPENCLAW_SUBDOMAIN_PREFIX"),
		ClaudeCodeSubdomainPrefix: os.Getenv("CLAUDECODE_SUBDOMAIN_PREFIX"),
	}

	// Parse comma-separated base domains.
	if raw := os.Getenv("BASE_DOMAIN"); raw != "" {
		for _, d := range strings.Split(raw, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				cfg.BaseDomains = append(cfg.BaseDomains, d)
			}
		}
	}

	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8082"
	}
	if cfg.OpencodeSubdomainPrefix == "" {
		cfg.OpencodeSubdomainPrefix = "code"
	}
	if cfg.OpenclawSubdomainPrefix == "" {
		cfg.OpenclawSubdomainPrefix = "claw"
	}
	if cfg.ClaudeCodeSubdomainPrefix == "" {
		cfg.ClaudeCodeSubdomainPrefix = "claude"
	}
	if cfg.OpencodeAssetDomain == "" && len(cfg.BaseDomains) > 0 {
		cfg.OpencodeAssetDomain = "opencodeapp." + cfg.BaseDomains[0]
	}
	return cfg
}
