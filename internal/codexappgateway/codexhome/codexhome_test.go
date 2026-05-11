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
	d, err := m.NewTmpDir("ws_a", "thr_1")
	if err != nil {
		t.Fatalf("NewTmpDir: %v", err)
	}
	if !strings.HasPrefix(d, filepath.Join(root, "ws_a", "thr_1")) {
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
	d, err := m.NewTmpDir("ws_a", "thr_1")
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

func TestRenderConfigTOML_DisablesBuiltinShellAndRegistersMCPServers(t *testing.T) {
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
		Executors: []ExecutorEntry{
			{
				ID:        "exe_alpha",
				BridgeURL: "ws://exec-gw:6060/bridge/exe_alpha",
				TokenEnv:  "CXG_BRIDGE_TOKEN_EXE_ALPHA",
				TokenVal:  "tok-alpha",
				Desc:      "Daisy's MacBook",
				CodexBin:  "/usr/local/bin/codex-app-gateway",
				TurnID:    "trn_xxx",
			},
			{
				ID:        "exe_beta",
				BridgeURL: "ws://exec-gw:6060/bridge/exe_beta",
				TokenEnv:  "CXG_BRIDGE_TOKEN_EXE_BETA",
				TokenVal:  "tok-beta",
				Desc:      "EC2 us-east-1",
				CodexBin:  "/usr/local/bin/codex-app-gateway",
				TurnID:    "trn_xxx",
			},
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
		`[mcp_servers.exe_alpha]`,
		`"--exe-id"`, `"exe_alpha"`,
		`"--bridge-url"`, `"ws://exec-gw:6060/bridge/exe_alpha"`,
		`"--token-env"`, `"CXG_BRIDGE_TOKEN_EXE_ALPHA"`,
		`[mcp_servers.exe_beta]`,
		`[projects."/tmp"]`,
		`trust_level = "trusted"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
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

func TestManager_WriteConfig_ProducesUsableTOML(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	d, _ := m.NewTmpDir("ws_a", "thr_1")
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
