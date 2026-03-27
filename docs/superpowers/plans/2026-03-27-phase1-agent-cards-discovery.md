# Phase 1: Agent Cards & Discovery — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enable agents in the same workspace to register their capabilities and discover each other via a server-centric registry.

**Architecture:** Add an `agent_cards` table to PostgreSQL, a new `agentAuthMiddleware` for proxy_token-based auth on `/api/agent/discovery/` routes, and HTTP handlers for card registration and discovery. Extend the existing tunnel `OnAgentInfo` callback to carry agent status. A background `AgentHealthMonitor` goroutine marks agents offline when heartbeats lapse.

**Tech Stack:** Go 1.26, chi/v5 router, PostgreSQL (raw SQL via embedded `*sql.DB`), nhooyr.io/websocket tunnel.

---

### File Structure

| Action | File | Responsibility |
|--------|------|----------------|
| Create | `internal/db/migrations/011_agent_cards.sql` | Schema for `agent_cards` table |
| Create | `internal/db/agent_cards.go` | Agent Card types, DB CRUD, default card generation |
| Create | `internal/server/agent_auth.go` | `agentAuthMiddleware` (proxy_token → sandbox context) |
| Create | `internal/server/agent_discovery.go` | Response types, capability matching, HTTP handlers |
| Create | `internal/server/agent_discovery_test.go` | Unit tests for capability matching and default card generation |
| Create | `internal/server/agent_health.go` | `AgentHealthMonitor` background goroutine |
| Modify | `internal/server/server.go` | Wire `/api/agent/discovery/` routes |
| Modify | `internal/sandboxproxy/tunnel.go` | Handle agent_status in heartbeat; auto-register card on connect |
| Modify | `cmd/serve.go` | Start/stop `AgentHealthMonitor` |

---

### Task 1: Database migration for agent_cards

**Files:**
- Create: `internal/db/migrations/011_agent_cards.sql`

- [ ] **Step 1: Write the migration SQL**

Create `internal/db/migrations/011_agent_cards.sql`:

```sql
-- Agent capability cards for multi-agent discovery.
-- agent_type and agent_status are indexed columns outside card_json
-- for efficient filtering. card_json stores skills, tools, and config.
CREATE TABLE agent_cards (
    sandbox_id   TEXT PRIMARY KEY REFERENCES sandboxes(id) ON DELETE CASCADE,
    agent_type   TEXT NOT NULL,
    agent_status TEXT NOT NULL DEFAULT 'available',
    card_json    TEXT NOT NULL,
    version      INTEGER NOT NULL DEFAULT 1,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_agent_cards_type ON agent_cards(agent_type);
CREATE INDEX idx_agent_cards_status ON agent_cards(agent_status);
```

- [ ] **Step 2: Verify migration applies**

Run: `cd /root/agentserver && go build ./... && go run ./cmd/agentserver serve --db-url "$DATABASE_URL" &`
Wait 3 seconds, then check:
Run: `psql "$DATABASE_URL" -c "\d agent_cards"`
Expected: Table with columns `sandbox_id`, `agent_type`, `agent_status`, `card_json`, `version`, `updated_at` and two indexes.
Stop the server.

- [ ] **Step 3: Commit**

```bash
git add internal/db/migrations/011_agent_cards.sql
git commit -m "feat(db): add agent_cards table for multi-agent discovery

Stores per-sandbox capability cards with indexed type and status
columns for efficient filtering. Card JSON holds skills, tools,
and configuration."
```

---

### Task 2: Agent Card Go types, DB methods, and default card generation

**Files:**
- Create: `internal/db/agent_cards.go`

`DefaultCardForType` lives here (in `db` package) so both `internal/server` and `internal/sandboxproxy` can import it without circular dependencies — both already import `internal/db`.

- [ ] **Step 1: Write Agent Card types, default card generation, and DB methods**

Create `internal/db/agent_cards.go`:

```go
package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Skill describes a high-level capability an agent can perform.
type Skill struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	InputTypes  []string `json:"input_types,omitempty"`
	OutputTypes []string `json:"output_types,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// MCPTool describes an MCP tool an agent exposes.
type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// AgentCardData is the JSON blob stored in agent_cards.card_json.
// Type and Status are NOT stored here — they live in indexed columns.
type AgentCardData struct {
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	Skills         []Skill   `json:"skills"`
	MCPTools       []MCPTool `json:"mcp_tools,omitempty"`
	SupportedModes []string  `json:"supported_modes"`
	MaxConcurrency int       `json:"max_concurrency"`
}

// AgentCardRow is the raw database row from agent_cards joined with sandbox info.
type AgentCardRow struct {
	SandboxID       string
	AgentType       string
	AgentStatus     string
	CardJSON        string
	Version         int
	UpdatedAt       time.Time
	IsLocal         bool
	LastHeartbeatAt sql.NullTime
}

