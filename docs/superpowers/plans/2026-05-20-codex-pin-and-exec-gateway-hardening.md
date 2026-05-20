# codex-pin + exec-gateway hardening (PR 1) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement PR 1 from the [Full Upstream Alignment spec](../specs/2026-05-20-codex-gateway-full-alignment-design.md): pin agentserver to a specific upstream `openai/codex` tag with CI-enforced drift detection, and close three operational gaps in `codex-exec-gateway` (unbounded read limits, no idle reaper, no per-executor stream cap).

**Architecture:** Add a Go-based pin checker (`cmd/check-codex-pin`) that verifies sha256 of upstream-tracked protocol artifacts against a checked-in `codex-pin.json`. Extend `codexexecgateway.Config` with three new bounded-resource knobs and wire them into the inbound/bridge ws lifecycle.

**Tech Stack:** Go, `nhooyr.io/websocket`, `google.golang.org/protobuf/proto`, plain `os/exec` for git clone, GitHub Actions for CI.

---

## File structure

**Create:**
- `codex-pin.json` — repo root, JSON manifest pinning upstream tag + sha256 of tracked files
- `cmd/check-codex-pin/main.go` — CLI program: verifies pin against an upstream copy
- `cmd/check-codex-pin/verify.go` — core verification logic (separated for testability)
- `cmd/check-codex-pin/verify_test.go` — unit tests with fixture-based offline mode
- `cmd/check-codex-pin/testdata/upstream-ok/` — fixture: minimal upstream tree matching pin
- `cmd/check-codex-pin/testdata/upstream-drift/` — fixture: one file with mutated sha

**Modify:**
- `Makefile` — add `codex-pin-check` and `codex-pin-bump` targets
- `.github/workflows/build.yml` — add pin-check job (parallel to existing `test` job)
- `internal/codexexecgateway/config.go` — add `MaxFrameBytes`, `BridgeIdleTimeout`, `MaxStreamsPerExecutor` fields + env loaders
- `internal/codexexecgateway/inbound.go` — replace `SetReadLimit(-1)` with `SetReadLimit(cfg.MaxFrameBytes)`; pass config into `newInboundConn`
- `internal/codexexecgateway/bridge.go` — replace `SetReadLimit(-1)`; check stream cap before `addRoute`; return 503 on cap exceeded; touch lastActivity on first frame
- `internal/codexexecgateway/multiplex.go` — add `lastActivity atomic.Int64` to `bridgeSession`; add `streamCount()` method; `inboundConn.startIdleReaper(idleTimeout)` goroutine that sends `RelayReset` and closes idle sessions
- `internal/codexexecgateway/server.go` — pass `cfg` into `newInboundConn` and bridge handler; start idle reaper

**Test:**
- `cmd/check-codex-pin/verify_test.go` — pin verification tests
- `internal/codexexecgateway/inbound_test.go` — add test for `SetReadLimit` enforcement
- `internal/codexexecgateway/bridge_test.go` — add tests for stream cap
- `internal/codexexecgateway/multiplex_test.go` — add tests for idle reaper

---

## Task 1: Bootstrap `codex-pin.json`

**Files:**
- Create: `codex-pin.json`

- [ ] **Step 1: Resolve upstream tag sha**

Run:
```bash
git ls-remote https://github.com/openai/codex.git refs/tags/rust-v0.131.0-alpha.22 | awk '{print $1}'
```

Record the sha printed (used as `<UPSTREAM_SHA>` below).

- [ ] **Step 2: Compute sha256 of current relay.proto (after stripping the Go-specific `option go_package` directive)**

Our local `internal/relaypb/relay.proto` is upstream's `codex.exec_server.relay.v1.proto` PLUS one local-only line: `option go_package = "github.com/agentserver/agentserver/internal/relaypb;relaypb";` (needed for `protoc-gen-go`; upstream doesn't need it because they use Rust's `prost`).

So byte-equality with upstream is impossible; we instead compare the schemas after stripping that directive (and any consecutive blank line that followed it):

```bash
sed -E '/^option go_package[[:space:]]*=/,/^$/d' internal/relaypb/relay.proto | sha256sum | awk '{print $1}'
```

Record the sha (used as `<RELAY_PROTO_NORMALIZED_SHA256>` below).

- [ ] **Step 3: Fetch upstream blob shas for the tracked files**

Run (replace `<UPSTREAM_SHA>` with the value from Step 1):
```bash
mkdir -p /tmp/codex-pin-bootstrap && cd /tmp/codex-pin-bootstrap
git clone --depth 1 --branch rust-v0.131.0-alpha.22 https://github.com/openai/codex.git
cd codex
for f in \
  codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto \
  codex-rs/app-server-protocol/src/protocol/v1.rs \
  codex-rs/app-server-protocol/src/protocol/v2/item.rs \
  codex-rs/app-server-protocol/src/protocol/v2/mcp.rs ; do
  printf '%s  %s\n' "$(sha256sum "$f" | awk '{print $1}')" "$f"
done
```

Record each sha for the JSON below.

- [ ] **Step 4: Verify upstream and local relay.proto match after strip-normalization**

Run:
```bash
tmp_upstream=$(mktemp -d)
git clone --depth 1 --branch rust-v0.131.0-alpha.22 https://github.com/openai/codex.git "$tmp_upstream/codex"
upstream_norm=$(sed -E '/^option go_package[[:space:]]*=/,/^$/d' "$tmp_upstream/codex/codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto" | sha256sum | awk '{print $1}')
local_norm=$(sed -E '/^option go_package[[:space:]]*=/,/^$/d' internal/relaypb/relay.proto | sha256sum | awk '{print $1}')
if [ "$upstream_norm" != "$local_norm" ]; then
  echo "MISMATCH after strip-normalize"; echo "upstream_norm=$upstream_norm"; echo "local_norm=$local_norm"
  exit 1
fi
echo "normalized schemas match: $upstream_norm"
rm -rf "$tmp_upstream"
```

Record `<RELAY_PROTO_NORMALIZED_SHA256>` = the matching value.

- [ ] **Step 5: Write `codex-pin.json` at repo root**

