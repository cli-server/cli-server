# Multi-Agent Registry Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Allow multiple local agents on the same machine by using `(directory, workspace_id)` as the unique key, with credentials stored in a central registry file.

**Architecture:** Replace the single `~/.agentserver/agent.json` config file with a `~/.agentserver/registry.json` that holds an array of agent entries, each keyed by `(dir, workspace_id)`. The `connect` command uses `os.Getwd()` to find matching entries. Add `list` and `remove` subcommands. Migrate legacy single-file config on first load.

**Tech Stack:** Go, cobra CLI, JSON file I/O. Server-side: zero changes.

---

### Task 1: Registry data model and CRUD operations

**Files:**
- Modify: `internal/agent/config.go` (rewrite)
- Create: `internal/agent/config_test.go`

This task replaces the single-config functions with a registry that stores multiple agent entries keyed by `(dir, workspace_id)`.

**Step 1: Write failing tests for registry operations**

Create `internal/agent/config_test.go`:

```go
package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	reg, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Entries) != 0 {
		t.Fatalf("expected empty registry, got %d entries", len(reg.Entries))
	}

	entry := &RegistryEntry{
		Dir:          "/home/alice/project-a",
		Server:       "https://example.com",
		SandboxID:    "sb-1",
		TunnelToken:  "tok-1",
		WorkspaceID:  "ws-1",
		Name:         "project-a",
		OpencodePort: 4096,
	}
	reg.Put(entry)
	if err := SaveRegistry(path, reg); err != nil {
		t.Fatal(err)
	}

	reg2, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg2.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(reg2.Entries))
	}
	got := reg2.Entries[0]
	if got.Dir != entry.Dir || got.SandboxID != entry.SandboxID || got.WorkspaceID != entry.WorkspaceID {
		t.Fatalf("entry mismatch: %+v", got)
	}
}

func TestRegistryLookup(t *testing.T) {
	reg := &Registry{}
	reg.Put(&RegistryEntry{Dir: "/a", WorkspaceID: "ws-1", SandboxID: "sb-1", OpencodePort: 4096})
	reg.Put(&RegistryEntry{Dir: "/a", WorkspaceID: "ws-2", SandboxID: "sb-2", OpencodePort: 4097})
	reg.Put(&RegistryEntry{Dir: "/b", WorkspaceID: "ws-1", SandboxID: "sb-3", OpencodePort: 4098})

	// Lookup by dir returns all entries for that dir.
	entries := reg.FindByDir("/a")
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries for /a, got %d", len(entries))
	}

	// Lookup by (dir, workspace) returns exactly one.
	e := reg.Find("/a", "ws-1")
	if e == nil || e.SandboxID != "sb-1" {
		t.Fatalf("expected sb-1, got %+v", e)
	}

	// Lookup miss.
	if reg.Find("/a", "ws-999") != nil {
		t.Fatal("expected nil for missing workspace")
	}
}

func TestRegistryPutOverwrite(t *testing.T) {
	reg := &Registry{}
	reg.Put(&RegistryEntry{Dir: "/a", WorkspaceID: "ws-1", SandboxID: "old", OpencodePort: 4096})
	reg.Put(&RegistryEntry{Dir: "/a", WorkspaceID: "ws-1", SandboxID: "new", OpencodePort: 4096})

	if len(reg.Entries) != 1 {
		t.Fatalf("expected 1 entry after overwrite, got %d", len(reg.Entries))
	}
	if reg.Entries[0].SandboxID != "new" {
		t.Fatalf("expected overwritten entry, got %s", reg.Entries[0].SandboxID)
	}
}

func TestRegistryRemove(t *testing.T) {
	reg := &Registry{}
	reg.Put(&RegistryEntry{Dir: "/a", WorkspaceID: "ws-1", SandboxID: "sb-1", OpencodePort: 4096})
	reg.Put(&RegistryEntry{Dir: "/a", WorkspaceID: "ws-2", SandboxID: "sb-2", OpencodePort: 4097})

	removed := reg.Remove("/a", "ws-1")
	if !removed {
		t.Fatal("expected Remove to return true")
	}
	if len(reg.Entries) != 1 {
		t.Fatalf("expected 1 entry after remove, got %d", len(reg.Entries))
	}
	if reg.Entries[0].WorkspaceID != "ws-2" {
		t.Fatal("wrong entry remained")
	}

	if reg.Remove("/a", "ws-1") {
		t.Fatal("expected Remove to return false for missing entry")
	}
}

func TestRegistryNextPort(t *testing.T) {
	reg := &Registry{}
	if port := reg.NextPort(); port != 4096 {
		t.Fatalf("expected 4096 for empty registry, got %d", port)
	}

	reg.Put(&RegistryEntry{Dir: "/a", WorkspaceID: "ws-1", OpencodePort: 4096})
	if port := reg.NextPort(); port != 4097 {
		t.Fatalf("expected 4097, got %d", port)
	}

	reg.Put(&RegistryEntry{Dir: "/b", WorkspaceID: "ws-1", OpencodePort: 4099})
	if port := reg.NextPort(); port != 4100 {
		t.Fatalf("expected 4100, got %d", port)
	}
}

func TestMigrateLegacyConfig(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "agent.json")
	registryPath := filepath.Join(dir, "registry.json")

	// Write a legacy config file.
	legacy := &Config{
		Server:      "https://example.com",
		SandboxID:   "sb-legacy",
		TunnelToken: "tok-legacy",
		WorkspaceID: "ws-legacy",
		Name:        "legacy-agent",
	}
	if err := SaveConfig(legacyPath, legacy); err != nil {
		t.Fatal(err)
	}

	// Migrate with a specific cwd.
	reg, err := MaybeMigrateLegacy(legacyPath, registryPath, "/home/alice/old-project")
	if err != nil {
		t.Fatal(err)
	}
	if reg == nil {
		t.Fatal("expected non-nil registry after migration")
	}
	if len(reg.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(reg.Entries))
	}
	e := reg.Entries[0]
	if e.Dir != "/home/alice/old-project" || e.SandboxID != "sb-legacy" || e.WorkspaceID != "ws-legacy" {
		t.Fatalf("migration mismatch: %+v", e)
	}

	// Verify registry was persisted.
	reg2, err := LoadRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg2.Entries) != 1 {
		t.Fatal("persisted registry should have 1 entry")
	}

	// Second call should be a no-op (registry already exists).
	reg3, err := MaybeMigrateLegacy(legacyPath, registryPath, "/different/dir")
	if err != nil {
		t.Fatal(err)
	}
	if reg3 != nil {
		t.Fatal("expected nil when registry already exists")
	}
}
```

