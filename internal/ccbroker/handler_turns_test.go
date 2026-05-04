package ccbroker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentsdk "github.com/agentserver/claude-agent-sdk-go"

	"github.com/agentserver/agentserver/internal/ccbroker/runner"
	"github.com/agentserver/agentserver/internal/ccbroker/tools"
	"github.com/agentserver/agentserver/internal/ccbroker/workspace"
)

// TestHandleProcessTurn_OrchestratesPipeline asserts the high-level flow:
// validation → session ensure → user-event insert → SDK pump → SSE done.
//
// The runner is faked to emit a scripted SDK message sequence; workspace
// Setup/Teardown are stubbed so no S3 calls happen and no real
// temp dir is created.
//
// NOTE: This test is currently t.Skip()ped because Server requires a real
// *Store backed by Postgres; no in-memory variant exists yet. The seam
// injection code is in place, scripted fake runner emits a 2-message
// stream, and the assertions are written out so the test can be activated
// when an in-memory store helper lands.
func TestHandleProcessTurn_OrchestratesPipeline(t *testing.T) {
	// Save and restore the package seams.
	origSetup := workspaceSetup
	origTeardown := workspaceTeardown
	origRun := runnerRun
	defer func() {
		workspaceSetup = origSetup
		workspaceTeardown = origTeardown
		runnerRun = origRun
	}()

	teardownCalled := atomic.Int32{}
	workspaceSetup = func(_ context.Context, wid, sid string, _ *workspace.S3Store) (*workspace.Workspace, error) {
		return &workspace.Workspace{
			WorkspaceID: wid,
			SessionID:   sid,
			TempDir:     "/tmp/fake",
			ClaudeDir:   "/tmp/fake/claude-config",
			ProjectDir:  "/tmp/fake/project",
			MemoryDir:   "/tmp/fake/claude-config/projects/ws_" + wid + "/memory",
		}, nil
	}
	workspaceTeardown = func(_ context.Context, _ *workspace.Workspace, _ *workspace.S3Store) error {
		teardownCalled.Add(1)
		return nil
	}

	// Scripted SDK output: one assistant message, one result.
	runnerRun = func(_ context.Context, _ *workspace.Workspace, _, _ string, _ runner.Config, _ *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
		ch := make(chan agentsdk.SDKMessage, 2)
		ch <- agentsdk.SDKMessage{
			Type: "assistant",
			Raw:  json.RawMessage(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`),
		}
		ch <- agentsdk.SDKMessage{
			Type:    "result",
			Subtype: "success",
			Raw:     json.RawMessage(`{"type":"result","subtype":"success","is_error":false}`),
		}
		close(ch)
		return ch, nil
	}

	srv := buildTestServer(t)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"session_id":   "cse_test1",
		"workspace_id": "ws_test",
		"user_message": "hello",
	})
	resp, err := http.Post(srv.URL+"/api/turns", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/turns: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), `"event_type":"done"`) {
		t.Fatalf("response missing done sentinel: %q", out)
	}
	if got := teardownCalled.Load(); got != 1 {
		t.Errorf("Teardown called %d times, want 1", got)
	}
}

// ----- fakeStore: in-memory implementation of storer -----

type fakeStore struct {
	mu       sync.Mutex
	sessions map[string]*Session
	events   []EventInput

	// Queue state
	turns      map[string]*AgentTurn // by turn_id
	turnOrder  map[string][]string   // session_id → ordered turn_ids
	resetCount int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		sessions:  make(map[string]*Session),
		turns:     make(map[string]*AgentTurn),
		turnOrder: make(map[string][]string),
	}
}

func (f *fakeStore) GetSession(_ context.Context, id string) (*Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[id], nil
}

func (f *fakeStore) CreateSession(_ context.Context, id, workspaceID, title, source string, externalID *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[id] = &Session{ID: id, WorkspaceID: workspaceID, Title: title, Source: source, ExternalID: externalID}
	return nil
}

func (f *fakeStore) GetSessionEpoch(_ context.Context, sessionID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[sessionID]; ok {
		return s.Epoch, nil
	}
	return 0, nil
}

func (f *fakeStore) InsertEvents(_ context.Context, _ string, _ int, events []EventInput) ([]InsertedEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, events...)
	inserted := make([]InsertedEvent, len(events))
	for i, e := range events {
		inserted[i] = InsertedEvent{SeqNum: int64(i + 1), EventID: e.EventID}
	}
	return inserted, nil
}

func (f *fakeStore) InsertEventsWithTurn(_ context.Context, _ string, _ int, _ string, events []EventInput) ([]InsertedEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]InsertedEvent, 0, len(events))
	for _, e := range events {
		out = append(out, InsertedEvent{SeqNum: int64(len(out) + 1), EventID: e.EventID})
	}
	return out, nil
}

func (f *fakeStore) EnqueueTurn(_ context.Context, t AgentTurn) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := t
	cp.State = "queued"
	cp.EnqueuedAt = time.Now()
	f.turns[t.ID] = &cp
	f.turnOrder[t.SessionID] = append(f.turnOrder[t.SessionID], t.ID)
	return nil
}

func (f *fakeStore) PickNextPending(_ context.Context, sessionID string) (*AgentTurn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tid := range f.turnOrder[sessionID] {
		t := f.turns[tid]
		if t.State == "queued" || t.State == "running" {
			cp := *t
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) MarkTurnRunning(_ context.Context, turnID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.turns[turnID]; ok && t.State == "queued" {
		t.State = "running"
	}
	return nil
}

func (f *fakeStore) MarkTurnDone(_ context.Context, turnID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.turns[turnID]; ok {
		t.State = "done"
	}
	return nil
}

func (f *fakeStore) MarkTurnCancelled(_ context.Context, turnID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.turns[turnID]; ok {
		t.State = "cancelled"
	}
	return nil
}

func (f *fakeStore) MarkTurnFailed(_ context.Context, turnID, errMsg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.turns[turnID]; ok {
		t.State = "failed"
		t.ErrorMsg = sql.NullString{String: errMsg, Valid: true}
	}
	return nil
}

func (f *fakeStore) GetTurn(_ context.Context, turnID string) (*AgentTurn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.turns[turnID]
	if !ok {
		return nil, nil
	}
	cp := *t
	return &cp, nil
}

func (f *fakeStore) ListSessionsWithPending(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]struct{}{}
	var sids []string
	for _, t := range f.turns {
		if t.State == "queued" || t.State == "running" {
			if _, ok := seen[t.SessionID]; !ok {
				seen[t.SessionID] = struct{}{}
				sids = append(sids, t.SessionID)
			}
		}
	}
	return sids, nil
}

func (f *fakeStore) ListSessionTurns(_ context.Context, sessionID string, limit int) ([]AgentTurn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []AgentTurn{}
	ids := f.turnOrder[sessionID]
	for i := len(ids) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, *f.turns[ids[i]])
	}
	return out, nil
}

func (f *fakeStore) ResetRunningToQueued(_ context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.turns {
		if t.State == "running" {
			t.State = "queued"
			n++
		}
	}
	f.resetCount = n
	return n, nil
}

func (f *fakeStore) CountPending(_ context.Context, sessionID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, tid := range f.turnOrder[sessionID] {
		if t := f.turns[tid]; t.State == "queued" || t.State == "running" {
			n++
		}
	}
	return n, nil
}

// ----- test server helpers -----

// newFakeServer builds a minimal Server backed by an in-memory fakeStore.
// Callers that need direct access to s.activeTurns / s.compactQueue use this;
// then wrap with httptest.NewServer(s.Routes()) themselves.
func newFakeServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		config: Config{},
		store:  newFakeStore(),
		wstoken: func(_ context.Context, _ string) (string, error) {
			return "test-workspace-token", nil
		},
		sse:          NewSSEBroker(),
		turnLock:     NewTurnLock(),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		activeTurns:  newActiveTurnRegistry(),
		compactQueue: newCompactQueue(),
	}
	s.gate = tools.NewGate(func(sid string, e tools.Event) {
		// noop for tests
	})
	return s
}

// newFakeHTTPServer builds a minimal Server backed by an in-memory fakeStore
// and returns its httptest.Server.
func newFakeHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := newFakeServer(t)
	return httptest.NewServer(s.Routes())
}

// postJSON sends a JSON body to path on srv and returns the ResponseRecorder.
// The srv argument must be an *httptest.Server whose handler is wired
// (i.e., use newFakeHTTPServer, not httptest.NewRecorder directly).
func postJSON(t *testing.T, srv *httptest.Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	rr := httptest.NewRecorder()
	rr.Code = resp.StatusCode
	b, _ := io.ReadAll(resp.Body)
	rr.Body = bytes.NewBuffer(b)
	for k, vs := range resp.Header {
		for _, v := range vs {
			rr.Header().Set(k, v)
		}
	}
	return rr
}

// stubWorkspaceSeams stubs the workspace Setup/Teardown seams with noop fakes
// and restores originals via t.Cleanup.
func stubWorkspaceSeams(t *testing.T) {
	t.Helper()
	origSetup := workspaceSetup
	origTeardown := workspaceTeardown
	workspaceSetup = func(_ context.Context, wid, sid string, _ *workspace.S3Store) (*workspace.Workspace, error) {
		return &workspace.Workspace{
			WorkspaceID: wid,
			SessionID:   sid,
			TempDir:     "/tmp/fake-t7",
			ClaudeDir:   "/tmp/fake-t7/claude-config",
			ProjectDir:  "/tmp/fake-t7/project",
			MemoryDir:   "/tmp/fake-t7/claude-config/projects/ws_" + wid + "/memory",
		}, nil
	}
	workspaceTeardown = func(_ context.Context, _ *workspace.Workspace, _ *workspace.S3Store) error {
		return nil
	}
	t.Cleanup(func() {
		workspaceSetup = origSetup
		workspaceTeardown = origTeardown
	})
}

// buildTestServer wires a Server backed by an in-memory store + SSE broker
// suitable for handler tests. Returns the httptest.Server.
//
// Currently t.Skip()ped: Store requires a Postgres *sql.DB. When an
// in-memory store helper is added (e.g. a fakeStore implementing the same
// query methods), replace the t.Skip() with a wired *Server and activate
// the assertions in TestHandleProcessTurn_OrchestratesPipeline.
func buildTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	t.Skip("buildTestServer not implemented: Store requires a real Postgres connection; " +
		"orchestration test is held here until an in-memory store helper is available")
	return nil
}

// ----- Task 7 tests -----

func TestProcessTurn_AcceptsMetadata(t *testing.T) {
	// We capture the runner.Config that handleProcessTurn computes via the
	// existing runnerRun test seam (defined at the top of handler_turns.go).
	captured := make(chan runner.Config, 1)
	origRunner := runnerRun
	runnerRun = func(ctx context.Context, ws *workspace.Workspace, sid, msg string,
		cfg runner.Config, mcp *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
		captured <- cfg
		ch := make(chan agentsdk.SDKMessage)
		close(ch)
		return ch, nil
	}
	defer func() { runnerRun = origRunner }()

	stubWorkspaceSeams(t)

	srv := newFakeHTTPServer(t)
	defer srv.Close()

	body := `{
		"session_id":"cse_md","workspace_id":"ws","user_message":"hi",
		"metadata":{"channel_type":"tui","creator_user_id":"u_alice",
		            "permission_mode":"ask","model":"claude-opus-4-7",
		            "preferred_executor_id":"exe_a","turn_kind":"user"}}`
	rr := postJSON(t, srv, "/api/turns", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rr.Code, rr.Body)
	}
	select {
	case got := <-captured:
		if got.ChannelType != "tui" || got.CreatorUserID != "u_alice" ||
			got.PermissionMode != "ask" || got.PreferredExecutorID != "exe_a" {
			t.Errorf("metadata not threaded: %+v", got)
		}
		if got.SessionID != "cse_md" {
			t.Errorf("SessionID not threaded: %q", got.SessionID)
		}
		if got.TurnID == "" {
			t.Errorf("TurnID should be generated")
		}
		if got.Model != "claude-opus-4-7" {
			t.Errorf("Model not threaded: %q", got.Model)
		}
	case <-time.After(time.Second):
		t.Fatal("runnerRun never invoked")
	}
}

func TestProcessTurn_DefaultsForLegacyIM(t *testing.T) {
	// Legacy IM turn body has no metadata field; defaults should apply.
	captured := make(chan runner.Config, 1)
	origRunner := runnerRun
	runnerRun = func(ctx context.Context, ws *workspace.Workspace, sid, msg string,
		cfg runner.Config, mcp *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
		captured <- cfg
		ch := make(chan agentsdk.SDKMessage)
		close(ch)
		return ch, nil
	}
	defer func() { runnerRun = origRunner }()

	stubWorkspaceSeams(t)

	srv := newFakeHTTPServer(t)
	defer srv.Close()

	body := `{"session_id":"cse_im","workspace_id":"ws","user_message":"hi","im_channel_id":"ch","im_user_id":"u"}`
	rr := postJSON(t, srv, "/api/turns", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rr.Code, rr.Body)
	}
	got := <-captured
	if got.ChannelType != "im" {
		t.Errorf("default ChannelType=%q want im", got.ChannelType)
	}
	if got.PermissionMode != "bypass" {
		t.Errorf("default PermissionMode=%q want bypass", got.PermissionMode)
	}
}

// ----- Task 13 tests -----

func TestProcessTurn_CallsTurnFinished(t *testing.T) {
	var (
		mu     sync.Mutex
		called map[string]string
	)
	fakeAS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/turn-finished") {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		called = b
		w.WriteHeader(200)
	}))
	defer fakeAS.Close()

	stubWorkspaceSeams(t)

	s := newFakeServer(t)
	s.config.AgentserverInternalURL = fakeAS.URL
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	origRunner := runnerRun
	runnerRun = func(ctx context.Context, ws *workspace.Workspace, sid, msg string,
		cfg runner.Config, mcp *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
		ch := make(chan agentsdk.SDKMessage)
		close(ch)
		return ch, nil
	}
	defer func() { runnerRun = origRunner }()

	body := `{"session_id":"cse_x","workspace_id":"ws","user_message":"hi"}`
	rr := postJSON(t, srv, "/api/turns", body)
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		if called != nil {
			mu.Unlock()
			break
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if called == nil {
		t.Fatal("turn-finished callback never fired")
	}
	if called["session_id"] != "cse_x" {
		t.Errorf("session_id=%q want cse_x", called["session_id"])
	}
	if called["turn_id"] == "" {
		t.Errorf("turn_id missing")
	}
}

func TestProcessTurn_RegistersAndClearsActiveTurn(t *testing.T) {
	stubWorkspaceSeams(t)

	s := newFakeServer(t)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	origRunner := runnerRun
	runnerRun = func(ctx context.Context, ws *workspace.Workspace, sid, msg string,
		cfg runner.Config, mcp *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
		// While runner is "running", verify activeTurns has this session.
		if tid, ok := s.activeTurns.Get(sid); !ok || tid == "" {
			t.Errorf("active turn not registered during runner.Run; got %q ok=%v", tid, ok)
		}
		ch := make(chan agentsdk.SDKMessage)
		close(ch)
		return ch, nil
	}
	defer func() { runnerRun = origRunner }()

	body := `{"session_id":"cse_at","workspace_id":"ws","user_message":"hi"}`
	rr := postJSON(t, srv, "/api/turns", body)
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	// After turn returns, active turn should be cleared.
	if tid, ok := s.activeTurns.Get("cse_at"); ok && tid != "" {
		t.Errorf("active turn not cleared after turn ended; tid=%q", tid)
	}
}

func TestProcessTurn_CompactQueueTakenAndOverridesTurnKind(t *testing.T) {
	stubWorkspaceSeams(t)

	s := newFakeServer(t)
	s.compactQueue.Set("cse_c") // queue the session for compaction
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	var capturedKind string
	origRunner := runnerRun
	runnerRun = func(ctx context.Context, ws *workspace.Workspace, sid, msg string,
		cfg runner.Config, mcp *agentsdk.McpSdkServer) (<-chan agentsdk.SDKMessage, error) {
		capturedKind = cfg.TurnKind
		ch := make(chan agentsdk.SDKMessage)
		close(ch)
		return ch, nil
	}
	defer func() { runnerRun = origRunner }()

	body := `{"session_id":"cse_c","workspace_id":"ws","user_message":"continue"}`
	rr := postJSON(t, srv, "/api/turns", body)
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	if capturedKind != "compaction" {
		t.Errorf("TurnKind=%q want compaction", capturedKind)
	}
	if s.compactQueue.IsSet("cse_c") {
		t.Errorf("compactQueue should have been Take()-d for cse_c")
	}
}
