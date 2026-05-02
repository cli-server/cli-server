# cc-broker Worker via Claude Agent SDK — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace cc-broker's broken `claude --sdk-url bridge` worker path with an in-process `claude-agent-sdk-go` agent loop. Preserve all external contracts (`POST /api/turns` SSE, `agent_session_events`, OpenViking download-run-upload).

**Architecture:** `handler_turns.go` orchestrates: `workspace.Setup` → `tools.BuildMcpServer(*Context)` → `runner.Run(...)` → consume `<-chan agentsdk.SDKMessage`. Three new subpackages under `internal/ccbroker/`: `workspace/` (OpenViking + temp dirs + diff-upload), `runner/` (SDK Client lifecycle with V1/V2 adapter seam), `tools/` (in-process MCP tools). Bridge HTTP endpoints and HTTP MCP server are deleted (~1500 LOC) — they were dead code anyway.

**Tech Stack:** Go 1.22+, `github.com/agentserver/claude-agent-sdk-go` (V1 API), chi router, PostgreSQL (no schema change).

**Spec:** [`docs/superpowers/specs/2026-05-02-ccbroker-sdk-worker-design.md`](../specs/2026-05-02-ccbroker-sdk-worker-design.md)

**Scope note:** This is one logical refactor. Diff is large (~1500 LOC delete + ~800 LOC add). The plan is structured so each phase compiles and tests cleanly on its own — phases 1–3 add new packages without removing anything; phase 4 swaps the wiring; phase 5 deletes the now-unused old code. If you want to ship intermediate PRs, phase boundaries are natural cut points (1+2+3 → infrastructure PR; 4+5 → switchover PR; 6+7 → deploy).