**Step 2: Run tests to verify they fail**

Run: `cd /root/agentserver && go test ./internal/agent/ -run 'TestRegistry|TestMigrate' -v`
Expected: Compilation errors — `Registry`, `RegistryEntry`, `LoadRegistry`, `SaveRegistry`, `MaybeMigrateLegacy` not defined.

**Step 3: Implement registry in config.go**

Rewrite `internal/agent/config.go`:

```go
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const basePort = 4096

// RegistryEntry holds credentials for one local agent instance,
// uniquely identified by (Dir, WorkspaceID).
type RegistryEntry struct {
	Dir          string `json:"dir"`
	Server       string `json:"server"`
	SandboxID    string `json:"sandbox_id"`
	TunnelToken  string `json:"tunnel_token"`
	WorkspaceID  string `json:"workspace_id"`
	Name         string `json:"name"`
	OpencodePort int    `json:"opencode_port"`
}

// Registry is the collection of all registered agent entries on this machine.
type Registry struct {
	Entries []*RegistryEntry `json:"entries"`
}

// Find returns the entry matching (dir, workspaceID), or nil.
func (r *Registry) Find(dir, workspaceID string) *RegistryEntry {
	for _, e := range r.Entries {
		if e.Dir == dir && e.WorkspaceID == workspaceID {
			return e
		}
	}
	return nil
}

// FindByDir returns all entries for the given directory.
func (r *Registry) FindByDir(dir string) []*RegistryEntry {
	var result []*RegistryEntry
	for _, e := range r.Entries {
		if e.Dir == dir {
			result = append(result, e)
		}
	}
	return result
}

// Put inserts or overwrites an entry keyed by (Dir, WorkspaceID).
func (r *Registry) Put(entry *RegistryEntry) {
	for i, e := range r.Entries {
		if e.Dir == entry.Dir && e.WorkspaceID == entry.WorkspaceID {
			r.Entries[i] = entry
			return
		}
	}
	r.Entries = append(r.Entries, entry)
}

// Remove deletes the entry matching (dir, workspaceID). Returns true if found.
func (r *Registry) Remove(dir, workspaceID string) bool {
	for i, e := range r.Entries {
		if e.Dir == dir && e.WorkspaceID == workspaceID {
			r.Entries = append(r.Entries[:i], r.Entries[i+1:]...)
			return true
		}
	}
	return false
}

// NextPort returns the next available opencode port (max used + 1, or basePort).
func (r *Registry) NextPort() int {
	max := basePort - 1
	for _, e := range r.Entries {
		if e.OpencodePort > max {
			max = e.OpencodePort
		}
	}
	return max + 1
}

// DefaultRegistryDir returns ~/.agentserver.
func DefaultRegistryDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".agentserver")
}

// DefaultRegistryPath returns ~/.agentserver/registry.json.
func DefaultRegistryPath() string {
	return filepath.Join(DefaultRegistryDir(), "registry.json")
}

// LoadRegistry reads the registry from disk. Returns an empty registry if the file doesn't exist.
func LoadRegistry(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Registry{}, nil
		}
		return nil, fmt.Errorf("read registry: %w", err)
	}
	var reg Registry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parse registry: %w", err)
	}
	return &reg, nil
}

// SaveRegistry writes the registry to disk.
func SaveRegistry(path string, reg *Registry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create registry dir: %w", err)
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal registry: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write registry: %w", err)
	}
	return nil
}

// --- Legacy single-file config (kept for migration) ---

// Config is the legacy single-agent config format.
type Config struct {
	Server      string `json:"server"`
	SandboxID   string `json:"sandbox_id"`
	TunnelToken string `json:"tunnel_token"`
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
}

// DefaultConfigPath returns the legacy config path ~/.agentserver/agent.json.
func DefaultConfigPath() string {
	return filepath.Join(DefaultRegistryDir(), "agent.json")
}

// LoadConfig reads the legacy agent config from disk.
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

// SaveConfig writes the legacy agent config to disk.
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

// MaybeMigrateLegacy checks for a legacy agent.json and migrates it into a new
// registry.json if the registry doesn't already exist. Returns the migrated
// registry, or nil if no migration was needed.
func MaybeMigrateLegacy(legacyPath, registryPath, cwd string) (*Registry, error) {
	// Skip if registry already exists.
	if _, err := os.Stat(registryPath); err == nil {
		return nil, nil
	}

	cfg, err := LoadConfig(legacyPath)
	if err != nil || cfg == nil || cfg.SandboxID == "" {
		return nil, err
	}

	reg := &Registry{}
	reg.Put(&RegistryEntry{
		Dir:          cwd,
		Server:       cfg.Server,
		SandboxID:    cfg.SandboxID,
		TunnelToken:  cfg.TunnelToken,
		WorkspaceID:  cfg.WorkspaceID,
		Name:         cfg.Name,
		OpencodePort: basePort,
	})

	if err := SaveRegistry(registryPath, reg); err != nil {
		return nil, fmt.Errorf("save migrated registry: %w", err)
	}
	return reg, nil
}
```

