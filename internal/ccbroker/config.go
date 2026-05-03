package ccbroker

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port                string
	DatabaseURL         string
	LogLevel            slog.Level
	ExecutorRegistryURL string
	AgentserverURL      string
	S3Endpoint          string
	S3Region            string
	S3Bucket            string
	S3AccessKeyID       string
	S3SecretAccessKey   string
	S3PathStyle         bool
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
	cfg.ExecutorRegistryURL = envOr("CCBROKER_EXECUTOR_REGISTRY_URL", "http://localhost:8084")
	cfg.AgentserverURL = envOr("CCBROKER_AGENTSERVER_URL", "http://localhost:8080")

	cfg.S3Endpoint = os.Getenv("CCBROKER_S3_ENDPOINT")
	cfg.S3Region = os.Getenv("CCBROKER_S3_REGION")
	cfg.S3Bucket = os.Getenv("CCBROKER_S3_BUCKET")
	cfg.S3AccessKeyID = os.Getenv("CCBROKER_S3_ACCESS_KEY_ID")
	cfg.S3SecretAccessKey = os.Getenv("CCBROKER_S3_SECRET_ACCESS_KEY")
	cfg.S3PathStyle = envBool("CCBROKER_S3_PATH_STYLE", false)
	if cfg.S3Endpoint == "" {
		return cfg, fmt.Errorf("CCBROKER_S3_ENDPOINT is required")
	}
	if cfg.S3Bucket == "" {
		return cfg, fmt.Errorf("CCBROKER_S3_BUCKET is required")
	}

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

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