**Testing strategy note:** Pure-Go units (snapshot/diff, options builder, events converter, tool input parsing) get strict TDD. Integration pieces (`runner.Run`'s SDK wiring, `handler_turns` orchestration) get fake-based handler tests. End-to-end with real `claude` CLI is deferred to manual smoke tests after deploy.

---

## File Structure

| Action | Path | Responsibility |
|---|---|---|
| Create | `internal/ccbroker/workspace/snapshot.go` | `takeFileSnapshot`, `diffSnapshot` (pure) |
| Create | `internal/ccbroker/workspace/snapshot_test.go` | Unit tests |
| Create | `internal/ccbroker/workspace/viking_client.go` | Moved verbatim from `internal/ccbroker/viking_client.go` |
| Create | `internal/ccbroker/workspace/workspace.go` | `Workspace` struct + `Setup` + `Teardown` |
| Create | `internal/ccbroker/workspace/workspace_test.go` | httptest mock viking |
| Create | `internal/ccbroker/runner/events.go` | `ToEventPayload(agentsdk.SDKMessage) ([]byte, error)` |
| Create | `internal/ccbroker/runner/events_test.go` | Golden tests per SDKMessage type |
| Create | `internal/ccbroker/runner/options.go` | `BuildClientOptions(...) []agentsdk.QueryOption` |
| Create | `internal/ccbroker/runner/options_test.go` | Table tests |
| Create | `internal/ccbroker/runner/runner.go` | `Run(ctx, ws, sess, userMsg, mcp) (<-chan SDKMessage, error)` + `sdkSession` adapter seam |
| Create | `internal/ccbroker/tools/context.go` | `Context` struct |
| Create | `internal/ccbroker/tools/executor.go` | `remote_*` + `list_executors` tools |
| Create | `internal/ccbroker/tools/executor_test.go` | Mock executor-registry |
| Create | `internal/ccbroker/tools/workspace.go` | `workspace_{read,write,ls}` |
| Create | `internal/ccbroker/tools/workspace_test.go` | Tempdir IO |
| Create | `internal/ccbroker/tools/im.go` | `send_{message,image,file}` |
| Create | `internal/ccbroker/tools/im_test.go` | Mock agentserver |
| Create | `internal/ccbroker/tools/scheduler.go` | Stub: returns "not connected" error |
| Create | `internal/ccbroker/tools/askuser.go` | Stub: returns "not implemented" error |
| Create | `internal/ccbroker/tools/router.go` | `BuildMcpServer(*Context) *agentsdk.McpSdkServer` |
| Modify | `internal/ccbroker/handler_turns.go` | Rewrite body to call workspace + tools + runner |
| Modify | `internal/ccbroker/handler_turns_test.go` (new file) | Orchestration test against fakes |
| Modify | `internal/ccbroker/server.go` | Remove bridge routes; keep `/api/turns` + `/api/sessions` |
| Modify | `internal/ccbroker/config.go` | Add (or surface) any new env knobs needed by runner |
| Delete | `internal/ccbroker/handler_bridge.go` | Bridge attach |
| Delete | `internal/ccbroker/handler_events.go` | Bridge event SSE/batch |
| Delete | `internal/ccbroker/handler_internal_events.go` | Bridge internal events |
| Delete | `internal/ccbroker/handler_worker.go` | Bridge worker state/heartbeat |
| Delete | `internal/ccbroker/jwt.go` | Bridge-only auth |
| Delete | `internal/ccbroker/middleware.go` | Bridge-only middleware |
| Delete | `internal/ccbroker/mcp_server.go` | HTTP MCP server |
| Delete | `internal/ccbroker/mcp_router.go` | HTTP-path tool routing |
| Delete | `internal/ccbroker/mcp_tools.go` | HTTP tool definitions |
| Delete | `internal/ccbroker/mcp_router_im_test.go` | Old test |
| Delete | `internal/ccbroker/mcp_server_test.go` | Old test |
| Delete | `internal/ccbroker/worker.go` | Replaced by workspace/ + runner/ |
| Delete | `internal/ccbroker/worker_test.go` | Old test |
| Delete | `internal/ccbroker/viking_client.go` | Moved into workspace/ |
| Modify or delete | `internal/ccbroker/integration_test.go` | Drop bridge-flavoured assertions; rewrite if anything stays |

---

## Phase 0 — Branch + spec commit

### Task 0.1: Create feature branch and commit the spec docs

**Files:**
- Create: feature branch `feature/ccbroker-sdk-worker`
- Add: `docs/superpowers/specs/2026-05-02-ccbroker-sdk-worker-design.md`
- Add: `docs/superpowers/plans/2026-05-02-ccbroker-sdk-worker.md`

- [ ] **Step 1: Branch off main**

```bash
cd /root/agentserver
git checkout main && git pull github main --ff-only
git checkout -b feature/ccbroker-sdk-worker
```

- [ ] **Step 2: Stage spec + plan**

```bash
git add docs/superpowers/specs/2026-05-02-ccbroker-sdk-worker-design.md \
        docs/superpowers/plans/2026-05-02-ccbroker-sdk-worker.md
```

- [ ] **Step 3: Commit**

```bash
git commit -m "$(cat <<'EOF'
docs: design + plan for cc-broker worker via Claude Agent SDK

Replace the broken claude --sdk-url bridge approach (rejected by the
CLI's hard-coded host allowlist) with in-process claude-agent-sdk-go
running a fresh `claude --print` subprocess per turn. WithResume
loads CLI session files persisted in OpenViking; in-process MCP
tools replace the HTTP MCP server. Bridge HTTP endpoints become dead
code and will be removed in the implementation PR.

Spec tracks both TS V1 (stable) and V2 preview semantics; runner/
package is structured with an adapter seam so the V1→V2 migration
is contained when claude-agent-sdk-go ships V2 bindings.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Phase 1 — `workspace/` subpackage (TDD)

### Task 1.1: Snapshot + diff (pure functions, strict TDD)

**Files:**
- Create: `internal/ccbroker/workspace/snapshot.go`
- Create: `internal/ccbroker/workspace/snapshot_test.go`

The current `worker.go:41-78` defines `takeFileSnapshot` and `diffSnapshot` over a directory. They walk the tree and capture `path → {mtime, size, sha256}`; `diffSnapshot` compares a current scan against the saved snapshot and returns added/modified/removed entries. We move and TDD them in the new package.

- [ ] **Step 1: Write the failing test first**

Create `internal/ccbroker/workspace/snapshot_test.go`:

```go
package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTakeAndDiffSnapshot(t *testing.T) {
	dir := t.TempDir()

	// Initial state: a/foo.txt + b/bar.txt
	mustWrite(t, filepath.Join(dir, "a", "foo.txt"), "v1")
	mustWrite(t, filepath.Join(dir, "b", "bar.txt"), "v1")

	snap := TakeSnapshot(dir)
	if got := len(snap); got != 2 {
		t.Fatalf("expected 2 files in snapshot, got %d", got)
	}

	// Mutate: change foo.txt, add c/baz.txt, remove b/bar.txt
	mustWrite(t, filepath.Join(dir, "a", "foo.txt"), "v2-changed")
	mustWrite(t, filepath.Join(dir, "c", "baz.txt"), "new")
	if err := os.Remove(filepath.Join(dir, "b", "bar.txt")); err != nil {
		t.Fatal(err)
	}

	changes := DiffSnapshot(dir, snap)

	byKind := map[string]int{}
	for _, c := range changes {
		byKind[c.Kind]++
	}
	if byKind["added"] != 1 || byKind["modified"] != 1 || byKind["removed"] != 1 {
		t.Fatalf("unexpected kind distribution: %v", byKind)
	}
}

func TestDiffSnapshotEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	snap := TakeSnapshot(dir)
	if got := DiffSnapshot(dir, snap); len(got) != 0 {
		t.Fatalf("expected 0 changes, got %d", len(got))
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run, verify it fails**

Run: `cd /root/agentserver && go test ./internal/ccbroker/workspace/ -run TestTakeAndDiffSnapshot -v`
Expected: build failure — `undefined: TakeSnapshot` / `undefined: DiffSnapshot`.

- [ ] **Step 3: Implement snapshot.go**

Create `internal/ccbroker/workspace/snapshot.go`:

```go
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
)

// FileInfo captures the identity of a file at snapshot time.
type FileInfo struct {
	ModTime int64  // Unix nanoseconds
	Size    int64
	SHA256  string // hex
}

// FileChange is one entry in a snapshot diff.
type FileChange struct {
	Path    string // absolute path
	RelPath string // path relative to the snapshot root
	Kind    string // "added" | "modified" | "removed"
}

// TakeSnapshot walks `dir` and records (mtime, size, sha256) for every regular
// file. Symlinks and directories are skipped. The returned map is keyed by
// path relative to `dir`.
func TakeSnapshot(dir string) map[string]FileInfo {
	out := make(map[string]FileInfo)
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		out[rel] = FileInfo{
			ModTime: info.ModTime().UnixNano(),
			Size:    info.Size(),
			SHA256:  hashFile(path),
		}
		return nil
	})
	return out
}

// DiffSnapshot scans `dir` and returns every file that was added, modified
// (size or sha256 changed), or removed since `old` was captured.
func DiffSnapshot(dir string, old map[string]FileInfo) []FileChange {
	current := TakeSnapshot(dir)
	var changes []FileChange

	for rel, cur := range current {
		prev, existed := old[rel]
		if !existed {
			changes = append(changes, FileChange{
				Path:    filepath.Join(dir, rel),
				RelPath: rel,
				Kind:    "added",
			})
			continue
		}
		if prev.Size != cur.Size || prev.SHA256 != cur.SHA256 {
			changes = append(changes, FileChange{
				Path:    filepath.Join(dir, rel),
				RelPath: rel,
				Kind:    "modified",
			})
		}
	}
	for rel := range old {
		if _, stillThere := current[rel]; !stillThere {
			changes = append(changes, FileChange{
				Path:    filepath.Join(dir, rel),
				RelPath: rel,
				Kind:    "removed",
			})
		}
	}
	return changes
}

func hashFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
```

- [ ] **Step 4: Run tests, verify pass**

Run: `cd /root/agentserver && go test ./internal/ccbroker/workspace/ -v`
Expected: `PASS` for `TestTakeAndDiffSnapshot` and `TestDiffSnapshotEmptyDirectory`.

- [ ] **Step 5: Commit**

```bash
git add internal/ccbroker/workspace/snapshot.go internal/ccbroker/workspace/snapshot_test.go
git commit -m "$(cat <<'EOF'
feat(ccbroker/workspace): TakeSnapshot + DiffSnapshot

Pure-Go file tree snapshot/diff used by the new workspace package
to detect which files changed during a CC turn so they can be
uploaded back to OpenViking on Teardown.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 1.2: Move VikingClient into workspace/ (no rewrite)

**Files:**
- Create: `internal/ccbroker/workspace/viking_client.go` (verbatim move)
- (Old `internal/ccbroker/viking_client.go` will be deleted in Phase 5; both copies compile in parallel until then.)

- [ ] **Step 1: Copy the existing file**

```bash
cp /root/agentserver/internal/ccbroker/viking_client.go \
   /root/agentserver/internal/ccbroker/workspace/viking_client.go
```

- [ ] **Step 2: Change package + rename type to avoid future name collision**

Open `internal/ccbroker/workspace/viking_client.go`. Change line 1 from `package ccbroker` to `package workspace`. The exported type `VikingClient`, constructor `NewVikingClient`, and methods (`DownloadTree`, `UploadFile`, `CreateFile`) keep their names — they will be referenced as `workspace.VikingClient` etc.

- [ ] **Step 3: Verify it compiles in isolation**

Run: `cd /root/agentserver && go build ./internal/ccbroker/workspace/...`
Expected: 0 errors. (The old `internal/ccbroker/viking_client.go` is still there as `ccbroker.VikingClient` and unused by `workspace/`.)

- [ ] **Step 4: Commit**

```bash
git add internal/ccbroker/workspace/viking_client.go
git commit -m "$(cat <<'EOF'
chore(ccbroker/workspace): move VikingClient verbatim

No behaviour change; just relocated to internal/ccbroker/workspace/
so the new workspace package can own its own OpenViking dependency.
The old internal/ccbroker/viking_client.go is removed in a later
phase once nothing in ccbroker/ imports it directly.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 1.3: `Workspace` struct + `Setup` + `Teardown`

**Files:**
- Create: `internal/ccbroker/workspace/workspace.go`
- Create: `internal/ccbroker/workspace/workspace_test.go`

`Setup` mirrors what `worker.go:84-205` does today (mkdir temp, download claude-home + project, ensure memory dir, snapshot). `Teardown` mirrors `worker.go:228-265` (diff, upload changes, remove temp dir).

- [ ] **Step 1: Write the failing test**

Create `internal/ccbroker/workspace/workspace_test.go`:

```go
package workspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeViking serves the minimum OpenViking surface Setup/Teardown need.
// Tracks uploads so tests can assert what got pushed back.
type fakeViking struct {
	uploads map[string]string // vikingURI → content
	tree    map[string]string // vikingURI → content (initial state)
}

func newFakeViking() *fakeViking {
	return &fakeViking{
		uploads: make(map[string]string),
		tree:    make(map[string]string),
	}
}

func (f *fakeViking) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/fs/ls", func(w http.ResponseWriter, r *http.Request) {
		uri, _ := url.QueryUnescape(r.URL.Query().Get("uri"))
		var entries []map[string]any
		for u, content := range f.tree {
			if !strings.HasPrefix(u, uri) {
				continue
			}
			rel := strings.TrimPrefix(u, uri)
			entries = append(entries, map[string]any{
				"name": filepath.Base(rel), "isDir": false,
				"uri": u, "rel_path": rel,
			})
			_ = content
		}
		writeViking(w, entries)
	})
	mux.HandleFunc("/api/v1/content/read", func(w http.ResponseWriter, r *http.Request) {
		uri, _ := url.QueryUnescape(r.URL.Query().Get("uri"))
		writeViking(w, f.tree[uri])
	})
	mux.HandleFunc("/api/v1/content/write", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ URI, Content string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.uploads[body.URI] = body.Content
		writeViking(w, "ok")
	})
	return mux
}

func writeViking(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "result": result})
}

func TestSetupAndTeardown(t *testing.T) {
	fv := newFakeViking()
	// Pre-populate one file so DownloadTree has something to fetch
	fv.tree["viking://resources/workspace_ws1/claude-home/CLAUDE.md"] = "global-claude"

	srv := httptest.NewServer(fv.handler())
	defer srv.Close()

	vc := NewVikingClient(srv.URL, "")
	ctx := context.Background()

	ws, err := Setup(ctx, "ws1", "cse_abc", vc)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// Downloaded file should now be in ClaudeDir
	got, err := os.ReadFile(filepath.Join(ws.ClaudeDir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read downloaded: %v", err)
	}
	if string(got) != "global-claude" {
		t.Fatalf("downloaded content mismatch: %q", got)
	}

	// Memory dir created at the deterministic path
	wantMem := filepath.Join(ws.ClaudeDir, "projects", "ws_ws1", "memory")
	if _, err := os.Stat(wantMem); err != nil {
		t.Fatalf("memory dir missing: %v", err)
	}

	// Mutate one tracked file + add a new one — both should upload on Teardown
	if err := os.WriteFile(filepath.Join(ws.ClaudeDir, "CLAUDE.md"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.ClaudeDir, "memory", "MEMORY.md"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Teardown(ctx, ws, vc); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	if _, err := os.Stat(ws.TempDir); !os.IsNotExist(err) {
		t.Fatalf("TempDir should be removed; err=%v", err)
	}
	if len(fv.uploads) != 2 {
		t.Fatalf("expected 2 uploads, got %d: %v", len(fv.uploads), keys(fv.uploads))
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
```

- [ ] **Step 2: Run, verify failure**

Run: `cd /root/agentserver && go test ./internal/ccbroker/workspace/ -run TestSetupAndTeardown -v`
Expected: build failure — `Setup`, `Teardown`, `Workspace` all undefined.

- [ ] **Step 3: Implement workspace.go**

Create `internal/ccbroker/workspace/workspace.go`:

```go
package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// Workspace is the ephemeral local filesystem view a single CC turn operates in.
type Workspace struct {
	WorkspaceID string
	SessionID   string

	TempDir    string // root: /tmp/cc-worker-<uuid>
	ClaudeDir  string // <TempDir>/claude-config — CLAUDE_CONFIG_DIR
	ProjectDir string // <TempDir>/project       — CLI cwd
	MemoryDir  string // <ClaudeDir>/projects/ws_<wid>/memory — auto-memory override

	snapshot map[string]FileInfo // captured at Setup, consumed by Teardown
}

// Setup creates the temp directory tree and downloads workspace context from
// OpenViking. The returned Workspace must be passed to Teardown so the temp
// directory is removed and changed files are uploaded back.
func Setup(ctx context.Context, workspaceID, sessionID string, vc *VikingClient) (*Workspace, error) {
	tempDir, err := os.MkdirTemp("", "cc-worker-"+uuid.NewString()+"-")
	if err != nil {
		return nil, fmt.Errorf("mkdir temp: %w", err)
	}

	ws := &Workspace{
		WorkspaceID: workspaceID,
		SessionID:   sessionID,
		TempDir:     tempDir,
		ClaudeDir:   filepath.Join(tempDir, "claude-config"),
		ProjectDir:  filepath.Join(tempDir, "project"),
	}
	ws.MemoryDir = filepath.Join(ws.ClaudeDir, "projects", "ws_"+workspaceID, "memory")

	for _, d := range []string{ws.ClaudeDir, ws.ProjectDir, ws.MemoryDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			os.RemoveAll(tempDir)
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	homeURI := fmt.Sprintf("viking://resources/workspace_%s/claude-home/", workspaceID)
	if err := vc.DownloadTree(ctx, homeURI, ws.ClaudeDir); err != nil {
		// Fail-open: a missing or partial workspace tree is not fatal — first-turn
		// workspaces start empty. Log and continue.
		fmt.Fprintf(os.Stderr, "workspace.Setup: download claude-home: %v\n", err)
	}

	projectURI := fmt.Sprintf("viking://resources/workspace_%s/project/", workspaceID)
	if err := vc.DownloadTree(ctx, projectURI, ws.ProjectDir); err != nil {
		fmt.Fprintf(os.Stderr, "workspace.Setup: download project: %v\n", err)
	}

	ws.snapshot = TakeSnapshot(ws.ClaudeDir)
	return ws, nil
}

// Teardown diffs the ClaudeDir against the snapshot taken at Setup, uploads
// every added/modified file back to OpenViking, then removes the temp dir.
// Removed files are not propagated (OpenViking content writes are append-or-replace).
// Returns nil even if individual upload calls fail; callers should monitor stderr.
func Teardown(ctx context.Context, ws *Workspace, vc *VikingClient) error {
	if ws == nil {
		return nil
	}
	defer func() { _ = os.RemoveAll(ws.TempDir) }()

	changes := DiffSnapshot(ws.ClaudeDir, ws.snapshot)
	for _, c := range changes {
		if c.Kind == "removed" {
			continue
		}
		content, err := os.ReadFile(c.Path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "workspace.Teardown: read %s: %v\n", c.Path, err)
			continue
		}
		uri := fmt.Sprintf("viking://resources/workspace_%s/claude-home/%s",
			ws.WorkspaceID, c.RelPath)
		if err := vc.CreateFile(ctx, uri, content); err != nil {
			fmt.Fprintf(os.Stderr, "workspace.Teardown: upload %s: %v\n", uri, err)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run all workspace tests**

Run: `cd /root/agentserver && go test ./internal/ccbroker/workspace/ -v`
Expected: all PASS, including `TestTakeAndDiffSnapshot`, `TestDiffSnapshotEmptyDirectory`, `TestSetupAndTeardown`.

- [ ] **Step 5: Commit**

```bash
git add internal/ccbroker/workspace/workspace.go internal/ccbroker/workspace/workspace_test.go
git commit -m "$(cat <<'EOF'
feat(ccbroker/workspace): Workspace.Setup + Teardown

Setup creates a per-turn temp dir tree (claude-config, project,
projects/ws_<wid>/memory), downloads workspace context from
OpenViking, snapshots claude-config for later diff. Teardown
uploads every added/modified file back to OpenViking and removes
the temp dir. Both fail-open on individual viking errors so a
flaky upload doesn't block the user-facing turn response.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Phase 2 — `runner/` subpackage

### Task 2.1: `runner/events.go` — SDKMessage → event payload (TDD)

**Files:**
- Create: `internal/ccbroker/runner/events.go`
- Create: `internal/ccbroker/runner/events_test.go`

We persist whatever the SDK emits as `json.RawMessage` payload, but we tag it with our internal `event_type` so the existing `agent_session_events` rows are queryable. The function is pure: input `agentsdk.SDKMessage`, output `(eventType string, payload json.RawMessage, ephemeral bool, err error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/ccbroker/runner/events_test.go`:

```go
package runner

import (
	"encoding/json"
	"testing"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"
)

func TestToEventPayload_AssistantMessage(t *testing.T) {
	raw := json.RawMessage(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`)
	msg := agentsdk.SDKMessage{Type: "assistant", Raw: raw}

	evt, err := ToEventPayload(msg)
	if err != nil {
		t.Fatalf("ToEventPayload: %v", err)
	}
	if evt.EventType != "assistant_message" {
		t.Fatalf("EventType=%q, want assistant_message", evt.EventType)
	}
	if !bytesEqual(evt.Payload, raw) {
		t.Fatalf("Payload not preserved verbatim")
	}
	if evt.Ephemeral {
		t.Fatalf("assistant messages must be persisted (Ephemeral=false)")
	}
}

