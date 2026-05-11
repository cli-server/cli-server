package wsbridge_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/wsbridge"
	"nhooyr.io/websocket"
)

func echoServer(t *testing.T) *httptest.Server {
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
	upstream := echoServer(t)
	defer upstream.Close()
	upURL := "ws" + strings.TrimPrefix(upstream.URL, "http")

	mux := http.NewServeMux()
	mux.HandleFunc("/proxy", func(w http.ResponseWriter, r *http.Request) {
		userWS, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer userWS.Close(websocket.StatusNormalClosure, "")
		childWS, _, err := websocket.Dial(r.Context(), upURL, nil)
		if err != nil {
			t.Errorf("dial child: %v", err)
			return
		}
		defer childWS.Close(websocket.StatusNormalClosure, "")
		_ = wsbridge.RunProxy(r.Context(), userWS, childWS, nil)
	})
	gw := httptest.NewServer(mux)
	defer gw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(gw.URL, "http")+"/proxy", nil)
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
		t.Errorf("got %q, want HELLO", got)
	}
}

func TestRunProxy_OnFrameInvokedPerFrame(t *testing.T) {
	upstream := echoServer(t)
	defer upstream.Close()
	upURL := "ws" + strings.TrimPrefix(upstream.URL, "http")

	var frameCount atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/proxy", func(w http.ResponseWriter, r *http.Request) {
		userWS, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer userWS.Close(websocket.StatusNormalClosure, "")
		childWS, _, err := websocket.Dial(r.Context(), upURL, nil)
		if err != nil {
			t.Errorf("dial child: %v", err)
			return
		}
		defer childWS.Close(websocket.StatusNormalClosure, "")
		_ = wsbridge.RunProxy(r.Context(), userWS, childWS, func() { frameCount.Add(1) })
	})
	gw := httptest.NewServer(mux)
	defer gw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(gw.URL, "http")+"/proxy", nil)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	if err := c.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err = c.Read(ctx); err != nil {
		t.Fatalf("read: %v", err)
	}
	// Give both pump goroutines time to invoke onFrame.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && frameCount.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := frameCount.Load(); got < 2 {
		t.Fatalf("frameCount = %d, want >= 2 (one per direction)", got)
	}
}

func TestPumpFrames_PreservesTextAndBinary(t *testing.T) {
	// Two ws server stubs; PumpFrames bridges their server-side conns.
	srvSideA := make(chan *websocket.Conn, 1)
	srvSideB := make(chan *websocket.Conn, 1)

	hsA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, _ := websocket.Accept(w, r, nil)
		srvSideA <- ws
	}))
	t.Cleanup(hsA.Close)
	hsB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, _ := websocket.Accept(w, r, nil)
		srvSideB <- ws
	}))
	t.Cleanup(hsB.Close)

	ctx := context.Background()
	cliA, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hsA.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	cliB, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hsB.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}

	sa := <-srvSideA
	sb := <-srvSideB
	go wsbridge.PumpFrames(ctx, sa, sb)
	go wsbridge.PumpFrames(ctx, sb, sa)

	tCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	// Text frame A → B
	if err := cliA.Write(tCtx, websocket.MessageText, []byte(`{"id":1}`)); err != nil {
		t.Fatalf("cliA.Write: %v", err)
	}
	mt, data, err := cliB.Read(tCtx)
	if err != nil {
		t.Fatalf("cliB.Read: %v", err)
	}
	if mt != websocket.MessageText || string(data) != `{"id":1}` {
		t.Fatalf("got mt=%v data=%q", mt, data)
	}

	// Binary frame B → A
	if err := cliB.Write(tCtx, websocket.MessageBinary, []byte{0x01, 0x02, 0x03}); err != nil {
		t.Fatalf("cliB.Write: %v", err)
	}
	mt, data, err = cliA.Read(tCtx)
	if err != nil {
		t.Fatalf("cliA.Read: %v", err)
	}
	if mt != websocket.MessageBinary || len(data) != 3 || data[2] != 0x03 {
		t.Fatalf("got mt=%v data=%v", mt, data)
	}

	cliA.Close(websocket.StatusNormalClosure, "")
	cliB.Close(websocket.StatusNormalClosure, "")
}
