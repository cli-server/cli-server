package llmproxy

import (
	"bytes"
	"io"
	"time"
)

// geminiStreamInterceptor wraps a response body, transparently passing through
// all bytes while parsing SSE events to extract Gemini usage data and TTFT.
type geminiStreamInterceptor struct {
	inner      io.ReadCloser
	buf        bytes.Buffer
	startTime  time.Time
	model      string
	usage      GeminiUsageMetadata
	ttft       int64
	gotFirst   bool
	onComplete func(model string, usage GeminiUsageMetadata, ttft int64)
	completed  bool
}

func newGeminiStreamInterceptor(inner io.ReadCloser, startTime time.Time, onComplete func(string, GeminiUsageMetadata, int64)) *geminiStreamInterceptor {
	return &geminiStreamInterceptor{
		inner:      inner,
		startTime:  startTime,
		onComplete: onComplete,
	}
}

func (si *geminiStreamInterceptor) Read(p []byte) (int, error) {
	n, err := si.inner.Read(p)
	if n > 0 {
		si.buf.Write(p[:n])
		si.processLines()
	}
	if err == io.EOF {
		si.flushRemaining()
		si.finish()
	}
	return n, err
}

func (si *geminiStreamInterceptor) Close() error {
	si.flushRemaining()
	si.finish()
	return si.inner.Close()
}

func (si *geminiStreamInterceptor) processLines() {
	for {
		line, err := si.buf.ReadBytes('\n')
		if err != nil {
			si.buf.Write(line)
			return
		}
		si.parseLine(line)
	}
}

func (si *geminiStreamInterceptor) flushRemaining() {
	if si.buf.Len() > 0 {
		si.parseLine(si.buf.Bytes())
		si.buf.Reset()
	}
}

func (si *geminiStreamInterceptor) parseLine(line []byte) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data: ")) {
		return
	}
	data := bytes.TrimPrefix(line, []byte("data: "))

	model, usage, hasUsage, hasParts := ParseGeminiStreamChunk(data)
	if model != "" {
		si.model = model
	}

	// TTFT: first chunk with content parts.
	if !si.gotFirst && hasParts {
		si.gotFirst = true
		si.ttft = time.Since(si.startTime).Milliseconds()
	}

	// Last chunk's usage wins (overwrite on each chunk that has it).
	if hasUsage {
		si.usage = usage
	}
}

func (si *geminiStreamInterceptor) finish() {
	if si.completed {
		return
	}
	si.completed = true
	if si.onComplete != nil {
		si.onComplete(si.model, si.usage, si.ttft)
	}
}
