package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config holds the local agent's persistent configuration.
type Config struct {
	Server      string `json:"server"`
	SandboxID   string `json:"sandboxId"`
	TunnelToken string `json:"tunnelToken"`
	WorkspaceID string `json:"workspaceId"`
	Name        string `json:"name"`
}

// DefaultConfigPath returns the default path for the agent config file.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".agentserver", "agent.json")
}

// LoadConfig reads the agent config from disk.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// SaveConfig writes the agent config to disk.
func SaveConfig(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
