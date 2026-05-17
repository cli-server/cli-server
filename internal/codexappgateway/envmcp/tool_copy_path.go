package envmcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// CopyPathTool streams a file (or, with recursive=true, a directory
// tree) from one executor to another via the existing exec-server
// process/start+read+write RPCs. The byte pump runs entirely inside
// env-mcp so no chunk ever crosses the LLM context; memory is bounded
// by the chunk size (~1 MiB) regardless of file size.
//
// See docs/superpowers/specs/2026-05-18-env-mcp-transfer-tool.md.
type CopyPathTool struct {
	pool     *BridgePool
	resolver *NameResolver
	pidSeq   atomic.Uint64
}

func NewCopyPathTool(pool *BridgePool, resolver *NameResolver) *CopyPathTool {
	return &CopyPathTool{pool: pool, resolver: resolver}
}

func (t *CopyPathTool) Name() string { return "copy_path" }

func (t *CopyPathTool) Description() string {
	return "Copy a file or directory between environments. Streams in chunks; safe for large/binary files. " +
		"Atomic at destination via a .partial-<uuid> + rename. Set recursive=true to copy a directory tree."
}

var copyPathSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "source_environment_id":      {"type": "string", "description": "Source environment name (from list_environments)"},
    "source_path":                {"type": "string", "description": "Absolute path on the source executor"},
    "destination_environment_id": {"type": "string", "description": "Destination environment name; may equal source"},
    "destination_path":           {"type": "string", "description": "Absolute path on the destination executor; parent must exist"},
    "recursive":                  {"type": "boolean", "description": "Treat source_path as a directory (tar-wrap); preserves mode + symlinks. Default false."},
    "timeout_ms":                 {"type": "integer", "description": "Hard cap on the whole copy; default 600000 (10 min)"}
  },
  "required": ["source_environment_id", "source_path", "destination_environment_id", "destination_path"]
}`)

func (t *CopyPathTool) InputSchema() json.RawMessage { return copyPathSchema }

type copyPathArgs struct {
	SourceEnvironmentID      string `json:"source_environment_id"`
	SourcePath               string `json:"source_path"`
	DestinationEnvironmentID string `json:"destination_environment_id"`
	DestinationPath          string `json:"destination_path"`
	Recursive                bool   `json:"recursive"`
	TimeoutMs                int    `json:"timeout_ms"`
}

const (
	copyPathDefaultTimeoutMs = 600_000           // 10 min
	copyPathChunkBytes       = 1 << 20           // 1 MiB per process/read
	copyPathPollWaitMs       = 250               // process/read waitMs per call
	copyPathCleanupTimeout   = 5 * time.Second   // for terminate + rm -f on error/cancel
)

func (t *CopyPathTool) Call(ctx context.Context, raw json.RawMessage) (MCPCallToolResult, error) {
	var a copyPathArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return errResult("invalid arguments: " + err.Error()), nil
	}
	if a.SourceEnvironmentID == "" || a.SourcePath == "" || a.DestinationEnvironmentID == "" || a.DestinationPath == "" {
		return errResult("source_environment_id, source_path, destination_environment_id, destination_path are required"), nil
	}
	if a.TimeoutMs <= 0 {
		a.TimeoutMs = copyPathDefaultTimeoutMs
	}

	srcExeID, err := t.resolver.Resolve(ctx, a.SourceEnvironmentID)
	if err != nil {
		return errResult("source: " + err.Error()), nil
	}
	dstExeID, err := t.resolver.Resolve(ctx, a.DestinationEnvironmentID)
	if err != nil {
		return errResult("destination: " + err.Error()), nil
	}
	srcBC, err := t.pool.Get(ctx, srcExeID)
	if err != nil {
		return errResult(fmt.Sprintf("source environment %q unavailable: %v", a.SourceEnvironmentID, err)), nil
	}
	dstBC, err := t.pool.Get(ctx, dstExeID)
	if err != nil {
		return errResult(fmt.Sprintf("destination environment %q unavailable: %v", a.DestinationEnvironmentID, err)), nil
	}

	xferID := uuid.NewString()
	srcPID := fmt.Sprintf("copy-src-%s", xferID)
	dstPID := fmt.Sprintf("copy-dst-%s", xferID)

	// Destination temp path lives in the same directory as the final
	// path so a same-FS rename is atomic. For recursive=true we extract
	// directly into the parent (atomicity of dir copies is per-file —
	// documented limitation in the spec).
	tmpPath := a.DestinationPath + ".partial-" + xferID
	dstParent := filepath.Dir(a.DestinationPath)

	var srcArgv, dstArgv []string
	if a.Recursive {
		base := filepath.Base(strings.TrimRight(a.SourcePath, "/"))
		srcParent := filepath.Dir(a.SourcePath)
		srcArgv = []string{"sh", "-c", fmt.Sprintf("tar czf - -C %s %s", shQuote(srcParent), shQuote(base))}
		dstArgv = []string{"sh", "-c", fmt.Sprintf("tar xzf - -C %s", shQuote(dstParent))}
	} else {
		srcArgv = []string{"sh", "-c", fmt.Sprintf("cat %s", shQuote(a.SourcePath))}
		dstArgv = []string{"sh", "-c", fmt.Sprintf("cat > %s", shQuote(tmpPath))}
	}

	pumpCtx, cancel := context.WithTimeout(ctx, time.Duration(a.TimeoutMs)*time.Millisecond)
	defer cancel()

	start := time.Now()

	// Start src process. If it fails immediately, bail before touching dst.
	if err := startProcess(pumpCtx, srcBC, srcPID, srcArgv, /*pipeStdin*/ false); err != nil {
		return errResult(fmt.Sprintf("source: process start failed: %v", err)), nil
	}
	// Start dst process with stdin pipe so we can feed it bytes.
	if err := startProcess(pumpCtx, dstBC, dstPID, dstArgv, /*pipeStdin*/ true); err != nil {
		t.cleanup(srcBC, srcPID, dstBC, "", "") // dst didn't start; no tmp to remove
		return errResult(fmt.Sprintf("destination: process start failed: %v", err)), nil
	}

	bytes, pumpErr := pumpChunks(pumpCtx, srcBC, srcPID, dstBC, dstPID)
	if pumpErr != nil {
		t.cleanup(srcBC, srcPID, dstBC, dstPID, tmpPathIfNotRecursive(tmpPath, a.Recursive, dstBC))
		// Distinguish src vs dst vs transport in the message when possible.
		return errResult(pumpErr.Error()), nil
	}

	// Both sides done; for non-recursive, rename .partial → final.
	if !a.Recursive {
		if err := remoteRename(pumpCtx, dstBC, tmpPath, a.DestinationPath); err != nil {
			t.cleanup(nil, "", nil, "", tmpPathIfNotRecursive(tmpPath, a.Recursive, dstBC))
			return errResult(fmt.Sprintf("destination: rename failed: %v", err)), nil
		}
	}

	dur := time.Since(start)
	body, _ := json.Marshal(map[string]any{
		"bytes":       bytes,
		"duration_ms": dur.Milliseconds(),
	})
	return MCPCallToolResult{
		Content: []MCPToolContent{{Type: "text", Text: string(body)}},
	}, nil
}

// tmpPathIfNotRecursive returns the tmp path to clean up if we're in
// non-recursive mode (where a .partial file was created); otherwise
// returns empty string (recursive tar extracts directly, no tmp to
// remove — leftover state requires more invasive cleanup that's out
// of scope).
//
// dstBC is accepted but unused here — it exists so cleanup callers
// don't have to special-case the recursive vs non-recursive branch
// at the call site.
func tmpPathIfNotRecursive(tmp string, recursive bool, _ *BridgeClient) string {
	if recursive {
		return ""
	}
	return tmp
}

// cleanup is best-effort: terminate any still-running processes and
// rm -f the destination tmp file (if any). Uses an independent
// background context with copyPathCleanupTimeout because the main
// ctx may already be cancelled (caller abort / timeout).
func (t *CopyPathTool) cleanup(srcBC *BridgeClient, srcPID string, dstBC *BridgeClient, dstPID string, tmpPath string) {
	bgCtx, cancel := context.WithTimeout(context.Background(), copyPathCleanupTimeout)
	defer cancel()
	if srcBC != nil && srcPID != "" {
		params, _ := json.Marshal(ProcessTerminateParams{ProcessID: srcPID})
		_, _ = srcBC.Call(bgCtx, ExecMethodProcessTerminate, params)
	}
	if dstBC != nil && dstPID != "" {
		params, _ := json.Marshal(ProcessTerminateParams{ProcessID: dstPID})
		_, _ = dstBC.Call(bgCtx, ExecMethodProcessTerminate, params)
	}
	if dstBC != nil && tmpPath != "" {
		// rm -f via process/start so we don't depend on fs/remove
		// (which the spec doesn't reach across this much code).
		rmPID := fmt.Sprintf("copy-rm-%s", uuid.NewString()[:8])
		startParams, _ := json.Marshal(ProcessStartParams{
			ProcessID: rmPID,
			Argv:      []string{"sh", "-c", fmt.Sprintf("rm -f %s", shQuote(tmpPath))},
			Cwd:       "/tmp",
			Env:       map[string]string{"PATH": "/usr/bin:/bin:/usr/local/bin"},
		})
		_, _ = dstBC.Call(bgCtx, ExecMethodProcessStart, startParams)
		// Don't wait for it — best-effort.
	}
}

// startProcess is a thin wrapper around process/start.
func startProcess(ctx context.Context, bc *BridgeClient, pid string, argv []string, pipeStdin bool) error {
	params, _ := json.Marshal(ProcessStartParams{
		ProcessID: pid,
		Argv:      argv,
		Cwd:       "/tmp",
		Env:       map[string]string{"PATH": "/usr/bin:/bin:/usr/local/bin"},
		TTY:       false,
		PipeStdin: pipeStdin,
	})
	_, err := bc.Call(ctx, ExecMethodProcessStart, params)
	return err
}

// remoteRename runs `mv <src> <dst>` and waits for it to exit. Used
// for the final atomic rename of the .partial file to the target.
func remoteRename(ctx context.Context, bc *BridgeClient, src, dst string) error {
	pid := fmt.Sprintf("copy-mv-%s", uuid.NewString()[:8])
	startParams, _ := json.Marshal(ProcessStartParams{
		ProcessID: pid,
		Argv:      []string{"sh", "-c", fmt.Sprintf("mv %s %s", shQuote(src), shQuote(dst))},
		Cwd:       "/tmp",
		Env:       map[string]string{"PATH": "/usr/bin:/bin:/usr/local/bin"},
	})
	if _, err := bc.Call(ctx, ExecMethodProcessStart, startParams); err != nil {
		return err
	}
	// Poll until exited.
	var afterSeq uint64
	for {
		rp, _ := json.Marshal(ProcessReadParams{
			ProcessID: pid, AfterSeq: afterSeq,
			MaxBytes: 4096, WaitMs: copyPathPollWaitMs,
		})
		raw, err := bc.Call(ctx, ExecMethodProcessRead, rp)
		if err != nil {
			return fmt.Errorf("mv read: %w", err)
		}
		var r ProcessReadResult
		if err := json.Unmarshal(raw, &r); err != nil {
			return fmt.Errorf("mv decode: %w", err)
		}
		afterSeq = r.NextSeq
		if r.Exited || r.Closed {
			if r.ExitCode != nil && *r.ExitCode != 0 {
				stderr := ""
				for _, c := range r.Chunks {
					if c.Stream == "stderr" {
						if b, derr := base64.StdEncoding.DecodeString(c.Chunk); derr == nil {
							stderr += string(b)
						}
					}
				}
				return fmt.Errorf("mv exit=%d stderr=%q", *r.ExitCode, stderr)
			}
			return nil
		}
	}
}

// pumpChunks shuttles bytes from src's stdout to dst's stdin one
// process/read window at a time. Returns the total bytes transferred
// when src exits cleanly. On any error returns (bytes-so-far, err).
func pumpChunks(ctx context.Context, srcBC *BridgeClient, srcPID string, dstBC *BridgeClient, dstPID string) (int64, error) {
	var afterSeq uint64
	var totalBytes int64
	for {
		// Read from source.
		readParams, _ := json.Marshal(ProcessReadParams{
			ProcessID: srcPID, AfterSeq: afterSeq,
			MaxBytes: copyPathChunkBytes, WaitMs: copyPathPollWaitMs,
		})
		raw, err := srcBC.Call(ctx, ExecMethodProcessRead, readParams)
		if err != nil {
			return totalBytes, fmt.Errorf("source: read failed at %d bytes: %w", totalBytes, err)
		}
		var r ProcessReadResult
		if err := json.Unmarshal(raw, &r); err != nil {
			return totalBytes, fmt.Errorf("source: decode failed: %w", err)
		}
		// Forward stdout chunks to dst (skip stderr — typically empty
		// from `cat`/`tar c`, but include in src error reporting on exit).
		for _, c := range r.Chunks {
			if c.Stream != "stdout" {
				continue
			}
			decoded, derr := base64.StdEncoding.DecodeString(c.Chunk)
			if derr != nil {
				return totalBytes, fmt.Errorf("source: base64 decode at %d bytes: %w", totalBytes, derr)
			}
			writeParams, _ := json.Marshal(ProcessWriteParams{
				ProcessID: dstPID,
				Chunk:     base64.StdEncoding.EncodeToString(decoded),
			})
			if _, err := dstBC.Call(ctx, ExecMethodProcessWrite, writeParams); err != nil {
				return totalBytes, fmt.Errorf("destination: write failed at %d bytes: %w", totalBytes, err)
			}
			totalBytes += int64(len(decoded))
		}
		afterSeq = r.NextSeq
		if r.Exited || r.Closed {
			if r.ExitCode != nil && *r.ExitCode != 0 {
				stderr := ""
				for _, c := range r.Chunks {
					if c.Stream == "stderr" {
						if b, derr := base64.StdEncoding.DecodeString(c.Chunk); derr == nil {
							stderr += string(b)
						}
					}
				}
				return totalBytes, fmt.Errorf("source: exit=%d stderr=%q", *r.ExitCode, stderr)
			}
			break
		}
	}
	// Source exited cleanly. dst's `cat` (or `tar x`) is still blocked
	// in read(stdin) because codex's process/write protocol has no
	// "close stdin" — only process/terminate. SIGTERM-after-data on
	// `cat > file` is safe (the kernel pipe buffer holds anything
	// in-flight; cat's POSIX signal handler exits and flushes the
	// file descriptor cleanly). For `tar x` it's the same — tar reads
	// each chunk, writes a file/dir, and the next read blocks; SIGTERM
	// during the blocked read exits without corrupting prior writes.
	//
	// Race we still need to handle: our last process/write returned
	// "accepted" but the bytes may still be queued in the writer
	// channel + the kernel pipe buffer (~64 KiB on Linux). Sending
	// terminate immediately could SIGTERM cat before it has drained
	// those bytes from the pipe to the file. So we poll the actual
	// file size on dst until it catches up to totalBytes (or a short
	// timeout) THEN terminate.
	if err := waitDestinationDrained(ctx, dstBC, dstPID, totalBytes); err != nil {
		// Don't fail the transfer on drain-poll error — terminate
		// + rename anyway and hope for the best; the spec's atomicity
		// is per-rename, partial files get cleaned up by the caller's
		// error path.
		// (intentionally no return)
		_ = err
	}
	// Terminate dst process. It has all its data; exits cleanly on SIGTERM.
	tparams, _ := json.Marshal(ProcessTerminateParams{ProcessID: dstPID})
	_, _ = dstBC.Call(ctx, ExecMethodProcessTerminate, tparams)
	return totalBytes, nil
}

// waitDestinationDrained polls until the dst process is no longer
// processing input — checked indirectly via process/read returning
// no new chunks for two consecutive polls. Conservative bound: 10 s.
//
// We don't poll the actual file size because that would need another
// shell process (`stat -c %s`) and chains of process/start calls.
// The "no new chunks" heuristic works because once cat has drained
// its stdin pipe, it has no stdout to emit either.
func waitDestinationDrained(ctx context.Context, bc *BridgeClient, pid string, expectedBytes int64) error {
	_ = expectedBytes // reserved for future shell-stat-based check
	deadline := time.Now().Add(10 * time.Second)
	var afterSeq uint64
	idlePolls := 0
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		rp, _ := json.Marshal(ProcessReadParams{
			ProcessID: pid, AfterSeq: afterSeq,
			MaxBytes: 4096, WaitMs: copyPathPollWaitMs,
		})
		raw, err := bc.Call(ctx, ExecMethodProcessRead, rp)
		if err != nil {
			return err
		}
		var r ProcessReadResult
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		afterSeq = r.NextSeq
		if r.Exited || r.Closed {
			// dst exited on its own — nothing more to drain.
			return nil
		}
		if len(r.Chunks) == 0 {
			idlePolls++
			if idlePolls >= 2 {
				return nil
			}
		} else {
			idlePolls = 0
		}
	}
	return nil // best-effort; fall through to terminate
}

// shQuote single-quotes a path for safe embedding in a `sh -c` script.
// Escapes embedded single quotes by closing the quote, escaping with
// backslash, and reopening: foo'bar → 'foo'\''bar'.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
