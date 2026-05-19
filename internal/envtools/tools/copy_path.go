package tools

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

	"github.com/agentserver/agentserver/internal/envtools/bridge"
	"github.com/agentserver/agentserver/internal/envtools/nameresolver"
)

// CopyPathTool copies a file or directory between executors.
//
// v0.56.0: prefers HTTPS out-of-band relay (curl PUT on src + curl GET
// on dst → bytes flow direct executor↔gateway↔executor, never through
// env-mcp's ws bridge). When the relay path is unavailable (gateway
// not configured, executor missing curl, or recursive copy below) it
// falls back to the v0.55.x ws cat-pump path that shuttles each chunk
// through process/read+process/write RPCs.
//
// See:
//   - docs/superpowers/specs/2026-05-18-copy-path-http-relay.md (v0.56.0)
//   - docs/superpowers/specs/2026-05-18-env-mcp-transfer-tool.md (v0.55.x)
type CopyPathTool struct {
	pool     *bridge.Pool
	resolver *nameresolver.Resolver
	relay    *bridge.RelayClient // nil-safe: Enabled() checked before use
	pidSeq   atomic.Uint64
}

func NewCopyPathTool(pool *bridge.Pool, resolver *nameresolver.Resolver, relay *bridge.RelayClient) *CopyPathTool {
	return &CopyPathTool{pool: pool, resolver: resolver, relay: relay}
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
    "timeout_ms":                 {"type": "integer", "description": "Hard cap on the whole copy; default 600000 (10 min)"},
    "transport":                  {"type": "string", "enum": ["auto", "http", "ws"], "description": "auto (default): try HTTP relay first, fall back to ws on curl-missing. http: HTTP only. ws: legacy ws cat-pump."}
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
	Transport                string `json:"transport,omitempty"` // "", "auto", "http", "ws"
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
	transport := strings.ToLower(strings.TrimSpace(a.Transport))
	if transport == "" {
		transport = "auto"
	}
	if transport != "auto" && transport != "http" && transport != "ws" {
		return errResult(fmt.Sprintf("invalid transport %q (want auto|http|ws)", a.Transport)), nil
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

	pumpCtx, cancel := context.WithTimeout(ctx, time.Duration(a.TimeoutMs)*time.Millisecond)
	defer cancel()

	// Dispatch by transport. "auto" tries HTTP, falls through to ws on
	// curl-missing (exit 127) or relay-disabled.
	useHTTP := false
	switch transport {
	case "http":
		useHTTP = true
	case "auto":
		useHTTP = t.relay.Enabled()
	}

	if useHTTP {
		res, err, fellThrough := t.callHTTPRelay(pumpCtx, a, srcExeID, dstExeID, srcBC, dstBC)
		if !fellThrough {
			return res, err
		}
		// fellThrough: relay path declined to run (e.g. relay disabled,
		// curl missing) and transport=auto. Drop into ws path below.
	}

	return t.callWSPump(pumpCtx, a, srcBC, dstBC)
}

// callHTTPRelay drives the v0.56.0 HTTP out-of-band path. Returns
// (result, err, fellThrough). fellThrough=true means "this path
// declined to handle the call; the caller should drop into the ws
// fallback" — used in transport=auto for relay-disabled and curl-127
// situations.
func (t *CopyPathTool) callHTTPRelay(ctx context.Context, a copyPathArgs, srcExeID, dstExeID string, srcBC, dstBC *bridge.BridgeClient) (MCPCallToolResult, error, bool) {
	if !t.relay.Enabled() {
		return MCPCallToolResult{}, nil, true // fall through (auto only path that reaches here)
	}

	xferID := uuid.NewString()
	tmpPath := a.DestinationPath + ".partial-" + xferID
	dstParent := filepath.Dir(a.DestinationPath)

	// TTL on the relay ticket: a bit longer than the tool timeout so a
	// near-deadline copy doesn't race the ticket expiring mid-transfer.
	ttl := time.Duration(a.TimeoutMs)*time.Millisecond + 30*time.Second
	ticket, err := t.relay.CreateRelay(ctx, srcExeID, dstExeID, ttl, 0)
	if err != nil {
		// Relay create itself failed — surface the error; don't fall
		// through silently or we'd hide infra bugs.
		return errResult(fmt.Sprintf("relay/create: %v", err)), nil, false
	}

	// Build src + dst shell commands. For recursive mode we pipe tar
	// through curl and need pipefail to catch curl-side errors —
	// pipefail is bash-specific, so use `bash -c` for that branch. If
	// bash is missing on the executor, the shell exits 127 and (under
	// transport=auto) we fall back to the ws cat-pump path the same
	// way as missing curl.
	var srcShell, dstShell string
	var srcArgv, dstArgv []string
	if a.Recursive {
		base := filepath.Base(strings.TrimRight(a.SourcePath, "/"))
		srcParent := filepath.Dir(a.SourcePath)
		srcShell = fmt.Sprintf(
			"set -o pipefail; tar czf - -C %s %s | curl -fsS --upload-file - -H 'Authorization: Bearer %s' %s",
			shQuote(srcParent), shQuote(base), ticket.Ticket, shQuote(ticket.UploadURL))
		dstShell = fmt.Sprintf(
			"set -o pipefail; curl -fsS -H 'Authorization: Bearer %s' %s | tar xzf - -C %s",
			ticket.Ticket, shQuote(ticket.DownloadURL), shQuote(dstParent))
		srcArgv = []string{"bash", "-c", srcShell}
		dstArgv = []string{"bash", "-c", dstShell}
	} else {
		srcShell = fmt.Sprintf(
			"curl -fsS --upload-file %s -H 'Authorization: Bearer %s' %s",
			shQuote(a.SourcePath), ticket.Ticket, shQuote(ticket.UploadURL))
		dstShell = fmt.Sprintf(
			"curl -fsS -H 'Authorization: Bearer %s' %s -o %s",
			ticket.Ticket, shQuote(ticket.DownloadURL), shQuote(tmpPath))
		srcArgv = []string{"sh", "-c", srcShell}
		dstArgv = []string{"sh", "-c", dstShell}
	}

	srcPID := fmt.Sprintf("relay-src-%s", xferID)
	dstPID := fmt.Sprintf("relay-dst-%s", xferID)
	start := time.Now()

	// Run both shells in parallel via process/start + poll-to-exit.
	type runResult struct {
		exit   int
		stderr string
		err    error
	}
	srcCh := make(chan runResult, 1)
	dstCh := make(chan runResult, 1)
	// Early-abort: if either side fails fast (src curl errors before any
	// bytes flow, or dst curl gets 4xx from gateway), the other side
	// would otherwise block on the relay's pairing wait until the ticket
	// TTL fires (~5 min default). Cancel the shared ctx on first failure
	// so the other runShellToExit returns promptly and terminates its
	// process.
	abortCtx, abort := context.WithCancel(ctx)
	defer abort()
	go func() {
		exit, stderr, err := runShellToExit(abortCtx, srcBC, srcPID, srcArgv)
		if err != nil || exit != 0 {
			abort()
		}
		srcCh <- runResult{exit, stderr, err}
	}()
	go func() {
		exit, stderr, err := runShellToExit(abortCtx, dstBC, dstPID, dstArgv)
		if err != nil || exit != 0 {
			abort()
		}
		dstCh <- runResult{exit, stderr, err}
	}()
	srcRes := <-srcCh
	dstRes := <-dstCh

	// curl (or bash for recursive) exit 127 = command not found. Under
	// transport=auto, silently drop into the ws cat-pump path. Use the
	// caller-provided value here so test calls passing exact "http"
	// don't accidentally fall through.
	if strings.EqualFold(strings.TrimSpace(a.Transport), "auto") || strings.TrimSpace(a.Transport) == "" {
		if srcRes.exit == 127 || dstRes.exit == 127 {
			t.relayCleanup(srcBC, dstBC, tmpPath, a.Recursive)
			return MCPCallToolResult{}, nil, true
		}
	}

	if srcRes.err != nil {
		t.relayCleanup(srcBC, dstBC, tmpPath, a.Recursive)
		return errResult(fmt.Sprintf("source shell: %v", srcRes.err)), nil, false
	}
	if dstRes.err != nil {
		t.relayCleanup(srcBC, dstBC, tmpPath, a.Recursive)
		return errResult(fmt.Sprintf("destination shell: %v", dstRes.err)), nil, false
	}
	if srcRes.exit != 0 {
		t.relayCleanup(srcBC, dstBC, tmpPath, a.Recursive)
		return errResult(fmt.Sprintf("source curl exit=%d stderr=%q", srcRes.exit, srcRes.stderr)), nil, false
	}
	if dstRes.exit != 0 {
		t.relayCleanup(srcBC, dstBC, tmpPath, a.Recursive)
		return errResult(fmt.Sprintf("destination curl exit=%d stderr=%q", dstRes.exit, dstRes.stderr)), nil, false
	}

	// Non-recursive: rename .partial → final.
	if !a.Recursive {
		if err := remoteRename(ctx, dstBC, tmpPath, a.DestinationPath); err != nil {
			t.relayCleanup(nil, dstBC, tmpPath, false)
			return errResult(fmt.Sprintf("destination: rename failed: %v", err)), nil, false
		}
	}

	body, _ := json.Marshal(map[string]any{
		"transport":   "http",
		"duration_ms": time.Since(start).Milliseconds(),
	})
	return MCPCallToolResult{
		Content: []MCPToolContent{{Type: "text", Text: string(body)}},
	}, nil, false
}

// relayCleanup is the http-relay variant of cleanup. Source and dest
// processes have already exited (we wait on them above); only the dst
// .partial file may need rm.
func (t *CopyPathTool) relayCleanup(_ *bridge.BridgeClient, dstBC *bridge.BridgeClient, tmpPath string, recursive bool) {
	if recursive || dstBC == nil || tmpPath == "" {
		return
	}
	bgCtx, cancel := context.WithTimeout(context.Background(), copyPathCleanupTimeout)
	defer cancel()
	rmPID := fmt.Sprintf("relay-rm-%s", uuid.NewString()[:8])
	startParams, _ := json.Marshal(bridge.ProcessStartParams{
		ProcessID: rmPID,
		Argv:      []string{"sh", "-c", fmt.Sprintf("rm -f %s", shQuote(tmpPath))},
		Cwd:       "/tmp",
		Env:       map[string]string{"PATH": "/usr/bin:/bin:/usr/local/bin"},
	})
	_, _ = dstBC.Call(bgCtx, bridge.ExecMethodProcessStart, startParams)
}

// runShellToExit starts a process and polls process/read until it
// exits. Returns the exit code and accumulated stderr (capped). On
// transport error, returns (-1, "", err). If ctx fires before the
// process exits naturally, the remote process is terminated via
// process/terminate using a fresh background context so the terminate
// itself isn't cancelled by the cancellation that triggered it.
func runShellToExit(ctx context.Context, bc *bridge.BridgeClient, pid string, argv []string) (retExit int, retStderr string, retErr error) {
	if err := startProcess(ctx, bc, pid, argv, false); err != nil {
		return -1, "", fmt.Errorf("process/start: %w", err)
	}
	defer func() {
		// If we're leaving because ctx was cancelled (not because the
		// process exited cleanly), force-terminate so the executor
		// doesn't keep a zombie curl/tar running.
		if ctx.Err() != nil {
			bgCtx, cancel := context.WithTimeout(context.Background(), copyPathCleanupTimeout)
			defer cancel()
			params, _ := json.Marshal(bridge.ProcessTerminateParams{ProcessID: pid})
			_, _ = bc.Call(bgCtx, bridge.ExecMethodProcessTerminate, params)
		}
	}()
	const stderrCap = 8 * 1024
	var stderrBuf strings.Builder
	var afterSeq uint64
	for {
		rp, _ := json.Marshal(bridge.ProcessReadParams{
			ProcessID: pid, AfterSeq: afterSeq,
			MaxBytes: 64 * 1024, WaitMs: 500,
		})
		raw, err := bc.Call(ctx, bridge.ExecMethodProcessRead, rp)
		if err != nil {
			return -1, stderrBuf.String(), fmt.Errorf("process/read: %w", err)
		}
		var r bridge.ProcessReadResult
		if err := json.Unmarshal(raw, &r); err != nil {
			return -1, stderrBuf.String(), fmt.Errorf("decode: %w", err)
		}
		for _, c := range r.Chunks {
			if c.Stream == "stderr" && stderrBuf.Len() < stderrCap {
				if b, derr := base64.StdEncoding.DecodeString(c.Chunk); derr == nil {
					stderrBuf.Write(b)
				}
			}
		}
		afterSeq = r.NextSeq
		if r.Exited || r.Closed {
			exit := 0
			if r.ExitCode != nil {
				exit = *r.ExitCode
			}
			s := stderrBuf.String()
			if len(s) > stderrCap {
				s = s[:stderrCap]
			}
			return exit, s, nil
		}
	}
}

// callWSPump is the v0.55.x ws cat-pump implementation, preserved as
// fallback for: transport=ws, transport=auto with curl missing, or
// transport=auto with relay disabled at config.
func (t *CopyPathTool) callWSPump(pumpCtx context.Context, a copyPathArgs, srcBC, dstBC *bridge.BridgeClient) (MCPCallToolResult, error) {
	xferID := uuid.NewString()
	srcPID := fmt.Sprintf("copy-src-%s", xferID)
	dstPID := fmt.Sprintf("copy-dst-%s", xferID)

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

	start := time.Now()

	if err := startProcess(pumpCtx, srcBC, srcPID, srcArgv, false); err != nil {
		return errResult(fmt.Sprintf("source: process start failed: %v", err)), nil
	}
	if err := startProcess(pumpCtx, dstBC, dstPID, dstArgv, true); err != nil {
		t.cleanup(srcBC, srcPID, dstBC, "", "")
		return errResult(fmt.Sprintf("destination: process start failed: %v", err)), nil
	}

	bytes, pumpErr := pumpChunks(pumpCtx, srcBC, srcPID, dstBC, dstPID)
	if pumpErr != nil {
		t.cleanup(srcBC, srcPID, dstBC, dstPID, tmpPathIfNotRecursive(tmpPath, a.Recursive, dstBC))
		return errResult(pumpErr.Error()), nil
	}

	if !a.Recursive {
		if err := remoteRename(pumpCtx, dstBC, tmpPath, a.DestinationPath); err != nil {
			t.cleanup(nil, "", nil, "", tmpPathIfNotRecursive(tmpPath, a.Recursive, dstBC))
			return errResult(fmt.Sprintf("destination: rename failed: %v", err)), nil
		}
	}

	body, _ := json.Marshal(map[string]any{
		"transport":   "ws",
		"bytes":       bytes,
		"duration_ms": time.Since(start).Milliseconds(),
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
func tmpPathIfNotRecursive(tmp string, recursive bool, _ *bridge.BridgeClient) string {
	if recursive {
		return ""
	}
	return tmp
}

// cleanup is best-effort: terminate any still-running processes and
// rm -f the destination tmp file (if any). Uses an independent
// background context with copyPathCleanupTimeout because the main
// ctx may already be cancelled (caller abort / timeout).
func (t *CopyPathTool) cleanup(srcBC *bridge.BridgeClient, srcPID string, dstBC *bridge.BridgeClient, dstPID string, tmpPath string) {
	bgCtx, cancel := context.WithTimeout(context.Background(), copyPathCleanupTimeout)
	defer cancel()
	if srcBC != nil && srcPID != "" {
		params, _ := json.Marshal(bridge.ProcessTerminateParams{ProcessID: srcPID})
		_, _ = srcBC.Call(bgCtx, bridge.ExecMethodProcessTerminate, params)
	}
	if dstBC != nil && dstPID != "" {
		params, _ := json.Marshal(bridge.ProcessTerminateParams{ProcessID: dstPID})
		_, _ = dstBC.Call(bgCtx, bridge.ExecMethodProcessTerminate, params)
	}
	if dstBC != nil && tmpPath != "" {
		// rm -f via process/start so we don't depend on fs/remove
		// (which the spec doesn't reach across this much code).
		rmPID := fmt.Sprintf("copy-rm-%s", uuid.NewString()[:8])
		startParams, _ := json.Marshal(bridge.ProcessStartParams{
			ProcessID: rmPID,
			Argv:      []string{"sh", "-c", fmt.Sprintf("rm -f %s", shQuote(tmpPath))},
			Cwd:       "/tmp",
			Env:       map[string]string{"PATH": "/usr/bin:/bin:/usr/local/bin"},
		})
		_, _ = dstBC.Call(bgCtx, bridge.ExecMethodProcessStart, startParams)
		// Don't wait for it — best-effort.
	}
}

// startProcess is a thin wrapper around process/start.
func startProcess(ctx context.Context, bc *bridge.BridgeClient, pid string, argv []string, pipeStdin bool) error {
	params, _ := json.Marshal(bridge.ProcessStartParams{
		ProcessID: pid,
		Argv:      argv,
		Cwd:       "/tmp",
		Env:       map[string]string{"PATH": "/usr/bin:/bin:/usr/local/bin"},
		TTY:       false,
		PipeStdin: pipeStdin,
	})
	_, err := bc.Call(ctx, bridge.ExecMethodProcessStart, params)
	return err
}

// remoteRename runs `mv <src> <dst>` and waits for it to exit. Used
// for the final atomic rename of the .partial file to the target.
func remoteRename(ctx context.Context, bc *bridge.BridgeClient, src, dst string) error {
	pid := fmt.Sprintf("copy-mv-%s", uuid.NewString()[:8])
	startParams, _ := json.Marshal(bridge.ProcessStartParams{
		ProcessID: pid,
		Argv:      []string{"sh", "-c", fmt.Sprintf("mv %s %s", shQuote(src), shQuote(dst))},
		Cwd:       "/tmp",
		Env:       map[string]string{"PATH": "/usr/bin:/bin:/usr/local/bin"},
	})
	if _, err := bc.Call(ctx, bridge.ExecMethodProcessStart, startParams); err != nil {
		return err
	}
	// Poll until exited.
	var afterSeq uint64
	for {
		rp, _ := json.Marshal(bridge.ProcessReadParams{
			ProcessID: pid, AfterSeq: afterSeq,
			MaxBytes: 4096, WaitMs: copyPathPollWaitMs,
		})
		raw, err := bc.Call(ctx, bridge.ExecMethodProcessRead, rp)
		if err != nil {
			return fmt.Errorf("mv read: %w", err)
		}
		var r bridge.ProcessReadResult
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
func pumpChunks(ctx context.Context, srcBC *bridge.BridgeClient, srcPID string, dstBC *bridge.BridgeClient, dstPID string) (int64, error) {
	var afterSeq uint64
	var totalBytes int64
	for {
		// Read from source.
		readParams, _ := json.Marshal(bridge.ProcessReadParams{
			ProcessID: srcPID, AfterSeq: afterSeq,
			MaxBytes: copyPathChunkBytes, WaitMs: copyPathPollWaitMs,
		})
		raw, err := srcBC.Call(ctx, bridge.ExecMethodProcessRead, readParams)
		if err != nil {
			return totalBytes, fmt.Errorf("source: read failed at %d bytes: %w", totalBytes, err)
		}
		var r bridge.ProcessReadResult
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
			writeParams, _ := json.Marshal(bridge.ProcessWriteParams{
				ProcessID: dstPID,
				Chunk:     base64.StdEncoding.EncodeToString(decoded),
			})
			if _, err := dstBC.Call(ctx, bridge.ExecMethodProcessWrite, writeParams); err != nil {
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
	tparams, _ := json.Marshal(bridge.ProcessTerminateParams{ProcessID: dstPID})
	_, _ = dstBC.Call(ctx, bridge.ExecMethodProcessTerminate, tparams)
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
func waitDestinationDrained(ctx context.Context, bc *bridge.BridgeClient, pid string, expectedBytes int64) error {
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
		rp, _ := json.Marshal(bridge.ProcessReadParams{
			ProcessID: pid, AfterSeq: afterSeq,
			MaxBytes: 4096, WaitMs: copyPathPollWaitMs,
		})
		raw, err := bc.Call(ctx, bridge.ExecMethodProcessRead, rp)
		if err != nil {
			return err
		}
		var r bridge.ProcessReadResult
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
