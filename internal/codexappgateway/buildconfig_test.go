package codexappgateway

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/codexexecgateway"
	"github.com/agentserver/agentserver/internal/codexexecgateway/execmodel"
)

type stubConnected struct {
	rows []execmodel.ConnectedExecutor
	err  error
	gotW string
}

func (s *stubConnected) Connected(_ context.Context, w string) ([]execmodel.ConnectedExecutor, error) {
	s.gotW = w
	return s.rows, s.err
}

func newTestCfg() ServeConfig {
	return ServeConfig{
		ExecGatewayWSURL:     "ws://exec-gw:6060",
		CapTokenHMACSecret:   []byte("cap-secret"),
		CapTokenTTL:          time.Minute,
		ModelProvider:        "modelserver",
		Model:                "gpt-5.5",
		ModelProviderBaseURL: "http://llmproxy:8085/v1",
		ModelProviderEnvKey:  "CODEX_API_KEY",
		ModelProviderWireAPI: "responses",
	}
}

func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

func TestBuildConfig_PopulatesExecutorsAndMintsValidTokens(t *testing.T) {
	stub := &stubConnected{rows: []execmodel.ConnectedExecutor{
		{ExeID: "exe_alpha", Description: "Daisy MBP"},
		{ExeID: "exe_beta", Description: "EC2"},
	}}
	cfg := newTestCfg()
	build := makeBuildConfig(cfg, stub, "/usr/local/bin/codex-app-gateway", newDiscardLogger())

	got, err := build(context.Background(), "ws_a")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if stub.gotW != "ws_a" {
		t.Errorf("client called with %q, want ws_a", stub.gotW)
	}
	if got.ModelProvider != "modelserver" || got.Model != "gpt-5.5" {
		t.Errorf("model: %+v", got)
	}
	if len(got.Executors) != 2 {
		t.Fatalf("executors: %+v", got.Executors)
	}
	if got.Executors[0].BridgeURL != "ws://exec-gw:6060/bridge/exe_alpha" {
		t.Errorf("bridge url[0] = %s", got.Executors[0].BridgeURL)
	}
	if got.Executors[0].TokenEnv != "CXG_BRIDGE_TOKEN_EXE_ALPHA" {
		t.Errorf("token env[0] = %s", got.Executors[0].TokenEnv)
	}
	if got.Executors[0].CodexBin != "/usr/local/bin/codex-app-gateway" {
		t.Errorf("codex bin[0] = %s", got.Executors[0].CodexBin)
	}
	// All entries share one turn_id (so revoke-turn cancels them as a unit).
	if got.Executors[0].TurnID == "" || got.Executors[0].TurnID != got.Executors[1].TurnID {
		t.Errorf("turn ids should match: %q %q", got.Executors[0].TurnID, got.Executors[1].TurnID)
	}
	// Each token must verify at the exec-gateway with its OWN exe_id and
	// reject other executors' ids.
	for i, e := range got.Executors {
		p, err := codexexecgateway.VerifyCapabilityToken(e.TokenVal, cfg.CapTokenHMACSecret)
		if err != nil {
			t.Fatalf("verify[%d]: %v", i, err)
		}
		if !p.AllowsExeID(e.ID) {
			t.Errorf("token[%d] does not allow its own exe_id", i)
		}
		other := got.Executors[(i+1)%len(got.Executors)].ID
		if p.AllowsExeID(other) {
			t.Errorf("token[%d] leaks access to %s", i, other)
		}
	}
	// Default trusted path applied when none configured.
	if len(got.ProjectTrustedPaths) != 1 || got.ProjectTrustedPaths[0] != "/tmp" {
		t.Errorf("trusted paths default: %v", got.ProjectTrustedPaths)
	}
}

func TestBuildConfig_FailSoftWhenExecGatewayDown(t *testing.T) {
	stub := &stubConnected{err: errors.New("connection refused")}
	cfg := newTestCfg()
	build := makeBuildConfig(cfg, stub, "/codex-app-gateway", newDiscardLogger())

	got, err := build(context.Background(), "ws_a")
	if err != nil {
		t.Fatalf("build should fail-soft, got %v", err)
	}
	if len(got.Executors) != 0 {
		t.Errorf("expected empty executors on degraded fetch, got %+v", got.Executors)
	}
	if got.Model == "" {
		t.Error("model should still be populated for chat-only mode")
	}
}

func TestBuildConfig_NoExecutorsStillProducesValidConfig(t *testing.T) {
	stub := &stubConnected{rows: nil}
	cfg := newTestCfg()
	build := makeBuildConfig(cfg, stub, "/codex-app-gateway", newDiscardLogger())
	got, err := build(context.Background(), "ws_a")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(got.Executors) != 0 {
		t.Errorf("got executors: %+v", got.Executors)
	}
}

func TestBuildConfig_RespectsConfiguredTrustedPaths(t *testing.T) {
	stub := &stubConnected{}
	cfg := newTestCfg()
	cfg.ProjectTrustedPaths = []string{"/workspace", "/data"}
	build := makeBuildConfig(cfg, stub, "/codex-app-gateway", newDiscardLogger())
	got, err := build(context.Background(), "ws_a")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if strings.Join(got.ProjectTrustedPaths, ",") != "/workspace,/data" {
		t.Errorf("trusted paths = %v", got.ProjectTrustedPaths)
	}
}

func TestBuildConfig_ExeIDWithDashesNormalisesEnvVar(t *testing.T) {
	stub := &stubConnected{rows: []execmodel.ConnectedExecutor{
		{ExeID: "exe-dashy-id"},
	}}
	cfg := newTestCfg()
	build := makeBuildConfig(cfg, stub, "/codex-app-gateway", newDiscardLogger())
	got, err := build(context.Background(), "ws_a")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got.Executors[0].TokenEnv != "CXG_BRIDGE_TOKEN_EXE_DASHY_ID" {
		t.Errorf("token env = %s", got.Executors[0].TokenEnv)
	}
}