// DefaultCardForType returns the default AgentCardData for a given agent type.
// Used by both server (card registration) and sandboxproxy (auto-registration on tunnel connect).
func DefaultCardForType(agentType, name string) AgentCardData {
	card := AgentCardData{
		Name:           name,
		SupportedModes: []string{"async"},
		MaxConcurrency: 1,
	}

	switch agentType {
	case "opencode":
		card.Description = "Development agent with code editing, terminal, and search capabilities"
		card.Skills = []Skill{
			{Name: "code-editing", Description: "Read, write, and edit source code files", Tags: []string{"code", "files"}},
			{Name: "terminal", Description: "Execute shell commands", Tags: []string{"bash", "shell"}},
			{Name: "code-search", Description: "Search and navigate codebases", Tags: []string{"grep", "find"}},
		}
	case "openclaw":
		card.Description = "Multi-model text generation and routing agent"
		card.Skills = []Skill{
			{Name: "text-generation", Description: "Generate text using multiple LLM models", Tags: []string{"multi-model"}},
			{Name: "model-routing", Description: "Route requests to appropriate models", Tags: []string{"anthropic", "openai"}},
		}
	case "nanoclaw":
		card.Description = "Autonomous task execution agent"
		card.Skills = []Skill{
			{Name: "autonomous-task", Description: "Execute autonomous multi-step tasks", Tags: []string{"autonomous"}},
			{Name: "knowledge-query", Description: "Query knowledge bases and documents", Tags: []string{"knowledge", "retrieval"}},
		}
	default:
		card.Description = name
	}

	return card
}

// UpsertAgentCard inserts or updates an agent card. Returns the new version number.
func (db *DB) UpsertAgentCard(sandboxID, agentType, cardJSON string) (int, error) {
	var version int
	err := db.QueryRow(`
		INSERT INTO agent_cards (sandbox_id, agent_type, card_json, version, updated_at)
		VALUES ($1, $2, $3, 1, NOW())
		ON CONFLICT (sandbox_id) DO UPDATE SET
			agent_type = EXCLUDED.agent_type,
			card_json = EXCLUDED.card_json,
			version = agent_cards.version + 1,
			updated_at = NOW()
		RETURNING version
	`, sandboxID, agentType, cardJSON).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("upsert agent card: %w", err)
	}
	return version, nil
}

// GetAgentCard returns the card for a specific sandbox, or nil if not found.
func (db *DB) GetAgentCard(sandboxID string) (*AgentCardRow, error) {
	row := &AgentCardRow{}
	err := db.QueryRow(`
		SELECT ac.sandbox_id, ac.agent_type, ac.agent_status, ac.card_json,
		       ac.version, ac.updated_at, s.is_local, s.last_heartbeat_at
		FROM agent_cards ac
		JOIN sandboxes s ON s.id = ac.sandbox_id
		WHERE ac.sandbox_id = $1
	`, sandboxID).Scan(
		&row.SandboxID, &row.AgentType, &row.AgentStatus, &row.CardJSON,
		&row.Version, &row.UpdatedAt, &row.IsLocal, &row.LastHeartbeatAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get agent card: %w", err)
	}
	return row, nil
}

