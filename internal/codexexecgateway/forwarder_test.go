package codexexecgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// dialPair creates two ws servers; server-side conns are bridged via pumpFrames
// (in both directions), returning the two client-side conns.
func dialPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
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
	cliA, _, err := websocket.Dial(ctx, "ws"+hsA.URL[len("http"):], nil)
	if err != nil {
		t.Fatal(err)
	}
	cliB, _, err := websocket.Dial(ctx, "ws"+hsB.URL[len("http"):], nil)
	if err != nil {
		t.Fatal(err)
	}

	sa := <-srvSideA
	sb := <-srvSideB
	go pumpFrames(ctx, sa, sb)
	go pumpFrames(ctx, sb, sa)
	return cliA, cliB
}

func TestPumpFrames_PreservesTextAndBinary(t *testing.T) {
	a, b := dialPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Text frame A → B
	if err := a.Write(ctx, websocket.MessageText, []byte(`{"id":1}`)); err != nil {
		t.Fatalf("a.Write: %v", err)
	}
	mt, data, err := b.Read(ctx)
	if err != nil {
		t.Fatalf("b.Read: %v", err)
	}
	if mt != websocket.MessageText || string(data) != `{"id":1}` {
		t.Fatalf("got mt=%v data=%q", mt, data)
	}

	// Binary frame B → A
	if err := b.Write(ctx, websocket.MessageBinary, []byte{0x01, 0x02, 0x03}); err != nil {
		t.Fatalf("b.Write: %v", err)
	}
	mt, data, err = a.Read(ctx)
	if err != nil {
		t.Fatalf("a.Read: %v", err)
	}
	if mt != websocket.MessageBinary || len(data) != 3 || data[2] != 0x03 {
		t.Fatalf("got mt=%v data=%v", mt, data)
	}

	a.Close(websocket.StatusNormalClosure, "")
	b.Close(websocket.StatusNormalClosure, "")
}
