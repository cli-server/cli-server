# codex-agent-sdk-go — Design Spec

**Status:** draft
**Date:** 2026-05-04
**Owner:** ccbroker
**Scope:** standalone Go SDK that wraps the `codex` CLI binary,
**fully aligned with `@openai/codex-sdk` (TypeScript) — bit-for-bit on the
wire and feature-for-feature on the API surface**, translated to Go idioms.

## Alignment principle

This SDK is a **Go port** of `@openai/codex-sdk`, not an independent design.
Every type, option, default, env var, and CLI argument the TS SDK produces
is reproduced here with a Go-idiomatic name. Any deviation must be
explicitly listed in the "Intentional divergences from TS SDK" section
with rationale. PRs that introduce new behavior MUST first land in the
upstream TS SDK or update the divergence list.

A reviewer should be able to take a TS SDK consumer's call:

```ts
codex.startThread({ sandboxMode: "workspace-write", ... })
     .runStreamed([{ type: "text", text: "hi" }, { type: "local_image", path: "/x.png" }],
                  { outputSchema: { type: "object", ... } })
```

and translate it line-by-line to Go without deciding what to drop or change.

## Why

ccbroker today drives the `claude` CLI through `claude-agent-sdk-go`. We're
adding `codex` as a second harness (per multi-harness design conversation,
2026-05-04). The simplest, most symmetric integration is to give ccbroker a
codex-side equivalent of `claude-agent-sdk-go`: same shape, same idioms, so
the broker's session-worker / runner layer can switch driver implementations
without re-architecting how it consumes events.

This SDK is **standalone** (its own Go module, parallel to
`claude-agent-sdk-go`), so it can be reused outside ccbroker and versioned
against codex CLI releases independently of agentserver.

## Non-goals

- We are **not** wrapping `codex app-server` (the persistent JSON-RPC server
  mode that the Python SDK targets). See "Why `codex exec`, not
  `codex app-server`" below.
- We are **not** providing an MCP server here — broker-side tools are wired
  through codex's external MCP config in a later phase.
- We are **not** auto-discovering the codex binary via npm-platform-package
  resolution like the TS SDK does (`@openai/codex-linux-x64` etc.). Default
  is PATH lookup; consumers can override via `CodexOptions.BinaryPath`. This
  is an intentional divergence (see list below).

## Why `codex exec`, not `codex app-server`

The codex binary exposes two machine-consumable modes:

1. `codex exec --experimental-json` — one-shot CLI: stdin = prompt, stdout =
   newline-delimited JSON events, exit when turn ends. Used by
   `@openai/codex-sdk` (TypeScript).
2. `codex app-server --listen stdio://` — long-running JSON-RPC v2 server,
   bidirectional, holds in-memory thread state across multiple turns. Used
   by `openai-codex-app-server-sdk` (Python).

ccbroker is a **stateless-harness** broker by design: between turns, no
harness process exists. Session continuity is reconstructed from disk (the
session JSONL written by codex into `~/.codex/sessions/<thread_id>.jsonl`)
on the next `resume`. This mirrors how the existing claude integration
works.

`codex app-server` exists specifically to *avoid* statelessness — its
value is keeping context warm across turns. Marrying it to a stateless
broker gives the worst of both worlds: pay JSON-RPC complexity (handshake,
id pairing, bidirectional flow control) and still cold-start every turn.
Conversely, keeping app-server alive across turns would violate the
stateless-harness principle and force a redesign of session_worker, idle
reaping, crash recovery, and concurrent-session resource accounting.

`codex exec` therefore wins on every relevant axis: zero harness process
between turns, session state lives on disk, fork-and-resume on next turn,
and one-line symmetry with the claude driver. If we ever drop the
stateless-harness principle, switching to app-server becomes a separate
SDK (`codex-appserver-sdk-go`) and a separate driver — not a refactor of
this one.

## Module layout

Repo: a new Git repository at `github.com/agentserver/codex-agent-sdk-go`
(parallel to `github.com/agentserver/claude-agent-sdk-go`).

