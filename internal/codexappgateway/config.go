package codexappgateway

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
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
	CapTokenTTL               time.Duration
	LogLevel                  slog.Level

	// Model provider config — written verbatim into each per-thread
	// config.toml. The codex subprocess reads ModelProviderEnvKey from its
	// own env (forwarded from CodexAPIKey here) to authenticate to the
	// LLM gateway (typically llmproxy in-cluster).
	ModelProvider        string
	Model                string
	ModelProviderBaseURL string
	ModelProviderEnvKey  string
	ModelProviderWireAPI string
	CodexAPIKey          string

	// ProjectTrustedPaths is the list of paths marked `trust_level = "trusted"`
	// in config.toml. Without at least one, codex refuses to run shell-side
	// operations on the project root.
	ProjectTrustedPaths []string

	// AgentserverInternalURL is the http base for codex token verification
	// (e.g. "http://release-agentserver.namespace.svc:8080"). Required when
	// the gateway uses RemoteVerifier (production default).
	AgentserverInternalURL string

	// AgentserverInternalSecret matches the agentserver's INTERNAL_API_SECRET
	// env. Sent in every verify request as X-Internal-Secret.
	AgentserverInternalSecret string

	// ListenAddr is the gateway's HTTP listen address (e.g. ":8086"). Used
	// to derive the loopback URL env-mcp uses for /internal/connected.
	// Set by main.go before NewServer; tests may leave it empty (codexhome
	// then emits no AppGatewayInternalURL and env-mcp won't be able to
	// list environments, which is fine for tests that don't exercise
	// list_environments).
	ListenAddr string

	// OperationLog endpoint + auth. When OperationLogURL is empty, the
	// /notebook/ws Interceptor is constructed but oplog Submit is a no-op
	// (Client is nil and the Interceptor guards check nil).
	OperationLogURL    string
	OperationLogSecret string // X-Internal-Secret header value
	OperationLogChan   int    // bounded channel capacity, default 1024
}

func LoadServeConfigFromEnv() (ServeConfig, error) {
	cfg := ServeConfig{
		TmpRoot: envOr("CXG_TMP_ROOT", "/tmp/codex-app-gateway"),
		IdleShutdown: 30 * time.Minute,
		// CapTokenTTL bounds the cap-token's validity. The token is
		// minted at codex app-server spawn and re-used by env-mcp for
		// the subprocess's whole lifetime. IdleShutdown is 30 min, so
		// 24h is comfortably longer than any realistic session — keeps
		// long-running codex --remote TUIs from hitting 401 mid-call
		// without giving up the bound altogether.
		CapTokenTTL: 24 * time.Hour,
		LogLevel:    slog.LevelInfo,
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
		ModelProvider:             envOr("CXG_MODEL_PROVIDER", "modelserver"),
		Model:                     envOr("CXG_MODEL", "gpt-5.5"),
		ModelProviderBaseURL:      envOr("CXG_MODEL_PROVIDER_BASE_URL", "http://llmproxy:8085/v1"),
		ModelProviderEnvKey:       envOr("CXG_MODEL_PROVIDER_ENV_KEY", "CODEX_API_KEY"),
		ModelProviderWireAPI:      envOr("CXG_MODEL_PROVIDER_WIRE_API", "responses"),
		CodexAPIKey:               os.Getenv("CXG_CODEX_API_KEY"),
	}
	if v := os.Getenv("CXG_PROJECT_TRUSTED_PATHS"); v != "" {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				cfg.ProjectTrustedPaths = append(cfg.ProjectTrustedPaths, p)
			}
		}
	}
	cfg.AgentserverInternalURL = os.Getenv("CXG_AGENTSERVER_INTERNAL_URL")
	cfg.AgentserverInternalSecret = os.Getenv("CXG_AGENTSERVER_INTERNAL_SECRET")
	cfg.OperationLogURL = os.Getenv("CXG_OPLOG_URL")
	cfg.OperationLogSecret = os.Getenv("CXG_OPLOG_SECRET")
	cfg.OperationLogChan = 1024
	if v := os.Getenv("CXG_OPLOG_CHAN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("parse CXG_OPLOG_CHAN: %q", v)
		}
		cfg.OperationLogChan = n
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
	if cfg.AgentserverInternalURL == "" {
		return cfg, fmt.Errorf("CXG_AGENTSERVER_INTERNAL_URL is required")
	}
	if cfg.AgentserverInternalSecret == "" {
		return cfg, fmt.Errorf("CXG_AGENTSERVER_INTERNAL_SECRET is required")
	}
	if v := os.Getenv("CXG_IDLE_SHUTDOWN"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("parse CXG_IDLE_SHUTDOWN: %w", err)
		}
		cfg.IdleShutdown = d
	}
	if v := os.Getenv("CXG_CAPTOKEN_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("parse CXG_CAPTOKEN_TTL: %w", err)
		}
		cfg.CapTokenTTL = d
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