**Step 4: Run tests to verify they pass**

Run: `cd /root/agentserver && go test ./internal/agent/ -run 'TestRegistry|TestMigrate' -v`
Expected: All PASS.

**Step 5: Commit**

```bash
git add internal/agent/config.go internal/agent/config_test.go
git commit -m "feat(agent): add multi-agent registry keyed by (dir, workspace_id)

Replace single agent.json with registry.json that supports multiple
agent entries. Each entry is uniquely identified by (directory, workspace_id).
Includes legacy config migration."
```

---

### Task 2: Update connect workflow to use registry

**Files:**
- Modify: `internal/agent/connect.go`
- Modify: `internal/agent/client.go` (only the `Register` function return type)

This task rewrites `RunConnect` to: get cwd, load registry, decide reconnect vs register, auto-assign ports, save back to registry.

**Step 1: Update `Register` to return `RegistryEntry`**

In `internal/agent/client.go`, change the `Register` function to return a `*RegistryEntry` instead of `*Config`:

```go
// Register registers a new local agent with the server using a one-time code.
func Register(serverURL, code, name string) (*RegistryEntry, error) {
	body := fmt.Sprintf(`{"code":%q,"name":%q}`, code, name)
	resp, err := http.Post(
		serverURL+"/api/agent/register",
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("register request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registration failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		SandboxID   string `json:"sandbox_id"`
		TunnelToken string `json:"tunnel_token"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &RegistryEntry{
		Server:      serverURL,
		SandboxID:   result.SandboxID,
		TunnelToken: result.TunnelToken,
		WorkspaceID: result.WorkspaceID,
		Name:        name,
	}, nil
}
```

**Step 2: Rewrite ConnectOptions and RunConnect**

Replace `internal/agent/connect.go` entirely:

```go
package agent

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// ConnectOptions holds all flags for the connect command.
type ConnectOptions struct {
	Server        string
	Code          string
	Name          string
	WorkspaceID   string // optional: disambiguate when dir has multiple workspaces
	OpencodeURL   string
	OpencodeURLSet bool
	OpencodeToken string
	AutoStart     bool
	OpencodeBin   string
	OpencodePort  int  // 0 = auto-assign from registry
	OpencodePortSet bool
}