```
codex-agent-sdk-go/
├── go.mod                  // module github.com/agentserver/codex-agent-sdk-go, go 1.22
├── README.md               // installation + quickstart
├── codex.go                // Codex (top-level entry), CodexOptions
├── thread.go               // Thread, RunInput, Turn, RunStreamed
├── exec.go                 // CodexExec — subprocess + JSONL stream (non-exported)
├── events.go               // ThreadEvent union (typed) + JSON unmarshal
├── items.go                // ThreadItem union (typed) + JSON unmarshal
├── options.go              // ThreadOptions, TurnOptions, ConfigOverride
├── error.go                // typed error types (Spawn, NonZeroExit, ParseEvent, ...)
├── examples/
│   └── quickstart/main.go  // mirrors TS SDK README example
└── exec_test.go, thread_test.go, events_test.go
```

Single-package design (`package codex`) — same convention as
`claude-agent-sdk-go`. Internal helpers don't need their own subpackage at
this size.

## Wire model (non-negotiable, set by codex CLI)

### Invocation

```
codex exec --experimental-json \
  [-c key=value]... \
  [--model M] [--sandbox MODE] [--cd DIR] [--add-dir DIR]... \
  [--skip-git-repo-check] [--output-schema FILE] \
  [resume <thread_id>]
```

- `--experimental-json` is **required** to get JSONL events on stdout.
- The prompt is written to **stdin**, then stdin is closed. Codex accepts
  prompt as positional too, but stdin avoids quoting/length issues — same
  choice the TS SDK made.
- `resume <thread_id>` is a **subcommand** of `exec` and goes at the end of
  the args (after all flags).
- Stdout: one JSON object per line (`thread.started`, `turn.started`,
  `item.started`, `item.updated`, `item.completed`, `turn.completed`,
  `turn.failed`, top-level `error`).
- Stderr: free-form diagnostic text. Captured for inclusion in non-zero-exit
  errors but not parsed.
- Exit: 0 on clean turn; non-zero indicates spawn or unhandled CLI error.

### Auth

- `CODEX_API_KEY` env var carries the bearer token. (NOT `OPENAI_API_KEY` —
  see TS SDK exec.ts:158.)
- `-c openai_base_url="<url>"` overrides the model endpoint.

### Sandboxing posture

We expose `--sandbox` mode (`read-only` / `workspace-write` /
`danger-full-access`) and `--config approval_policy=...`
(`never` / `on-request` / `on-failure` / `untrusted`). Caller decides what's
appropriate; SDK passes through.

## Public API

The TS-to-Go name mapping is mechanical: TS `camelCase` → Go `PascalCase`,
TS `string?` → Go zero value, TS `unknown` → Go `any`, TS unions → Go
sealed interface. Promise/AsyncGenerator → ctx + channel. Every TS field
in `CodexOptions` / `ThreadOptions` / `TurnOptions` is present below.