func TestToEventPayload_StreamEventIsEphemeral(t *testing.T) {
	raw := json.RawMessage(`{"type":"stream_event","event":{"type":"content_block_delta"}}`)
	msg := agentsdk.SDKMessage{Type: "stream_event", Raw: raw}

	evt, err := ToEventPayload(msg)
	if err != nil {
		t.Fatalf("ToEventPayload: %v", err)
	}
	if !evt.Ephemeral {
		t.Fatalf("partial stream events must be marked ephemeral")
	}
}

func TestToEventPayload_KnownTypes(t *testing.T) {
	cases := []struct {
		sdkType, sdkSubtype, want string
		ephemeral                 bool
	}{
		{"user", "", "user_message", false},
		{"assistant", "", "assistant_message", false},
		{"tool_result", "", "tool_result", false},
		{"result", "success", "turn_result", false},
		{"system", "init", "system_init", false},
		{"system", "compact_boundary", "compact_boundary", false},
		{"stream_event", "", "stream_event", true},
		{"tool_progress", "", "tool_progress", true},
	}
	for _, c := range cases {
		raw := json.RawMessage(`{"type":"` + c.sdkType + `","subtype":"` + c.sdkSubtype + `"}`)
		msg := agentsdk.SDKMessage{Type: c.sdkType, Subtype: c.sdkSubtype, Raw: raw}
		evt, err := ToEventPayload(msg)
		if err != nil {
			t.Fatalf("[%s/%s] err: %v", c.sdkType, c.sdkSubtype, err)
		}
		if evt.EventType != c.want {
			t.Fatalf("[%s/%s] EventType=%q want %q", c.sdkType, c.sdkSubtype, evt.EventType, c.want)
		}
		if evt.Ephemeral != c.ephemeral {
			t.Fatalf("[%s/%s] Ephemeral=%v want %v", c.sdkType, c.sdkSubtype, evt.Ephemeral, c.ephemeral)
		}
	}
}

