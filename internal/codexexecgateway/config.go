package codexexecgateway

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// WebSocket keepalive (ping interval + idle timeout) is phase-2; nhooyr's defaults govern for now.
type Config struct {
	Port                 string
	DatabaseURL          string
	CapTokenHMACSecret   []byte
	InternalSharedSecret string
	LogLevel             slog.Level
}

// Validate checks that security-critical fields are populated. NewServer calls
// this so that direct Config{} construction cannot silently bypass HMAC checks.
func (cfg Config) Validate() error {
	if len(cfg.CapTokenHMACSecret) == 0 {
		return fmt.Errorf("CapTokenHMACSecret is required")
	}
	if cfg.InternalSharedSecret == "" {
		return fmt.Errorf("InternalSharedSecret is required")
	}
	return nil
}

func LoadConfigFromEnv() (Config, error) {
	cfg := Config{
		Port:                 envOr("CXG_PORT", "6060"),
		DatabaseURL:          os.Getenv("CXG_DATABASE_URL"),
		CapTokenHMACSecret:   []byte(os.Getenv("CXG_CAPTOKEN_HMAC_SECRET")),
		InternalSharedSecret: os.Getenv("CXG_INTERNAL_SHARED_SECRET"),
		LogLevel:             slog.LevelInfo,
	}
	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("CXG_DATABASE_URL is required")
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	if v := os.Getenv("CXG_LOG_LEVEL"); v != "" {
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