```go
package codex

// Codex is the top-level handle. Cheap to construct; safe to share across
// goroutines (ThreadOptions / state are per-Thread).
type Codex struct { /* unexported */ }

type CodexOptions struct {
    // TS: codexPathOverride. Path to codex binary. Default "codex" (PATH lookup).
    BinaryPath string
    // TS: baseUrl. Becomes `-c openai_base_url="<value>"` on every spawn
    // (NOT an env var).
    BaseURL string
    // TS: apiKey. Set as CODEX_API_KEY env var on every spawn.
    APIKey string
    // TS: config. Extra TOML-typed config overrides; flattened to
    // dotted-key form and serialized as TOML literals (see "Config
    // serialization" section). Applied as `-c key=value` on every spawn,
    // BEFORE per-thread/per-turn flags.
    Config map[string]any
    // TS: env. If non-nil, replaces process env entirely. If nil, current
    // process env is inherited. In both cases, after composition:
    //   - if CODEX_INTERNAL_ORIGINATOR_OVERRIDE is unset, set it to "codex_sdk_go"
    //   - if APIKey != "", set CODEX_API_KEY = APIKey
    Env map[string]string
}

func New(opts CodexOptions) *Codex

// StartThread builds a Thread with no thread_id. On first RunStreamed, codex
// generates a fresh thread_id and emits it via the `thread.started` event,
// which the SDK captures into Thread.ID() automatically.
func (c *Codex) StartThread(opts ThreadOptions) *Thread

// ResumeThread builds a Thread already bound to an existing thread_id. Every
// RunStreamed appends `resume <threadID>` to the codex invocation.
func (c *Codex) ResumeThread(threadID string, opts ThreadOptions) *Thread

type ThreadOptions struct {
    Model                string          // TS: model              → -m <value>
    SandboxMode          SandboxMode     // TS: sandboxMode        → -s <value>
    WorkingDirectory     string          // TS: workingDirectory   → --cd <value>
    AdditionalDirs       []string        // TS: additionalDirectories → --add-dir <v> (repeated)
    SkipGitRepoCheck     bool            // TS: skipGitRepoCheck   → --skip-git-repo-check
    ModelReasoningEffort ReasoningEffort // TS: modelReasoningEffort → -c model_reasoning_effort="<v>"
    NetworkAccessEnabled *bool           // TS: networkAccessEnabled → -c sandbox_workspace_write.network_access=<v>
    WebSearchMode        WebSearchMode   // TS: webSearchMode      → -c web_search="<v>"
    WebSearchEnabled     *bool           // TS: webSearchEnabled (legacy). Used only if WebSearchMode is "":
                                         //   true  → -c web_search="live"
                                         //   false → -c web_search="disabled"
    ApprovalPolicy       ApprovalMode    // TS: approvalPolicy     → -c approval_policy="<v>"
}

type SandboxMode string
const (
    SandboxReadOnly         SandboxMode = "read-only"
    SandboxWorkspaceWrite   SandboxMode = "workspace-write"
    SandboxDangerFullAccess SandboxMode = "danger-full-access"
)

type ApprovalMode string
const (
    ApprovalNever     ApprovalMode = "never"
    ApprovalOnRequest ApprovalMode = "on-request"
    ApprovalOnFailure ApprovalMode = "on-failure"
    ApprovalUntrusted ApprovalMode = "untrusted"
)

type ReasoningEffort string
const (
    ReasoningMinimal ReasoningEffort = "minimal"
    ReasoningLow     ReasoningEffort = "low"
    ReasoningMedium  ReasoningEffort = "medium"
    ReasoningHigh    ReasoningEffort = "high"
    ReasoningXHigh   ReasoningEffort = "xhigh"
)

type WebSearchMode string
const (
    WebSearchDisabled WebSearchMode = "disabled"
    WebSearchCached   WebSearchMode = "cached"
    WebSearchLive     WebSearchMode = "live"
)

// Input is the prompt parameter. Mirror of TS `Input = string | UserInput[]`.
// A bare string is equivalent to []UserInput{{Type: InputText, Text: s}}.
type Input interface{ codexInput() }

type StringInput string
func (StringInput) codexInput() {}

type PartsInput []UserInput
func (PartsInput) codexInput() {}

// UserInput mirrors TS `UserInput = {type:"text",text:string} | {type:"local_image",path:string}`.
type UserInput struct {
    Type UserInputType
    // Set when Type == InputText.
    Text string
    // Set when Type == InputLocalImage. Path on local FS to an image file.
    // Becomes `--image <Path>` on the codex invocation.
    Path string
}

type UserInputType string
const (
    InputText       UserInputType = "text"
    InputLocalImage UserInputType = "local_image"
)

type Thread struct { /* unexported */ }

// ID returns the thread_id captured from the first `thread.started` event,
// or the explicit id passed to ResumeThread. Returns "" if neither.
// Mirror of TS `Thread.id: string | null` (Go uses "" instead of null).
func (t *Thread) ID() string

// Run is the buffered convenience wrapper. Drains RunStreamed, aggregates
// items, and resolves to a Turn. Mirror of TS `Thread.run`:
//
//   - On `item.completed` events: appends item to Turn.Items; if the item
//     is an AgentMessageItem, Turn.FinalResponse is overwritten with its Text.
//   - On `turn.completed`: Turn.Usage is set.
//   - On `turn.failed`: Run returns (zero Turn, *TurnFailedError{Message}).
//     The events channel is drained to completion before returning.
//   - Spawn / non-zero exit / ctx errors: returned as Wait() does.
func (t *Thread) Run(ctx context.Context, input Input, opts TurnOptions) (Turn, error)

// RunStreamed sends the prompt and returns a stream of typed events. Mirror
// of TS `Thread.runStreamed`. The stream's events channel emits every
// JSONL event codex writes, in order. The `thread.started` event's id is
// captured into Thread.ID() before being yielded.
//
// Cancellation: cancelling ctx terminates the subprocess. See "Subprocess
// behavior" for signal escalation.
func (t *Thread) RunStreamed(ctx context.Context, input Input, opts TurnOptions) (*StreamedTurn, error)

type StreamedTurn struct { /* unexported */ }
func (s *StreamedTurn) Events() <-chan ThreadEvent
// Wait blocks until the events channel is fully drained AND the codex
// subprocess has exited. Returns nil on clean exit; non-nil on
// SpawnError / NonZeroExitError / ctx.Err. Note: a `turn.failed` event
// alone does NOT cause Wait to return non-nil (codex still exits 0 in
// that case) — Run() surfaces turn.failed as an error, RunStreamed
// leaves it to the caller. Mirrors TS exactly.
func (s *StreamedTurn) Wait() error

type TurnOptions struct {
    // TS: outputSchema. Arbitrary JSON-serializable Go value (typically a
    // map[string]any representing a JSON Schema). When non-nil, the SDK:
    //   1. Marshals to JSON
    //   2. Writes to a fresh tempdir under os.TempDir() with prefix
    //      "codex-output-schema-"
    //   3. Passes path via --output-schema
    //   4. Removes the tempdir when the turn ends (success OR failure)
    // Type matches TS `unknown`.
    OutputSchema any
    // (no Signal field — Go uses ctx)
}

type Turn struct {
    Items         []ThreadItem  // appended from item.completed events, in order
    FinalResponse string        // last AgentMessageItem.Text seen, "" if none
    Usage         *Usage        // from turn.completed; nil if no turn.completed seen
}

// Type aliases preserved for parity with TS index.ts exports:
type RunResult = Turn
type RunStreamedResult = StreamedTurn
```

