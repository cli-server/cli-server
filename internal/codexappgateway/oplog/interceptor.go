package oplog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	defaultArgsMaxBytes   = 64 * 1024
	defaultResultMaxBytes = 4 * 1024
)

// Submitter is the subset of Client that the Interceptor needs. Lets
// tests pass a capture stub.
type Submitter interface {
	Submit(Operation)
}

// Config tunes the Interceptor at construction.
type Config struct {
	Source         string // "sdk" | "tui"
	WorkspaceID    string // pinned from the ws Identity
	ArgsMaxBytes   int    // 0 -> 64 KiB
	ResultMaxBytes int    // 0 -> 4 KiB
}

// Interceptor parses JSON-RPC frames as they cross the ws bridge. On a
// matched request+response for mcpServer/tool/call, it emits one
// Operation to the Submitter.
type Interceptor struct {
	sub Submitter
	cfg Config

	mu      sync.Mutex
	pending map[any]pendingReq
}

type pendingReq struct {
	startedAt time.Time
	env       string
	tool      string
	threadID  string
	userID    string
	args      json.RawMessage
	argsSize  int
}

func NewInterceptor(s Submitter, cfg Config) *Interceptor {
	if cfg.ArgsMaxBytes <= 0 {
		cfg.ArgsMaxBytes = defaultArgsMaxBytes
	}
	if cfg.ResultMaxBytes <= 0 {
		cfg.ResultMaxBytes = defaultResultMaxBytes
	}
	return &Interceptor{sub: s, cfg: cfg, pending: map[any]pendingReq{}}
}

// OnClientFrame is called on every client->server frame BEFORE it's forwarded.
// Never blocks. Errors are silent — parsing failures are not Interceptor's
// problem; the underlying pump still forwards the bytes.
func (i *Interceptor) OnClientFrame(frame []byte) {
	var msg struct {
		ID     any             `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frame, &msg); err != nil {
		return
	}
	if msg.ID == nil || msg.Method != "mcpServer/tool/call" {
		return
	}
	var p struct {
		ThreadID  string          `json:"thread_id"`
		Server    string          `json:"server"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			UserID string `json:"agentserver_user_id"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		return
	}
	var envID string
	if len(p.Arguments) > 0 {
		var args struct {
			EnvironmentID string `json:"environment_id"`
		}
		_ = json.Unmarshal(p.Arguments, &args)
		envID = args.EnvironmentID
	}

	i.mu.Lock()
	i.pending[msg.ID] = pendingReq{
		startedAt: time.Now().UTC(),
		env:       envID,
		tool:      p.Tool,
		threadID:  p.ThreadID,
		userID:    p.Meta.UserID,
		args:      p.Arguments,
		argsSize:  len(p.Arguments),
	}
	i.mu.Unlock()
}

// OnServerFrame is called on every server->client frame. If it pairs with
// a pending request, emits an Operation.
func (i *Interceptor) OnServerFrame(frame []byte) {
	var msg struct {
		ID     any             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(frame, &msg); err != nil {
		return
	}
	if msg.ID == nil {
		return
	}
	i.mu.Lock()
	pr, ok := i.pending[msg.ID]
	if ok {
		delete(i.pending, msg.ID)
	}
	i.mu.Unlock()
	if !ok {
		return
	}

	op := Operation{
		ID:          uuid.NewString(),
		WorkspaceID: i.cfg.WorkspaceID,
		Source:      i.cfg.Source,
		EnvID:       pr.env,
		Tool:        pr.tool,
		StartedAt:   pr.startedAt,
		CompletedAt: time.Now().UTC(),
	}
	if pr.threadID != "" {
		t := pr.threadID
		op.ThreadID = &t
	}
	if pr.userID != "" {
		u := pr.userID
		op.UserID = &u
	}
	op.DurationMs = int32(op.CompletedAt.Sub(op.StartedAt) / time.Millisecond)

	// Arguments — truncate if oversized
	if pr.argsSize > i.cfg.ArgsMaxBytes {
		sum := sha256.Sum256(pr.args)
		op.ArgumentsMeta = mustJSON(map[string]any{
			"truncated":  true,
			"size_bytes": pr.argsSize,
			"sha256":     hex.EncodeToString(sum[:]),
		})
	} else {
		op.Arguments = pr.args
	}

	// Result — pull text content as result_summary; record error flag
	if len(msg.Error) > 0 {
		op.IsError = true
		var er struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(msg.Error, &er)
		s := er.Message
		op.ResultSummary = &s
	} else if len(msg.Result) > 0 {
		var r struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		}
		_ = json.Unmarshal(msg.Result, &r)
		op.IsError = r.IsError
		var b []byte
		for _, c := range r.Content {
			if c.Type == "text" {
				b = append(b, c.Text...)
			}
		}
		total := len(b)
		if total > i.cfg.ResultMaxBytes {
			truncated := string(b[:i.cfg.ResultMaxBytes])
			op.ResultSummary = &truncated
			op.ResultMeta = mustJSON(map[string]any{
				"truncated":   true,
				"total_bytes": total,
			})
		} else if total > 0 {
			s := string(b)
			op.ResultSummary = &s
		}
	}

	i.sub.Submit(op)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
