# codexhome: Split S3 Tarballs into Workspace + Per-Session

**Date:** 2026-05-18
**Status:** Approved for v0.55.0 (shipping alongside `transfer` tool)
**Supersedes:** the single-tarball layout in `internal/codexappgateway/codexhome/s3.go`.

## Problem

Today `S3Backend.Upload` walks the entire CODEX_HOME and packs it into one
gzip'd buffer, then PUTs at `codex-app-gateway/<workspace>.tar.gz`. With
five sessions and a 34 MB shared sqlite log this means every save:

- repacks every historical session's `rollout-*.jsonl` even if untouched
- buffers the full ~40 MB blob in process memory
- single PUT — no concurrency
- single Download has to fetch the whole blob even to resume one session

Per the user request the v0.55.0 layout splits CODEX_HOME into:

- **workspace pack** — everything that's shared across sessions
- **N per-session packs** — one for each session_id

Load and store touch both categories.

## File classification

Files in CODEX_HOME are exactly one of:

1. **per-session**, identified by filename:
   - `sessions/**/rollout-*-<session_id>.jsonl`
   - `shell_snapshots/<session_id>.*` (any extension)
2. **workspace-shared** — everything else (logs/state sqlites, memories,
   skills, plugins, installation_id, .personality_migration, etc.)
3. **excluded** — `config.toml` (gateway regenerates each spawn, no need
   to round-trip through S3)

The classifier lives in a single `func classifyPath(rel string) (sessionID
string, skip bool)` so adding/removing a category is one place.

## S3 key layout

```
codex-app-gateway/<workspace_id>/workspace.tar.gz
codex-app-gateway/<workspace_id>/sessions/<session_id>.tar.gz
```

Listing all session keys uses S3's `ListObjectsV2` with prefix
`codex-app-gateway/<workspace_id>/sessions/`.

## Upload (Save)

1. Walk CODEX_HOME once.
2. Bucket files into a `map[sessionID][]filePath` plus a `workspace` slice.
3. **Workspace pack:** build tar.gz from workspace files in memory; PUT to
   `workspace.tar.gz`.
4. **Session packs:** for each session_id, build tar.gz; PUT to
   `sessions/<sid>.tar.gz`. Run up to **4 in parallel** (bounded by a
   semaphore) to overlap S3 latency with serialization cost.
5. **(Skip-unchanged optimization)** Before packing a session, compare the
   max mtime of its files to the previous upload's recorded mtime (kept in
   a tiny in-memory map keyed by workspace_id+session_id). If unchanged,
   skip the upload entirely. This is the actual perf win for the
   "long-running thread + small new session" case. Cache cleared on
   process restart — first save after pod restart re-uploads everything,
   which is fine (correctness over perf).
6. Return only after all PUTs succeed. On partial failure, return error
   and DO NOT try to roll back partially-uploaded session tars (they're
   self-contained and valid for whichever sessions they hold).

Memory peak: one session tar at a time per parallel slot. Workspace tar
is one shot (could be 30+ MB of sqlite). To keep memory predictable for
the workspace pack, use an `io.Pipe` + S3 multipart upload — but for v1,
buffered is fine (workspace pack is at most ~50 MB in observed traffic).

## Download (Load)

1. Get `workspace.tar.gz` → extract into dst.
2. List S3 with prefix `sessions/`; for each key, Get + extract.
3. Up to **4 in parallel** downloads.
4. ErrObjectNotFound on workspace.tar.gz is fine (first-spawn case) —
   treat as "no prior state".
5. ErrObjectNotFound on a session tar that appears in the listing is a
   race (deleted concurrently); skip with warn.

Memory peak: one tar in flight per parallel slot.

## Concurrency / atomicity

- One Supervisor allows at most one spawn per workspace today, so two
  parallel Uploads for the same workspace can't happen from our own code.
- A future Supervisor with concurrent spawns per workspace would need:
  - S3 conditional PUT (`If-Match` on ETag) on workspace.tar.gz
  - Per-session keys are inherently isolated by session_id
- Out of scope until that future arrives — flag in the new spec.

## Cleanup

- When a session_id disappears from CODEX_HOME (codex deletes it
  locally), the corresponding S3 `sessions/<sid>.tar.gz` should be
  DELETEd. Implementation: at the end of Upload, list all S3 session
  keys and DELETE any that aren't in our just-uploaded set.
- Pruning workspace-scoped state is harder (we don't know when a
  workspace is gone for good). Out of scope.

## ObjectStore interface gains List + Delete-prefix

Today's interface is `Put / Get / Delete`. We add:

```go
type ObjectStore interface {
    Put(ctx context.Context, key string, data []byte) error
    Get(ctx context.Context, key string) ([]byte, error)
    Delete(ctx context.Context, key string) error
    List(ctx context.Context, prefix string) ([]string, error)   // NEW
}
```

S3 client wrapper grows a List paginator over `ListObjectsV2`.

## Backward compatibility

Existing workspaces have a single `codex-app-gateway/<wsid>.tar.gz` (no
sub-path). On the FIRST Download under the new code:

1. Try the new `workspace.tar.gz` + `sessions/` layout first.
2. If `workspace.tar.gz` returns `ErrObjectNotFound`, try the legacy
   `<wsid>.tar.gz` key.
3. If the legacy key exists, extract it (full tree, including any
   sessions/) and then DELETE the legacy key. Subsequent saves use the
   new split layout exclusively.

This migrates each workspace lazily — no offline backfill job needed.

## Out of scope (v2)

- io.Pipe + S3 multipart upload (drop buffered-in-memory workspace pack)
- ETag-based concurrent-write protection
- Session-level retention policy / GC
- Streaming download (extract while fetching next file)

## Testing

Unit:
- `classifyPath` table-driven: 10+ cases covering the documented patterns
  + edge cases (file at top level, deeply-nested under sessions/,
  filenames with multiple dashes, etc.).
- `Upload` with a fake `ObjectStore` and a temp dir: assert exact set of
  keys PUT (one workspace + N session) + assert config.toml is NOT in
  any pack.
- `Upload` skip-unchanged: second upload with no file changes PUTs only
  workspace (or zero, depending on whether workspace files changed).
- `Download` legacy-key fallback: pre-seed fake store with just the
  legacy key, assert extraction works + legacy key DELETE was called.

Integration: existing prod sample tree (~40 MB, 5 sessions) round-trips
intact through fake `ObjectStore`.

## Versioning

`v0.55.0` (minor — new S3 keys, lazy migration; back-compat read).
Bundle with the `transfer` tool change since both touch `codex-app-gateway`.
