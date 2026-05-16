package codexappgateway

import (
	"bytes"
	"context"
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

// stubTokenFetcher returns empty token (caller falls back to static
// CodexAPIKey or none); good enough for tests that don't care about the
// env value, only about the config.toml content.
type stubTokenFetcher struct{}

func (stubTokenFetcher) FetchToken(_ context.Context, _ string) (string, error) {
	return "", nil
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
		ListenAddr:           ":8086",
	}
}

func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

func TestBuildConfig_EmitsAgentserverMCPAndMintsWorkspaceToken(t *testing.T) {
	cfg := newTestCfg()
	build := makeBuildConfig(cfg, &stubConnected{}, stubTokenFetcher{}, "/usr/local/bin/codex-app-gateway", newDiscardLogger())

	got, err := build(context.Background(), "ws_a", "lb-token-xyz")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	m := got.Config.AgentServer
	if m.CodexBin != "/usr/local/bin/codex-app-gateway" {
		t.Errorf("CodexBin = %q", m.CodexBin)
	}
	if m.WorkspaceID != "ws_a" {
		t.Errorf("WorkspaceID = %q", m.WorkspaceID)
	}
	if m.ExecGatewayURL != "ws://exec-gw:6060/bridge" {
		t.Errorf("ExecGatewayURL = %q", m.ExecGatewayURL)
	}
	if m.AppGatewayInternalURL != "http://127.0.0.1:8086" {
		t.Errorf("AppGatewayInternalURL = %q", m.AppGatewayInternalURL)
	}
	if m.LoopbackToken != "lb-token-xyz" {
		t.Errorf("LoopbackToken = %q", m.LoopbackToken)
	}
	p, err := codexexecgateway.VerifyCapabilityToken(m.WorkspaceToken, cfg.CapTokenHMACSecret)
	if err != nil {
		t.Fatalf("workspace token verify: %v", err)
	}
	if p.WorkspaceID != "ws_a" {
		t.Errorf("workspace token .workspace_id = %q", p.WorkspaceID)
	}
	if p.TurnID == "" {
		t.Error("workspace token .turn_id empty")
	}
}

func TestBuildConfig_NoExecGatewayFetchHappens(t *testing.T) {
	// Per redesign, build() no longer hits the exec-gateway at spawn time;
	// the connected client should not be touched.
	stub := &stubConnected{gotW: ""}
	cfg := newTestCfg()
	build := makeBuildConfig(cfg, stub, stubTokenFetcher{}, "/x", newDiscardLogger())
	if _, err := build(context.Background(), "ws_a", "lb"); err != nil {
		t.Fatalf("build: %v", err)
	}
	if stub.gotW != "" {
		t.Errorf("connectedClient.Connected was called (gotW=%q); list_environments now reads it live", stub.gotW)
	}
}

func TestBuildConfig_RespectsConfiguredTrustedPaths(t *testing.T) {
	cfg := newTestCfg()
	cfg.ProjectTrustedPaths = []string{"/workspace", "/data"}
	build := makeBuildConfig(cfg, &stubConnected{}, stubTokenFetcher{}, "/x", newDiscardLogger())
	got, err := build(context.Background(), "ws_a", "lb")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if strings.Join(got.Config.ProjectTrustedPaths, ",") != "/workspace,/data" {
		t.Errorf("trusted paths = %v", got.Config.ProjectTrustedPaths)
	}
}

func TestLoopbackInternalURL(t *testing.T) {
	cases := map[string]string{
		":8086":         "http://127.0.0.1:8086",
		"0.0.0.0:9000":  "http://127.0.0.1:9000",
		"127.0.0.1:80":  "http://127.0.0.1:80",
		"":              "",
	}
	for in, want := range cases {
		if got := loopbackInternalURL(in); got != want {
			t.Errorf("loopbackInternalURL(%q) = %q, want %q", in, got, want)
		}
	}
}
