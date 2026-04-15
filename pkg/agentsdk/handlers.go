package agentsdk

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"

	"github.com/agentserver/agentserver/internal/tunnel"
)

// handleHTTPStream reads the stream header and delegates to handleHTTPStreamWithMeta.
func handleHTTPStream(stream net.Conn, handler http.Handler) {
	_, metaBytes, err := tunnel.ReadStreamHeader(stream)
	if err != nil {
		return
	}
	handleHTTPStreamWithMeta(stream, metaBytes, handler)
}

// handleHTTPStreamWithMeta processes an HTTP proxy stream with pre-read metadata.
// It reconstructs the HTTP request, calls the handler, and writes the response
// back using the tunnel protocol.
func handleHTTPStreamWithMeta(stream net.Conn, metaBytes []byte, handler http.Handler) {
	// 1. Unmarshal request metadata.
	var meta tunnel.HTTPStreamMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return
	}

	// 2. Read exactly BodyLen bytes of request body.
	var reqBody []byte
	if meta.BodyLen > 0 {
		reqBody = make([]byte, meta.BodyLen)
		if _, err := io.ReadFull(stream, reqBody); err != nil {
			return
		}
	}

	// 3. Reconstruct *http.Request.
	reqURL, err := url.ParseRequestURI(meta.Path)
	if err != nil {
		reqURL = &url.URL{Path: meta.Path}
	}
	req := &http.Request{
		Method:     meta.Method,
		URL:        reqURL,
		RequestURI: meta.Path,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(reqBody)),
		Host:       meta.Headers["Host"],
	}
	for k, v := range meta.Headers {
		req.Header.Set(k, v)
	}

	// 4. Call handler with a buffering response writer.
	rw := &streamResponseWriter{
		header: make(http.Header),
		status: http.StatusOK,
	}
	handler.ServeHTTP(rw, req)

	// 5. Write response back to stream.
	rw.finish(stream)
}

// streamResponseWriter implements http.ResponseWriter, buffering the response
// so it can be written to the stream using the tunnel protocol.
type streamResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *streamResponseWriter) Header() http.Header {
	return w.header
}

func (w *streamResponseWriter) WriteHeader(code int) {
	w.status = code
}

func (w *streamResponseWriter) Write(data []byte) (int, error) {
	return w.body.Write(data)
}

// finish writes the HTTP response to the stream using the tunnel protocol:
// a stream header with HTTPResponseMeta followed by the response body.
func (w *streamResponseWriter) finish(stream net.Conn) {
	// Build response headers map (single-value).
	headers := make(map[string]string, len(w.header))
	for k := range w.header {
		headers[k] = w.header.Get(k)
	}

	respMeta := tunnel.HTTPResponseMeta{
		Status:  w.status,
		Headers: headers,
	}
	metaJSON, err := json.Marshal(respMeta)
	if err != nil {
		return
	}

	if err := tunnel.WriteStreamHeader(stream, tunnel.StreamTypeHTTP, metaJSON); err != nil {
		return
	}
	stream.Write(w.body.Bytes())
}