```json
{
  "upstream_repo": "openai/codex",
  "tag": "rust-v0.131.0-alpha.22",
  "sha": "<UPSTREAM_SHA>",
  "tracked_files": {
    "codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto": "<sha-from-step-3>",
    "codex-rs/app-server-protocol/src/protocol/v1.rs": "<sha-from-step-3>",
    "codex-rs/app-server-protocol/src/protocol/v2/item.rs": "<sha-from-step-3>",
    "codex-rs/app-server-protocol/src/protocol/v2/mcp.rs": "<sha-from-step-3>"
  },
  "normalized_equivalent_files": {
    "internal/relaypb/relay.proto": {
      "upstream_path": "codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto",
      "normalized_sha256": "<RELAY_PROTO_NORMALIZED_SHA256>",
      "comment": "Strip Go-only `option go_package = \"...\";` directive and the blank line that follows it, then compare schemas. Required because agentserver's protoc-gen-go needs this directive; upstream's prost (Rust) doesn't."
    }
  },
  "approval_methods": [
    "item/commandExecution/requestApproval",
    "item/fileChange/requestApproval",
    "item/permissions/requestApproval",
    "item/tool/requestUserInput",
    "mcpServer/elicitation/request"
  ]
}
```

- [ ] **Step 6: Commit**

```bash
git add codex-pin.json
git commit -m "feat(codex-pin): bootstrap pin to rust-v0.131.0-alpha.22"
```

---

## Task 2: `verify.go` — core verification logic (TDD)

**Files:**
- Create: `cmd/check-codex-pin/verify.go`
- Create: `cmd/check-codex-pin/verify_test.go`
- Create: `cmd/check-codex-pin/testdata/upstream-ok/` (fixture tree)
- Create: `cmd/check-codex-pin/testdata/upstream-drift/`

- [ ] **Step 1: Create fixture directories**

```bash
mkdir -p cmd/check-codex-pin/testdata/upstream-ok/codex-rs/exec-server/src/proto
mkdir -p cmd/check-codex-pin/testdata/upstream-ok/codex-rs/app-server-protocol/src/protocol/v2

# Copy the real relay.proto (it's our canonical "upstream-ok" copy)
cp internal/relaypb/relay.proto cmd/check-codex-pin/testdata/upstream-ok/codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto

# Minimal stand-ins for v1.rs/item.rs/mcp.rs — content doesn't matter, only the sha
echo "// fixture v1.rs" > cmd/check-codex-pin/testdata/upstream-ok/codex-rs/app-server-protocol/src/protocol/v1.rs
echo "// fixture item.rs" > cmd/check-codex-pin/testdata/upstream-ok/codex-rs/app-server-protocol/src/protocol/v2/item.rs
echo "// fixture mcp.rs" > cmd/check-codex-pin/testdata/upstream-ok/codex-rs/app-server-protocol/src/protocol/v2/mcp.rs

# Drift fixture: same files but item.rs has a different sha
cp -r cmd/check-codex-pin/testdata/upstream-ok cmd/check-codex-pin/testdata/upstream-drift
echo "// drifted item.rs" > cmd/check-codex-pin/testdata/upstream-drift/codex-rs/app-server-protocol/src/protocol/v2/item.rs
```

- [ ] **Step 2: Write the failing test for "OK case"**

`cmd/check-codex-pin/verify_test.go`:
```go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func mustHashFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func writePinFile(t *testing.T, tracked map[string]string) string {
	t.Helper()
	pin := Pin{
		UpstreamRepo: "openai/codex",
		Tag:          "rust-v0.131.0-alpha.22",
		Sha:          "deadbeef",
		TrackedFiles: tracked,
		ByteIdenticalFiles: map[string]string{
			"internal/relaypb/relay.proto": "codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto",
		},
		ApprovalMethods: []string{
			"item/commandExecution/requestApproval",
			"item/fileChange/requestApproval",
			"item/permissions/requestApproval",
			"item/tool/requestUserInput",
			"mcpServer/elicitation/request",
		},
	}
	b, _ := json.Marshal(pin)
	path := filepath.Join(t.TempDir(), "codex-pin.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write pin: %v", err)
	}
	return path
}

