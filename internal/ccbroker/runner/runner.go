package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"

	"github.com/agentserver/agentserver/internal/ccbroker/workspace"
)

// sdkSession is the V1/V2 adapter seam — see spec §1.5.
// Only the V1 implementation exists today; a v2Session can be slotted in
// later when claude-agent-sdk-go ships V2 bindings, with no changes to
// callers of Run.
type sdkSession interface {
	Send(ctx context.Context, userMessage string) error
	Messages() <-chan agentsdk.SDKMessage
	Close() error
}

// Run starts a Claude Agent SDK session for a single turn. The returned
// channel emits SDKMessages until the session terminates (result message,
// SDK error, or ctx cancel). The output channel is closed automatically
// when the SDK message stream ends; the underlying CLI subprocess is
// closed in the same defer so callers don't need to call sess.Close().
func Run(
	ctx context.Context,
	ws *workspace.Workspace,
	sessionID, userMessage string,
	cfg Config,
	mcp *agentsdk.McpSdkServer,
) (<-chan agentsdk.SDKMessage, error) {
	spec := BuildSpec(ws, sessionID, cfg, sessionJSONLExists(ws, sessionID))
	spec.McpServer = mcp

	sess, err := newV1Session(ctx, spec.ToOptions())
	if err != nil {
		return nil, fmt.Errorf("connect SDK session: %w", err)
	}
	if err := sess.Send(ctx, userMessage); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("send user message: %w", err)
	}

	out := make(chan agentsdk.SDKMessage, 32)
	go func() {
		defer close(out)
		defer sess.Close()
		for msg := range sess.Messages() {
			select {
			case <-ctx.Done():
				return
			case out <- msg:
			}
		}
	}()
	return out, nil
}

// sessionJSONLExists checks whether a Claude CLI session jsonl for the
// given sessionID is already on disk under ws.ClaudeDir. The CLI stores
// session files at <CLAUDE_CONFIG_DIR>/projects/<encoded-cwd>/<UUID>.jsonl,
// where <encoded-cwd> is derived from the worker's Cwd via a CLI-internal
// transformation we do not replicate here. A glob over all encoded-cwd
// subdirectories matches whichever one the CLI would compute, since each
// turn's Cwd is deterministic for a given sessionID.
//
// Returns true → caller must use --resume; false → caller must use
// --session-id (the two flags are mutually exclusive in the CLI).
func sessionJSONLExists(ws *workspace.Workspace, sessionID string) bool {
	bare := strings.TrimPrefix(sessionID, "cse_")
	if bare == "" {
		return false
	}
	matches, _ := filepath.Glob(filepath.Join(ws.ClaudeDir, "projects", "*", bare+".jsonl"))
	return len(matches) > 0
}

// v1Session is the agentsdk.Client-based adapter for the stable V1 SDK API.
type v1Session struct {
	client *agentsdk.Client
	msgCh  <-chan agentsdk.SDKMessage
}

func newV1Session(ctx context.Context, opts []agentsdk.QueryOption) (sdkSession, error) {
	client := agentsdk.NewClient(opts...)
	if err := client.Connect(ctx); err != nil {
		return nil, err
	}
	return &v1Session{client: client, msgCh: client.Messages()}, nil
}

func (s *v1Session) Send(ctx context.Context, msg string) error {
	return s.client.Send(ctx, msg)
}

func (s *v1Session) Messages() <-chan agentsdk.SDKMessage { return s.msgCh }
func (s *v1Session) Close() error                          { return s.client.Close() }
