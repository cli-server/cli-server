package oplog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClient_Submit_FireAndForgetPOST(t *testing.T) {
	var (
		mu      sync.Mutex
		bodies  [][]byte
		gotAuth string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-Internal-Secret")
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		mu.Lock()
		bodies = append(bodies, buf)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/internal/operations", "secret-x", 16)
	defer c.Close()

	op := Operation{
		ID: "op-1", WorkspaceID: "ws", Source: "sdk",
		EnvID: "alpha", Tool: "shell", IsError: false,
		StartedAt: time.Now().UTC(), CompletedAt: time.Now().UTC(),
		DurationMs: 1,
	}
	c.Submit(op)
	c.Submit(op)

	if !waitFor(2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(bodies) == 2
	}) {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		t.Fatalf("flushed %d, want 2", n)
	}
	if gotAuth != "secret-x" {
		t.Fatalf("X-Internal-Secret = %q, want secret-x", gotAuth)
	}
}

func TestClient_Submit_BoundedDropsOnFull(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	c := NewClient(srv.URL+"/", "s", 2)
	defer c.Close()

	op := Operation{
		ID: "op", WorkspaceID: "ws", Source: "sdk",
		EnvID: "a", Tool: "shell",
		StartedAt: time.Now().UTC(), CompletedAt: time.Now().UTC(),
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			c.Submit(op)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Submit blocked")
	}
	if c.Dropped() == 0 {
		t.Fatalf("expected drops, got %d", c.Dropped())
	}
}

func TestClient_Submit_DoesNotBlockOnServerError(t *testing.T) {
	hits := int64(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/", "s", 16)
	defer c.Close()

	op := Operation{
		ID: "x", WorkspaceID: "ws", Source: "sdk",
		EnvID: "a", Tool: "shell",
		StartedAt: time.Now().UTC(), CompletedAt: time.Now().UTC(),
	}
	c.Submit(op)
	if !waitFor(time.Second, func() bool { return atomic.LoadInt64(&hits) >= 1 }) {
		t.Fatal("server never received the post")
	}
	c.Submit(op)
	if !waitFor(time.Second, func() bool { return atomic.LoadInt64(&hits) >= 2 }) {
		t.Fatal("client stopped sending after server error")
	}
}

func TestOperation_MarshalJSON(t *testing.T) {
	o := Operation{
		ID: "x", WorkspaceID: "w", Source: "sdk",
		EnvID: "a", Tool: "shell", IsError: false,
		Arguments:   json.RawMessage(`{"a":1}`),
		StartedAt:   time.Unix(1700000000, 0).UTC(),
		CompletedAt: time.Unix(1700000001, 0).UTC(),
		DurationMs:  5,
	}
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"workspace_id":"w"`) || !strings.Contains(s, `"arguments":{"a":1}`) {
		t.Fatalf("body = %s", b)
	}
}

func TestListClient_ForwardsParamsAndReturnsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Secret") != "s" {
			t.Fatalf("auth")
		}
		q := r.URL.Query()
		if q.Get("workspace_id") != "ws" || q.Get("tool") != "shell" || q.Get("limit") != "10" {
			t.Fatalf("query: %v", q)
		}
		_, _ = w.Write([]byte(`{"operations":[{"id":"op_1"}]}`))
	}))
	defer srv.Close()

	lc := NewListClient(srv.URL+"/", "s")
	body, err := lc.List(t.Context(),
		map[string]string{"workspace_id": "ws", "tool": "shell", "limit": "10"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !strings.Contains(string(body), `"op_1"`) {
		t.Fatalf("body = %s", body)
	}
}

func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}
