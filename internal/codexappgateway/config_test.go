package codexappgateway

import (
	"strings"
	"testing"
	"time"
)

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("CXG_S3_ENDPOINT", "http://s3")
	t.Setenv("CXG_S3_BUCKET", "buck")
	t.Setenv("CXG_EXEC_GATEWAY_URL", "ws://exec-gw:6060")
	t.Setenv("CXG_EXEC_GATEWAY_INTERNAL_URL", "http://exec-gw:6060")
	t.Setenv("CXG_EXEC_GATEWAY_INTERNAL_SECRET", "internal-sec")
	t.Setenv("CXG_CAPTOKEN_HMAC_SECRET", "captok-sec")
	t.Setenv("CXG_AGENTSERVER_INTERNAL_URL", "http://agentserver:8080")
	t.Setenv("CXG_AGENTSERVER_INTERNAL_SECRET", "agentserver-internal-sec")
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
	if cfg.CapTokenTTL != 24*time.Hour {
		t.Errorf("CapTokenTTL = %v", cfg.CapTokenTTL)
	}
	if cfg.ModelProvider != "modelserver" || cfg.Model != "gpt-5.5" {
		t.Errorf("model defaults: provider=%q model=%q", cfg.ModelProvider, cfg.Model)
	}
	if cfg.ModelProviderEnvKey != "CODEX_API_KEY" {
		t.Errorf("ModelProviderEnvKey = %q", cfg.ModelProviderEnvKey)
	}
}

func TestLoadServeConfig_ParsesProjectTrustedPaths(t *testing.T) {
	setRequired(t)
	t.Setenv("CXG_PROJECT_TRUSTED_PATHS", "/workspace, /data, ")
	cfg, err := LoadServeConfigFromEnv()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.ProjectTrustedPaths) != 2 ||
		cfg.ProjectTrustedPaths[0] != "/workspace" ||
		cfg.ProjectTrustedPaths[1] != "/data" {
		t.Errorf("ProjectTrustedPaths = %v", cfg.ProjectTrustedPaths)
	}
}

func TestLoadServeConfig_RequiresAgentserverURL(t *testing.T) {
	setRequired(t)
	t.Setenv("CXG_AGENTSERVER_INTERNAL_URL", "")
	_, err := LoadServeConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), "CXG_AGENTSERVER_INTERNAL_URL") {
		t.Fatalf("want agentserver-url-required, got %v", err)
	}
}

func TestLoadServeConfig_RequiresAgentserverSecret(t *testing.T) {
	setRequired(t)
	t.Setenv("CXG_AGENTSERVER_INTERNAL_SECRET", "")
	_, err := LoadServeConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), "CXG_AGENTSERVER_INTERNAL_SECRET") {
		t.Fatalf("want agentserver-secret-required, got %v", err)
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
