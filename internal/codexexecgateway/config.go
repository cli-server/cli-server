package codexexecgateway

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// WebSocket keepalive (ping interval + idle timeout) is phase-2; nhooyr's defaults govern for now.
type Config struct {
	Port                      string
	DatabaseURL               string
	CapTokenHMACSecret        []byte
	InternalSharedSecret      string
	AgentserverInternalSecret string
	// PublicWSBaseURL is the wss:// origin used in the response of the
	// upstream-compat `POST /cloud/executor/{exe_id}/register` endpoint.
	// Example: "wss://codex-exec.agent.cs.ac.cn:443". When empty, the
	// endpoint synthesises a URL from the incoming request's Host header
	// (less reliable behind proxies but useful in dev).
	PublicWSBaseURL string
	// PublicHTTPSBaseURL is the https:// origin the relay endpoint is
	// reachable at — embedded in CreateRelay responses so env-mcp can
	// build curl PUT/GET commands. Example:
	// "https://codex-exec.agent.cs.ac.cn". When empty, the relay
	// /api/exec-gateway/relay/create endpoint refuses to mint tickets
	// (env-mcp falls back to the ws cat-pump path).
	PublicHTTPSBaseURL string
	// RelayDefaultTTL caps how long a minted ticket waits for both
	// sides to connect before timing out. Defaults to 5 minutes.
	RelayDefaultTTL time.Duration
	// RelayMaxPerWorkspace caps concurrent relays per workspace.
	// Defaults to 16; protects gateway memory from runaway agents.
	RelayMaxPerWorkspace int
	// MaxFrameBytes caps each inbound/bridge ws frame. Default 16 MiB.
	// Override via CXG_MAX_FRAME_BYTES. Frames exceeding this are
	// rejected with close code 1009 (Message Too Big) by nhooyr.
	MaxFrameBytes int64
	// BridgeIdleTimeout is how long a bridge session can be silent
	// (no in/out frames) before the gateway sends RelayReset and
	// closes the bridge ws. Default 5m. Override via
	// CXG_BRIDGE_IDLE_TIMEOUT.
	BridgeIdleTimeout time.Duration
	// MaxStreamsPerExecutor bounds concurrent /bridge sessions per
	// executor. Default 32. Beyond this, /bridge returns 503.
	// Override via CXG_MAX_STREAMS_PER_EXECUTOR.
	MaxStreamsPerExecutor int
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
		Port:                      envOr("CXG_PORT", "6060"),
		DatabaseURL:               os.Getenv("CXG_DATABASE_URL"),
		CapTokenHMACSecret:        []byte(os.Getenv("CXG_CAPTOKEN_HMAC_SECRET")),
		InternalSharedSecret:      os.Getenv("CXG_INTERNAL_SHARED_SECRET"),
		AgentserverInternalSecret: os.Getenv("CXG_AGENTSERVER_INTERNAL_SECRET"),
		PublicWSBaseURL:           os.Getenv("CXG_PUBLIC_WS_BASE_URL"),
		PublicHTTPSBaseURL:        os.Getenv("CXG_PUBLIC_HTTPS_BASE_URL"),
		RelayDefaultTTL:           parseDurationOr("CXG_RELAY_DEFAULT_TTL", 5*time.Minute),
		RelayMaxPerWorkspace:      parseIntOr("CXG_RELAY_MAX_PER_WORKSPACE", 16),
		MaxFrameBytes:             parseInt64Or("CXG_MAX_FRAME_BYTES", 16*1024*1024),
		BridgeIdleTimeout:         parseDurationOr("CXG_BRIDGE_IDLE_TIMEOUT", 5*time.Minute),
		MaxStreamsPerExecutor:      parseIntOr("CXG_MAX_STREAMS_PER_EXECUTOR", 32),
		LogLevel:                  slog.LevelInfo,
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

func parseDurationOr(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func parseIntOr(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func parseInt64Or(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}
