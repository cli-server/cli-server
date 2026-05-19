package server

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDispatcherSerializesPerKey(t *testing.T) {
	var (
		mu       sync.Mutex
		started  []string
		finished []string
	)
	processFn := func(req codexInboundRequest) {
		mu.Lock()
		started = append(started, req.Text)
		mu.Unlock()
		time.Sleep(40 * time.Millisecond)
		mu.Lock()
		finished = append(finished, req.Text)
		mu.Unlock()
	}
	d := newCodexDispatcher(processFn, 5)
	defer d.Stop()

	d.Enqueue(codexInboundRequest{ChannelID: "ch", WechatUserID: "u", Text: "A"})
	d.Enqueue(codexInboundRequest{ChannelID: "ch", WechatUserID: "u", Text: "B"})
	d.Enqueue(codexInboundRequest{ChannelID: "ch", WechatUserID: "u", Text: "C"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(finished)
		mu.Unlock()
		if done == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(finished) != 3 {
		t.Fatalf("finished=%v want all 3", finished)
	}
	want := []string{"A", "B", "C"}
	for i := range want {
		if started[i] != want[i] {
			t.Errorf("started[%d]=%s want %s", i, started[i], want[i])
		}
	}
}

func TestDispatcherIndependentKeysRunConcurrently(t *testing.T) {
	var inFlight, peakInFlight int32
	processFn := func(_ codexInboundRequest) {
		now := atomic.AddInt32(&inFlight, 1)
		for {
			p := atomic.LoadInt32(&peakInFlight)
			if now <= p || atomic.CompareAndSwapInt32(&peakInFlight, p, now) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
	}
	d := newCodexDispatcher(processFn, 5)
	defer d.Stop()
	d.Enqueue(codexInboundRequest{ChannelID: "ch", WechatUserID: "u1", Text: "A"})
	d.Enqueue(codexInboundRequest{ChannelID: "ch", WechatUserID: "u2", Text: "B"})
	d.Enqueue(codexInboundRequest{ChannelID: "ch", WechatUserID: "u3", Text: "C"})
	time.Sleep(150 * time.Millisecond)
	if atomic.LoadInt32(&peakInFlight) < 2 {
		t.Errorf("peakInFlight=%d want >=2 (independent keys should overlap)", peakInFlight)
	}
}

func TestDispatcherStopIsIdempotent(t *testing.T) {
	d := newCodexDispatcher(func(codexInboundRequest) {}, 5)
	d.Stop()
	d.Stop() // must not panic
}

func TestDispatcherDropsOldestPastCap(t *testing.T) {
	var processed []string
	var mu sync.Mutex
	processFn := func(req codexInboundRequest) {
		if req.Text == "first" {
			time.Sleep(200 * time.Millisecond)
		}
		mu.Lock()
		processed = append(processed, req.Text)
		mu.Unlock()
	}
	d := newCodexDispatcher(processFn, 2)
	defer d.Stop()
	d.Enqueue(codexInboundRequest{ChannelID: "ch", WechatUserID: "u", Text: "first"})
	for _, msg := range []string{"a", "b", "c", "d"} {
		d.Enqueue(codexInboundRequest{ChannelID: "ch", WechatUserID: "u", Text: msg})
	}
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(processed) > 3 {
		t.Errorf("processed=%v want at most 3", processed)
	}
	if processed[0] != "first" {
		t.Errorf("processed[0]=%s want first", processed[0])
	}
}