func bytesEqual(a, b json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

- [ ] **Step 2: Run, verify failure**

Run: `cd /root/agentserver && go test ./internal/ccbroker/runner/ -run TestToEventPayload -v`
Expected: build failure — `undefined: ToEventPayload`.

- [ ] **Step 3: Implement events.go**

Create `internal/ccbroker/runner/events.go`:

```go
package runner

import (
	"encoding/json"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"
)

// Event is the cc-broker-side projection of an SDK message ready to be
// inserted into agent_session_events and broadcast over SSE.
type Event struct {
	EventType string          // canonical short tag for our own queries
	Payload   json.RawMessage // verbatim SDK message JSON
	Ephemeral bool            // true = SSE-only, do not persist
}

// ToEventPayload classifies an SDKMessage. The raw JSON is preserved as
// payload so frontend consumers and audit replay see exactly what the SDK
// produced. The EventType field is our internal tag — useful for indexed
// queries — and is intentionally a small enumeration so new SDK message
// types fall through to a generic "sdk_event" without breaking callers.
func ToEventPayload(msg agentsdk.SDKMessage) (Event, error) {
	if len(msg.Raw) == 0 {
		// Defensive: SDK should always populate Raw, but tolerate empty
		// by emitting a minimal envelope so downstream INSERT does not panic.
		raw, err := json.Marshal(map[string]string{"type": msg.Type, "subtype": msg.Subtype})
		if err != nil {
			return Event{}, err
		}
		msg.Raw = raw
	}
	tag, ephemeral := classify(msg.Type, msg.Subtype)
	return Event{EventType: tag, Payload: msg.Raw, Ephemeral: ephemeral}, nil
}

func classify(sdkType, sdkSubtype string) (string, bool) {
	switch sdkType {
	case "user":
		return "user_message", false
	case "assistant":
		return "assistant_message", false
	case "tool_result":
		return "tool_result", false
	case "result":
		return "turn_result", false
	case "system":
		switch sdkSubtype {
		case "init":
			return "system_init", false
		case "compact_boundary":
			return "compact_boundary", false
		default:
			return "system_" + safeSubtype(sdkSubtype), false
		}
	case "stream_event":
		return "stream_event", true
	case "tool_progress":
		return "tool_progress", true
	default:
		return "sdk_event", false
	}
}

func safeSubtype(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
```

- [ ] **Step 4: Run tests, verify pass**

Run: `cd /root/agentserver && go test ./internal/ccbroker/runner/ -v`
Expected: PASS for all `TestToEventPayload_*`.

- [ ] **Step 5: Commit**

```bash
git add internal/ccbroker/runner/events.go internal/ccbroker/runner/events_test.go
git commit -m "$(cat <<'EOF'
feat(ccbroker/runner): ToEventPayload classifies SDKMessages

Pure function that maps an agentsdk.SDKMessage to (event_type,
payload, ephemeral) so handler_turns can persist + broadcast each
SDK event consistently. Stream/tool-progress events are marked
ephemeral (SSE-only); user/assistant/tool_result/result/system get
persisted into agent_session_events with stable tags.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 2.2: `runner/options.go` — `BuildClientOptions` (TDD)

**Files:**
- Create: `internal/ccbroker/runner/options.go`
- Create: `internal/ccbroker/runner/options_test.go`

A pure function that takes a `*workspace.Workspace`, a system prompt, an MCP server, an env passthrough map, and broker config; returns `[]agentsdk.QueryOption`. Tested by inspecting the resulting `queryConfig` — but `queryConfig` is unexported in the SDK. So we test by building options + applying them to a small introspector helper.

A simpler approach: the options function just builds a slice of `agentsdk.QueryOption` (which are functions). We can call each function on a local fake config struct in tests to verify the inputs were captured correctly. But that requires duplicating the SDK's config shape. Even simpler: `BuildClientOptions` returns its captured inputs as a side product `Spec` struct that the test can inspect. The SDK options are then derived from the Spec.

- [ ] **Step 1: Write the failing test**

Create `internal/ccbroker/runner/options_test.go`:

```go
package runner

import (
	"testing"

	"github.com/agentserver/agentserver/internal/ccbroker/workspace"
)

func TestBuildSpec(t *testing.T) {
	ws := &workspace.Workspace{
		WorkspaceID: "ws1",
		SessionID:   "cse_abc",
		ClaudeDir:   "/tmp/x/claude-config",
		ProjectDir:  "/tmp/x/project",
		MemoryDir:   "/tmp/x/claude-config/projects/ws_ws1/memory",
	}
	cfg := Config{
		SystemPrompt:                "you are a helpful assistant",
		MaxTurns:                    50,
		AnthropicAPIKey:             "",
		AnthropicAuthToken:          "tok-123",
		AnthropicBaseURL:            "https://gateway.example",
		DisableFileCheckpointing:    true,
		AutoCompactWindow:           165000,
	}
	spec := BuildSpec(ws, "cse_abc", cfg)

	if spec.Resume != "cse_abc" {
		t.Errorf("Resume=%q want cse_abc", spec.Resume)
	}
	if spec.Cwd != ws.ProjectDir {
		t.Errorf("Cwd=%q want %s", spec.Cwd, ws.ProjectDir)
	}
	wantEnv := map[string]string{
		"CLAUDE_CONFIG_DIR":                       ws.ClaudeDir,
		"CLAUDE_COWORK_MEMORY_PATH_OVERRIDE":      ws.MemoryDir,
		"ANTHROPIC_AUTH_TOKEN":                    "tok-123",
		"ANTHROPIC_BASE_URL":                      "https://gateway.example",
		"CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING":  "1",
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW":         "165000",
	}
	for k, v := range wantEnv {
		if spec.Env[k] != v {
			t.Errorf("Env[%q]=%q want %q", k, spec.Env[k], v)
		}
	}
	if _, ok := spec.Env["ANTHROPIC_API_KEY"]; ok {
		t.Errorf("ANTHROPIC_API_KEY should be omitted when empty")
	}

	wantTools := []string{"WebSearch", "WebFetch", "mcp__cc-broker__*"}
	if len(spec.AllowedTools) != len(wantTools) {
		t.Fatalf("AllowedTools=%v want %v", spec.AllowedTools, wantTools)
	}
	for i, w := range wantTools {
		if spec.AllowedTools[i] != w {
			t.Errorf("AllowedTools[%d]=%q want %q", i, spec.AllowedTools[i], w)
		}
	}
	if !spec.PermissionBypass {
		t.Errorf("PermissionBypass must be true")
	}
	if !spec.AllowDangerouslySkipPermissions {
		t.Errorf("AllowDangerouslySkipPermissions must be true (paired with PermissionBypass per SDK)")
	}
	if spec.MaxTurns != 50 {
		t.Errorf("MaxTurns=%d want 50", spec.MaxTurns)
	}
	if spec.SystemPrompt != "you are a helpful assistant" {
		t.Errorf("SystemPrompt mismatch")
	}
}

func TestBuildSpec_PrefersAPIKeyWhenBothSet(t *testing.T) {
	ws := &workspace.Workspace{ClaudeDir: "/c", ProjectDir: "/p", MemoryDir: "/m"}
	cfg := Config{
		AnthropicAPIKey:    "key-1",
		AnthropicAuthToken: "tok-2",
	}
	spec := BuildSpec(ws, "sid", cfg)

	if spec.Env["ANTHROPIC_API_KEY"] != "key-1" {
		t.Errorf("API_KEY not forwarded")
	}
	if spec.Env["ANTHROPIC_AUTH_TOKEN"] != "tok-2" {
		t.Errorf("AUTH_TOKEN should still be forwarded so CLI picks whichever it prefers")
	}
}
```

- [ ] **Step 2: Run, verify failure**

Run: `cd /root/agentserver && go test ./internal/ccbroker/runner/ -run TestBuildSpec -v`
Expected: build failure — `undefined: BuildSpec`, `undefined: Config`.

- [ ] **Step 3: Implement options.go**

Create `internal/ccbroker/runner/options.go`:

```go
package runner

import (
	"strconv"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"

	"github.com/agentserver/agentserver/internal/ccbroker/workspace"
)

// Config holds the broker-level configuration relevant to spawning a CC worker.
// All fields are populated from cc-broker's process env at startup.
type Config struct {
	SystemPrompt             string
	MaxTurns                 int
	AnthropicAPIKey          string
	AnthropicAuthToken       string
	AnthropicBaseURL         string
	DisableFileCheckpointing bool
	AutoCompactWindow        int
}

// Spec is the SDK-agnostic projection of "everything we are about to pass to
// the Claude SDK for one turn." It exists so tests can assert exactly what we
// would have asked the SDK to do, without depending on the SDK's unexported
// queryConfig. ToOptions() translates a Spec into an agentsdk option slice.
type Spec struct {
	Resume                          string
	Cwd                             string
	Env                             map[string]string
	SystemPrompt                    string
	AllowedTools                    []string
	PermissionBypass                bool
	AllowDangerouslySkipPermissions bool
	MaxTurns                        int
	McpServer                       *agentsdk.McpSdkServer
}

// BuildSpec composes a Spec from workspace + sessionID + config. Pure.
// Mirrors §2 of the design spec.
func BuildSpec(ws *workspace.Workspace, sessionID string, cfg Config) Spec {
	env := map[string]string{
		"CLAUDE_CONFIG_DIR":                  ws.ClaudeDir,
		"CLAUDE_COWORK_MEMORY_PATH_OVERRIDE": ws.MemoryDir,
	}
	if cfg.AnthropicAPIKey != "" {
		env["ANTHROPIC_API_KEY"] = cfg.AnthropicAPIKey
	}
	if cfg.AnthropicAuthToken != "" {
		env["ANTHROPIC_AUTH_TOKEN"] = cfg.AnthropicAuthToken
	}
	if cfg.AnthropicBaseURL != "" {
		env["ANTHROPIC_BASE_URL"] = cfg.AnthropicBaseURL
	}
	if cfg.DisableFileCheckpointing {
		env["CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING"] = "1"
	}
	if cfg.AutoCompactWindow > 0 {
		env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"] = strconv.Itoa(cfg.AutoCompactWindow)
	}
	return Spec{
		Resume:                          sessionID,
		Cwd:                             ws.ProjectDir,
		Env:                             env,
		SystemPrompt:                    cfg.SystemPrompt,
		AllowedTools:                    []string{"WebSearch", "WebFetch", "mcp__cc-broker__*"},
		PermissionBypass:                true,
		AllowDangerouslySkipPermissions: true,
		MaxTurns:                        cfg.MaxTurns,
	}
}

// ToOptions translates a Spec into the agentsdk option slice.
func (s Spec) ToOptions() []agentsdk.QueryOption {
	opts := []agentsdk.QueryOption{
		agentsdk.WithResume(s.Resume),
		agentsdk.WithCwd(s.Cwd),
		agentsdk.WithEnv(s.Env),
		agentsdk.WithSystemPrompt(s.SystemPrompt),
		agentsdk.WithAllowedTools(s.AllowedTools...),
	}
	if s.PermissionBypass {
		opts = append(opts, agentsdk.WithPermissionMode(agentsdk.PermissionBypassAll))
	}
	if s.AllowDangerouslySkipPermissions {
		opts = append(opts, agentsdk.WithAllowDangerouslySkipPermissions())
	}
	if s.MaxTurns > 0 {
		opts = append(opts, agentsdk.WithMaxTurns(s.MaxTurns))
	}
	if s.McpServer != nil {
		opts = append(opts, agentsdk.WithMcpServers(map[string]agentsdk.McpServerConfig{
			"cc-broker": {SDK: s.McpServer},
		}))
	}
	return opts
}
```

- [ ] **Step 4: Tests pass + commit**

```
cd /root/agentserver && go test ./internal/ccbroker/runner/ -v
git add internal/ccbroker/runner/options.go internal/ccbroker/runner/options_test.go
git commit -m "feat(ccbroker/runner): BuildSpec + ToOptions"
```

---

## Phases 2.3 → 7 (compact form)

The remaining phases follow the same TDD-then-commit cadence. To keep this plan readable, the rest is presented compactly: each task lists files, what to build (with key code shown only where novel), tests, and commit message. An executor (or subagent-driven-development driver) treats each numbered task as one bite-sized unit.

### Task 2.3 — `runner/runner.go` (sdkSession seam + Run)

**Files:** Create `internal/ccbroker/runner/runner.go`.

Implement `Run(ctx, ws, sessionID, userMessage, cfg, mcp) (<-chan agentsdk.SDKMessage, error)`:
1. `spec := BuildSpec(ws, sessionID, cfg); spec.McpServer = mcp`
2. `sess, err := newV1Session(ctx, spec.ToOptions())`
3. `sess.Send(ctx, userMessage)` — propagate err with `sess.Close()` on failure
4. Spawn a goroutine that drains `sess.Messages()` into a buffered output channel; closes channel + `sess.Close()` when SDK channel closes or `ctx.Done()`

Define `sdkSession` interface (`Send / Messages / Close`) per spec §1.5; `v1Session` wraps `*agentsdk.Client` (`NewClient(opts...).Connect(ctx)`, `client.Query(ctx, msg)`, `client.Messages()`, `client.Close()`). Verify the exact method names against `/root/claude-agent-sdk-go/client.go` before committing — adjust `v1Session` only if the SDK uses different names. Build verification: `go build ./internal/ccbroker/runner/...`. Commit: `feat(ccbroker/runner): Run + V1/V2 sdkSession adapter seam`.

### Task 3.1 — `tools/context.go`

Create struct `Context { SessionID, WorkspaceID, IMChannelID, IMUserID, ExecutorRegistryURL, AgentserverURL, InternalAPISecret string; Workspace *workspace.Workspace; Viking *workspace.VikingClient; HTTP *http.Client }`. Build + commit.

### Task 3.2 — `tools/executor.go` + `tools/executor_test.go`

Define typed input structs for each of `remote_{bash,read,edit,write,glob,grep,ls}` and `list_executors` (each has an `executor_id` field plus tool-specific args; `list_executors` takes `status_filter`). Provide `executorTools(tctx *Context) []agentsdk.McpTool`:

- All `remote_*` handlers route through `forwardExecute(tctx, toolName, args)` which:
  - JSON-marshals `{executor_id, tool, arguments}` (with `executor_id` stripped from `arguments` via JSON round-trip)
  - POSTs to `tctx.ExecutorRegistryURL + "/api/execute"`
  - Returns the response body wrapped in `&agentsdk.McpToolResult{Content: [{Type:"text", Text: body}]}`
  - On HTTP/network error returns `errResult(err)` (`IsError: true`)
- `list_executors` handler GETs `/api/executors?workspace_id=<wid>` and returns body verbatim

Helpers `errResult(err) *McpToolResult` and `textResult(s) *McpToolResult` are introduced here and reused by all subsequent tools/* files.

Test (`executor_test.go`): httptest server captures one `remote_bash` invocation; assert (a) URL hit, (b) body shape, (c) `executor_id` stripped from `arguments`, (d) `tool=Bash`. Run + commit.

### Task 3.3 — `tools/workspace.go` + `tools/workspace_test.go`

Three tools: `workspace_read`, `workspace_write`, `workspace_ls`. All operate against `tctx.Workspace.ClaudeDir` via `os.ReadFile / os.WriteFile / os.ReadDir`. Path safety helper `safeWorkspacePath(tctx, rel)` joins ClaudeDir + cleaned rel, rejects results that escape ClaudeDir (path traversal). `workspace_write` does `os.MkdirAll(filepath.Dir(p), 0o755)` before write.

Test: tempdir as `Workspace.ClaudeDir`; round-trip write→read→ls; separate test for path traversal returning `IsError`. Run + commit.

### Task 3.4 — `tools/im.go` + `tools/im_test.go`

Three tools: `send_message / send_image / send_file`. Each posts to `tctx.AgentserverURL + "/api/internal/im/send"` with body `{channel_id, user_id, kind, payload}` and `X-Internal-Secret` header from `tctx.InternalAPISecret`. Verify the URL path + payload shape against the current `mcp_router.go:routeToIM` (lines ~220-340) before committing — match it exactly so agentserver-side handler keeps working.

If `IMChannelID` or `IMUserID` are empty (turn was triggered by a non-IM caller), return `errResult(fmt.Errorf("not invoked from an IM turn"))`. httptest assertion verifies URL + body. Commit.

### Task 3.5 — `tools/scheduler.go` + `tools/askuser.go` (stubs)

Three scheduler tools (`create_scheduled_task / list_scheduled_tasks / cancel_scheduled_task`) and one ask-user tool (`AskUserQuestion`). All return `errResult(fmt.Errorf("<tool>: <subsystem> not yet implemented in agentserver"))`. Build + commit. Marker for future work: when agentserver gains a scheduler service or pending-question queue, swap stubs for real HTTP calls — tracked in spec §1.3 Non-Goals.

### Task 3.6 — `tools/router.go`

`BuildMcpServer(*Context) *agentsdk.McpSdkServer`: appends `executorTools(tctx)` + `workspaceTools(tctx)` + `imTools(tctx)` + `schedulerTools(tctx)` + `askUserTools(tctx)` into one slice, returns `agentsdk.CreateSdkMcpServer("cc-broker", "1.0.0", tools...)`. The server name `cc-broker` matches the wildcard `mcp__cc-broker__*` in the AllowedTools list. Build + commit.

---

## Phase 4 — Wire `handler_turns.go`

### Task 4.1 — Rewrite `handleProcessTurn` body

**Files:** Modify `internal/ccbroker/handler_turns.go`. Keep the function signature, body decode, validation, TurnLock acquire, ensure-session-exists, get-epoch, insert-user-message blocks unchanged. Replace the spawn-worker block (currently `s.SpawnWorker(...)`) and the wait-for-process goroutine with:

1. `vc := workspace.NewVikingClient(s.config.OpenVikingURL, s.config.OpenVikingAPIKey)`
2. `ws, err := workspace.Setup(r.Context(), req.WorkspaceID, req.SessionID, vc)` — on error: 500 + return
3. Construct `tctx := &tools.Context{...}` populating SessionID/WorkspaceID/IMChannelID/IMUserID/ExecutorRegistryURL/AgentserverURL/InternalAPISecret/Workspace=ws/Viking=vc/HTTP=http.DefaultClient
4. `mcp := tools.BuildMcpServer(tctx)`
5. Build `cfg := runner.Config{SystemPrompt, MaxTurns, AnthropicAPIKey, AnthropicAuthToken, AnthropicBaseURL from os.Getenv, DisableFileCheckpointing: true, AutoCompactWindow: 165000}`
6. `msgCh, err := runner.Run(r.Context(), ws, req.SessionID, req.UserMessage, cfg, mcp)` — on error: schedule background `workspace.Teardown(context.Background(), ws, vc)`, 500 + return

Then keep the existing flusher check + SSE response headers + `s.sse.Subscribe(req.SessionID)` block unchanged.

Replace the message-pump goroutine. New version: ranges over `msgCh`, calls `runner.ToEventPayload(sdkMsg)` per message; if `!evt.Ephemeral` inserts into `agent_session_events` via `s.store.InsertEvents(...)`; calls `s.sse.Broadcast(req.SessionID, sdkMsg)` for every message. After channel closes: broadcast a done sentinel and call `workspace.Teardown(context.Background(), ws, vc)` to upload changes + clean temp dir.

The bottom for-select loop that streams `sub.Ch` events to the HTTP response writer remains identical to today.

Add imports: the three new subpackages plus `"github.com/google/uuid"` (already present) and `"context"`/`"os"` if not. Remove imports for `os/exec` and any other now-unused symbols.

Build verification: `cd /root/agentserver && go build ./internal/ccbroker/...` — fix unused import warnings via `goimports -w internal/ccbroker/`. Commit: `feat(ccbroker): wire handler_turns to workspace+runner+tools`.

### Task 4.2 — Optional: handler_turns orchestration test

Defer if PR is already large. Pattern: declare package-level vars `var workspaceSetup = workspace.Setup; var runnerRun = runner.Run` in `handler_turns.go` so a test can swap them for fakes. Fake runner emits a scripted sequence of `agentsdk.SDKMessage`. Assert: TurnLock acquired+released; expected rows in `agent_session_events`; expected SSE broadcasts; teardown called. Commit if written.

### Task 4.3 — Trim `server.go` routes

**Files:** Modify `internal/ccbroker/server.go`. Delete the 6 bridge routes (`POST /v1/sessions/{sessionId}/bridge`, `GET .../worker/events/stream`, `POST .../worker/events`, `POST .../worker/internal-events`, `PUT .../worker/`, `POST .../worker/heartbeat`) and any Server-struct fields that only existed to serve them (e.g. `bridgeJWT`, `mcpHTTP`). Keep `/api/turns` and `/api/sessions`. Build verification deferred to Phase 5 (some references stay until the deletes happen). Commit: `chore(ccbroker): drop bridge routes from server.go`.

---

## Phase 5 — Delete old files

### Task 5.1 — Bulk `git rm`

```
cd /root/agentserver
git rm \
  internal/ccbroker/handler_bridge.go \
  internal/ccbroker/handler_events.go \
  internal/ccbroker/handler_internal_events.go \
  internal/ccbroker/handler_worker.go \
  internal/ccbroker/jwt.go \
  internal/ccbroker/middleware.go \
  internal/ccbroker/mcp_server.go \
  internal/ccbroker/mcp_router.go \
  internal/ccbroker/mcp_tools.go \
  internal/ccbroker/mcp_router_im_test.go \
  internal/ccbroker/mcp_server_test.go \
  internal/ccbroker/worker.go \
  internal/ccbroker/worker_test.go \
  internal/ccbroker/viking_client.go
```

Then `go build ./internal/ccbroker/...`. Likely cleanup: remove unused fields on Server struct in `server.go`; run `goimports -w internal/ccbroker/` to drop dead imports. Then `go test -race ./internal/ccbroker/...` — should PASS for `workspace`, `runner`, `tools`. Commit: `chore(ccbroker): remove dead bridge + old worker files (~1500 LOC)`.

### Task 5.2 — Prune `integration_test.go`

`grep -n "^func Test" internal/ccbroker/integration_test.go` to inventory. If every test asserted against bridge endpoints, `git rm` it. Otherwise delete the bridge-flavoured test functions, keep the rest. Run `go test -race ./internal/ccbroker/...` to confirm tree is green. Commit.

---

## Phase 6 — Final verify

### Task 6.1 — Build + vet + race tests across the repo

```
cd /root/agentserver
go build ./...
go vet ./...
go test -race ./internal/ccbroker/...
```

All three should be 0-error / PASS. No commit needed (prior tasks already left tree clean).

---

## Phase 7 — Deploy + smoke

### Task 7.1 — Push + open PR

`git push -u github feature/ccbroker-sdk-worker` then `gh pr create --title "feat(ccbroker): in-process Claude Agent SDK worker" --body ...`. PR body summarises the rewrite, links the spec, lists the test plan from §6, calls out scheduler/askuser stubs and OpenViking tenant-auth as known residuals (per spec §1.3 + §7).

### Task 7.2 — Merge + rollout

Wait for `build-cc-broker` green (other build-* should also pass since PRs #45/#46/#47 fixed CI). Then `gh pr merge <N> --merge --delete-branch`. Capture old digest, `kubectl rollout restart deploy/agentserver-ccbroker`, wait for `rollout status` success, capture new digest — assert digests differ. Confirm pod logs show only `cc-broker listening on :8085` (no MCP / sdk-url / API_KEY errors).

### Task 7.3 — Smoke test via WeChat

Send a message into the `routing_mode=stateless_cc` channel (Mr.YAO's Workspace, channel `a1e97fdf-b1d9-49f2-a95a-3a28550b89e6`). Tail four logs in parallel:

- `kubectl logs deploy/agentserver-imbridge` — expect `forward to agentserver` (no connection refused)
- `kubectl logs deploy/agentserver` — expect `POST /im/inbound` 202
- `kubectl logs deploy/agentserver-ccbroker` — expect `POST /api/turns` 200 with **no** `ANTHROPIC_API_KEY`/`Invalid MCP configuration`/`--sdk-url rejected` errors
- (optional) tail `agent_session_events` row count

Verify: WeChat reply lands; `agent_session_events` count grew by ≥2 (user + assistant). If tenant-auth is fixed beforehand, also verify the session `.jsonl` was uploaded back to OpenViking (`viking://resources/workspace_<wid>/claude-home/.claude/projects/<proj_hash>/<sid>.jsonl`).

---

## Self-Review

**Spec coverage** — every spec section maps to one or more tasks:

| Spec section | Implementing tasks |
|---|---|
| §1.1 / §1.2 Problem & Goal | Whole plan |
| §1.3 Non-Goals | Honoured throughout: no schema change (Phase 6 verifies); no edits to agentserver/imbridge/executor-registry/openviking (only `internal/ccbroker/` touched); scheduler+askuser are explicit stubs (Task 3.5) |
| §1.4 Relation to prior spec | Phase 5 removes bridge files; Task 4.1 wires SDK path |
| §1.5 SDK V1+V2 alignment | `runner/runner.go` `sdkSession` adapter (Task 2.3); `runner/options.go` includes `WithAllowDangerouslySkipPermissions` (Task 2.2) |
| §2 Architecture (full opts list) | Task 2.2 covers Resume/Cwd/Env/SystemPrompt/AllowedTools/PermissionBypass/AllowDangerouslySkip/MaxTurns/McpServer; env passthrough for `ANTHROPIC_*` and `CLAUDE_CODE_*` |
| §3 File structure | Tasks 1.x / 2.x / 3.x create new files; Task 5.1 removes old files |
| §4 Data flow | Task 4.1 wires the orchestration |
| §5 Error handling | Setup fail-open in Task 1.3; runner failure path in Task 4.1; tool errors via `errResult` (Tasks 3.2-3.6) |
| §6 Testing | Pure-unit TDD on snapshot/events/options; fake-based tool tests; orchestration test in Task 4.2 (optional) |
| §7 Migration | Phase 7 deploy + rollout; spec calls out `persistSession`/`settingSources` defaults — `runner/options.go` deliberately does NOT call `WithPersistSession` or `WithSettingSources`, honouring those defaults |
| §8 Open Risks | CLI version pinning is a deployment concern (existing image build); SDK protocol drift covered by `go test ./...` in CI |

**Placeholder scan**: searched the plan for `TBD`/`TODO`/`FIXME`/`fill in`/`similar to Task` — none in tasks themselves. The "verify against /root/claude-agent-sdk-go/client.go" guidance in Task 2.3 and the "verify URL path against mcp_router.go:routeToIM" in Task 3.4 are guarded fact-checks, not unspecified requirements.

**Type / name consistency**: `Workspace`, `Spec`, `Config`, `Context`, `Event`, `sdkSession`, `BuildSpec`, `BuildMcpServer`, `Run`, `ToEventPayload`, `Setup`, `Teardown`, `TakeSnapshot`, `DiffSnapshot`, `errResult`, `textResult` — used consistently across phases. Package import paths align with the file structure table.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-05-02-ccbroker-sdk-worker.md`. Two execution options:

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints

Which approach?