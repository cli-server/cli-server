package ccbroker

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

type Config struct {
	Port        string
	DatabaseURL string
	JWTSecret   []byte
	LogLevel    slog.Level
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