// RunConnect executes the agent connect workflow using the registry.
func RunConnect(opts ConnectOptions) {
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get working directory: %v", err)
	}

	registryPath := DefaultRegistryPath()

	// Attempt legacy migration.
	if migrated, err := MaybeMigrateLegacy(DefaultConfigPath(), registryPath, cwd); err != nil {
		log.Printf("Warning: legacy migration failed: %v", err)
	} else if migrated != nil {
		log.Printf("Migrated legacy config to registry (%d entries)", len(migrated.Entries))
	}

	reg, err := LoadRegistry(registryPath)
	if err != nil {
		log.Fatalf("Failed to load registry: %v", err)
	}

	var entry *RegistryEntry

	if opts.Code != "" {
		// --- New registration ---
		if opts.Server == "" {
			log.Fatal("--server is required for registration")
		}
		if opts.Name == "" {
			hostname, _ := os.Hostname()
			if hostname != "" {
				opts.Name = hostname
			} else {
				opts.Name = "Local Agent"
			}
		}

		log.Printf("Registering with server %s...", opts.Server)
		entry, err = Register(opts.Server, opts.Code, opts.Name)
		if err != nil {
			log.Fatalf("Registration failed: %v", err)
		}
		log.Printf("Registered successfully (sandbox: %s, workspace: %s)", entry.SandboxID, entry.WorkspaceID)

		entry.Dir = cwd

		// Assign port: use explicit flag, or auto-assign.
		if opts.OpencodePortSet {
			entry.OpencodePort = opts.OpencodePort
		} else {
			entry.OpencodePort = reg.NextPort()
		}

		// Check if (dir, workspace) already exists — overwrite with warning.
		if existing := reg.Find(cwd, entry.WorkspaceID); existing != nil {
			log.Printf("Overwriting existing registration for this directory + workspace (old sandbox: %s)", existing.SandboxID)
		}

		reg.Put(entry)
		if err := SaveRegistry(registryPath, reg); err != nil {
			log.Printf("Warning: failed to save registry: %v", err)
		} else {
			log.Printf("Registry saved to %s", registryPath)
		}
	} else {
		// --- Reconnect using saved credentials ---
		entries := reg.FindByDir(cwd)
		switch len(entries) {
		case 0:
			log.Fatal("No agent registered for this directory. Use --code to register first.")
		case 1:
			entry = entries[0]
		default:
			// Multiple workspaces — need --workspace to disambiguate.
			if opts.WorkspaceID == "" {
				log.Println("Multiple agents registered for this directory:")
				for _, e := range entries {
					log.Printf("  workspace: %s  name: %s  sandbox: %s", e.WorkspaceID, e.Name, e.SandboxID)
				}
				log.Fatal("Use --workspace to specify which one to connect.")
			}
			entry = reg.Find(cwd, opts.WorkspaceID)
			if entry == nil {
				log.Fatalf("No agent registered for workspace %s in this directory.", opts.WorkspaceID)
			}
		}

		log.Printf("Using saved credentials (sandbox: %s, workspace: %s)", entry.SandboxID, entry.WorkspaceID)
		if opts.Server != "" {
			entry.Server = opts.Server
		}
	}

	// Determine opencode port.
	opencodePort := entry.OpencodePort
	if opts.OpencodePortSet {
		opencodePort = opts.OpencodePort
	}

	// Auto-start opencode if requested.
	var opencodeProc *OpencodeProcess
	if opts.AutoStart {
		opencodeURL := fmt.Sprintf("http://localhost:%d", opencodePort)

		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(opencodeURL + "/")
		if err == nil {
			resp.Body.Close()
			log.Printf("opencode already running on port %d, skipping auto-start", opencodePort)
		} else {
			log.Printf("Starting opencode on port %d...", opencodePort)
			opencodeProc, err = StartOpencode(opts.OpencodeBin, opencodePort, opts.OpencodeToken)
			if err != nil {
				log.Fatalf("Failed to start opencode: %v", err)
			}

			readyCtx, readyCancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := opencodeProc.WaitReady(readyCtx, 30*time.Second); err != nil {
				readyCancel()
				opencodeProc.Stop()
				log.Fatalf("opencode failed to become ready: %v", err)
			}
			readyCancel()
		}

		if !opts.OpencodeURLSet {
			opts.OpencodeURL = opencodeURL
		}
	}

	if opts.OpencodeURL == "" {
		opts.OpencodeURL = fmt.Sprintf("http://localhost:%d", opencodePort)
	}

	tunnelClient := NewClient(entry.Server, entry.SandboxID, entry.TunnelToken, opts.OpencodeURL, opts.OpencodeToken)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		log.Printf("Received %v, disconnecting...", sig)
		cancel()
		if opencodeProc != nil {
			opencodeProc.Stop()
		}
	}()

	log.Printf("Connecting to %s (forwarding to %s)...", entry.Server, opts.OpencodeURL)
	if err := tunnelClient.Run(ctx); err != nil && ctx.Err() == nil {
		if opencodeProc != nil {
			opencodeProc.Stop()
		}
		log.Fatalf("Agent error: %v", err)
	}

	if opencodeProc != nil {
		opencodeProc.Stop()
	}
	log.Println("Agent disconnected.")
}
```

**Step 3: Verify it compiles**

Run: `cd /root/agentserver && go build ./internal/agent/`
Expected: Clean build, no errors.

**Step 4: Commit**

```bash
git add internal/agent/connect.go internal/agent/client.go
git commit -m "feat(agent): rewrite connect workflow to use registry