## Event / item types

Mirror the TS SDK union (events.ts, items.ts) verbatim, translated to Go
sum-types via tagged interfaces:

```go
type ThreadEvent interface{ threadEvent() }

type ThreadStartedEvent struct {
    Type     string `json:"type"`      // "thread.started"
    ThreadID string `json:"thread_id"`
}
func (ThreadStartedEvent) threadEvent() {}

type TurnStartedEvent   struct{ Type string `json:"type"` }
type TurnCompletedEvent struct{ Type string `json:"type"`; Usage Usage `json:"usage"` }
type TurnFailedEvent    struct{ Type string `json:"type"`; Error ThreadError `json:"error"` }
type ItemStartedEvent   struct{ Type string `json:"type"`; Item ThreadItem `json:"item"` }
type ItemUpdatedEvent   struct{ Type string `json:"type"`; Item ThreadItem `json:"item"` }
type ItemCompletedEvent struct{ Type string `json:"type"`; Item ThreadItem `json:"item"` }
type ThreadErrorEvent   struct{ Type string `json:"type"`; Message string `json:"message"` }

type ThreadError struct{ Message string `json:"message"` }
type Usage struct {
    InputTokens             int `json:"input_tokens"`
    CachedInputTokens       int `json:"cached_input_tokens"`
    OutputTokens            int `json:"output_tokens"`
    ReasoningOutputTokens   int `json:"reasoning_output_tokens"`
}
```

`ThreadItem` is a similar discriminated-union: `AgentMessageItem`,
`ReasoningItem`, `CommandExecutionItem`, `FileChangeItem`,
`McpToolCallItem`, `WebSearchItem`, `TodoListItem`, `ErrorItem`. Each
has a `Type` field used for `json.Unmarshal` discrimination.

### Unmarshal strategy

