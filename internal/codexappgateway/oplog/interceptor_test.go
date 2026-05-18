package oplog

import (
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type captureClient struct {
	mu  sync.Mutex
	ops []Operation
}

func (c *captureClient) Submit(op Operation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ops = append(c.ops, op)
}
func (c *captureClient) Dropped() uint64 { return 0 }

func (c *captureClient) snapshot() []Operation {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Operation, len(c.ops))
	copy(out, c.ops)
	return out
}

func TestInterceptor_ParesRequestAndResponse(t *testing.T) {
	cc := &captureClient{}
	i := NewInterceptor(cc, Config{Source: "sdk", WorkspaceID: "ws-1"})

	req := []byte(`{"jsonrpc":"2.0","id":7,"method":"mcpServer/tool/call","params":{
		"thread_id":"th-1","server":"env_mcp","tool":"shell",
		"arguments":{"environment_id":"alpha","command":"ls"},
		"_meta":{"agentserver_user_id":"u-1"}}}`)
	resp := []byte(`{"jsonrpc":"2.0","id":7,"result":{
		"content":[{"type":"text","text":"hi"}],"isError":false}}`)

	i.OnClientFrame(req)
	i.OnServerFrame(resp)

	ops := cc.snapshot()
	if len(ops) != 1 {
		t.Fatalf("ops = %d", len(ops))
	}
	op := ops[0]
	if op.WorkspaceID != "ws-1" || op.Source != "sdk" ||
		op.EnvID != "alpha" || op.Tool != "shell" || op.IsError {
		t.Fatalf("op = %+v", op)
	}
	if op.UserID == nil || *op.UserID != "u-1" {
		t.Fatalf("user_id = %v", op.UserID)
	}
	if op.ThreadID == nil || *op.ThreadID != "th-1" {
		t.Fatalf("thread_id = %v", op.ThreadID)
	}
	if op.ResultSummary == nil || *op.ResultSummary != "hi" {
		t.Fatalf("result_summary = %v", op.ResultSummary)
	}
}

func TestInterceptor_IgnoresIrrelevantFrames(t *testing.T) {
	cc := &captureClient{}
	i := NewInterceptor(cc, Config{Source: "sdk", WorkspaceID: "ws-1"})

	i.OnClientFrame([]byte(`{"method":"initialized"}`))
	i.OnClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"thread/start","params":{}}`))
	i.OnServerFrame([]byte(`{"jsonrpc":"2.0","id":1,"result":{"thread_id":"x"}}`))
	i.OnServerFrame([]byte(`not json`))

	if len(cc.snapshot()) != 0 {
		t.Fatalf("logged unrelated frames: %+v", cc.snapshot())
	}
}

func TestInterceptor_RecordsIsError(t *testing.T) {
	cc := &captureClient{}
	i := NewInterceptor(cc, Config{Source: "sdk", WorkspaceID: "ws"})
	i.OnClientFrame([]byte(`{"jsonrpc":"2.0","id":3,"method":"mcpServer/tool/call","params":{
		"thread_id":"t","server":"env_mcp","tool":"shell",
		"arguments":{"environment_id":"a","command":"bad"}}}`))
	i.OnServerFrame([]byte(`{"jsonrpc":"2.0","id":3,"result":{
		"content":[{"type":"text","text":"oops"}],"isError":true}}`))

	ops := cc.snapshot()
	if len(ops) != 1 || !ops[0].IsError {
		t.Fatalf("ops = %+v", ops)
	}
}

func TestInterceptor_TruncatesLargeArguments(t *testing.T) {
	cc := &captureClient{}
	i := NewInterceptor(cc, Config{Source: "sdk", WorkspaceID: "ws", ArgsMaxBytes: 50})

	big := make([]byte, 200)
	for x := range big {
		big[x] = 'a'
	}
	args, _ := json.Marshal(map[string]any{"data": string(big), "environment_id": "a"})
	frame := append([]byte(`{"jsonrpc":"2.0","id":11,"method":"mcpServer/tool/call","params":{
		"server":"env_mcp","tool":"write_file","arguments":`), args...)
	frame = append(frame, '}', '}')
	i.OnClientFrame(frame)
	i.OnServerFrame([]byte(`{"jsonrpc":"2.0","id":11,"result":{"content":[],"isError":false}}`))

	ops := cc.snapshot()
	if len(ops) != 1 {
		t.Fatalf("ops = %d", len(ops))
	}
	op := ops[0]
	if op.Arguments != nil {
		t.Fatal("arguments should be nil when truncated")
	}
	if op.ArgumentsMeta == nil {
		t.Fatal("arguments_meta missing")
	}
	var m map[string]any
	_ = json.Unmarshal(op.ArgumentsMeta, &m)
	if m["truncated"] != true {
		t.Fatalf("arguments_meta = %v", m)
	}
}

func TestInterceptor_TruncatesLargeTextResult(t *testing.T) {
	cc := &captureClient{}
	i := NewInterceptor(cc, Config{Source: "sdk", WorkspaceID: "ws", ResultMaxBytes: 10})

	long := make([]byte, 100)
	for x := range long {
		long[x] = 'z'
	}
	i.OnClientFrame([]byte(`{"jsonrpc":"2.0","id":4,"method":"mcpServer/tool/call","params":{
		"server":"env_mcp","tool":"read_file","arguments":{"environment_id":"a","path":"/x"}}}`))
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 4,
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(long)}},
			"isError": false,
		},
	})
	i.OnServerFrame(resp)

	op := cc.snapshot()[0]
	if op.ResultSummary == nil || len(*op.ResultSummary) > 10 {
		t.Fatalf("result_summary = %v", op.ResultSummary)
	}
	if op.ResultMeta == nil {
		t.Fatal("result_meta missing")
	}
}

func TestInterceptor_ConcurrentPairs(t *testing.T) {
	cc := &captureClient{}
	i := NewInterceptor(cc, Config{Source: "sdk", WorkspaceID: "ws"})

	var wg sync.WaitGroup
	var sent int64
	for n := 0; n < 50; n++ {
		wg.Add(1)
		id := n + 1
		go func() {
			defer wg.Done()
			req := []byte(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"mcpServer/tool/call","params":{
				"server":"env_mcp","tool":"shell","arguments":{"environment_id":"a","command":"x"}}}`)
			resp := []byte(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"result":{"content":[],"isError":false}}`)
			i.OnClientFrame(req)
			i.OnServerFrame(resp)
			atomic.AddInt64(&sent, 1)
		}()
	}
	wg.Wait()
	if !waitFor(2*time.Second, func() bool {
		return len(cc.snapshot()) == 50
	}) {
		t.Fatalf("ops = %d, want 50", len(cc.snapshot()))
	}
}
