package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func pairServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			_ = c.Write(ctx, typ, []byte(strings.ToUpper(string(data))))
		}
	}))
}

func TestRunProxy_BidirectionalForwarding(t *testing.T) {
	upstream := pairServer(t)
	defer upstream.Close()
	wsURL := "ws" + strings.TrimPrefix(upstream.URL, "http")

	mux := http.NewServeMux()
	mux.HandleFunc("/proxy", func(w http.ResponseWriter, r *http.Request) {
		userWS, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer userWS.Close(websocket.StatusNormalClosure, "")
		childWS, _, err := websocket.Dial(r.Context(), wsURL, nil)
		if err != nil {
			t.Errorf("dial child: %v", err)
			return
		}
		defer childWS.Close(websocket.StatusNormalClosure, "")
		_ = RunProxy(r.Context(), userWS, childWS, nil)
	})
	gateway := httptest.NewServer(mux)
	defer gateway.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(gateway.URL, "http")+"/proxy", nil)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	if err := c.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, got, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "HELLO" {
		t.Errorf("got %q", got)
	}
}

func TestRunProxy_OnFrameInvokedPerFrame(t *testing.T) {
	upstream := pairServer(t)
	defer upstream.Close()
	wsURL := "ws" + strings.TrimPrefix(upstream.URL, "http")

	var frameCount atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/proxy", func(w http.ResponseWriter, r *http.Request) {
		userWS, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer userWS.Close(websocket.StatusNormalClosure, "")
		childWS, _, err := websocket.Dial(r.Context(), wsURL, nil)
		if err != nil {
			t.Errorf("dial child: %v", err)
			return
		}
		defer childWS.Close(websocket.StatusNormalClosure, "")
		_ = RunProxy(r.Context(), userWS, childWS, func() { frameCount.Add(1) })
	})
	gateway := httptest.NewServer(mux)
	defer gateway.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(gateway.URL, "http")+"/proxy", nil)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	if err := c.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err = c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Give the second pump's onFrame call time to fire.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && frameCount.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := frameCount.Load(); got < 2 {
		t.Fatalf("frameCount = %d, want >= 2 (one per direction)", got)
	}
}