Codex events arrive as raw JSON lines. Strategy: decode envelope `{type:
string}` first, then re-decode into the concrete struct. If `type` is
unknown, return an `UnknownEvent{Type, Raw json.RawMessage}` so consumers
can choose to log/skip rather than crash. **Forward-compat is essential** —
codex CLI versions ahead of the SDK MUST NOT break consumers.

`ThreadItem` unmarshalling uses the same envelope-then-concrete pattern.
Unknown item types become `UnknownItem{Type, Raw}`.

## Subprocess / streaming behavior

Implementation in `exec.go`. Argument list is built in this exact order
(mirrors TS `exec.ts:73-148` line-for-line so behavior is bit-identical):

1. `["exec", "--experimental-json"]`
2. For each entry from `serializeConfigOverrides(CodexOptions.Config)`:
   `"--config", "<dotted.key>=<toml-value>"` (CodexOptions.Config first
   so per-thread/per-turn flags can override)
3. If `CodexOptions.BaseURL != ""`:
   `"--config", "openai_base_url=" + tomlString(BaseURL)`
4. If `Model != ""`: `"--model", Model`
5. If `SandboxMode != ""`: `"--sandbox", string(SandboxMode)`
6. If `WorkingDirectory != ""`: `"--cd", WorkingDirectory`
7. For each dir in `AdditionalDirs`: `"--add-dir", dir`
8. If `SkipGitRepoCheck`: `"--skip-git-repo-check"`
9. If `OutputSchema != nil` (path computed in step 0): `"--output-schema", schemaPath`
10. If `ModelReasoningEffort != ""`:
    `"--config", "model_reasoning_effort=\"" + ReasoningEffort + "\""`
11. If `NetworkAccessEnabled != nil`:
    `"--config", "sandbox_workspace_write.network_access=" + bool`
12. Web search:
    - If `WebSearchMode != ""`: `"--config", "web_search=\"" + WebSearchMode + "\""`
    - else if `WebSearchEnabled != nil && *WebSearchEnabled`:
      `"--config", "web_search=\"live\""`
    - else if `WebSearchEnabled != nil && !*WebSearchEnabled`:
      `"--config", "web_search=\"disabled\""`
13. If `ApprovalPolicy != ""`:
    `"--config", "approval_policy=\"" + ApprovalPolicy + "\""`
14. If Thread has thread_id (resume mode): `"resume", threadID`
15. For each image path collected from Input: `"--image", path`
    **(yes — images come AFTER `resume <id>`. They're parsed by the
    `resume` subcommand, which also accepts `--image`. Verified against
    `codex exec resume --help` on codex-cli 0.125.0.)**

### Environment composition

Mirrors `exec.ts:148-167`:

```
env := map[string]string{}
if CodexOptions.Env != nil {
    for k, v := range CodexOptions.Env { env[k] = v }
} else {
    for _, kv := range os.Environ() { ...split-and-copy... }
}
if env["CODEX_INTERNAL_ORIGINATOR_OVERRIDE"] == "" {
    env["CODEX_INTERNAL_ORIGINATOR_OVERRIDE"] = "codex_sdk_go"
}
if CodexOptions.APIKey != "" {
    env["CODEX_API_KEY"] = CodexOptions.APIKey
}
```

### Stdio handling

1. `cmd := exec.CommandContext(ctx, BinaryPath, args...)`; `cmd.Env = env`.
2. `stdin, _ := cmd.StdinPipe()`. After `cmd.Start()`, write the assembled
   prompt string in one shot, then `stdin.Close()`. (Prompt comes from
   `joinTextParts(input)` — see "Input handling".)
3. `stdout, _ := cmd.StdoutPipe()`. Wrap in `bufio.Scanner`. Set buffer
   `Buffer(make([]byte, 0, 64KB), 4*1024*1024)` (file_change events can
   be large — TS readline has no analogous limit because Node strings are
   unbounded; 4MB is the Go-side cap).
4. `stderr, _ := cmd.StderrPipe()`. Drain into a `bytes.Buffer` capped at
   64KB (drop overflow with `"...[truncated]\n"` marker). Used in
   `NonZeroExitError.Stderr`.
