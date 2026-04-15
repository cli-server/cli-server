package agentsdk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/tunnel"
)

// mockConn is a net.Conn backed by a read buffer and a write buffer.
// Reads consume from readBuf; writes append to writeBuf.
type mockConn struct {
	readBuf  *bytes.Buffer
	writeBuf *bytes.Buffer
}

func newMockConn(input []byte) *mockConn {
	return &mockConn{
		readBuf:  bytes.NewBuffer(input),
		writeBuf: &bytes.Buffer{},
	}
}

func (c *mockConn) Read(b []byte) (int, error)  { return c.readBuf.Read(b) }
func (c *mockConn) Write(b []byte) (int, error) { return c.writeBuf.Write(b) }
func (c *mockConn) Close() error                { return nil }
func (c *mockConn) LocalAddr() net.Addr          { return mockAddr{} }
func (c *mockConn) RemoteAddr() net.Addr         { return mockAddr{} }
func (c *mockConn) SetDeadline(time.Time) error      { return nil }
func (c *mockConn) SetReadDeadline(time.Time) error   { return nil }
func (c *mockConn) SetWriteDeadline(time.Time) error  { return nil }

type mockAddr struct{}

func (mockAddr) Network() string { return "mock" }
func (mockAddr) String() string  { return "mock" }

// buildHTTPStreamRequest writes a tunnel HTTP stream (header + meta + body)
// into a buffer, suitable for reading by handleHTTPStream.
func buildHTTPStreamRequest(method, path string, headers map[string]string, body []byte) ([]byte, error) {
	meta := tunnel.HTTPStreamMeta{
		Method:  method,
		Path:    path,
		Headers: headers,
		BodyLen: len(body),
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := tunnel.WriteStreamHeader(&buf, tunnel.StreamTypeHTTP, metaJSON); err != nil {
		return nil, err
	}
	if len(body) > 0 {
		buf.Write(body)
	}
	return buf.Bytes(), nil
}

func TestHandleHTTPStream_GET(t *testing.T) {
	// Build a GET request stream.
	input, err := buildHTTPStreamRequest("GET", "/hello", map[string]string{"Host": "example.com"}, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	conn := newMockConn(input)

	// Handler that returns a greeting.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/hello" {
			t.Errorf("expected /hello, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "Hello, World!")
	})

	handleHTTPStream(conn, handler)

	// Read back the response from the write buffer.
	respReader := bytes.NewReader(conn.writeBuf.Bytes())
	streamType, metaBytes, err := tunnel.ReadStreamHeader(respReader)
	if err != nil {
		t.Fatalf("read response header: %v", err)
	}
	if streamType != tunnel.StreamTypeHTTP {
		t.Fatalf("expected stream type HTTP (%d), got %d", tunnel.StreamTypeHTTP, streamType)
	}

	var respMeta tunnel.HTTPResponseMeta
	if err := json.Unmarshal(metaBytes, &respMeta); err != nil {
		t.Fatalf("unmarshal response meta: %v", err)
	}
	if respMeta.Status != http.StatusOK {
		t.Errorf("expected status 200, got %d", respMeta.Status)
	}
	if ct, ok := respMeta.Headers["Content-Type"]; !ok || ct != "text/plain" {
		t.Errorf("expected Content-Type text/plain, got %q", respMeta.Headers["Content-Type"])
	}

	body, err := io.ReadAll(respReader)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(body) != "Hello, World!" {
		t.Errorf("expected body %q, got %q", "Hello, World!", string(body))
	}
}

func TestHandleHTTPStream_POST_Echo(t *testing.T) {
	// Build a POST request with a body.
	reqBody := []byte(`{"message":"ping"}`)
	input, err := buildHTTPStreamRequest("POST", "/echo", map[string]string{
		"Host":         "example.com",
		"Content-Type": "application/json",
	}, reqBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	conn := newMockConn(input)

	// Echo handler: reads request body and writes it back.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/echo" {
			t.Errorf("expected /echo, got %s", r.URL.Path)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	})

	handleHTTPStream(conn, handler)

	// Read back the response.
	respReader := bytes.NewReader(conn.writeBuf.Bytes())
	streamType, metaBytes, err := tunnel.ReadStreamHeader(respReader)
	if err != nil {
		t.Fatalf("read response header: %v", err)
	}
	if streamType != tunnel.StreamTypeHTTP {
		t.Fatalf("expected stream type HTTP, got %d", streamType)
	}

	var respMeta tunnel.HTTPResponseMeta
	if err := json.Unmarshal(metaBytes, &respMeta); err != nil {
		t.Fatalf("unmarshal response meta: %v", err)
	}
	if respMeta.Status != http.StatusOK {
		t.Errorf("expected status 200, got %d", respMeta.Status)
	}

	body, err := io.ReadAll(respReader)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(body) != string(reqBody) {
		t.Errorf("expected echoed body %q, got %q", string(reqBody), string(body))
	}
}