Connect now resolves credentials by (cwd, workspace_id) from the
registry. Supports auto-reconnect, multi-workspace disambiguation
via --workspace flag, and automatic port assignment."
```

---

### Task 3: Update CLI commands (connect flags, add list and remove)

**Files:**
- Modify: `cmd/agentserver-agent/main.go` (rewrite)

This task updates the cobra CLI: adjusts connect flags, adds `list` and `remove` subcommands.

**Step 1: Rewrite main.go**

```go
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/agentserver/agentserver/internal/agent"
	"github.com/spf13/cobra"
)

var (
	server        string
	code          string
	name          string
	workspaceID   string
	opencodeURL   string
	opencodeToken string
	autoStart     bool
	opencodeBin   string
	opencodePort  int
)

var rootCmd = &cobra.Command{
	Use:   "agentserver",
	Short: "Connect local opencode to agentserver",
	Long:  `Lightweight agent client that connects a local opencode instance to agentserver via a WebSocket tunnel.`,
}

var connectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Connect local opencode to agentserver",
	Long: `Establish a WebSocket tunnel between a local opencode instance and agentserver.

On first run, provide --server and --code to register with the server.
On subsequent runs, the saved credentials for the current directory are used automatically.

Each directory + workspace combination gets its own agent registration.
If the current directory has multiple workspace registrations, use --workspace to select one.

By default, opencode serve is started automatically with an auto-assigned port.
Use --auto-start=false to disable this and manage opencode manually.`,
	Run: func(cmd *cobra.Command, args []string) {
		agent.RunConnect(agent.ConnectOptions{
			Server:          server,
			Code:            code,
			Name:            name,
			WorkspaceID:     workspaceID,
			OpencodeURL:     opencodeURL,
			OpencodeURLSet:  cmd.Flags().Changed("opencode-url"),
			OpencodeToken:   opencodeToken,
			AutoStart:       autoStart,
			OpencodeBin:     opencodeBin,
			OpencodePort:    opencodePort,
			OpencodePortSet: cmd.Flags().Changed("opencode-port"),
		})
	},
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all registered agents",
	Long:  `Show all agent registrations stored in the local registry.`,
	Run: func(cmd *cobra.Command, args []string) {
		reg, err := agent.LoadRegistry(agent.DefaultRegistryPath())
		if err != nil {
			log.Fatalf("Failed to load registry: %v", err)
		}
		if len(reg.Entries) == 0 {
			fmt.Println("No agents registered.")
			return
		}
		fmt.Printf("%-40s %-15s %-12s %-5s %s\n", "DIRECTORY", "NAME", "WORKSPACE", "PORT", "SANDBOX")
		for _, e := range reg.Entries {
			dir := e.Dir
			if len(dir) > 40 {
				dir = "..." + dir[len(dir)-37:]
			}
			wsID := e.WorkspaceID
			if len(wsID) > 12 {
				wsID = wsID[:12]
			}
			fmt.Printf("%-40s %-15s %-12s %-5d %s\n", dir, e.Name, wsID, e.OpencodePort, e.SandboxID)
		}
	},
}

