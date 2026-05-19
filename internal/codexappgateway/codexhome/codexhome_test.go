package codexhome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManager_NewTmpDir_LayoutAndPermissions(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	d, err := m.NewTmpDir("ws_a")
	if err != nil {
		t.Fatalf("NewTmpDir: %v", err)
	}
	if !strings.HasPrefix(d, filepath.Join(root, "ws_a")) {
		t.Errorf("path = %s", d)
	}
	st, err := os.Stat(d)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !st.IsDir() {
		t.Fatal("not a dir")
	}
	if st.Mode().Perm() != 0o700 {
		t.Errorf("perm = %v", st.Mode().Perm())
	}
}

func TestManager_RemoveTmpDir(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	d, err := m.NewTmpDir("ws_a")
	if err != nil {
		t.Fatalf("NewTmpDir: %v", err)
	}
	if err := m.RemoveTmpDir(d); err != nil {
		t.Fatalf("RemoveTmpDir: %v", err)
	}
	if _, err := os.Stat(d); !os.IsNotExist(err) {
		t.Errorf("dir still exists: %v", err)
	}
}

func TestManager_RemoveTmpDir_RejectsOutsideRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	m := NewManager(root)
	// Sibling directory whose name starts with the same prefix as root
	sibling := root + "-evil"
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveTmpDir(filepath.Join(sibling, "x")); err == nil {
		t.Fatal("RemoveTmpDir should reject sibling-prefix path")
	}
	// Confirm the sibling still exists.
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("sibling unexpectedly removed: %v", err)
	}
}

func TestRenderConfigTOML_DisablesBuiltinShellAndRegistersAgentserverMCP(t *testing.T) {
	cfg := ConfigInput{
		ModelProvider: "modelserver",
		Model:         "gpt-5.5",
		ModelProviders: map[string]ModelProvider{
			"modelserver": {
				Name:    "modelserver",
				BaseURL: "http://llmproxy:8085/v1",
				EnvKey:  "CODEX_API_KEY",
				WireAPI: "responses",
			},
		},
		AgentServer: AgentServerMCP{
			CodexBin:              "/usr/local/bin/codex-app-gateway",
			WorkspaceID:           "ws_a",
			ExecGatewayURL:        "wss://exec-gw.example/bridge",
			AppGatewayInternalURL: "http://127.0.0.1:8086",
			WorkspaceToken:        "wstok",
			LoopbackToken:         "lbtok",
		},
		ProjectTrustedPaths: []string{"/tmp"},
	}
	out, err := RenderConfigTOML(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		`model_provider = "modelserver"`,
		`shell_tool = false`,
		`unified_exec = false`,
		`apply_patch_freeform = false`,
		`[mcp_servers.agentserver]`,
		`"--workspace-id"`, `"ws_a"`,
		`"--exec-gateway-url"`, `"wss://exec-gw.example/bridge"`,
		`"--app-gateway-internal"`, `"http://127.0.0.1:8086"`,
		`"--workspace-token-env"`, `"CXG_WORKSPACE_TOKEN"`,
		`"--loopback-token-env"`, `"CXG_LOOPBACK_TOKEN"`,
		`CXG_WORKSPACE_TOKEN = "wstok"`,
		`CXG_LOOPBACK_TOKEN = "lbtok"`,
		`[projects."/tmp"]`,
		`trust_level = "trusted"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderConfigTOML_HTTPRelayEnabledEmitsFlagAndEnv(t *testing.T) {
	cfg := ConfigInput{
		ModelProvider: "modelserver",
		Model:         "gpt-5.5",
		AgentServer: AgentServerMCP{
			CodexBin:                  "/usr/local/bin/codex-app-gateway",
			WorkspaceID:               "ws_a",
			ExecGatewayURL:            "wss://exec-gw.example/bridge",
			AppGatewayInternalURL:     "http://127.0.0.1:8086",
			WorkspaceToken:            "wstok",
			LoopbackToken:             "lbtok",
			ExecGatewayInternalURL:    "http://codex-exec-gateway:6060",
			ExecGatewayInternalSecret: "shh-its-a-secret",
		},
	}
	out, err := RenderConfigTOML(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		`"--exec-gateway-internal-url"`,
		`"http://codex-exec-gateway:6060"`,
		`"--exec-gateway-internal-secret-env"`,
		`"CXG_EXEC_GATEWAY_INTERNAL_SECRET"`,
		`CXG_EXEC_GATEWAY_INTERNAL_SECRET = "shh-its-a-secret"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderConfigTOML_HTTPRelayDisabledOmitsFlagAndEnv(t *testing.T) {
	cfg := ConfigInput{
		ModelProvider: "modelserver",
		Model:         "gpt-5.5",
		AgentServer: AgentServerMCP{
			CodexBin:              "/usr/local/bin/codex-app-gateway",
			WorkspaceID:           "ws_a",
			ExecGatewayURL:        "wss://exec-gw.example/bridge",
			AppGatewayInternalURL: "http://127.0.0.1:8086",
			WorkspaceToken:        "wstok",
			LoopbackToken:         "lbtok",
			// ExecGatewayInternalURL + Secret deliberately empty
		},
	}
	out, err := RenderConfigTOML(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, banned := range []string{
		`--exec-gateway-internal-url`,
		`--exec-gateway-internal-secret-env`,
		`CXG_EXEC_GATEWAY_INTERNAL_SECRET`,
	} {
		if strings.Contains(out, banned) {
			t.Errorf("unexpected %q in disabled-relay config:\n%s", banned, out)
		}
	}
}

func TestRenderConfigTOML_RejectsActiveProviderNotInMap(t *testing.T) {
	cfg := ConfigInput{
		ModelProvider: "missing",
		Model:         "m",
		ModelProviders: map[string]ModelProvider{
			"other": {Name: "other", BaseURL: "http://x", EnvKey: "K", WireAPI: "responses"},
		},
	}
	_, err := RenderConfigTOML(cfg)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("want error naming missing provider, got %v", err)
	}
}

func TestWriteConfigEmitsDefaultToolsApprovalMode(t *testing.T) {
	input := ConfigInput{
		ModelProvider: "modelserver",
		Model:         "gpt-5.5",
		AgentServer: AgentServerMCP{
			CodexBin:              "/usr/local/bin/codex-app-gateway",
			WorkspaceID:           "ws-test",
			ExecGatewayURL:        "wss://exec-gw.example/bridge",
			AppGatewayInternalURL: "http://127.0.0.1:8086",
			WorkspaceToken:        "wstok",
			LoopbackToken:         "lbtok",
		},
	}
	out, err := RenderConfigTOML(input)
	if err != nil {
		t.Fatalf("RenderConfigTOML: %v", err)
	}
	if !strings.Contains(out, `default_tools_approval_mode = "approve"`) {
		t.Errorf("missing default_tools_approval_mode in agentserver MCP block:\n%s", out)
	}
}

func TestManager_WriteConfig_ProducesUsableTOML(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	d, _ := m.NewTmpDir("ws_a")
	cfg := ConfigInput{
		ModelProvider: "p",
		Model:         "m",
		ModelProviders: map[string]ModelProvider{
			"p": {Name: "p", BaseURL: "http://x", EnvKey: "K", WireAPI: "responses"},
		},
	}
	if err := m.WriteConfig(d, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(d, "config.toml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), `model = "m"`) {
		t.Errorf("config missing model: %s", b)
	}
}
