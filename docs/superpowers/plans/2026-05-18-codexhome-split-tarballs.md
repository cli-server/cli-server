# codexhome split-tarballs — Implementation Plan

> Use superpowers:subagent-driven-development. Checkbox-tracked.

**Goal:** split the single CODEX_HOME tarball at `codex-app-gateway/<wid>.tar.gz`
into one workspace pack + N per-session packs. Lazy-migrate existing
workspaces. Spec: `docs/superpowers/specs/2026-05-18-codexhome-split-tarballs.md`.

---

## Task 1 — `classifyPath` + tests

**Files:** new `internal/codexappgateway/codexhome/classify.go` + test.

- [ ] `func classifyPath(rel string) (sessionID string, skip bool)` per the
  spec's classification rules.
  - skip: `rel == "config.toml"`
  - per-session: matches `sessions/.+/rollout-.+-(?P<sid>[0-9a-f-]+)\.jsonl`
    OR `shell_snapshots/(?P<sid>[0-9a-f-]+)\.[^/]+`
  - else: workspace (sessionID="")
- [ ] Table-driven tests with all observed real paths (see spec) +
  unusual edge cases (dotfiles, deeply nested under .tmp/plugins).
- [ ] Commit `feat(codexhome): classifyPath bucket per-session vs workspace files`.

---

## Task 2 — `ObjectStore` gains `List`

**Files:** `internal/codexappgateway/codexhome/s3.go`,
`internal/codexappgateway/s3store.go` (the production wrapper), test fakes.

- [ ] Add `List(ctx, prefix) ([]string, error)` to interface.
- [ ] Implement on the prod wrapper (`ListObjectsV2` paginator).
- [ ] Update map-backed test fake to support prefix scan.
- [ ] Commit `feat(codexhome): ObjectStore.List for prefix scans`.

---

## Task 3 — Rewrite `S3Backend.Upload` to split

**Files:** `internal/codexappgateway/codexhome/s3.go`.

- [ ] New keys:
  ```go
  func (b *S3Backend) workspaceKey() string { return fmt.Sprintf("codex-app-gateway/%s/workspace.tar.gz", b.workspaceID) }
  func (b *S3Backend) sessionKey(sid string) string { return fmt.Sprintf("codex-app-gateway/%s/sessions/%s.tar.gz", b.workspaceID, sid) }
  ```
- [ ] Walk `src`, build map[sessionID][]paths + workspace paths.
- [ ] Build workspace tar.gz; PUT under workspaceKey.
- [ ] Build each session tar.gz; PUT under sessionKey. Run with a
  bounded semaphore (size 4).
- [ ] Add skip-unchanged optimization: an in-memory `map[string]time.Time`
  keyed by `workspace+sessionID`, recording max mtime of last upload.
  Reset on process restart. Lives on `S3Backend` as `sessionMtimes`.
- [ ] After all uploads succeed: List session prefix and DELETE any
  S3 session keys NOT in the just-uploaded set (cleanup of removed
  sessions).
- [ ] Commit `refactor(codexhome): split Upload into workspace + per-session tarballs`.

---

## Task 4 — Rewrite `S3Backend.Download` with legacy fallback

**Files:** `internal/codexappgateway/codexhome/s3.go`.

- [ ] Try `Get(workspaceKey)` first.
- [ ] If `ErrObjectNotFound`: try legacy `codex-app-gateway/<wid>.tar.gz`.
  If found: extract, then `Delete(legacy)`, return.
- [ ] If new workspaceKey found: extract; then List `sessions/` prefix;
  Get + extract each (bounded parallelism 4).
- [ ] Skip session keys that return `ErrObjectNotFound` (race with
  concurrent cleanup); log warn.
- [ ] Commit `refactor(codexhome): split Download + legacy-key migration fallback`.

---

## Task 5 — Tests

**Files:** `internal/codexappgateway/codexhome/s3_test.go` (replace),
`classify_test.go` (from Task 1).

- [ ] Round-trip: temp dir with simulated CODEX_HOME (2 sessions + sqlite +
  config.toml) → Upload → assert keys PUT + workspaceKey contains the
  shared files only + each sessionKey contains exactly its files +
  config.toml absent from all packs.
- [ ] Download into an empty dir: assert tree matches original.
- [ ] Legacy fallback: seed fake store with `codex-app-gateway/<wid>.tar.gz`,
  call Download, assert extraction + legacy key deletion.
- [ ] Skip-unchanged: two consecutive Upload calls; second one PUTs zero
  session keys (workspace unchanged → also skipped, or just workspace
  reuploaded — be explicit which).
- [ ] Cleanup: Upload that omits a previously-uploaded session →
  S3 session key is DELETEd.
- [ ] Commit `test(codexhome): split-tarball round-trip + legacy + cleanup`.

---

## Task 6 — Chart + release

- [ ] Bump chart to `0.55.0`.
- [ ] Commit, tag, push, CI.
- [ ] Pulumi up; rollout codex-app-gateway.
- [ ] Smoke test: take a live workspace through one session, verify in
  S3 console that the new layout appears + legacy key was deleted.

---

## Out of scope (v2)

- io.Pipe + S3 multipart upload
- ETag-based concurrent write protection
- Session GC / retention policy
- Streaming extract during download
