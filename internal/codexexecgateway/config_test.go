package codexexecgateway

import (
	"os"
	"testing"
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