5. Each scanner line → `parseEvent(line)` → typed `ThreadEvent` → channel.
6. First `ThreadStartedEvent` atomically updates `Thread.id` (mutex)
   **before** the event is yielded on the channel — so by the time the
   consumer sees it, `Thread.ID()` already returns the new id.
7. On `Scanner.Scan()` returning false (EOF): `cmd.Wait()`. Translation:
   - `*exec.ExitError` with non-zero code → terminal error =
     `*NonZeroExitError{Code, Signal, Stderr}`; also push a synthetic
     `ThreadErrorEvent{Message: "codex exited with code N: <stderr-tail>"}`
     onto the channel before closing it.
   - signal-killed by ctx: terminal error = `ctx.Err()`.
   - exit 0: terminal error = nil.
8. Channel is closed **after** the synthetic terminal event (if any) is
   pushed. `Wait()` returns the terminal error.

### Cancellation

`exec.CommandContext` already SIGKILLs on ctx done. We override that with a
custom `cmd.Cancel = func() error { return cmd.Process.Signal(SIGTERM) }`
and `cmd.WaitDelay = 2 * time.Second` (Go 1.20+). Result: SIGTERM first,
SIGKILL after 2s if codex hasn't exited.

This is an **intentional divergence** from TS (which only calls
`child.kill()` = SIGTERM with no escalation, leaving a hung codex to
deadlock the parent). Documented in the divergence list.

### Input handling (`joinTextParts`)

Mirrors TS `normalizeInput`:

```go
func joinTextParts(input Input) (prompt string, images []string) {
    switch v := input.(type) {
    case StringInput:
        return string(v), nil
    case PartsInput:
        var texts []string
        for _, p := range v {
            switch p.Type {
            case InputText:       texts = append(texts, p.Text)
            case InputLocalImage: images = append(images, p.Path)
            }
        }
        return strings.Join(texts, "\n\n"), images
    }
    return "", nil
}
```

### Thread.id capture

The first `ThreadStartedEvent` updates the parent `Thread`'s id field via
`atomic.Pointer[string]` (or sync.Mutex if simpler). After that, subsequent
`RunStreamed` calls on the same `Thread` see a non-empty id and switch to
the resume path automatically — meaning a `StartThread()` Thread becomes
"resumed" after its first turn, transparently. This matches TS Thread
state behavior at thread.ts:101-103.

## Error model

```go
type SpawnError       struct{ Err error }                              // exec.Command error pre-Wait
type NonZeroExitError struct{ Code int; Signal string; Stderr string } // codex exited != 0
type ParseEventError  struct{ Line string; Err error }                 // bad JSONL line
type TurnFailedError  struct{ Message string }                         // turn.failed event (Run only)
```

`ParseEventError` does NOT terminate the stream — the offending line is
wrapped into a synthetic `ThreadErrorEvent{Message: "parse: <line excerpt>: <err>"}`
emitted on the channel, and the scanner moves on. `Wait()` still returns
nil if the subprocess exits cleanly. Rationale: codex CLI is allowed to
introduce new event shapes without the SDK panicking; bad lines are loud
in the stream but recoverable.

`SpawnError` and `NonZeroExitError` always terminate the stream and are
returned from `Wait()`.

`TurnFailedError` is **only** returned from `Run()`. `RunStreamed` yields
the `TurnFailedEvent` on the channel and `Wait()` returns nil (because
codex still exited 0). This split mirrors TS exactly: `thread.ts:127-130`
shows `run()` translating turn.failed into a thrown Error while
`runStreamed()` does not.

## Config serialization (`serializeConfigOverrides`)

Direct port of `exec.ts:235-330`. Recursively flattens nested maps into
dotted-key form and emits each leaf as `key=tomlValue`.

**Algorithm:**

