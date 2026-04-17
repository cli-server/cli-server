package ccbroker

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

type Config struct {
	Port                string
	DatabaseURL         string
	JWTSecret           []byte
	LogLevel            slog.Level
	ExecutorRegistryURL string
	AgentserverURL      string
	OpenVikingURL       string
	OpenVikingAPIKey    string
	IMBridgeURL         string
	IMBridgeSecret      string
}

func LoadConfigFromEnv() (Config, error) {
	cfg := Config{
		Port:        envOr("CCBROKER_PORT", "8085"),
		DatabaseURL: os.Getenv("CCBROKER_DATABASE_URL"),
		LogLevel:    slog.LevelInfo,
	}
	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("CCBROKER_DATABASE_URL is required")
	}
	secret := os.Getenv("CCBROKER_JWT_SECRET")
	if secret == "" {
		return cfg, fmt.Errorf("CCBROKER_JWT_SECRET is required (32+ chars)")
	}
	cfg.JWTSecret = []byte(secret)
	cfg.ExecutorRegistryURL = envOr("CCBROKER_EXECUTOR_REGISTRY_URL", "http://localhost:8084")
	cfg.AgentserverURL = envOr("CCBROKER_AGENTSERVER_URL", "http://localhost:8080")
	cfg.OpenVikingURL = envOr("CCBROKER_OPENVIKING_URL", "http://localhost:1933")
	cfg.OpenVikingAPIKey = os.Getenv("CCBROKER_OPENVIKING_API_KEY")
	cfg.IMBridgeURL = os.Getenv("CCBROKER_IMBRIDGE_URL")
	cfg.IMBridgeSecret = os.Getenv("INTERNAL_API_SECRET")
	if v := os.Getenv("CCBROKER_LOG_LEVEL"); v != "" {
		switch strings.ToLower(v) {
		case "debug":
			cfg.LogLevel = slog.LevelDebug
		case "warn":
			cfg.LogLevel = slog.LevelWarn
		case "error":
			cfg.LogLevel = slog.LevelError
		}
	}
	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
