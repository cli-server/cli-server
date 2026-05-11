package codexappgateway

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"
)

// S3Config matches the shape used by internal/ccbroker/workspace/s3store.go;
// dedup into a shared storage package is a known follow-up. Until then,
// keep validation here in sync with ccbroker's.
type S3Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	PathStyle       bool
}

type ServeConfig struct {
	InboundHMACSecret         []byte
	S3                        S3Config
	TmpRoot                   string
	IdleShutdown              time.Duration
	ExecGatewayWSURL          string
	ExecGatewayInternalURL    string
	ExecGatewayInternalSecret string
	CapTokenHMACSecret        []byte
	LogLevel                  slog.Level
}

func LoadServeConfigFromEnv() (ServeConfig, error) {
	cfg := ServeConfig{
		TmpRoot:      envOr("CXG_TMP_ROOT", "/tmp/codex-app-gateway"),
		IdleShutdown: 30 * time.Minute,
		LogLevel:     slog.LevelInfo,
		S3: S3Config{
			Endpoint:        os.Getenv("CXG_S3_ENDPOINT"),
			Region:          envOr("CXG_S3_REGION", "us-east-1"),
			Bucket:          os.Getenv("CXG_S3_BUCKET"),
			AccessKeyID:     os.Getenv("CXG_S3_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("CXG_S3_SECRET_ACCESS_KEY"),
			PathStyle:       strings.EqualFold(os.Getenv("CXG_S3_PATH_STYLE"), "true"),
		},
		InboundHMACSecret:         []byte(os.Getenv("CXG_INBOUND_HMAC_SECRET")),
		ExecGatewayWSURL:          os.Getenv("CXG_EXEC_GATEWAY_URL"),
		ExecGatewayInternalURL:    os.Getenv("CXG_EXEC_GATEWAY_INTERNAL_URL"),
		ExecGatewayInternalSecret: os.Getenv("CXG_EXEC_GATEWAY_INTERNAL_SECRET"),
		CapTokenHMACSecret:        []byte(os.Getenv("CXG_CAPTOKEN_HMAC_SECRET")),
	}
	if len(cfg.InboundHMACSecret) == 0 {
		return cfg, fmt.Errorf("CXG_INBOUND_HMAC_SECRET is required")
	}
	if cfg.S3.Endpoint == "" {
		return cfg, fmt.Errorf("CXG_S3_ENDPOINT is required")
	}
	if u, err := url.Parse(cfg.S3.Endpoint); err != nil {
		return cfg, fmt.Errorf("CXG_S3_ENDPOINT not a valid URL: %w", err)
	} else if u.Scheme != "http" && u.Scheme != "https" {
		return cfg, fmt.Errorf("CXG_S3_ENDPOINT must use http:// or https:// scheme, got %q", cfg.S3.Endpoint)
	}
	if cfg.S3.Bucket == "" {
		return cfg, fmt.Errorf("CXG_S3_BUCKET is required")
	}
	if cfg.ExecGatewayWSURL == "" {
		return cfg, fmt.Errorf("CXG_EXEC_GATEWAY_URL is required")
	}
	if cfg.ExecGatewayInternalURL == "" {
		return cfg, fmt.Errorf("CXG_EXEC_GATEWAY_INTERNAL_URL is required")
	}
	if cfg.ExecGatewayInternalSecret == "" {
		return cfg, fmt.Errorf("CXG_EXEC_GATEWAY_INTERNAL_SECRET is required")
	}
	if len(cfg.CapTokenHMACSecret) == 0 {
		return cfg, fmt.Errorf("CXG_CAPTOKEN_HMAC_SECRET is required")
	}
	if v := os.Getenv("CXG_IDLE_SHUTDOWN"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("parse CXG_IDLE_SHUTDOWN: %w", err)
		}
		cfg.IdleShutdown = d
	}
	if v := strings.ToLower(os.Getenv("CXG_LOG_LEVEL")); v != "" {
		switch v {
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