```
flatten(value, prefix, out):
  if value is not a map (leaf):
    if prefix is empty → error "config overrides must be a plain object"
    out.append(prefix + "=" + tomlValue(value, prefix))
    return
  if prefix is empty and map is empty → return (skip)
  if prefix is non-empty and map is empty:
    out.append(prefix + "={}")
    return
  for each (k, v) in map (insertion order — use Go map iteration order
                          but document that callers should not depend on
                          stable ordering — TS uses Object.entries which
                          is insertion-ordered):
    if k == "" → error "keys must be non-empty"
    if v == nil → continue (TS skips undefined; nil is the Go analog)
    childPrefix := prefix == "" ? k : prefix + "." + k
    flatten(v, childPrefix, out)
```

**`tomlValue(v, path)`:**

| Go type        | Output                                              |
|----------------|-----------------------------------------------------|
| string         | `strconv.Quote(v)` (JSON-style; matches TS `JSON.stringify`) |
| int / int64    | `strconv.FormatInt(v, 10)`                          |
| float64        | reject NaN/±Inf; otherwise `strconv.FormatFloat(v, 'g', -1, 64)` |
| bool           | `"true"` / `"false"`                                |
| []any          | `"[" + comma-join(tomlValue(elem)) + "]"`           |
| map[string]any | inline-table: `"{" + comma-join("k = " + tomlValue(v)) + "}"` |
| nil            | error `"config override at <path> cannot be null"`  |
| anything else  | error `"unsupported config override at <path>"`     |

Inline-table keys: bare key (`[A-Za-z0-9_-]+`) emitted as-is, otherwise
`strconv.Quote`. Mirror of TS `formatTomlKey`.

## OutputSchema lifecycle

Mirrors `outputSchemaFile.ts`:

```go
func (t *Thread) prepareOutputSchema(opts TurnOptions) (path string, cleanup func(), err error) {
    if opts.OutputSchema == nil { return "", func(){}, nil }
    // Reject non-objects (matches isJsonObject check)
    rv := reflect.ValueOf(opts.OutputSchema)
    if rv.Kind() != reflect.Map && rv.Kind() != reflect.Struct {
        return "", nil, errors.New("OutputSchema must be a JSON object (map or struct)")
    }
    dir, err := os.MkdirTemp("", "codex-output-schema-")
    if err != nil { return "", nil, err }
    cleanup = func() { _ = os.RemoveAll(dir) }
    path = filepath.Join(dir, "schema.json")
    data, err := json.Marshal(opts.OutputSchema)
    if err != nil { cleanup(); return "", nil, err }
    if err := os.WriteFile(path, data, 0o600); err != nil { cleanup(); return "", nil, err }
    return path, cleanup, nil
}
```

`cleanup` is **always** invoked when the turn ends (success, failure, ctx
cancel — wired via deferred call in `RunStreamed`).

## Concurrency contract

- `Codex` is safe to share across goroutines.
- `Thread` is **not** safe for concurrent `Run` / `RunStreamed` calls. Codex
  CLI is one-prompt-per-process; serializing Runs is the caller's job (in
  ccbroker, session_worker is the natural serialization point).
- The events channel from `RunStreamed` is single-consumer.

## Testing strategy

Three test layers:

1. **Pure unit (no subprocess)** — `events_test.go`, `items_test.go`:
   table-driven JSON → typed-event roundtrips. Forward-compat assertions
   (unknown type → UnknownEvent, no panic).

2. **Mocked CLI** — `exec_test.go`: replace `CodexPathOverride` with a Bash script
   in `t.TempDir()` that emits scripted JSONL on stdout, sleeps, exits with
   chosen code. Covers: clean turn, turn.failed event, non-zero exit,
   stderr capture, ctx cancel kills child, large lines, malformed lines.

3. **Live integration (gated)** — `integration_test.go` behind a
   `-tags=integration` build tag. Spawns the real `codex` binary against a
   small prompt; asserts thread.started + turn.completed appear. Skipped in
   normal `go test ./...` so the suite has no external dependencies.

CI: layers 1+2 run on every commit. Layer 3 runs in a manual / nightly
workflow that has the codex binary preinstalled.

## Intentional divergences from TS SDK

These are the only behaviors that differ from the TS SDK. Anything not
listed here MUST match TS exactly.

