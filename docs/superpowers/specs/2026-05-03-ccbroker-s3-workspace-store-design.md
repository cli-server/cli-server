# ccbroker: replace OpenViking with S3-compatible workspace store

**Date:** 2026-05-03
**Status:** design
**Scope:** ccbroker workspace persistence layer
**Related:** `2026-04-15-stateless-cc-design.md`, `2026-05-02-ccbroker-sdk-worker-design.md`

## Goal

Replace OpenViking as the per-turn workspace persistence backend in ccbroker
with a generic S3-compatible object store, configured by endpoint / region /
bucket / AK / SK. OpenViking integration code, Helm subchart, and
docker-compose service are removed entirely.

## Motivation

ccbroker uses OpenViking to persist a per-workspace `claude-home` tree across
Claude Code turns: download on Setup, snapshot, run the turn, diff, upload
changed files on Teardown, delete temp dir.

OpenViking is positioned as a "context database for AI agents" with vector
search, embeddings, and VLM integration. ccbroker uses **none** of those — it
only calls `fs/ls`, `content/read|write`, and `resources/temp_upload|resources`.
We are paying for an unused stack (4Gi RAM limit, embedding/VLM config in
chart), depending on a 0.x project's custom REST surface (no rclone / aws cli
ecosystem), and have a known semantic gap (`workspace.go:91` — "Removed files
are not propagated").

S3 is the universal object store protocol. Every cloud has a managed offering;
MinIO is the de-facto self-host. A single `minio-go` client works against all
of them. The migration moves us off a niche dependency onto an interchangeable
commodity layer.

## Non-goals

- **Event-sourced workspace state.** Considered and explicitly deferred. Real
  benefits exist (audit trail, transactionality with turn metadata) but the
  architectural cost (snapshot lifecycle, replay, schema evolution) is
  out-of-scope for a backend swap. If pursued, it should be its own design.
- **Sticky session routing / cross-turn workspace caching.** Per-turn full
  sync remains the model. Optimization deferred until measured.
- **Production data migration from OpenViking.** Confirmed not needed —
  workspace contents may be reset.
- **Backwards compatibility with the OpenViking client.** No interface
  abstraction over both backends. Direct cut.

## Architecture

### Per-turn flow

```
handler_turns.HandleTurn
  ↓
workspace.Setup(workspaceID, sessionID, store)
  ├─ mkdir TempDir / ClaudeDir / ProjectDir / MemoryDir
  └─ store.DownloadTarGz("workspaces/<wid>/claude-home.tar.gz", ClaudeDir)
       (404 → empty workspace, leave dirs empty)
  ↓
runner.Run(...)        ← cwd = ProjectDir (empty; only used for proj_hash)
  ↓
workspace.Teardown(ws, store)
  ├─ store.UploadTarGz(ClaudeDir, "workspaces/<wid>/claude-home.tar.gz")
  └─ RemoveAll(TempDir)
```

### S3 layout

```
<bucket>/
  workspaces/
    <workspace_id>/
      claude-home.tar.gz     ← settings, CLAUDE.md, skills/, projects/<hash>/<sid>.jsonl, memory/
```

One object per workspace. `project.tar.gz` does **not** exist: ccbroker
disables every built-in filesystem tool (`Bash, Read, Edit, Write, Glob, Grep,
LS, Task, BashOutput, KillShell, NotebookEdit` — see `runner/options.go:91`),
so the agent has no way to write files into ProjectDir. ProjectDir is created
empty solely to give Claude CLI a stable cwd whose proj_hash points at the
correct session jsonl inside ClaudeDir.

## Components

### `internal/ccbroker/workspace/s3store.go` (new)

Replaces `viking_client.go` in full.

```go
type S3Config struct {
    Endpoint        string  // "s3.amazonaws.com" / "minio:9000" / "oss-cn-hangzhou.aliyuncs.com"
    Region          string
    Bucket          string
    AccessKeyID     string
    SecretAccessKey string
    UseSSL          bool
    PathStyle       bool   // true for MinIO / on-prem; false for AWS / OSS / COS
}

type S3Store struct {
    client *minio.Client
    bucket string
}

func NewS3Store(cfg S3Config) (*S3Store, error)

// Stream-download key, gunzip, untar into destDir.
// NoSuchKey is treated as empty workspace and returns nil.
// Tar entries with paths escaping destDir are skipped and logged.
func (s *S3Store) DownloadTarGz(ctx context.Context, key, destDir string) error

// WalkDir(srcDir), stream a tar.gz through io.Pipe to PutObject.
// Skips symlinks. Uses 0644/0755 mode unconditionally.
func (s *S3Store) UploadTarGz(ctx context.Context, srcDir, key string) error
```

**SDK choice:** `github.com/minio/minio-go/v7`. Single module, cross-provider
proven, smaller than `aws-sdk-go-v2`'s sub-module ecosystem, simpler call
surface (`client.GetObject`, `client.PutObject`).

### `internal/ccbroker/workspace/workspace.go` (modified)

Signatures change `*VikingClient` → `*S3Store`:

```go
func Setup(ctx context.Context, workspaceID, sessionID string, store *S3Store) (*Workspace, error)
func Teardown(ctx context.Context, ws *Workspace, store *S3Store) error
```

Internals:
- Setup mkdirs, then a single `store.DownloadTarGz` for `claude-home.tar.gz`.
- Teardown defers `RemoveAll(TempDir)`, then a single `store.UploadTarGz` for
  `claude-home.tar.gz`. Upload errors are logged but do not propagate (matches
  current "do not block turn response on flaky upload" semantics).

The `Workspace.snapshot` field is removed.

### Removed code

- `internal/ccbroker/workspace/viking_client.go` (full)
- `internal/ccbroker/workspace/snapshot.go` + `snapshot_test.go` (tar.gz
  obsoletes per-file diff)
- `tools/context.go:21` `Viking *workspace.VikingClient` field — no tool reads
  it; all `workspace_*` tools operate on `ClaudeDir` directly.

### `internal/ccbroker/handler_turns.go` (modified)

- L108: drop `workspace.NewVikingClient(...)`. Replace with `s.store` carried
  on the server struct, constructed once at startup.
- L127: drop `Viking: vc,` from the `tools.Context` literal.
- L148: `workspaceTeardown(..., vc)` → `workspaceTeardown(..., s.store)`.

### `internal/ccbroker/config.go` (modified)

Remove:
```go
OpenVikingURL    string
OpenVikingAPIKey string
```

Add:
```go
S3Endpoint        string
S3Region          string
S3Bucket          string
S3AccessKeyID     string
S3SecretAccessKey string
S3UseSSL          bool
S3PathStyle       bool
```

Env vars: `CCBROKER_S3_ENDPOINT`, `CCBROKER_S3_REGION`, `CCBROKER_S3_BUCKET`,
`CCBROKER_S3_ACCESS_KEY_ID`, `CCBROKER_S3_SECRET_ACCESS_KEY`,
`CCBROKER_S3_USE_SSL`, `CCBROKER_S3_PATH_STYLE`. Bucket is required —
`LoadConfigFromEnv` returns an error if unset.

## Data flow detail

### Download (Setup)

```
GetObject(bucket, key) → io.ReadCloser
  └─ gzip.NewReader → tar.NewReader → loop entries:
       - validate filepath.Clean(entry.Name) has no ".." and final path
         remains within destDir; skip + log if not
       - if entry is dir: MkdirAll(destPath, 0755)
       - if entry is regular file: MkdirAll(filepath.Dir(destPath), 0755) +
         create file 0644 + io.Copy
       - other types (symlink etc.): skip
NoSuchKey → return nil (empty workspace)
```

### Upload (Teardown)

```
pr, pw := io.Pipe()
go func() {
    defer pw.Close()
    gw := gzip.NewWriter(pw); defer gw.Close()
    tw := tar.NewWriter(gw);  defer tw.Close()
    filepath.WalkDir(srcDir, ...) {
        skip symlinks
        WriteHeader(tar header with normalized rel path, mode 0644/0755)
        if regular file: io.Copy(tw, file)
    }
}()
PutObject(bucket, key, pr, -1, opts{ContentType: "application/gzip"})
   ↑ size=-1 → minio-go uses multipart upload automatically
```

### Concurrency

- `*minio.Client` is goroutine-safe; one `*S3Store` is held by the cc-broker
  server and shared across requests.
- Same-`sessionID` turns are serialized by the existing `TurnLock` in
  `handler_turns`, so no two turns ever Setup/Teardown the same key
  concurrently.
- Cross-workspace operations are independent — no additional locking.

### Error matrix

| Phase | Condition | Behavior |
|---|---|---|
| Setup | S3 unreachable / auth fail | RemoveAll TempDir, return error → handler returns 500 |
| Setup | Object 404 | Return nil; ClaudeDir stays empty (first-turn workspace) |
| Setup | Tar entry escapes destDir | Skip entry, log, continue |
| Setup | Gzip / tar parse error mid-stream | RemoveAll TempDir, return error → 500 (corrupt object is a real bug) |
| Teardown | S3 unreachable | Log error, RemoveAll TempDir, return nil |
| Teardown | File read error during walk | Log, skip file, continue |

Setup must `RemoveAll(TempDir)` on every error path that comes after the
initial mkdirs — otherwise a failing Setup leaks `/tmp/cc-broker/sess_*`
directories with no Teardown to claim them (handler_turns.go only calls
Teardown when Setup succeeds).

### Sizing & limits

No proactive cap on object size. `claude-home.tar.gz` is dominated by the
session jsonl, which grows append-only over a session lifetime; gzip
compresses chat text well, so realistic ceiling is single-digit MB even for
long conversations. Add limits when measured pain appears — YAGNI.

## Testing

### Unit (`s3store_test.go`)

`httptest.NewServer` implementing the minimal S3 surface (`GET /<bucket>/<key>`
returning bytes or 404, `PUT /<bucket>/<key>` capturing bytes). Cases:

- `DownloadTarGz` round-trip: a known tar.gz served back, verify file tree
  reconstructs exactly.
- `DownloadTarGz` 404 → returns nil, destDir untouched.
- `DownloadTarGz` malicious tar entry (`../etc/passwd`, absolute path) →
  skipped, no write outside destDir.
- `UploadTarGz` round-trip: pack a temp tree, fake S3 captures bytes, unpack
  through tar/gzip readers, assert content matches.
- `UploadTarGz` skips symlinks (verify a tree with a symlink uploads only the
  regular files).

### Workspace (`workspace_test.go`)

Refactor existing fake `VikingClient` test double into an `S3Store` backed by
the same httptest server pattern. Preserve current behaviors:

- Setup creates the four required directories.
- Setup with absent S3 object yields a usable empty workspace.
- Teardown removes TempDir.
- Same `sessionID` produces stable `ProjectDir` and `ClaudeDir` paths across
  successive Setup calls (proj_hash continuity).

Add:

- Setup → write a file under ClaudeDir → Teardown → fresh Setup → file is
  present.
- Teardown with fake S3 returning 500 still removes TempDir and returns nil.

### Integration

Deferred. `httptest` covers the contract; testcontainers + real MinIO is
extra CI cost without proportional confidence. Add when a real bug escapes
the unit suite.

### Manual verification (pre-merge checklist)

1. `docker-compose up`, send an IM message, observe a turn complete.
2. `mc ls local/ccbroker/workspaces/` shows `claude-home.tar.gz` for that
   workspace.
3. Send a second IM message in the same session — Claude resumes (proj_hash
   finds the jsonl that was packed, uploaded, downloaded, unpacked).
4. Stop the minio container — Setup fails with 500. Restart minio — recovers.

## Deployment

### Helm

**Removed:**
- `deploy/helm/agentserver/charts/openviking/` (entire subchart)
- `Chart.yaml` openviking dependency entry
- `values.yaml` `openviking:` block and `ccbroker.openviking:` block
- `templates/cc-broker.yaml` lines 93-110 (the `$ovURL` / `$ovEnabled`
  computation and `CCBROKER_OPENVIKING_*` env)

**Added** to `values.yaml`:
```yaml
ccbroker:
  s3:
    endpoint: ""           # required
    region: ""             # required for AWS-style; "" for MinIO is OK
    bucket: ""             # required
    useSSL: true
    pathStyle: false       # true for MinIO / on-prem
    existingSecret: ""     # secret with keys: access_key_id, secret_access_key
```

**Added** to `templates/cc-broker.yaml`:
```yaml
- name: CCBROKER_S3_ENDPOINT
  value: {{ .Values.ccbroker.s3.endpoint | quote }}
- name: CCBROKER_S3_REGION
  value: {{ .Values.ccbroker.s3.region | quote }}
- name: CCBROKER_S3_BUCKET
  value: {{ .Values.ccbroker.s3.bucket | quote }}
- name: CCBROKER_S3_USE_SSL
  value: {{ .Values.ccbroker.s3.useSSL | quote }}
- name: CCBROKER_S3_PATH_STYLE
  value: {{ .Values.ccbroker.s3.pathStyle | quote }}
- name: CCBROKER_S3_ACCESS_KEY_ID
  valueFrom:
    secretKeyRef:
      name: {{ .Values.ccbroker.s3.existingSecret }}
      key: access_key_id
- name: CCBROKER_S3_SECRET_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.ccbroker.s3.existingSecret }}
      key: secret_access_key
```

No bundled MinIO subchart. Operators choose: managed cloud S3, externally
deployed MinIO, etc.

### docker-compose

**Removed:** `openviking` service, `CCBROKER_OPENVIKING_*` env on cc-broker.

**Added:** `minio` + `minio-init` services (local-dev convenience only):
```yaml
minio:
  image: minio/minio:latest
  command: server /data --console-address ":9001"
  environment:
    MINIO_ROOT_USER: minioadmin
    MINIO_ROOT_PASSWORD: minioadmin
  ports: ["9000:9000", "9001:9001"]
  volumes: ["minio-data:/data"]

minio-init:
  image: minio/mc:latest
  depends_on: [minio]
  entrypoint: >
    sh -c "mc alias set local http://minio:9000 minioadmin minioadmin &&
           mc mb -p local/ccbroker || true"
```

cc-broker env:
```yaml
CCBROKER_S3_ENDPOINT: "minio:9000"
CCBROKER_S3_REGION: ""
CCBROKER_S3_BUCKET: "ccbroker"
CCBROKER_S3_USE_SSL: "false"
CCBROKER_S3_PATH_STYLE: "true"
CCBROKER_S3_ACCESS_KEY_ID: "minioadmin"
CCBROKER_S3_SECRET_ACCESS_KEY: "minioadmin"
```

### Dependencies

`go.mod`: add `github.com/minio/minio-go/v7`.

### Rollback

No data migration in this PR. Rollback = `git revert` + redeploy the
OpenViking subchart. Existing tar.gz objects in S3 become orphaned but
harmless.