// ListAgentCardsByWorkspace returns all non-offline agent cards in a workspace,
// excluding the specified sandbox (the requester).
func (db *DB) ListAgentCardsByWorkspace(workspaceID, excludeSandboxID string) ([]*AgentCardRow, error) {
	rows, err := db.Query(`
		SELECT ac.sandbox_id, ac.agent_type, ac.agent_status, ac.card_json,
		       ac.version, ac.updated_at, s.is_local, s.last_heartbeat_at
		FROM agent_cards ac
		JOIN sandboxes s ON s.id = ac.sandbox_id
		WHERE s.workspace_id = $1
		  AND ac.sandbox_id != $2
		  AND ac.agent_status != 'offline'
		ORDER BY ac.updated_at DESC
	`, workspaceID, excludeSandboxID)
	if err != nil {
		return nil, fmt.Errorf("list agent cards: %w", err)
	}
	defer rows.Close()

	var result []*AgentCardRow
	for rows.Next() {
		row := &AgentCardRow{}
		if err := rows.Scan(
			&row.SandboxID, &row.AgentType, &row.AgentStatus, &row.CardJSON,
			&row.Version, &row.UpdatedAt, &row.IsLocal, &row.LastHeartbeatAt,
		); err != nil {
			return nil, fmt.Errorf("scan agent card: %w", err)
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// UpdateAgentCardStatus sets the agent_status for a card.
func (db *DB) UpdateAgentCardStatus(sandboxID, status string) error {
	_, err := db.Exec(`
		UPDATE agent_cards SET agent_status = $1, updated_at = NOW()
		WHERE sandbox_id = $2
	`, status, sandboxID)
	if err != nil {
		return fmt.Errorf("update agent card status: %w", err)
	}
	return nil
}

// MarkStaleAgentsOffline sets agent_status = 'offline' for all agents
// whose sandbox heartbeat is older than the given threshold.
func (db *DB) MarkStaleAgentsOffline(threshold time.Duration) (int64, error) {
	result, err := db.Exec(`
		UPDATE agent_cards ac SET agent_status = 'offline', updated_at = NOW()
		FROM sandboxes s
		WHERE ac.sandbox_id = s.id
		  AND ac.agent_status != 'offline'
		  AND (s.last_heartbeat_at IS NULL OR s.last_heartbeat_at < NOW() - $1::interval)
	`, threshold.String())
	if err != nil {
		return 0, fmt.Errorf("mark stale agents offline: %w", err)
	}
	return result.RowsAffected()
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /root/agentserver && go build ./internal/db/`
Expected: Clean build, no errors.

- [ ] **Step 3: Commit**

```bash
git add internal/db/agent_cards.go
git commit -m "feat(db): add Agent Card types, defaults, and CRUD methods

Types: Skill, MCPTool, AgentCardData, AgentCardRow.
DefaultCardForType generates default cards for opencode/openclaw/nanoclaw.
Methods: UpsertAgentCard, GetAgentCard, ListAgentCardsByWorkspace,
UpdateAgentCardStatus, MarkStaleAgentsOffline."
```

---

### Task 3: AgentAuthMiddleware

**Files:**
- Create: `internal/server/agent_auth.go`

The middleware authenticates `/api/agent/discovery/` routes using the sandbox's `proxy_token` (from the `sandboxes` table) passed as a Bearer token. It reuses the existing `DB.GetSandboxByProxyToken(token)` method (defined in `internal/db/sandboxes.go:208`).

The `Sandbox.ProxyToken` field is `sql.NullString`, but we don't need to read it — `GetSandboxByProxyToken` looks up by token and returns the full `*Sandbox`.

- [ ] **Step 1: Write the middleware**

Create `internal/server/agent_auth.go`:

```go
package server

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/agentserver/agentserver/internal/db"
)

type agentContextKey int

const agentSandboxContextKey agentContextKey = iota

// agentAuthMiddleware authenticates agent-to-server API calls using
// the sandbox's proxy_token passed as a Bearer token in the Authorization header.
// On success, the sandbox is injected into the request context.
func (s *Server) agentAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "unauthorized: missing bearer token", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		if token == "" {
			http.Error(w, "unauthorized: empty token", http.StatusUnauthorized)
			return
		}

		sbx, err := s.DB.GetSandboxByProxyToken(token)
		if err != nil {
			log.Printf("agent auth: db error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if sbx == nil {
			http.Error(w, "unauthorized: invalid token", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), agentSandboxContextKey, sbx)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// agentSandboxFromContext extracts the authenticated sandbox from the request context.
func agentSandboxFromContext(ctx context.Context) *db.Sandbox {
	sbx, _ := ctx.Value(agentSandboxContextKey).(*db.Sandbox)
	return sbx
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /root/agentserver && go build ./internal/server/`
Expected: Clean build, no errors.

- [ ] **Step 3: Commit**

```bash
git add internal/server/agent_auth.go
git commit -m "feat(server): add agentAuthMiddleware for proxy_token auth

Authenticates /api/agent/discovery/ routes using sandbox proxy_token
from the Authorization: Bearer header. Injects the resolved
sandbox into request context."
```

---

### Task 4: Default card generation tests and capability matching logic

**Files:**
- Create: `internal/server/agent_discovery_test.go`
- Create: `internal/server/agent_discovery.go` (types and pure logic only — handlers in Tasks 5–6)

This is the first test file in `internal/server/`. Tests use standard Go patterns: `TestFunctionName(t *testing.T)` with `t.Fatalf`/`t.Errorf`.

- [ ] **Step 1: Write failing tests for default cards and capability matching**

Create `internal/server/agent_discovery_test.go`:

```go
package server

import (
	"testing"

	"github.com/agentserver/agentserver/internal/db"
)

func TestDefaultCardForType(t *testing.T) {
	tests := []struct {
		agentType  string
		wantSkills []string
	}{
		{"opencode", []string{"code-editing", "terminal", "code-search"}},
		{"openclaw", []string{"text-generation", "model-routing"}},
		{"nanoclaw", []string{"autonomous-task", "knowledge-query"}},
	}
	for _, tt := range tests {
		t.Run(tt.agentType, func(t *testing.T) {
			card := db.DefaultCardForType(tt.agentType, "test-agent")
			if len(card.Skills) != len(tt.wantSkills) {
				t.Fatalf("expected %d skills, got %d", len(tt.wantSkills), len(card.Skills))
			}
			for i, name := range tt.wantSkills {
				if card.Skills[i].Name != name {
					t.Errorf("skill %d: expected %q, got %q", i, name, card.Skills[i].Name)
				}
			}
			if card.MaxConcurrency != 1 {
				t.Errorf("expected MaxConcurrency 1, got %d", card.MaxConcurrency)
			}
		})
	}
}

func TestDefaultCardForUnknownType(t *testing.T) {
	card := db.DefaultCardForType("unknown", "test")
	if len(card.Skills) != 0 {
		t.Fatalf("expected 0 skills for unknown type, got %d", len(card.Skills))
	}
}

func TestMatchAgents_FilterBySkill(t *testing.T) {
	agents := []*agentCardResponse{
		{AgentID: "a1", Status: "available", Skills: []db.Skill{{Name: "code-review", Tags: []string{"go"}}}},
		{AgentID: "a2", Status: "available", Skills: []db.Skill{{Name: "translation", Tags: []string{"en", "zh"}}}},
		{AgentID: "a3", Status: "available", Skills: []db.Skill{{Name: "code-review", Tags: []string{"python"}}}},
	}
	result := matchAndRankAgents(agents, agentFilter{Skill: "code-review"})
	if len(result) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(result))
	}
	for _, a := range result {
		if a.AgentID != "a1" && a.AgentID != "a3" {
			t.Errorf("unexpected agent %s in results", a.AgentID)
		}
	}
}

func TestMatchAgents_FilterByTag(t *testing.T) {
	agents := []*agentCardResponse{
		{AgentID: "a1", Status: "available", Skills: []db.Skill{{Name: "code-review", Tags: []string{"go", "python"}}}},
		{AgentID: "a2", Status: "available", Skills: []db.Skill{{Name: "code-review", Tags: []string{"java"}}}},
	}
	result := matchAndRankAgents(agents, agentFilter{Tag: "go"})
	if len(result) != 1 || result[0].AgentID != "a1" {
		t.Fatalf("expected a1, got %v", result)
	}
}

func TestMatchAgents_FilterByType(t *testing.T) {
	agents := []*agentCardResponse{
		{AgentID: "a1", Type: "opencode", Status: "available", Skills: []db.Skill{{Name: "x"}}},
		{AgentID: "a2", Type: "nanoclaw", Status: "available", Skills: []db.Skill{{Name: "x"}}},
	}
	result := matchAndRankAgents(agents, agentFilter{Type: "opencode"})
	if len(result) != 1 || result[0].AgentID != "a1" {
		t.Fatalf("expected a1, got %v", result)
	}
}

func TestMatchAgents_FilterByStatus(t *testing.T) {
	agents := []*agentCardResponse{
		{AgentID: "a1", Status: "available"},
		{AgentID: "a2", Status: "busy"},
	}
	result := matchAndRankAgents(agents, agentFilter{Status: "available"})
	if len(result) != 1 || result[0].AgentID != "a1" {
		t.Fatalf("expected a1, got %v", result)
	}
}

func TestMatchAgents_RankAvailableBeforeBusy(t *testing.T) {
	agents := []*agentCardResponse{
		{AgentID: "busy1", Status: "busy", Skills: []db.Skill{{Name: "x"}}},
		{AgentID: "avail1", Status: "available", Skills: []db.Skill{{Name: "x"}}},
	}
	result := matchAndRankAgents(agents, agentFilter{})
	if len(result) != 2 {
		t.Fatalf("expected 2, got %d", len(result))
	}
	if result[0].AgentID != "avail1" {
		t.Errorf("expected available agent first, got %s", result[0].AgentID)
	}
}

func TestMatchAgents_Limit(t *testing.T) {
	agents := make([]*agentCardResponse, 20)
	for i := range agents {
		agents[i] = &agentCardResponse{AgentID: "a", Status: "available"}
	}
	result := matchAndRankAgents(agents, agentFilter{Limit: 5})
	if len(result) != 5 {
		t.Fatalf("expected 5, got %d", len(result))
	}
}

func TestMatchAgents_DefaultLimit(t *testing.T) {
	agents := make([]*agentCardResponse, 20)
	for i := range agents {
		agents[i] = &agentCardResponse{AgentID: "a", Status: "available"}
	}
	result := matchAndRankAgents(agents, agentFilter{})
	if len(result) != 10 {
		t.Fatalf("expected default limit 10, got %d", len(result))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /root/agentserver && go test ./internal/server/ -run 'TestDefault|TestMatch' -v`
Expected: Compilation errors — `matchAndRankAgents`, `agentCardResponse`, `agentFilter` not defined.

- [ ] **Step 3: Implement types and matching logic**

Create `internal/server/agent_discovery.go`:

```go
package server

import (
	"database/sql"
	"encoding/json"
	"sort"
	"time"

	"github.com/agentserver/agentserver/internal/db"
)

// agentCardResponse is the API response for a single agent card.
type agentCardResponse struct {
	AgentID        string       `json:"agent_id"`
	Name           string       `json:"name"`
	Type           string       `json:"type"`
	Description    string       `json:"description"`
	Skills         []db.Skill   `json:"skills"`
	MCPTools       []db.MCPTool `json:"mcp_tools,omitempty"`
	Status         string       `json:"status"`
	IsLocal        bool         `json:"is_local"`
	LastSeenAt     *time.Time   `json:"last_seen_at,omitempty"`
	SupportedModes []string     `json:"supported_modes"`
	MaxConcurrency int          `json:"max_concurrency"`
}

// agentFilter holds query parameters for discovery filtering.
type agentFilter struct {
	Type   string
	Status string
	Skill  string
	Tag    string
	Limit  int
}

// rowToResponse converts a DB row to an API response, merging column values with card_json.
func rowToResponse(row *db.AgentCardRow) (*agentCardResponse, error) {
	var data db.AgentCardData
	if err := json.Unmarshal([]byte(row.CardJSON), &data); err != nil {
		return nil, err
	}
	resp := &agentCardResponse{
		AgentID:        row.SandboxID,
		Name:           data.Name,
		Type:           row.AgentType,
		Description:    data.Description,
		Skills:         data.Skills,
		Status:         row.AgentStatus,
		IsLocal:        row.IsLocal,
		SupportedModes: data.SupportedModes,
		MaxConcurrency: data.MaxConcurrency,
	}
	if row.LastHeartbeatAt.Valid {
		resp.LastSeenAt = &row.LastHeartbeatAt.Time
	}
	return resp, nil
}

// matchAndRankAgents filters and sorts agents by the given criteria.
// Ranking priority: available > busy, then stable order.
func matchAndRankAgents(agents []*agentCardResponse, f agentFilter) []*agentCardResponse {
	var matched []*agentCardResponse

	for _, a := range agents {
		if f.Type != "" && a.Type != f.Type {
			continue
		}
		if f.Status != "" && a.Status != f.Status {
			continue
		}
		if f.Skill != "" && !agentHasSkill(a, f.Skill) {
			continue
		}
		if f.Tag != "" && !agentHasTag(a, f.Tag) {
			continue
		}
		matched = append(matched, a)
	}

	// Sort: available before busy.
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Status != matched[j].Status {
			return matched[i].Status == "available"
		}
		return false
	})

	// Apply limit.
	limit := f.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	if len(matched) > limit {
		matched = matched[:limit]
	}
	return matched
}

func agentHasSkill(a *agentCardResponse, skill string) bool {
	for _, s := range a.Skills {
		if s.Name == skill {
			return true
		}
	}
	return false
}

func agentHasTag(a *agentCardResponse, tag string) bool {
	for _, s := range a.Skills {
		for _, t := range s.Tags {
			if t == tag {
				return true
			}
		}
	}
	return false
}
```

Note: `database/sql` is imported for `sql.NullTime` used in `rowToResponse`. The `time` import is used for `*time.Time` in `agentCardResponse`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /root/agentserver && go test ./internal/server/ -run 'TestDefault|TestMatch' -v`
Expected: All PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server/agent_discovery.go internal/server/agent_discovery_test.go
git commit -m "feat(server): add Agent Card response types and capability matching

- agentCardResponse type for API responses
- rowToResponse converts DB rows handling sql.NullTime
- matchAndRankAgents filters by type/status/skill/tag and ranks
  available agents before busy ones
- Comprehensive unit tests for defaults and all matching logic"
```

---

### Task 5: Card registration handler

**Files:**
- Modify: `internal/server/agent_discovery.go` (add handler + imports)
- Modify: `internal/server/server.go` (wire route)

- [ ] **Step 1: Add card registration handler to agent_discovery.go**

Append to `internal/server/agent_discovery.go`, and add `"log"`, `"net/http"` to the import block:

```go
// --- HTTP Handlers ---

// handleRegisterAgentCard handles POST /api/agent/discovery/cards.
// Registers or updates the calling agent's capability card.
func (s *Server) handleRegisterAgentCard(w http.ResponseWriter, r *http.Request) {
	sbx := agentSandboxFromContext(r.Context())
	if sbx == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req db.AgentCardData
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: invalid JSON", http.StatusBadRequest)
		return
	}

	// Merge with defaults: if the agent sends no skills, use type defaults.
	if len(req.Skills) == 0 {
		defaults := db.DefaultCardForType(sbx.Type, sbx.Name)
		req.Skills = defaults.Skills
		if req.Description == "" {
			req.Description = defaults.Description
		}
	}
	if req.Name == "" {
		req.Name = sbx.Name
	}
	if req.MaxConcurrency <= 0 {
		req.MaxConcurrency = 1
	}
	if len(req.SupportedModes) == 0 {
		req.SupportedModes = []string{"async"}
	}

	cardJSON, err := json.Marshal(req)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	version, err := s.DB.UpsertAgentCard(sbx.ID, sbx.Type, string(cardJSON))
	if err != nil {
		log.Printf("agent card upsert error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"version": version})
}
```

The import block at the top of `agent_discovery.go` should now be:

```go
import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/agentserver/agentserver/internal/db"
)
```

- [ ] **Step 2: Wire the route in server.go**

In `internal/server/server.go`, inside the `Router()` method, add the `/api/agent/discovery/` route group **after** the existing `r.Post("/api/agent/register", s.handleAgentRegister)` line (around line 32) and **before** the auth endpoints:

```go
	// Agent-to-server API routes (proxy_token auth, not cookie auth).
	r.Route("/api/agent/discovery", func(r chi.Router) {
		r.Use(s.agentAuthMiddleware)
		r.Post("/cards", s.handleRegisterAgentCard)
	})
```

- [ ] **Step 3: Verify it compiles**

Run: `cd /root/agentserver && go build ./...`
Expected: Clean build, no errors.

- [ ] **Step 4: Commit**

```bash
git add internal/server/agent_discovery.go internal/server/server.go
git commit -m "feat(server): add card registration endpoint POST /api/agent/discovery/cards

Agents call this with their proxy_token to register or update
their capability card. Merges with type defaults when no skills
are provided. Returns the new card version number."
```

---

### Task 6: Discovery handlers

**Files:**
- Modify: `internal/server/agent_discovery.go` (add discovery handlers + imports)
- Modify: `internal/server/server.go` (wire routes)

- [ ] **Step 1: Add discovery handlers to agent_discovery.go**

Add `"strconv"` and `"github.com/go-chi/chi/v5"` to the import block in `agent_discovery.go`.

Append to `internal/server/agent_discovery.go`:

```go
// handleDiscoverAgents handles GET /api/agent/discovery/agents.
// Returns agents in the same workspace matching the query filters.
func (s *Server) handleDiscoverAgents(w http.ResponseWriter, r *http.Request) {
	sbx := agentSandboxFromContext(r.Context())
	if sbx == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	rows, err := s.DB.ListAgentCardsByWorkspace(sbx.WorkspaceID, sbx.ID)
	if err != nil {
		log.Printf("discovery list error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Convert DB rows to response objects.
	var agents []*agentCardResponse
	for _, row := range rows {
		resp, err := rowToResponse(row)
		if err != nil {
			log.Printf("discovery: failed to parse card for %s: %v", row.SandboxID, err)
			continue
		}
		agents = append(agents, resp)
	}

	// Parse filter from query params.
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	f := agentFilter{
		Type:   r.URL.Query().Get("type"),
		Status: r.URL.Query().Get("status"),
		Skill:  r.URL.Query().Get("skill"),
		Tag:    r.URL.Query().Get("tag"),
		Limit:  limit,
	}

	matched := matchAndRankAgents(agents, f)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"agents": matched,
		"total":  len(matched),
	})
}

// handleGetAgentCard handles GET /api/agent/discovery/agents/{sandbox_id}.
// Returns the full card for a specific agent, including MCP tool schemas.
func (s *Server) handleGetAgentCard(w http.ResponseWriter, r *http.Request) {
	sbx := agentSandboxFromContext(r.Context())
	if sbx == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	targetID := chi.URLParam(r, "sandbox_id")
	row, err := s.DB.GetAgentCard(targetID)
	if err != nil {
		log.Printf("get agent card error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if row == nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}

	// Verify same workspace.
	targetSbx, err := s.DB.GetSandbox(targetID)
	if err != nil || targetSbx == nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	if targetSbx.WorkspaceID != sbx.WorkspaceID {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}

	resp, err := rowToResponse(row)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Include full MCP tools in single-agent response.
	var data db.AgentCardData
	if err := json.Unmarshal([]byte(row.CardJSON), &data); err == nil {
		resp.MCPTools = data.MCPTools
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
```

The full import block in `agent_discovery.go` should now be:

```go
import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/agentserver/agentserver/internal/db"
	"github.com/go-chi/chi/v5"
)
```

Note: The `database/sql` import was added in Task 4 for `rowToResponse`'s handling of `sql.NullTime`. If the compiler reports it unused (because `rowToResponse` doesn't directly reference `sql` by name — it uses `row.LastHeartbeatAt.Valid` which is a struct field), remove it.

- [ ] **Step 2: Wire routes in server.go**

In the `/api/agent/discovery` route group in `internal/server/server.go`, add the discovery endpoints (expanding what was added in Task 5):

```go
	// Agent-to-server API routes (proxy_token auth, not cookie auth).
	r.Route("/api/agent/discovery", func(r chi.Router) {
		r.Use(s.agentAuthMiddleware)
		r.Post("/cards", s.handleRegisterAgentCard)
		r.Get("/agents", s.handleDiscoverAgents)
		r.Get("/agents/{sandbox_id}", s.handleGetAgentCard)
	})
```

- [ ] **Step 3: Verify it compiles**

Run: `cd /root/agentserver && go build ./...`
Expected: Clean build, no errors.

- [ ] **Step 4: Run all tests**

Run: `cd /root/agentserver && go test ./internal/server/ -v`
Expected: All PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server/agent_discovery.go internal/server/server.go
git commit -m "feat(server): add discovery endpoints GET /api/agent/discovery/agents

- List agents filtered by type, status, skill, tag with pagination
- Get single agent card with full MCP tool schemas
- Workspace isolation: agents only see peers in same workspace
- Requesting agent excluded from results"
```

---

### Task 7: Extended heartbeat and auto-registration in tunnel

**Files:**
- Modify: `internal/sandboxproxy/tunnel.go` (handle agent_status in heartbeat; auto-register card on connect)

The `sandboxproxy` package already imports `internal/db`. It does NOT import `internal/server`, so there is no circular dependency. `DefaultCardForType` lives in `internal/db` (created in Task 2), callable from here.

- [ ] **Step 1: Extend the OnAgentInfo callback in tunnel.go**

In `internal/sandboxproxy/tunnel.go`, replace the existing `OnAgentInfo` callback (lines 56–66) with:

```go
	t.OnAgentInfo = func(data json.RawMessage) {
		var info db.AgentInfo
		if err := json.Unmarshal(data, &info); err != nil {
			log.Printf("tunnel %s: failed to unmarshal agent info: %v", sandboxID, err)
			return
		}
		info.SandboxID = sandboxID
		if err := s.DB.UpsertAgentInfo(&info); err != nil {
			log.Printf("tunnel %s: failed to upsert agent info: %v", sandboxID, err)
		}

		// Handle extended heartbeat fields for agent card status.
		var ext struct {
			AgentStatus string `json:"agent_status"`
		}
		if err := json.Unmarshal(data, &ext); err == nil && ext.AgentStatus != "" {
			if err := s.DB.UpdateAgentCardStatus(sandboxID, ext.AgentStatus); err != nil {
				log.Printf("tunnel %s: failed to update card status: %v", sandboxID, err)
			}
		}
	}
```

- [ ] **Step 2: Auto-register default card on tunnel connect**

In `internal/sandboxproxy/tunnel.go`, after the initial `s.DB.UpdateSandboxHeartbeat(sandboxID)` call (line 72), add auto-registration:

```go
	s.DB.UpdateSandboxHeartbeat(sandboxID)

	// Auto-register a default agent card if one doesn't exist yet.
	if existingCard, _ := s.DB.GetAgentCard(sandboxID); existingCard == nil {
		sbxForCard, err := s.DB.GetSandbox(sandboxID)
		if err == nil && sbxForCard != nil {
			defaults := db.DefaultCardForType(sbxForCard.Type, sbxForCard.Name)
			cardJSON, _ := json.Marshal(defaults)
			if _, err := s.DB.UpsertAgentCard(sandboxID, sbxForCard.Type, string(cardJSON)); err != nil {
				log.Printf("tunnel %s: failed to auto-register card: %v", sandboxID, err)
			} else {
				log.Printf("tunnel %s: auto-registered default %s card", sandboxID, sbxForCard.Type)
			}
		}
	}
```

- [ ] **Step 3: Verify it compiles**

Run: `cd /root/agentserver && go build ./...`
Expected: Clean build, no errors.

- [ ] **Step 4: Run all tests**

Run: `cd /root/agentserver && go test ./... -v`
Expected: All PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sandboxproxy/tunnel.go
git commit -m "feat(tunnel): extend heartbeat with agent_status and auto-register cards

- OnAgentInfo callback now updates agent_cards.agent_status from heartbeat
- Auto-registers default card on first tunnel connection using
  db.DefaultCardForType (no circular imports)"
```

---

### Task 8: AgentHealthMonitor background goroutine

**Files:**
- Create: `internal/server/agent_health.go`
- Modify: `cmd/serve.go` (start and stop monitor)

- [ ] **Step 1: Write the health monitor**

Create `internal/server/agent_health.go`:

```go
package server

import (
	"log"
	"sync"
	"time"

	"github.com/agentserver/agentserver/internal/db"
)

// AgentHealthMonitor periodically sweeps the agent_cards table and marks
// agents as offline when their heartbeat lapses beyond the threshold.
type AgentHealthMonitor struct {
	db        *db.DB
	interval  time.Duration // sweep interval (default 30s)
	threshold time.Duration // offline threshold (default 60s)
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

// NewAgentHealthMonitor creates a new health monitor.
func NewAgentHealthMonitor(database *db.DB) *AgentHealthMonitor {
	return &AgentHealthMonitor{
		db:        database,
		interval:  30 * time.Second,
		threshold: 60 * time.Second,
		stopCh:    make(chan struct{}),
	}
}

// Start begins the background sweep goroutine.
func (m *AgentHealthMonitor) Start() {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				m.sweep()
			case <-m.stopCh:
				return
			}
		}
	}()
	log.Printf("Agent health monitor started (interval: %s, threshold: %s)", m.interval, m.threshold)
}

// Stop signals the background goroutine to exit and waits for it.
func (m *AgentHealthMonitor) Stop() {
	close(m.stopCh)
	m.wg.Wait()
	log.Println("Agent health monitor stopped")
}

func (m *AgentHealthMonitor) sweep() {
	count, err := m.db.MarkStaleAgentsOffline(m.threshold)
	if err != nil {
		log.Printf("agent health sweep error: %v", err)
		return
	}
	if count > 0 {
		log.Printf("agent health sweep: marked %d agent(s) offline", count)
	}
}
```

- [ ] **Step 2: Start and stop the monitor in cmd/serve.go**

In `cmd/serve.go`, add the import for `server` package if not already present:

```go
"github.com/agentserver/agentserver/internal/server"
```

After the `idleWatcher.Start()` line (line 231), add:

```go
	// Start agent health monitor.
	agentHealthMonitor := server.NewAgentHealthMonitor(database)
	agentHealthMonitor.Start()
```

In the shutdown goroutine (inside the `go func()` that handles SIGTERM/SIGINT, lines 237–246), add `agentHealthMonitor.Stop()` before `procMgr.Close()`:

```go
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		log.Printf("Received %v, shutting down...", sig)
		httpServer.Shutdown(context.Background())
		idleWatcher.Stop()
		agentHealthMonitor.Stop()
		log.Println("Cleaning up active sandboxes...")
		procMgr.Close()
	}()
```

- [ ] **Step 3: Verify it compiles**

Run: `cd /root/agentserver && go build ./...`
Expected: Clean build, no errors.

- [ ] **Step 4: Commit**

```bash
git add internal/server/agent_health.go cmd/serve.go
git commit -m "feat(server): add AgentHealthMonitor for offline detection

Background goroutine sweeps agent_cards every 30s and marks
agents as offline when their heartbeat lapses beyond 60s.
Started alongside the idle watcher in cmd/serve.go."
```

---

### Task 9: End-to-end verification

**Files:** None (verification only)

- [ ] **Step 1: Build the full project**

Run: `cd /root/agentserver && go build ./...`
Expected: Clean build, no errors.

- [ ] **Step 2: Run all tests**

Run: `cd /root/agentserver && go test ./... -v`
Expected: All PASS, including new discovery tests.

- [ ] **Step 3: Verify API routes are registered**

Check `internal/server/server.go` confirms the route group contains:

```
POST /api/agent/discovery/cards           → handleRegisterAgentCard
GET  /api/agent/discovery/agents          → handleDiscoverAgents
GET  /api/agent/discovery/agents/{id}     → handleGetAgentCard
```

All routes inside the `agentAuthMiddleware` group.

- [ ] **Step 4: Verify migration file ordering**

Run: `ls -1 /root/agentserver/internal/db/migrations/`
Expected: `011_agent_cards.sql` appears after `010_unified_im_bindings.sql`.

- [ ] **Step 5: Final commit (if any cleanup needed)**

Only commit if there are actual changes:
```bash
git status
# If clean, skip. Otherwise:
git add -A
git commit -m "chore: cleanup after Phase 1 agent cards and discovery"
```

---

## Summary

Phase 1 delivers:
- `agent_cards` table with indexed type/status columns (migration 011)
- `agentAuthMiddleware` for proxy_token-based auth on `/api/agent/discovery/` routes
- Card registration (`POST /api/agent/discovery/cards`) with default merging
- Agent discovery (`GET /api/agent/discovery/agents`) with filtering and ranking
- Single agent detail (`GET /api/agent/discovery/agents/{sandbox_id}`) with MCP tools
- Default card auto-registration on tunnel connect
- Extended heartbeat with `agent_status` updates
- `AgentHealthMonitor` background goroutine for offline detection

**Key corrections from original plan:**
1. Migration number 011 (not 010 — `010_unified_im_bindings.sql` already exists)
2. `DefaultCardForType` placed in `db` package from the start (avoids circular import refactoring)
3. `AgentCardRow.LastHeartbeatAt` uses `sql.NullTime` (matches `Sandbox` struct convention)
4. DB methods use `fmt.Errorf("operation: %w", err)` wrapping (matches project convention)
5. Removed unused `FrameTypeCardUpdate` constant from file structure
6. `rowToResponse` properly handles `sql.NullTime` → `*time.Time` conversion

**Next phase:** Phase 2 (Task Delegation) builds on this foundation by adding the `agent_tasks` table, delegation endpoints, and task delivery to agents.
