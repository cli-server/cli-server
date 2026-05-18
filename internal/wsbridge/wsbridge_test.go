package wsbridge_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// newWSPair returns two server-side websocket conns. They are the
// "bridge ends" the tests pass to RunProxyWithInterceptor. The
// corresponding client-side dial conns are stashed and the
// writeText/readText helpers route through them automatically by
// looking up the pair in pairRegistry.
//
// Why two ends rather than one conn-pair: RunProxyWithInterceptor
// reads from `a`, writes to `b`, and vice versa. To exercise that, the
// test must inject a frame into `a` from the "other side" (so the
// pump's Read on `a` returns it) and observe a forwarded frame out the
// other side of `b`. That requires four endpoints: a-server (bridge),
// a-client (test), b-server (bridge), b-client (test).
func newWSPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	srvCh := func() (*httptest.Server, chan *websocket.Conn) {
		ch := make(chan *websocket.Conn, 1)
		hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			ch <- ws
			<-r.Context().Done()
		}))
		return hs, ch
	}
	hsA, chA := srvCh()
	t.Cleanup(hsA.Close)
	hsB, chB := srvCh()
	t.Cleanup(hsB.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cliA, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hsA.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial A: %v", err)
	}
	cliB, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hsB.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	srvA := <-chA
	srvB := <-chB
	t.Cleanup(func() {
		cliA.Close(websocket.StatusNormalClosure, "")
		cliB.Close(websocket.StatusNormalClosure, "")
	})
	// Register the client-side counterparts so writeText/readText can
	// find them when given the server-side conns.
	pairRegistry.Lock()
	if pairRegistry.m == nil {
		pairRegistry.m = map[*websocket.Conn]*websocket.Conn{}
	}
	pairRegistry.m[srvA] = cliA
	pairRegistry.m[srvB] = cliB
	pairRegistry.Unlock()
	t.Cleanup(func() {
		pairRegistry.Lock()
		delete(pairRegistry.m, srvA)
		delete(pairRegistry.m, srvB)
		pairRegistry.Unlock()
	})
	return srvA, srvB
}

// pairRegistry maps a bridge-side conn to its client-side counterpart
// so writeText/readText can route a "write on a" → "actually write on
// the client end of a so the bridge reads it" without changing the
// test surface.
var pairRegistry = struct {
	sync.Mutex
	m map[*websocket.Conn]*websocket.Conn
}{}

func pairOf(c *websocket.Conn) *websocket.Conn {
	pairRegistry.Lock()
	defer pairRegistry.Unlock()
	if peer, ok := pairRegistry.m[c]; ok {
		return peer
	}
	return c
}

// writeText writes a text frame "to c" — but actually writes on c's
// client-side peer so the bridge reading from c receives it.
func writeText(c *websocket.Conn, s string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return pairOf(c).Write(ctx, websocket.MessageText, []byte(s))
}

// readText reads a text frame "from c" — but actually reads on c's
// client-side peer so it observes what the bridge wrote to c.
func readText(t *testing.T, c *websocket.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	mt, data, err := pairOf(c).Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if mt != websocket.MessageText {
		t.Fatalf("expected text frame, got %v", mt)
	}
	return string(data)
}

func TestRunProxyWithInterceptor_CallbacksAndRewrite(t *testing.T) {
	a, b := newWSPair(t)
	defer a.Close(websocket.StatusNormalClosure, "")
	defer b.Close(websocket.StatusNormalClosure, "")

	var (
		ctc, stc [][]byte
		mu       sync.Mutex
	)
	intc := wsbridge.Interceptor{
		OnClientFrame: func(frame []byte) []byte {
			mu.Lock()
			defer mu.Unlock()
			ctc = append(ctc, append([]byte(nil), frame...))
			return nil
		},
		OnServerFrame: func(frame []byte) []byte {
			mu.Lock()
			defer mu.Unlock()
			stc = append(stc, append([]byte(nil), frame...))
			if string(frame) == "swap-me" {
				return []byte(`{"intercepted":true}`)
			}
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go wsbridge.RunProxyWithInterceptor(ctx, a, b, intc, nil)

	if err := writeText(a, "hello-server"); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, b); got != "hello-server" {
		t.Fatalf("server got %q", got)
	}

	if err := writeText(b, "swap-me"); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, a); got != `{"intercepted":true}` {
		t.Fatalf("client got %q", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(ctc) != 1 || string(ctc[0]) != "hello-server" {
		t.Fatalf("ctc=%q", ctc)
	}
	if len(stc) != 1 || string(stc[0]) != "swap-me" {
		t.Fatalf("stc=%q", stc)
	}
}

func TestRunProxyWithInterceptor_DropFrameSwallows(t *testing.T) {
	a, b := newWSPair(t)
	defer a.Close(websocket.StatusNormalClosure, "")
	defer b.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go wsbridge.RunProxyWithInterceptor(ctx, a, b, wsbridge.Interceptor{
		OnClientFrame: func(frame []byte) []byte {
			if string(frame) == "drop-me" {
				return wsbridge.DropFrame
			}
			return nil
		},
	}, nil)

	if err := writeText(a, "drop-me"); err != nil {
		t.Fatal(err)
	}
	// b must not receive the dropped frame within a window. Read on b's
	// client-side peer; the bridge's write-out of b is what we'd see.
	bctx, bcancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer bcancel()
	_, _, err := pairOf(b).Read(bctx)
	if err == nil {
		t.Fatal("b should not have received the dropped frame")
	}
}

func TestRunProxyWithInterceptor_NilCallbacksForwardUnchanged(t *testing.T) {
	a, b := newWSPair(t)
	defer a.Close(websocket.StatusNormalClosure, "")
	defer b.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// All-nil interceptor — should behave exactly like RunProxy
	go wsbridge.RunProxyWithInterceptor(ctx, a, b, wsbridge.Interceptor{}, nil)

	if err := writeText(a, "ping"); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, b); got != "ping" {
		t.Fatalf("got %q", got)
	}
	if err := writeText(b, "pong"); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, a); got != "pong" {
		t.Fatalf("got %q", got)
	}
}
