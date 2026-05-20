package codexexecgateway

import (
	"os"
	"testing"
	"time"
)

func TestLoadConfigFromEnv_Defaults(t *testing.T) {
	t.Setenv("CXG_DATABASE_URL", "postgres://x")
	t.Setenv("CXG_CAPTOKEN_HMAC_SECRET", "s3cret")
	t.Setenv("CXG_INTERNAL_SHARED_SECRET", "intern")
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if cfg.Port != "6060" {
		t.Errorf("Port: want 6060, got %q", cfg.Port)
	}
}

func TestLoadConfigFromEnv_RequiresDB(t *testing.T) {
	os.Unsetenv("CXG_DATABASE_URL")
	t.Setenv("CXG_CAPTOKEN_HMAC_SECRET", "s3cret")
	t.Setenv("CXG_INTERNAL_SHARED_SECRET", "intern")
	if _, err := LoadConfigFromEnv(); err == nil {
		t.Fatal("expected error when CXG_DATABASE_URL unset")
	}
}

func setRequiredConfigEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CXG_DATABASE_URL", "postgres://test")
	t.Setenv("CXG_CAPTOKEN_HMAC_SECRET", "test-secret-32-bytes-minimum-aaaa")
	t.Setenv("CXG_INTERNAL_SHARED_SECRET", "test-internal")
}

func TestLoadConfig_BoundedResourceDefaults(t *testing.T) {
	setRequiredConfigEnv(t)
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if cfg.MaxFrameBytes != 16*1024*1024 {
		t.Errorf("MaxFrameBytes default: got %d, want 16 MiB (%d)", cfg.MaxFrameBytes, 16*1024*1024)
	}
	if cfg.BridgeIdleTimeout != 5*time.Minute {
		t.Errorf("BridgeIdleTimeout default: got %v, want 5m", cfg.BridgeIdleTimeout)
	}
	if cfg.MaxStreamsPerExecutor != 32 {
		t.Errorf("MaxStreamsPerExecutor default: got %d, want 32", cfg.MaxStreamsPerExecutor)
	}
}

func TestLoadConfig_BoundedResourceEnvOverride(t *testing.T) {
	setRequiredConfigEnv(t)
	t.Setenv("CXG_MAX_FRAME_BYTES", "1048576")
	t.Setenv("CXG_BRIDGE_IDLE_TIMEOUT", "30s")
	t.Setenv("CXG_MAX_STREAMS_PER_EXECUTOR", "8")
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if cfg.MaxFrameBytes != 1024*1024 {
		t.Errorf("MaxFrameBytes override: got %d, want 1 MiB", cfg.MaxFrameBytes)
	}
	if cfg.BridgeIdleTimeout != 30*time.Second {
		t.Errorf("BridgeIdleTimeout override: got %v, want 30s", cfg.BridgeIdleTimeout)
	}
	if cfg.MaxStreamsPerExecutor != 8 {
		t.Errorf("MaxStreamsPerExecutor override: got %d, want 8", cfg.MaxStreamsPerExecutor)
	}
}
