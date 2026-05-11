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
	if cfg.PingInterval != 30*time.Second {
		t.Errorf("PingInterval: want 30s, got %v", cfg.PingInterval)
	}
	if cfg.IdleTimeout != 5*time.Minute {
		t.Errorf("IdleTimeout: want 5m, got %v", cfg.IdleTimeout)
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

func TestLoadConfigFromEnv_OverridesDuration(t *testing.T) {
	t.Setenv("CXG_DATABASE_URL", "postgres://x")
	t.Setenv("CXG_CAPTOKEN_HMAC_SECRET", "s3cret")
	t.Setenv("CXG_INTERNAL_SHARED_SECRET", "intern")
	t.Setenv("CXG_PING_INTERVAL", "10s")
	t.Setenv("CXG_IDLE_TIMEOUT", "2m")
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if cfg.PingInterval != 10*time.Second {
		t.Errorf("PingInterval: want 10s, got %v", cfg.PingInterval)
	}
	if cfg.IdleTimeout != 2*time.Minute {
		t.Errorf("IdleTimeout: want 2m, got %v", cfg.IdleTimeout)
	}
}
