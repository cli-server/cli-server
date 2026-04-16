package executorregistry

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

type Config struct {
	Port        string
	DatabaseURL string
	LogLevel    slog.Level
}

func LoadConfigFromEnv() (Config, error) {
	cfg := Config{
		Port:        envOr("EXECREG_PORT", "8084"),
		DatabaseURL: os.Getenv("EXECREG_DATABASE_URL"),
		LogLevel:    slog.LevelInfo,
	}
	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("EXECREG_DATABASE_URL is required")
	}
	if v := os.Getenv("EXECREG_LOG_LEVEL"); v != "" {
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
