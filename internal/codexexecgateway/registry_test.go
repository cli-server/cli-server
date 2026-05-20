package codexexecgateway

import (
	"sync"
	"testing"
)

func newFakeInbound(exeID string) *inboundConn {
	return newInboundConn(exeID, nil, nil, 0) // ws=nil is fine for identity-only tests
}

func TestConnRegistry_RegisterAndLookup(t *testing.T) {
	r := NewConnRegistry()
	c1 := newFakeInbound("exe_a")
	if evicted := r.Register("exe_a", c1); evicted != nil {
		t.Fatalf("first register should not evict: got %p", evicted)
	}
	got, ok := r.Lookup("exe_a")
	if !ok || got != c1 {
		t.Fatalf("lookup: ok=%v got=%p want %p", ok, got, c1)
	}
}

func TestConnRegistry_RegisterEvictsExisting(t *testing.T) {
	r := NewConnRegistry()
	c1, c2 := newFakeInbound("exe_a"), newFakeInbound("exe_a")
	r.Register("exe_a", c1)
	evicted := r.Register("exe_a", c2)
	if evicted != c1 {
		t.Fatalf("evicted: got %p want %p", evicted, c1)
	}
	got, _ := r.Lookup("exe_a")
	if got != c2 {
		t.Fatalf("after eviction lookup: got %p want %p", got, c2)
	}
}

func TestConnRegistry_UnregisterOnlyIfMatches(t *testing.T) {
	r := NewConnRegistry()
	c1, c2 := newFakeInbound("exe_a"), newFakeInbound("exe_a")
	r.Register("exe_a", c1)
	// Try to unregister with a stale inbound — must NOT remove c1.
	r.Unregister("exe_a", c2)
	if got, _ := r.Lookup("exe_a"); got != c1 {
		t.Fatalf("stale unregister should be no-op; got %p", got)
	}
	r.Unregister("exe_a", c1)
	if _, ok := r.Lookup("exe_a"); ok {
		t.Fatal("should be removed")
	}
}

func TestConnRegistry_ConnectedIDs(t *testing.T) {
	r := NewConnRegistry()
	r.Register("exe_a", newFakeInbound("exe_a"))
	r.Register("exe_b", newFakeInbound("exe_b"))
	got := r.ConnectedIDs()
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestConnRegistry_Concurrent(t *testing.T) {
	r := NewConnRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := newFakeInbound("exe_x")
			r.Register("exe_x", c)
			r.Lookup("exe_x")
			r.Unregister("exe_x", c)
		}()
	}
	wg.Wait()
}
