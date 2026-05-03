// internal/ccbroker/tools/permission.go
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrCrossUserDenied  = errors.New("cross_user_denied: cross-user executor invocation not allowed")
	ErrPermissionDenied = errors.New("permission_denied")
)

type CheckRequest struct {
	SessionID                 string
	TurnID                    string
	Tool                      string
	ExecutorID                string
	Args                      json.RawMessage
	PermissionMode            string
	SessionCreatorUserID      string
	ExecutorOwnerUserID       string
	ExecutorSharedToWorkspace bool
	Timeout                   time.Duration
}

type Decision struct {
	Verdict string `json:"verdict"`           // "allow" | "deny"
	Scope   string `json:"scope"`             // "once" | "always"
	By      string `json:"by,omitempty"`
}

type Event struct {
	Type         string          `json:"event_type"`              // "permission_request" | "permission_resolved"
	SessionID    string          `json:"session_id,omitempty"`
	TurnID       string          `json:"turn_id,omitempty"`
	PermissionID string          `json:"permission_id,omitempty"`
	Tool         string          `json:"tool,omitempty"`
	ExecutorID   string          `json:"executor_id,omitempty"`
	Args         json.RawMessage `json:"args,omitempty"`
	Decision     *Decision       `json:"decision,omitempty"`
	Source       string          `json:"source,omitempty"`       // "live" | "sticky"
	EmittedAt    time.Time       `json:"emitted_at,omitempty"`
}

type Notifier func(sessionID string, evt Event)

type pendingReq struct {
	pid       string
	sessionID string
	turnID    string
	tool      string
	execID    string
	args      json.RawMessage
	deadline  time.Time
	decided   chan Decision
	ruleKey   string
}

type Gate struct {
	notify  Notifier
	mu      sync.Mutex
	pending map[string]*pendingReq         // pid → req
	sticky  map[string]map[string]Decision // sessionID → ruleKey → decision
}

func NewGate(notify Notifier) *Gate {
	return &Gate{
		notify:  notify,
		pending: map[string]*pendingReq{},
		sticky:  map[string]map[string]Decision{},
	}
}

func (g *Gate) Check(ctx context.Context, req CheckRequest) error {
	// 1. cross-user.
	//
	// SessionCreatorUserID == "" means a legacy IM session (no authenticated
	// workspace user attached). IM flow is exempt — those sessions can invoke
	// any executor in their workspace.
	//
	// ExecutorOwnerUserID is NOT guarded by != "": under the Restrictive
	// policy (spec §4.11, §6.4) the store layer COALESCEs NULL → 'unknown',
	// so an empty string here means a caller-side bug, and should still
	// trigger denial against any real SessionCreatorUserID. Defense in depth.
	if req.SessionCreatorUserID != "" &&
		req.ExecutorOwnerUserID != req.SessionCreatorUserID && !req.ExecutorSharedToWorkspace {
		return ErrCrossUserDenied
	}
	// 2. bypass
	if req.PermissionMode == "bypass" {
		return nil
	}
	// 3. sticky
	ruleKey := makeRuleKey(req.Tool, req.ExecutorID, req.Args)
	if d, ok := g.lookupSticky(req.SessionID, ruleKey); ok {
		g.emit(req, Event{Type: "permission_resolved", Decision: &d, Source: "sticky"})
		if d.Verdict == "allow" {
			return nil
		}
		return ErrPermissionDenied
	}
	// 4. ask: emit + block
	pid := "perm_" + uuid.NewString()
	pr := &pendingReq{
		pid: pid, sessionID: req.SessionID, turnID: req.TurnID,
		tool: req.Tool, execID: req.ExecutorID, args: req.Args,
		deadline: time.Now().Add(req.Timeout),
		decided:  make(chan Decision, 1),
		ruleKey:  ruleKey,
	}
	g.mu.Lock()
	g.pending[pid] = pr
	g.mu.Unlock()

	g.emit(req, Event{
		Type: "permission_request", PermissionID: pid,
	})

	var d Decision
	timer := time.NewTimer(time.Until(pr.deadline))
	defer timer.Stop()
	select {
	case d = <-pr.decided:
	case <-timer.C:
		d = Decision{Verdict: "deny", Scope: "timeout"}
	case <-ctx.Done():
		d = Decision{Verdict: "deny", Scope: "cancelled"}
	}

	g.mu.Lock()
	delete(g.pending, pid)
	g.mu.Unlock()

	if d.Verdict == "allow" && d.Scope == "always" {
		g.recordSticky(req.SessionID, ruleKey, d)
	}
	g.emit(req, Event{Type: "permission_resolved", PermissionID: pid, Decision: &d, Source: "live"})

	if d.Verdict == "allow" {
		return nil
	}
	return ErrPermissionDenied
}

func (g *Gate) Resolve(pid string, d Decision) error {
	g.mu.Lock()
	pr, ok := g.pending[pid]
	g.mu.Unlock()
	if !ok {
		return errors.New("already_resolved_or_unknown")
	}
	select {
	case pr.decided <- d:
		return nil
	default:
		return errors.New("already_resolved")
	}
}

func (g *Gate) CancelTurn(turnID string) {
	g.mu.Lock()
	var prs []*pendingReq
	for _, pr := range g.pending {
		if pr.turnID == turnID {
			prs = append(prs, pr)
		}
	}
	g.mu.Unlock()
	for _, pr := range prs {
		select {
		case pr.decided <- Decision{Verdict: "deny", Scope: "cancelled"}:
		default:
		}
	}
}

func (g *Gate) lookupSticky(sessionID, key string) (Decision, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	m, ok := g.sticky[sessionID]
	if !ok {
		return Decision{}, false
	}
	d, ok := m[key]
	return d, ok
}

func (g *Gate) recordSticky(sessionID, key string, d Decision) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sticky[sessionID] == nil {
		g.sticky[sessionID] = map[string]Decision{}
	}
	g.sticky[sessionID][key] = d
}

func (g *Gate) emit(req CheckRequest, e Event) {
	e.SessionID = req.SessionID
	e.TurnID = req.TurnID
	if e.Tool == "" {
		e.Tool = req.Tool
	}
	if e.ExecutorID == "" {
		e.ExecutorID = req.ExecutorID
	}
	if e.Args == nil {
		e.Args = req.Args
	}
	if e.EmittedAt.IsZero() {
		e.EmittedAt = time.Now()
	}
	if g.notify != nil {
		g.notify(req.SessionID, e)
	}
}

func makeRuleKey(tool, executorID string, args json.RawMessage) string {
	switch tool {
	case "remote_bash":
		cmd := extractStringField(args, "command")
		toks := strings.Fields(cmd)
		n := len(toks)
		if n > 2 {
			n = 2
		}
		head := strings.Join(toks[:n], " ")
		return fmt.Sprintf("%s|%s|cmd:%s", tool, executorID, head)
	case "remote_read", "remote_write", "remote_edit",
		"remote_glob", "remote_ls", "remote_grep":
		path := extractStringField(args, "file_path")
		if path == "" {
			path = extractStringField(args, "path")
		}
		return fmt.Sprintf("%s|%s|dir:%s", tool, executorID, filepath.Dir(path))
	default:
		return fmt.Sprintf("%s|%s", tool, executorID)
	}
}

func extractStringField(args json.RawMessage, field string) string {
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return ""
	}
	v, _ := m[field].(string)
	return v
}