func TestVerify_OK(t *testing.T) {
	upstream := "testdata/upstream-ok"
	tracked := map[string]string{
		"codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto": mustHashFile(t, filepath.Join(upstream, "codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto")),
		"codex-rs/app-server-protocol/src/protocol/v1.rs":                 mustHashFile(t, filepath.Join(upstream, "codex-rs/app-server-protocol/src/protocol/v1.rs")),
		"codex-rs/app-server-protocol/src/protocol/v2/item.rs":            mustHashFile(t, filepath.Join(upstream, "codex-rs/app-server-protocol/src/protocol/v2/item.rs")),
		"codex-rs/app-server-protocol/src/protocol/v2/mcp.rs":             mustHashFile(t, filepath.Join(upstream, "codex-rs/app-server-protocol/src/protocol/v2/mcp.rs")),
	}
	pinPath := writePinFile(t, tracked)

	// repo root for byte-identical check: use the upstream fixture as if it's our repo,
	// so internal/relaypb/relay.proto points at the same file as the tracked proto.
	repoRoot := t.TempDir()
	internalDir := filepath.Join(repoRoot, "internal/relaypb")
	if err := os.MkdirAll(internalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(upstream, "codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto"))
	if err := os.WriteFile(filepath.Join(internalDir, "relay.proto"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Verify(pinPath, repoRoot, upstream)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(report.Mismatches) != 0 {
		t.Fatalf("expected no mismatches, got %+v", report.Mismatches)
	}
}
```

- [ ] **Step 3: Run test to verify it fails (no `Pin` type yet)**

Run: `go test ./cmd/check-codex-pin -run TestVerify_OK -v`
Expected: FAIL (build error — `Pin`/`Verify` undefined)

- [ ] **Step 4: Implement minimal `verify.go`**

`cmd/check-codex-pin/verify.go`:
```go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Pin mirrors codex-pin.json.
type Pin struct {
	UpstreamRepo       string            `json:"upstream_repo"`
	Tag                string            `json:"tag"`
	Sha                string            `json:"sha"`
	TrackedFiles       map[string]string `json:"tracked_files"`        // path-in-upstream → sha256
	ByteIdenticalFiles map[string]string `json:"byte_identical_files"` // path-in-this-repo → path-in-upstream
	ApprovalMethods    []string          `json:"approval_methods"`
}

// Mismatch describes one failing assertion.
type Mismatch struct {
	File   string
	Reason string // "tracked-sha", "byte-identical", "missing"
	Want   string
	Got    string
}

// Report aggregates all mismatches from a Verify call.
type Report struct {
	Mismatches []Mismatch
}

// Verify checks the pin against an upstream source tree (already checked out
// somewhere local). repoRoot is the root of THIS repo (so byte-identical
// checks can read our own files).
func Verify(pinPath, repoRoot, upstreamRoot string) (*Report, error) {
	rep := &Report{}
	pinBytes, err := os.ReadFile(pinPath)
	if err != nil {
		return nil, fmt.Errorf("read pin: %w", err)
	}
	var pin Pin
	if err := json.Unmarshal(pinBytes, &pin); err != nil {
		return nil, fmt.Errorf("parse pin: %w", err)
	}

	for relPath, wantSha := range pin.TrackedFiles {
		abs := filepath.Join(upstreamRoot, relPath)
		got, err := hashFile(abs)
		if err != nil {
			rep.Mismatches = append(rep.Mismatches, Mismatch{File: relPath, Reason: "missing", Want: wantSha, Got: err.Error()})
			continue
		}
		if got != wantSha {
			rep.Mismatches = append(rep.Mismatches, Mismatch{File: relPath, Reason: "tracked-sha", Want: wantSha, Got: got})
		}
	}

	for ourPath, upstreamPath := range pin.ByteIdenticalFiles {
		our, errOur := hashFile(filepath.Join(repoRoot, ourPath))
		ups, errUps := hashFile(filepath.Join(upstreamRoot, upstreamPath))
		if errOur != nil {
			rep.Mismatches = append(rep.Mismatches, Mismatch{File: ourPath, Reason: "missing", Got: errOur.Error()})
			continue
		}
		if errUps != nil {
			rep.Mismatches = append(rep.Mismatches, Mismatch{File: upstreamPath, Reason: "missing", Got: errUps.Error()})
			continue
		}
		if our != ups {
			rep.Mismatches = append(rep.Mismatches, Mismatch{File: ourPath, Reason: "byte-identical", Want: ups, Got: our})
		}
	}

	return rep, nil
}

func hashFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
```

Also create a stub `main.go` so the package builds:
```go
package main

func main() {}
```

- [ ] **Step 5: Run OK test, verify it passes**

Run: `go test ./cmd/check-codex-pin -run TestVerify_OK -v`
Expected: PASS

- [ ] **Step 6: Add drift test**

Append to `verify_test.go`:
```go
func TestVerify_DriftDetected(t *testing.T) {
	upstream := "testdata/upstream-drift"
	// Pin uses the OK fixture's shas; upstream-drift mutated item.rs.
	tracked := map[string]string{
		"codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto": mustHashFile(t, "testdata/upstream-ok/codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto"),
		"codex-rs/app-server-protocol/src/protocol/v1.rs":                 mustHashFile(t, "testdata/upstream-ok/codex-rs/app-server-protocol/src/protocol/v1.rs"),
		"codex-rs/app-server-protocol/src/protocol/v2/item.rs":            mustHashFile(t, "testdata/upstream-ok/codex-rs/app-server-protocol/src/protocol/v2/item.rs"),
		"codex-rs/app-server-protocol/src/protocol/v2/mcp.rs":             mustHashFile(t, "testdata/upstream-ok/codex-rs/app-server-protocol/src/protocol/v2/mcp.rs"),
	}
	pinPath := writePinFile(t, tracked)

	repoRoot := t.TempDir()
	internalDir := filepath.Join(repoRoot, "internal/relaypb")
	if err := os.MkdirAll(internalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(upstream, "codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto"))
	if err := os.WriteFile(filepath.Join(internalDir, "relay.proto"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Verify(pinPath, repoRoot, upstream)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(report.Mismatches) != 1 {
		t.Fatalf("expected exactly 1 mismatch (item.rs drift), got %d: %+v", len(report.Mismatches), report.Mismatches)
	}
	m := report.Mismatches[0]
	if m.File != "codex-rs/app-server-protocol/src/protocol/v2/item.rs" {
		t.Errorf("mismatch file: got %q, want item.rs", m.File)
	}
	if m.Reason != "tracked-sha" {
		t.Errorf("mismatch reason: got %q, want tracked-sha", m.Reason)
	}
}
```

- [ ] **Step 7: Run drift test, verify it passes (verify.go already handles it)**

Run: `go test ./cmd/check-codex-pin -run TestVerify_DriftDetected -v`
Expected: PASS

- [ ] **Step 8: Add byte-identical-mismatch test**

```go
func TestVerify_ByteIdenticalMismatch(t *testing.T) {
	upstream := "testdata/upstream-ok"
	tracked := map[string]string{
		"codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto": mustHashFile(t, filepath.Join(upstream, "codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto")),
		"codex-rs/app-server-protocol/src/protocol/v1.rs":                 mustHashFile(t, filepath.Join(upstream, "codex-rs/app-server-protocol/src/protocol/v1.rs")),
		"codex-rs/app-server-protocol/src/protocol/v2/item.rs":            mustHashFile(t, filepath.Join(upstream, "codex-rs/app-server-protocol/src/protocol/v2/item.rs")),
		"codex-rs/app-server-protocol/src/protocol/v2/mcp.rs":             mustHashFile(t, filepath.Join(upstream, "codex-rs/app-server-protocol/src/protocol/v2/mcp.rs")),
	}
	pinPath := writePinFile(t, tracked)

	repoRoot := t.TempDir()
	internalDir := filepath.Join(repoRoot, "internal/relaypb")
	if err := os.MkdirAll(internalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a DIFFERENT relay.proto in our repo — should trip the byte-identical check.
	if err := os.WriteFile(filepath.Join(internalDir, "relay.proto"), []byte("// mutated proto\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Verify(pinPath, repoRoot, upstream)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	found := false
	for _, m := range report.Mismatches {
		if m.File == "internal/relaypb/relay.proto" && m.Reason == "byte-identical" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected byte-identical mismatch on relay.proto, got %+v", report.Mismatches)
	}
}
```

Run: `go test ./cmd/check-codex-pin -v`
Expected: all 3 tests PASS

- [ ] **Step 9: Commit**

```bash
git add cmd/check-codex-pin/
git commit -m "feat(codex-pin): verifier with sha + byte-identical checks"
```

---

## Task 3: `main.go` — CLI wrapper that fetches upstream and runs Verify

**Files:**
- Modify: `cmd/check-codex-pin/main.go`

- [ ] **Step 1: Write `main.go`**

```go
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	pinPath := flag.String("pin", "codex-pin.json", "path to codex-pin.json")
	repoRoot := flag.String("repo-root", ".", "path to this repo's root")
	upstreamSource := flag.String("upstream-source", "", "local path to upstream codex checkout (if empty, will clone)")
	flag.Parse()

	upstream := *upstreamSource
	if upstream == "" {
		var err error
		upstream, err = cloneUpstream(*pinPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "clone upstream: %v\n", err)
			os.Exit(2)
		}
		defer os.RemoveAll(upstream)
	}

	report, err := Verify(*pinPath, *repoRoot, upstream)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify: %v\n", err)
		os.Exit(2)
	}
	if len(report.Mismatches) == 0 {
		fmt.Println("codex-pin: OK")
		return
	}
	fmt.Fprintf(os.Stderr, "codex-pin: %d mismatch(es):\n", len(report.Mismatches))
	for _, m := range report.Mismatches {
		fmt.Fprintf(os.Stderr, "  %s [%s] want=%s got=%s\n", m.File, m.Reason, m.Want, m.Got)
	}
	os.Exit(1)
}

func cloneUpstream(pinPath string) (string, error) {
	pinBytes, err := os.ReadFile(pinPath)
	if err != nil {
		return "", err
	}
	var pin struct {
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal(pinBytes, &pin); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp("", "codex-pin-*")
	if err != nil {
		return "", err
	}
	dst := filepath.Join(tmp, "codex")
	cmd := exec.Command("git", "clone", "--depth", "1", "--branch", pin.Tag, "https://github.com/openai/codex.git", dst)
	cmd.Stdout = os.Stderr // git progress to stderr, keep stdout clean for our output
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("git clone: %w", err)
	}
	return dst, nil
}

```

Top-of-file imports in `main.go`:
```go
import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)
```

- [ ] **Step 2: Verify it builds**

Run: `go build ./cmd/check-codex-pin`
Expected: success, binary produced at `./check-codex-pin` (delete after: `rm check-codex-pin`)

- [ ] **Step 3: Run offline against fixture**

```bash
go run ./cmd/check-codex-pin -pin cmd/check-codex-pin/testdata/upstream-ok-pin.json -repo-root cmd/check-codex-pin/testdata/upstream-ok -upstream-source cmd/check-codex-pin/testdata/upstream-ok
```

Expected: error — `upstream-ok-pin.json` doesn't exist yet. (This is fine; the offline smoke is covered by unit tests.)

- [ ] **Step 4: Run online against real upstream**

```bash
go run ./cmd/check-codex-pin
```

Expected: prints `codex-pin: OK` (since the codex-pin.json bootstrapped in Task 1 should match upstream).

If it fails with mismatches, double-check Task 1's sha values.

- [ ] **Step 5: Commit**

```bash
git add cmd/check-codex-pin/main.go
git commit -m "feat(codex-pin): CLI wrapper with online git-clone fetcher"
```

---

## Task 4: Makefile targets

**Files:**
- Modify: `Makefile`

- [ ] **Step 1: Add targets**

Append to `Makefile`:
```makefile
codex-pin-check:
	go run ./cmd/check-codex-pin

codex-pin-bump:
	@if [ -z "$(TAG)" ]; then echo "usage: make codex-pin-bump TAG=rust-v0.x.y"; exit 2; fi
	@echo "Bumping codex-pin to $(TAG)..."
	@tmp=$$(mktemp -d) && \
	  git clone --depth 1 --branch $(TAG) https://github.com/openai/codex.git $$tmp/codex && \
	  sha=$$(cd $$tmp/codex && git rev-parse HEAD) && \
	  relay_sha=$$(sha256sum $$tmp/codex/codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto | awk '{print $$1}') && \
	  v1_sha=$$(sha256sum $$tmp/codex/codex-rs/app-server-protocol/src/protocol/v1.rs | awk '{print $$1}') && \
	  item_sha=$$(sha256sum $$tmp/codex/codex-rs/app-server-protocol/src/protocol/v2/item.rs | awk '{print $$1}') && \
	  mcp_sha=$$(sha256sum $$tmp/codex/codex-rs/app-server-protocol/src/protocol/v2/mcp.rs | awk '{print $$1}') && \
	  cp $$tmp/codex/codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto internal/relaypb/relay.proto && \
	  echo "{" > codex-pin.json && \
	  echo "  \"upstream_repo\": \"openai/codex\"," >> codex-pin.json && \
	  echo "  \"tag\": \"$(TAG)\"," >> codex-pin.json && \
	  echo "  \"sha\": \"$$sha\"," >> codex-pin.json && \
	  echo "  \"tracked_files\": {" >> codex-pin.json && \
	  echo "    \"codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto\": \"$$relay_sha\"," >> codex-pin.json && \
	  echo "    \"codex-rs/app-server-protocol/src/protocol/v1.rs\": \"$$v1_sha\"," >> codex-pin.json && \
	  echo "    \"codex-rs/app-server-protocol/src/protocol/v2/item.rs\": \"$$item_sha\"," >> codex-pin.json && \
	  echo "    \"codex-rs/app-server-protocol/src/protocol/v2/mcp.rs\": \"$$mcp_sha\"" >> codex-pin.json && \
	  echo "  }," >> codex-pin.json && \
	  echo "  \"byte_identical_files\": {" >> codex-pin.json && \
	  echo "    \"internal/relaypb/relay.proto\": \"codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto\"" >> codex-pin.json && \
	  echo "  }," >> codex-pin.json && \
	  echo "  \"approval_methods\": [" >> codex-pin.json && \
	  echo "    \"item/commandExecution/requestApproval\"," >> codex-pin.json && \
	  echo "    \"item/fileChange/requestApproval\"," >> codex-pin.json && \
	  echo "    \"item/permissions/requestApproval\"," >> codex-pin.json && \
	  echo "    \"item/tool/requestUserInput\"," >> codex-pin.json && \
	  echo "    \"mcpServer/elicitation/request\"" >> codex-pin.json && \
	  echo "  ]" >> codex-pin.json && \
	  echo "}" >> codex-pin.json && \
	  rm -rf $$tmp && \
	  echo "Bumped to $(TAG). Verify with: make codex-pin-check"
```

Also extend the `.PHONY` line at the top of the Makefile:
```makefile
.PHONY: dev build clean frontend backend agent agent-all llmproxy credentialproxy test docker docker-agent docker-llmproxy docker-credentialproxy docker-openclaw docker-all codex-pin-check codex-pin-bump
```

- [ ] **Step 2: Smoke test**

Run: `make codex-pin-check`
Expected: `codex-pin: OK`

- [ ] **Step 3: Commit**

```bash
git add Makefile
git commit -m "feat(codex-pin): make targets codex-pin-check and codex-pin-bump"
```

---

## Task 5: CI integration

**Files:**
- Modify: `.github/workflows/build.yml`

- [ ] **Step 1: Add `codex-pin-check` job**

Insert after the existing `test:` job (parallel to `build-server:`):

```yaml
  codex-pin:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6

      - uses: actions/setup-go@v6
        with:
          go-version-file: go.mod

      - name: Verify codex-pin
        run: make codex-pin-check
```

- [ ] **Step 2: Verify YAML is valid**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/build.yml'))"`
Expected: no output (no exception)

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/build.yml
git commit -m "ci: add codex-pin verification job"
```

---

## Task 6: exec-gateway Config — three new bounded-resource knobs

**Files:**
- Modify: `internal/codexexecgateway/config.go`
- Modify: `internal/codexexecgateway/config_test.go`

- [ ] **Step 1: Write failing test for defaults + env override**

In `config_test.go`, append:
```go
func TestLoadConfig_BoundedResourceDefaults(t *testing.T) {
	setRequiredConfigEnv(t)  // helper that sets DB URL + HMAC secret + internal secret
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if cfg.MaxFrameBytes != 16*1024*1024 {
		t.Errorf("MaxFrameBytes default: got %d, want 16 MiB", cfg.MaxFrameBytes)
	}
	if cfg.BridgeIdleTimeout != 5*time.Minute {
		t.Errorf("BridgeIdleTimeout default: got %v, want 5m", cfg.BridgeIdleTimeout)
	}
	if cfg.MaxStreamsPerExecutor != 32 {
		t.Errorf("MaxStreamsPerExecutor default: got %d, want 32", cfg.MaxStreamsPerExecutor)
	}
}

func TestLoadConfig_BoundedResourceEnvOverride(t *testing.T) {
	setRequiredConfigEnv(t)
	t.Setenv("CXG_MAX_FRAME_BYTES", "1048576")            // 1 MiB
	t.Setenv("CXG_BRIDGE_IDLE_TIMEOUT", "30s")
	t.Setenv("CXG_MAX_STREAMS_PER_EXECUTOR", "8")
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if cfg.MaxFrameBytes != 1024*1024 {
		t.Errorf("MaxFrameBytes override: got %d, want 1 MiB", cfg.MaxFrameBytes)
	}
	if cfg.BridgeIdleTimeout != 30*time.Second {
		t.Errorf("BridgeIdleTimeout override: got %v, want 30s", cfg.BridgeIdleTimeout)
	}
	if cfg.MaxStreamsPerExecutor != 8 {
		t.Errorf("MaxStreamsPerExecutor override: got %d, want 8", cfg.MaxStreamsPerExecutor)
	}
}
```

If `setRequiredConfigEnv` doesn't already exist in `config_test.go`, define it inline at the top of the file:
```go
func setRequiredConfigEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CXG_DATABASE_URL", "postgres://test")
	t.Setenv("CXG_CAPTOKEN_HMAC_SECRET", "test-secret-32-bytes-minimum-aaaa")
	t.Setenv("CXG_INTERNAL_SHARED_SECRET", "test-internal")
}
```

- [ ] **Step 2: Run test, verify failure**

Run: `go test ./internal/codexexecgateway -run TestLoadConfig_BoundedResource -v`
Expected: FAIL (compile error — fields don't exist)

- [ ] **Step 3: Add fields to `Config`**

In `config.go`, inside the `Config` struct (before the closing brace):
```go
	// MaxFrameBytes caps each inbound/bridge ws frame. Default 16 MiB.
	// Override via CXG_MAX_FRAME_BYTES. Frames exceeding this are
	// rejected with close code 1009 (Message Too Big) by nhooyr.
	MaxFrameBytes int64
	// BridgeIdleTimeout is how long a bridge session can be silent
	// (no in/out frames) before the gateway sends RelayReset and
	// closes the bridge ws. Default 5m. Override via
	// CXG_BRIDGE_IDLE_TIMEOUT.
	BridgeIdleTimeout time.Duration
	// MaxStreamsPerExecutor bounds concurrent /bridge sessions per
	// executor. Default 32. Beyond this, /bridge returns 503. Override
	// via CXG_MAX_STREAMS_PER_EXECUTOR.
	MaxStreamsPerExecutor int
```

- [ ] **Step 4: Wire env loaders in `LoadConfigFromEnv`**

After `LogLevel: slog.LevelInfo,` (inside the `Config{}` literal):
```go
		MaxFrameBytes:         parseInt64Or("CXG_MAX_FRAME_BYTES", 16*1024*1024),
		BridgeIdleTimeout:     parseDurationOr("CXG_BRIDGE_IDLE_TIMEOUT", 5*time.Minute),
		MaxStreamsPerExecutor: parseIntOr("CXG_MAX_STREAMS_PER_EXECUTOR", 32),
```

Add `parseInt64Or` near `parseIntOr` (likely at the bottom of `config.go`):
```go
func parseInt64Or(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/codexexecgateway -run TestLoadConfig -v`
Expected: PASS (both new tests + any pre-existing config tests)

- [ ] **Step 6: Commit**

```bash
git add internal/codexexecgateway/config.go internal/codexexecgateway/config_test.go
git commit -m "feat(codex-exec-gateway): config knobs for bounded resources"
```

---

## Task 7: Apply `MaxFrameBytes` (replace `SetReadLimit(-1)`)

**Files:**
- Modify: `internal/codexexecgateway/inbound.go:54`
- Modify: `internal/codexexecgateway/bridge.go:99`
- Modify: `internal/codexexecgateway/server.go` (pass cfg to handlers)
- Modify: `internal/codexexecgateway/multiplex.go` (newInboundConn takes maxFrameBytes)

- [ ] **Step 1: Write failing test — oversized inbound frame is rejected**

Append to `internal/codexexecgateway/inbound_test.go`:
```go
func TestInbound_RejectsOversizedFrame(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.MaxFrameBytes = 1024 // 1 KiB cap for the test
	srv := newTestServerWithConfig(t, cfg)
	exeID, token := registerTestExecutor(t, srv)

	// Connect as inbound
	ws := dialInbound(t, srv, exeID, token)
	defer ws.Close(websocket.StatusNormalClosure, "")

	// Write a frame larger than the cap. Use a valid RelayMessageFrame
	// envelope so the close is for size, not parse failure.
	big := make([]byte, 2048)
	frame := &relaypb.RelayMessageFrame{
		Version: 1, StreamId: "test",
		Body: &relaypb.RelayMessageFrame_Data{Data: &relaypb.RelayData{
			Seq: 1, SegmentIndex: 0, SegmentCount: 1, Payload: big,
		}},
	}
	b, _ := proto.Marshal(frame)
	if err := ws.Write(context.Background(), websocket.MessageBinary, b); err != nil {
		// Some implementations error on Write; the close should still come back on Read.
		t.Logf("write returned: %v (acceptable)", err)
	}

	// Read should now fail with close code 1009 (MessageTooBig).
	_, _, err := ws.Read(context.Background())
	if err == nil {
		t.Fatal("expected ws.Read to fail after oversized frame")
	}
	var ce websocket.CloseError
	if errors.As(err, &ce) && ce.Code != websocket.StatusMessageTooBig {
		t.Errorf("got close code %d, want 1009 (MessageTooBig)", ce.Code)
	}
}
```

Note: `newTestServerWithConfig`, `registerTestExecutor`, `dialInbound`, and `newTestConfig` may need to be added or extended; check existing `*_test.go` helpers and reuse/extend them. If they don't exist, define them in a new `internal/codexexecgateway/testhelpers_test.go`. (Keep test code minimal — extend, don't rewrite.)

- [ ] **Step 2: Run, verify failure**

Run: `go test ./internal/codexexecgateway -run TestInbound_RejectsOversizedFrame -v`
Expected: FAIL — either compile error (missing helpers) or the read succeeds (no limit applied yet).

- [ ] **Step 3: Wire `MaxFrameBytes` through to `newInboundConn`**

In `multiplex.go`, change `newInboundConn` signature:
```go
func newInboundConn(exeID string, ws *websocket.Conn, logger *slog.Logger) *inboundConn {
```
to:
```go
func newInboundConn(exeID string, ws *websocket.Conn, logger *slog.Logger, maxFrameBytes int64) *inboundConn {
```
Inside, before returning:
```go
	ws.SetReadLimit(maxFrameBytes)
```

In `inbound.go:54`, delete:
```go
	ws.SetReadLimit(-1) // codex exec-server streams large process/read responses
```
and update the `newInboundConn` call (a few lines down) to pass `s.config.MaxFrameBytes`:
```go
	ic := newInboundConn(exeID, ws, s.logger.With("exe_id", exeID), s.config.MaxFrameBytes)
```

In `bridge.go:99`, replace:
```go
	bridgeWS.SetReadLimit(-1)
```
with:
```go
	bridgeWS.SetReadLimit(s.config.MaxFrameBytes)
```

- [ ] **Step 4: Run test, verify pass**

Run: `go test ./internal/codexexecgateway -run TestInbound_RejectsOversizedFrame -v`
Expected: PASS

- [ ] **Step 5: Run full test suite to confirm no regressions**

Run: `go test ./internal/codexexecgateway -count=1`
Expected: all pre-existing tests still PASS

- [ ] **Step 6: Commit**

```bash
git add internal/codexexecgateway/
git commit -m "feat(codex-exec-gateway): bound ws read with MaxFrameBytes"
```

---

## Task 8: Per-executor stream cap (`MaxStreamsPerExecutor`)

**Files:**
- Modify: `internal/codexexecgateway/multiplex.go` — `streamCount() int` method on `*inboundConn`
- Modify: `internal/codexexecgateway/bridge.go` — return 503 when exceeded
- Modify: `internal/codexexecgateway/bridge_test.go` — add cap-enforcement test

- [ ] **Step 1: Write failing test — 33rd bridge returns 503 with cap=32**

Append to `bridge_test.go`:
```go
func TestBridge_StreamCapReturns503(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.MaxStreamsPerExecutor = 2 // small cap to keep test fast
	srv := newTestServerWithConfig(t, cfg)
	exeID, _ := registerTestExecutor(t, srv)
	dialInboundAndKeepAlive(t, srv, exeID) // helper: holds inbound conn open

	// Dial 2 bridges successfully
	for i := 0; i < 2; i++ {
		ws := dialBridge(t, srv, exeID, fmt.Sprintf("stream-%d", i))
		defer ws.Close(websocket.StatusNormalClosure, "")
	}

	// 3rd dial should be rejected with 503
	resp, err := dialBridgeRaw(t, srv, exeID, "stream-3")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("got status %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Errorf("missing Retry-After header")
	}
}
```

Helpers (`dialBridge`, `dialInboundAndKeepAlive`, `dialBridgeRaw`) may already exist — check `bridge_test.go` / `multiplex_e2e_test.go`. Extend or add as needed; keep them minimal.

- [ ] **Step 2: Run, verify failure**

Run: `go test ./internal/codexexecgateway -run TestBridge_StreamCapReturns503 -v`
Expected: FAIL — 3rd dial succeeds (no cap yet).

- [ ] **Step 3: Add `streamCount()` method to `inboundConn`**

In `multiplex.go`, after the existing `lookup` method, add:
```go
// streamCount returns the number of currently registered routes.
func (i *inboundConn) streamCount() int {
	i.routesMu.RLock()
	defer i.routesMu.RUnlock()
	return len(i.routes)
}
```

- [ ] **Step 4: Add pre-upgrade cap check in bridge handler**

In `bridge.go`, find the section where the handler has just resolved `inbound` (via `s.registry.Get(exeID)` or equivalent) and is about to call `websocket.Accept`. Insert a cap check **before** the `websocket.Accept` call:

```go
	if cap := s.config.MaxStreamsPerExecutor; cap > 0 && inbound.streamCount() >= cap {
		s.logger.Warn("bridge: per-executor stream cap exceeded",
			"exe_id", exeID, "cap", cap, "current", inbound.streamCount())
		w.Header().Set("Retry-After", "30")
		http.Error(w, "too many concurrent streams for this executor", http.StatusServiceUnavailable)
		return
	}
```

(Race note: this check is intentionally not atomic with the eventual `addRoute`. In practice bridge dials are sequential per env-mcp, and any concurrent dial slipping past the cap will succeed at `addRoute`. Accept this; the cap is a guardrail, not a strict invariant.)

If you cannot find a clear "have `inbound`, about to Accept" point — `bridge.go` doesn't currently fetch the inbound until after the upgrade because it needs the streamID — then the cap check has to move post-upgrade. In that case:
1. Do the Accept and Resume-frame parsing as today.
2. Just before the existing `inbound.addRoute(...)`, check `if cap > 0 && inbound.streamCount() >= cap`.
3. If exceeded, write a `RelayReset{reason:"cap-exceeded"}` over the bridge ws, then `bridgeWS.Close(websocket.StatusTryAgainLater, "cap exceeded")`. Do not call `addRoute`.
4. Update the test to dial the ws, expect the Resume frame to be ack'd, then expect a Reset frame with reason "cap-exceeded" within 100ms, then expect the ws to close with code 1013.

Read `bridge.go` lines 90-150 to pick the right placement; document the chosen path in a `// Per-executor stream cap (PR 1):` comment.

- [ ] **Step 5: Run test, verify pass**

Run: `go test ./internal/codexexecgateway -run TestBridge_StreamCapReturns503 -v`
Expected: PASS

- [ ] **Step 6: Run full suite**

Run: `go test ./internal/codexexecgateway -count=1`
Expected: all PASS

- [ ] **Step 7: Commit**

```bash
git add internal/codexexecgateway/
git commit -m "feat(codex-exec-gateway): cap concurrent bridges per executor"
```

---

## Task 9: Per-bridge idle timeout

**Files:**
- Modify: `internal/codexexecgateway/multiplex.go` — `bridgeSession.lastActivity` atomic; `inboundConn.startIdleReaper`
- Modify: `internal/codexexecgateway/inbound.go` — touch lastActivity on each routed frame
- Modify: `internal/codexexecgateway/bridge.go` — touch lastActivity on outbound frames; start reaper
- Modify: `internal/codexexecgateway/multiplex_test.go` — add idle timeout test

- [ ] **Step 1: Write failing test — idle bridge dies after timeout**

Append to `multiplex_test.go`:
```go
func TestIdleReaper_ClosesIdleBridgeAndSendsReset(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.BridgeIdleTimeout = 200 * time.Millisecond
	srv := newTestServerWithConfig(t, cfg)
	exeID, _ := registerTestExecutor(t, srv)
	inboundWS := dialInbound(t, srv, exeID, /* token from registry */ "")
	defer inboundWS.Close(websocket.StatusNormalClosure, "")

	bridgeWS := dialBridge(t, srv, exeID, "stream-idle")
	// (dialBridge already sends the Resume frame internally)

	// Don't send any further frames; wait 2× timeout.
	time.Sleep(450 * time.Millisecond)

	// bridgeWS.Read should now error (closed by reaper).
	_, _, err := bridgeWS.Read(context.Background())
	if err == nil {
		t.Fatal("expected bridge ws to be closed after idle timeout")
	}

	// inboundWS should have received a Reset frame for this stream.
	gotReset := false
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		mt, data, rerr := inboundWS.Read(ctx)
		cancel()
		if rerr != nil {
			break
		}
		if mt != websocket.MessageBinary {
			continue
		}
		var f relaypb.RelayMessageFrame
		if proto.Unmarshal(data, &f) != nil {
			continue
		}
		if r, ok := f.Body.(*relaypb.RelayMessageFrame_Reset_); ok && f.StreamId == "stream-idle" && r.Reset_.Reason == "idle-timeout" {
			gotReset = true
			break
		}
	}
	if !gotReset {
		t.Fatal("expected RelayReset{reason:idle-timeout} on inbound for stream-idle")
	}
}
```

- [ ] **Step 2: Run, verify failure**

Run: `go test ./internal/codexexecgateway -run TestIdleReaper -v -timeout 10s`
Expected: FAIL — bridge stays open forever.

- [ ] **Step 3: Add `lastActivity` to `bridgeSession`**

In `multiplex.go`, find the `bridgeSession` struct and add (before `closed`):
```go
	lastActivity atomic.Int64 // unix nanos, updated on every frame touching this session
```

Add a helper:
```go
func (b *bridgeSession) touch() {
	b.lastActivity.Store(time.Now().UnixNano())
}
```

Initialize on construction (find where bridgeSession is created in `bridge.go`):
```go
	session := &bridgeSession{
		streamID: streamID, inbound: inbound, bridgeWS: bridgeWS,
		closed: make(chan struct{}),
	}
	session.touch() // start ticking from session-create time
```

- [ ] **Step 4: Touch on every routed frame**

In `inbound.go`'s reader loop (the section that does `routes[frame.StreamId]` lookup and writes to the bridge ws), after a successful lookup:
```go
	if b, ok := ic.lookup(frame.StreamId); ok {
		b.touch()
		// existing write to bridge ws
	}
```

In `bridge.go`'s outbound pump (the goroutine that reads from `bridgeWS` and writes to inbound), after each successful read:
```go
	session.touch()
```

- [ ] **Step 5: Implement `startIdleReaper`**

In `multiplex.go`, add:
```go
// startIdleReaper runs until i.closed. Every idleTimeout/4 (min 50ms),
// it scans routes and closes any session whose lastActivity is older
// than idleTimeout. On close, sends a RelayReset frame to inbound (so
// the executor's relay layer tears down its per-stream JSON-RPC
// session) and closes the bridge ws.
func (i *inboundConn) startIdleReaper(ctx context.Context, idleTimeout time.Duration) {
	if idleTimeout <= 0 {
		return
	}
	tick := idleTimeout / 4
	if tick < 50*time.Millisecond {
		tick = 50 * time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-i.closed:
			return
		case <-t.C:
			i.reapIdle(ctx, idleTimeout)
		}
	}
}

func (i *inboundConn) reapIdle(ctx context.Context, idleTimeout time.Duration) {
	cutoff := time.Now().Add(-idleTimeout).UnixNano()
	i.routesMu.RLock()
	candidates := []*bridgeSession{}
	for _, b := range i.routes {
		if b.lastActivity.Load() < cutoff {
			candidates = append(candidates, b)
		}
	}
	i.routesMu.RUnlock()

	for _, b := range candidates {
		resetFrame := &relaypb.RelayMessageFrame{
			Version: 1, StreamId: b.streamID,
			Body: &relaypb.RelayMessageFrame_Reset_{Reset_: &relaypb.RelayReset{Reason: "idle-timeout"}},
		}
		data, err := proto.Marshal(resetFrame)
		if err != nil {
			i.logger.Warn("reaper: marshal reset", "stream_id", b.streamID, "err", err)
			continue
		}
		if werr := i.write(ctx, websocket.MessageBinary, data); werr != nil {
			i.logger.Warn("reaper: write reset to inbound", "stream_id", b.streamID, "err", werr)
		}
		i.removeRoute(b.streamID, b)
		b.close(nil)
		i.logger.Info("reaper: closed idle bridge", "stream_id", b.streamID, "timeout", idleTimeout)
	}
}
```

Add imports to `multiplex.go` if missing:
```go
import (
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/agentserver/agentserver/internal/relaypb"
)
```

- [ ] **Step 6: Start the reaper when inbound connects**

In `inbound.go`, after `newInboundConn(...)` and before the reader loop:
```go
	go ic.startIdleReaper(r.Context(), s.config.BridgeIdleTimeout)
```

- [ ] **Step 7: Run test, verify pass**

Run: `go test ./internal/codexexecgateway -run TestIdleReaper -v -timeout 10s`
Expected: PASS

- [ ] **Step 8: Run full suite — ensure no flakes in existing tests**

Run: `go test ./internal/codexexecgateway -count=3 -timeout 60s`
(Run 3× to catch race conditions from the new reaper goroutine.)
Expected: all PASS, no flakes.

- [ ] **Step 9: Run with -race**

Run: `go test ./internal/codexexecgateway -race -count=1 -timeout 120s`
Expected: PASS, no race warnings.

- [ ] **Step 10: Commit**

```bash
git add internal/codexexecgateway/
git commit -m "feat(codex-exec-gateway): per-bridge idle timeout reaper"
```

---

## Task 10: Final verification

- [ ] **Step 1: Run full test suite**

Run: `make test`
Expected: PASS

- [ ] **Step 2: Run pin check**

Run: `make codex-pin-check`
Expected: `codex-pin: OK`

- [ ] **Step 3: Confirm CI YAML is loaded by GitHub Actions linter (optional)**

If `gh` CLI is available:
```bash
gh workflow view build.yml
```
Expected: prints the workflow definition without error.

- [ ] **Step 4: Final commit if anything was tweaked**

```bash
git status   # should be clean
```

If clean, the PR is ready. Push and open the PR:
```bash
git push -u origin <branch-name>
gh pr create --title "feat: codex-pin + exec-gateway bounded resources" \
  --body "$(cat <<'EOF'
## Summary
- Pin agentserver protocol assumptions to upstream codex tag `rust-v0.131.0-alpha.22`
- CI fails on drift via `make codex-pin-check`
- exec-gateway: bound ws frames (16 MiB default), per-bridge idle timeout (5 min default), per-executor stream cap (32 default)

Spec: `docs/superpowers/specs/2026-05-20-codex-gateway-full-alignment-design.md` (PR 1 of 3)

## Test plan
- [ ] `make test` passes locally
- [ ] `make codex-pin-check` passes locally
- [ ] CI green
EOF
)"
```

---

## Self-review notes

Spec coverage check:
- Gap 1 (codex-pin) → Tasks 1–5
- Gap 2 (bounded reads) → Task 7
- Gap 3 (idle timeout + stream cap) → Tasks 8 (cap) + 9 (idle)

Type consistency: `Config.MaxFrameBytes` is `int64` (matches `ws.SetReadLimit`'s signature in nhooyr); `MaxStreamsPerExecutor` is `int` (matches `streamCount() int` and the comparison in `bridge.go`); `BridgeIdleTimeout` is `time.Duration` (matches `startIdleReaper` and the reaper logic).

No placeholders. All code blocks are concrete; the only `<...>` markers in the plan are values that must be computed from a live `git ls-remote` / `sha256sum` at Task 1, which is itself the instruction to produce them.
