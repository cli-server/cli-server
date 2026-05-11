// Package codexhome owns per-thread CODEX_HOME tmpdirs: creation,
// destruction, and the rendering of the config.toml fragment we plant
// inside each one before spawning `codex app-server`.
package codexhome

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type ModelProvider struct {
	Name    string
	BaseURL string
	EnvKey  string
	WireAPI string
}

type ExecutorEntry struct {
	ID        string
	BridgeURL string
	TokenEnv  string
	TokenVal  string // injected via env, not written to TOML
	Desc      string
	CodexBin  string // path to codex-app-gateway binary (for `env-mcp` subcommand)
	TurnID    string
}

type ConfigInput struct {
	ModelProvider       string
	Model               string
	ModelProviders      map[string]ModelProvider
	Executors           []ExecutorEntry
	ProjectTrustedPaths []string
}

// Manager creates per-thread CODEX_HOME tmpdirs under root.
type Manager struct{ root string }

func NewManager(root string) *Manager { return &Manager{root: root} }

// NewTmpDir creates `<root>/<workspaceID>/<threadID>/` with mode 0700.
// Idempotent: returns the existing path if already present.
func (m *Manager) NewTmpDir(workspaceID, threadID string) (string, error) {
	if workspaceID == "" || threadID == "" {
		return "", fmt.Errorf("codexhome: empty workspace or thread id")
	}
	d := filepath.Join(m.root, workspaceID, threadID)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", d, err)
	}
	if err := os.Chmod(d, 0o700); err != nil {
		return "", fmt.Errorf("chmod %s: %w", d, err)
	}
	return d, nil
}

// RemoveTmpDir removes a previously-created tmpdir tree, but refuses
// to remove anything outside the manager's root.
func (m *Manager) RemoveTmpDir(path string) error {
	rel, err := filepath.Rel(m.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("codexhome: refusing to remove %s outside root %s", path, m.root)
	}
	return os.RemoveAll(path)
}

// WriteConfig renders `config.toml` into the given CODEX_HOME dir.
func (m *Manager) WriteConfig(codexHome string, cfg ConfigInput) error {
	out, err := RenderConfigTOML(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(out), 0o600)
}

// RenderConfigTOML produces the TOML body. Pure function so tests can
// assert exact substrings without filesystem.
func RenderConfigTOML(cfg ConfigInput) (string, error) {
	if cfg.ModelProvider == "" {
		return "", fmt.Errorf("codexhome: ModelProvider required")
	}
	if cfg.Model == "" {
		return "", fmt.Errorf("codexhome: Model required")
	}
	if len(cfg.ModelProviders) > 0 {
		if _, ok := cfg.ModelProviders[cfg.ModelProvider]; !ok {
			return "", fmt.Errorf("codexhome: ModelProvider %q not in ModelProviders", cfg.ModelProvider)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "model_provider = %q\n", cfg.ModelProvider)
	fmt.Fprintf(&b, "model = %q\n\n", cfg.Model)

	for name, p := range cfg.ModelProviders {
		fmt.Fprintf(&b, "[model_providers.%s]\n", tomlKey(name))
		fmt.Fprintf(&b, "name = %q\n", p.Name)
		fmt.Fprintf(&b, "base_url = %q\n", p.BaseURL)
		fmt.Fprintf(&b, "env_key = %q\n", p.EnvKey)
		fmt.Fprintf(&b, "wire_api = %q\n\n", p.WireAPI)
	}

	for _, p := range cfg.ProjectTrustedPaths {
		fmt.Fprintf(&b, "[projects.%q]\n", p)
		b.WriteString("trust_level = \"trusted\"\n\n")
	}

	// Disable codex's builtin local-execution paths so the only way the
	// LLM can reach a shell is through the env-mcp children below.
	b.WriteString("[features]\n")
	b.WriteString("shell_tool = false\n")
	b.WriteString("unified_exec = false\n")
	b.WriteString("apply_patch_freeform = false\n\n")

	for _, e := range cfg.Executors {
		fmt.Fprintf(&b, "[mcp_servers.%s]\n", tomlKey(e.ID))
		fmt.Fprintf(&b, "command = %q\n", e.CodexBin)
		args := []string{
			"env-mcp",
			"--exe-id", e.ID,
			"--bridge-url", e.BridgeURL,
			"--token-env", e.TokenEnv,
			"--exe-desc", e.Desc,
		}
		if e.TurnID != "" {
			args = append(args, "--turn-id", e.TurnID)
		}
		b.WriteString("args = [")
		for i, a := range args {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%q", a)
		}
		b.WriteString("]\n")
		fmt.Fprintf(&b, "env = { %s = %q }\n\n", e.TokenEnv, e.TokenVal)
	}
	return b.String(), nil
}

// tomlKey leaves bare keys for safe identifiers, otherwise quotes.
func tomlKey(s string) string {
	for _, r := range s {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return fmt.Sprintf("%q", s)
		}
	}
	return s
}