var (
	removeWorkspace string
	removeDir       string
)

var removeCmd = &cobra.Command{
	Use:   "remove",
	Short: "Remove an agent registration",
	Long: `Remove an agent registration from the local registry.

By default, removes the registration for the current directory.
Use --dir to specify a different directory.
Use --workspace to specify a workspace when the directory has multiple registrations.`,
	Run: func(cmd *cobra.Command, args []string) {
		dir := removeDir
		if dir == "" {
			var err error
			dir, err = os.Getwd()
			if err != nil {
				log.Fatalf("Failed to get working directory: %v", err)
			}
		}

		registryPath := agent.DefaultRegistryPath()
		reg, err := agent.LoadRegistry(registryPath)
		if err != nil {
			log.Fatalf("Failed to load registry: %v", err)
		}

		wsID := removeWorkspace
		if wsID == "" {
			entries := reg.FindByDir(dir)
			switch len(entries) {
			case 0:
				log.Fatal("No agent registered for this directory.")
			case 1:
				wsID = entries[0].WorkspaceID
			default:
				log.Println("Multiple agents registered for this directory:")
				for _, e := range entries {
					log.Printf("  workspace: %s  name: %s", e.WorkspaceID, e.Name)
				}
				log.Fatal("Use --workspace to specify which one to remove.")
			}
		}

		if !reg.Remove(dir, wsID) {
			log.Fatal("No matching registration found.")
		}

		if err := agent.SaveRegistry(registryPath, reg); err != nil {
			log.Fatalf("Failed to save registry: %v", err)
		}
		fmt.Printf("Removed agent registration for %s (workspace: %s)\n", dir, wsID)
	},
}

