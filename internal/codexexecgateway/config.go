package codexexecgateway

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

type Config struct {
	Port                 string
	DatabaseURL          string
	CapTokenHMACSecret   []byte
	InternalSharedSecret string
	PingInterval         time.Duration
	IdleTimeout          time.Duration
	LogLevel             slog.Level
}

func LoadConfigFromEnv() (Config, error) {
	cfg := Config{
		Port:                 envOr("CXG_PORT", "6060"),
		DatabaseURL:          os.Getenv("CXG_DATABASE_URL"),
		CapTokenHMACSecret:   []byte(os.Getenv("CXG_CAPTOKEN_HMAC_SECRET")),
		InternalSharedSecret: os.Getenv("CXG_INTERNAL_SHARED_SECRET"),
		PingInterval:         30 * time.Second,
		IdleTimeout:          5 * time.Minute,
		LogLevel:             slog.LevelInfo,
	}
	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("CXG_DATABASE_URL is required")
	}
	if len(cfg.CapTokenHMACSecret) == 0 {
		return cfg, fmt.Errorf("CXG_CAPTOKEN_HMAC_SECRET is required")
	}
	if cfg.InternalSharedSecret == "" {
		return cfg, fmt.Errorf("CXG_INTERNAL_SHARED_SECRET is required")
	}
	if v := os.Getenv("CXG_PING_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("parse CXG_PING_INTERVAL: %w", err)
		}
		cfg.PingInterval = d
	}
	if v := os.Getenv("CXG_IDLE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("parse CXG_IDLE_TIMEOUT: %w", err)
		}
		cfg.IdleTimeout = d
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