| # | Area | TS behavior | Go behavior | Why |
|---|---|---|---|---|
| 1 | Binary discovery | npm platform-package fallback (`@openai/codex-linux-x64` etc.) before PATH | PATH lookup; `CodexPathOverride` field | No npm equivalent in Go. Consumers wanting bundled binaries can vendor / Bazel / custom path. |
| 2 | ctx cancel | `child.kill()` (SIGTERM, no escalation) — can hang on uncooperative codex | SIGTERM → 2s grace → SIGKILL via `cmd.WaitDelay` | Defensive. TS pattern is a latent deadlock; Go fixes it. |
| 3 | Originator value | `"codex_sdk_ts"` | `"codex_sdk_go"` | Identifies the language wrapper. Cosmetic. |
| 4 | Concurrency on `Thread` | Single-threaded JS; no concurrent-Run guard documented | `Thread.RunStreamed` not safe for concurrent calls; explicitly documented | Idiomatic Go; ccbroker serializes via session_worker anyway. |
| 5 | Cancellation surface | `TurnOptions.signal: AbortSignal` per-turn | `ctx context.Context` parameter on `Run` / `RunStreamed` | Idiomatic Go; AbortSignal has no portable equivalent. Behavior is identical (cancellation kills the subprocess). |
| 6 | `StreamedTurn.Wait()` | Generator throws; consumer catches via `try/for await` | Explicit `Wait() error` method on `*StreamedTurn` | Go channels can't carry errors. The `Wait()` method is the natural place to surface SpawnError / NonZeroExitError / ctx.Err / ParseEventError. Consumer pattern: `for evt := range stream.Events() {}; if err := stream.Wait(); err != nil {}`. |
| 7 | Unknown event/item type | TS yields the parsed object as-is (consumer's `switch` misses it silently) | Go returns `*UnknownEvent` or `*UnknownItem` carrying the raw JSON | Go has typed sealed interfaces; a discriminator the SDK doesn't recognize must be representable as *something*. The Unknown* types preserve forward-compat without crashing or silently dropping. |

## Out-of-scope (deferred, but reserved API real estate)

- **`codex app-server` mode** — separate SDK if/when stateful harness model
  is adopted. See "Why `codex exec`, not `codex app-server`" above.

## Decisions worth flagging

- **Codex CLI minimum version**: validated against `codex-cli 0.125.0`
  (the version on the dev box). README pins a `>= 0.125.0` floor and the
  integration test asserts the version string at startup. Bumping the
  floor is non-breaking for SDK consumers.
- **Versioning**: tag `v0.1.0` after first ccbroker integration ships, and
  pin agentserver's `go.mod` to that tag (mirror how `claude-agent-sdk-go`
  is consumed).
- **Repo location**: new git repo at `/root/codex-agent-sdk-go`, parallel to
  `/root/claude-agent-sdk-go`. Module path
  `github.com/agentserver/codex-agent-sdk-go`. Same publishing model.

## Acceptance

**Functional parity:** Every test case in `/root/codex/sdk/typescript/tests/`
that doesn't depend on Node/JS-specific machinery has an equivalent in
this SDK's `*_test.go` and produces an equivalent assertion.

**Wire parity:** A spy script (test fixture) recording the codex
invocation's `argv` + stdin + env produces a byte-identical (modulo
divergence list) trace for the same inputs as the TS SDK does. We will
land a `tests/wire_parity_test.go` that scripts both SDKs against the
same scenarios and diffs the captured invocation.

**Quickstart sanity:**

```go
c := codex.New(codex.CodexOptions{APIKey: os.Getenv("OPENAI_API_KEY")})
t := c.StartThread(codex.ThreadOptions{
    SandboxMode:      codex.SandboxWorkspaceWrite,
    WorkingDirectory: "/tmp/work",
    SkipGitRepoCheck: true,
})
turn, err := t.Run(ctx, codex.StringInput("List files"), codex.TurnOptions{})
// turn.FinalResponse populated; t.ID() returns the codex-assigned thread id;
// a follow-up t.Run(...) implicitly resumes.
```

…and the same pattern with `RunStreamed` to consume events as they arrive.
The package compiles and passes `go vet` / `staticcheck` clean.