func init() {
	rootCmd.AddCommand(connectCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(removeCmd)

	connectCmd.Flags().StringVar(&server, "server", "", "Agent server URL (e.g., https://cli.example.com)")
	connectCmd.Flags().StringVar(&code, "code", "", "One-time registration code from Web UI")
	connectCmd.Flags().StringVar(&name, "name", "", "Name for this agent (default: hostname)")
	connectCmd.Flags().StringVar(&workspaceID, "workspace", "", "Workspace ID (needed when dir has multiple registrations)")
	connectCmd.Flags().StringVar(&opencodeURL, "opencode-url", "", "Local opencode server URL (default: auto)")
	connectCmd.Flags().StringVar(&opencodeToken, "opencode-token", "", "Local opencode server token")
	connectCmd.Flags().BoolVar(&autoStart, "auto-start", true, "Automatically start opencode serve")
	connectCmd.Flags().StringVar(&opencodeBin, "opencode-bin", "opencode", "Path to the opencode binary")
	connectCmd.Flags().IntVar(&opencodePort, "opencode-port", 4096, "Port to start opencode on (default: auto-assigned)")

	removeCmd.Flags().StringVar(&removeWorkspace, "workspace", "", "Workspace ID to remove")
	removeCmd.Flags().StringVar(&removeDir, "dir", "", "Directory to remove (default: current directory)")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
```

**Step 2: Verify it compiles and runs**

Run: `cd /root/agentserver && go build ./cmd/agentserver-agent/`
Expected: Clean build.

Run: `cd /root/agentserver && go run ./cmd/agentserver-agent/ list`
Expected: "No agents registered." (or migrated entries if legacy config exists).

Run: `cd /root/agentserver && go run ./cmd/agentserver-agent/ remove --help`
Expected: Help text with --workspace and --dir flags.

**Step 3: Commit**

```bash
git add cmd/agentserver-agent/main.go
git commit -m "feat(agent): add list and remove commands, update connect flags

- connect: add --workspace flag, remove --config flag, auto-assign ports
- list: show all registered agents in a table
- remove: remove a registration by dir + workspace"
```

---

### Task 4: Clean up — remove dead code and verify end-to-end

**Files:**
- Modify: `internal/agent/config.go` (remove legacy helpers if no longer needed by tests)

**Step 1: Run all agent tests**

Run: `cd /root/agentserver && go test ./internal/agent/ -v`
Expected: All PASS.

**Step 2: Run full project build**

Run: `cd /root/agentserver && go build ./...`
Expected: Clean build, no errors.

**Step 3: Verify CLI commands**

Run: `cd /root/agentserver && go run ./cmd/agentserver-agent/ --help`
Expected: Shows `connect`, `list`, `remove` subcommands.

Run: `cd /root/agentserver && go run ./cmd/agentserver-agent/ connect --help`
Expected: Shows --server, --code, --name, --workspace, --opencode-url, --opencode-token, --auto-start, --opencode-bin, --opencode-port flags. No --config flag.

**Step 4: Commit (if any cleanup was needed)**

```bash
git add -A
git commit -m "chore(agent): clean up after multi-agent registry migration"
```
