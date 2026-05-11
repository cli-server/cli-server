package codexappgateway

import (
	"strings"
	"testing"
	"time"
)

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("CXG_INBOUND_HMAC_SECRET", "in-sec")
	t.Setenv("CXG_S3_ENDPOINT", "http://s3")
	t.Setenv("CXG_S3_BUCKET", "buck")
	t.Setenv("CXG_EXEC_GATEWAY_URL", "ws://exec-gw:6060")
	t.Setenv("CXG_EXEC_GATEWAY_INTERNAL_URL", "http://exec-gw:6060")
	t.Setenv("CXG_EXEC_GATEWAY_INTERNAL_SECRET", "internal-sec")
	t.Setenv("CXG_CAPTOKEN_HMAC_SECRET", "captok-sec")
}

func TestLoadServeConfig_Defaults(t *testing.T) {
	setRequired(t)
	cfg, err := LoadServeConfigFromEnv()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.TmpRoot != "/tmp/codex-app-gateway" {
		t.Errorf("TmpRoot = %q", cfg.TmpRoot)
	}
	if cfg.IdleShutdown != 30*time.Minute {
		t.Errorf("IdleShutdown = %v", cfg.IdleShutdown)
	}
	if cfg.S3.Region != "us-east-1" {
		t.Errorf("S3 default region = %q", cfg.S3.Region)
	}
}

func TestLoadServeConfig_RequiresInboundSecret(t *testing.T) {
	setRequired(t)
	t.Setenv("CXG_INBOUND_HMAC_SECRET", "")
	_, err := LoadServeConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), "CXG_INBOUND_HMAC_SECRET") {
		t.Fatalf("want secret-required error, got %v", err)
	}
}

func TestLoadServeConfig_RequiresExecGatewayURL(t *testing.T) {
	setRequired(t)
	t.Setenv("CXG_EXEC_GATEWAY_URL", "")
	_, err := LoadServeConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), "CXG_EXEC_GATEWAY_URL") {
		t.Fatalf("want exec-gateway-url-required, got %v", err)
	}
}

func TestLoadServeConfig_RejectsBadS3Endpoint(t *testing.T) {
	setRequired(t)
	t.Setenv("CXG_S3_ENDPOINT", "no-scheme.example.com")
	_, err := LoadServeConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("want scheme error, got %v", err)
	}
}

func TestLoadServeConfig_OverridesIdleShutdown(t *testing.T) {
	setRequired(t)
	t.Setenv("CXG_IDLE_SHUTDOWN", "5m")
	cfg, err := LoadServeConfigFromEnv()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.IdleShutdown != 5*time.Minute {
		t.Errorf("IdleShutdown = %v", cfg.IdleShutdown)
	}
}
