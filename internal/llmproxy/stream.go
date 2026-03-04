package llmproxy

import (
	"bytes"
	"io"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// streamInterceptor wraps a response body, transparently passing through
// all bytes while parsing SSE events to extract usage data and TTFT.
type streamInterceptor struct {
	inner      io.ReadCloser
	buf        bytes.Buffer // line buffer for SSE parsing
	startTime  time.Time
	model      string
	msgID      string
	usage      anthropic.Usage // accumulator
	ttft       int64           // time to first token (ms)
	gotFirst   bool            // whether first content token was seen
	onComplete func(model, msgID string, usage anthropic.Usage, ttft int64)
	completed  bool
}

func newStreamInterceptor(inner io.ReadCloser, startTime time.Time, onComplete func(string, string, anthropic.Usage, int64)) *streamInterceptor {
	return &streamInterceptor{
		inner:      inner,
		startTime:  startTime,
		onComplete: onComplete,
	}
}

// Read passes through bytes from the inner reader while parsing SSE events.
// The original data is never modified.
func (si *streamInterceptor) Read(p []byte) (int, error) {
	n, err := si.inner.Read(p)
	if n > 0 {
		si.buf.Write(p[:n])
		si.processLines()
	}
	if err == io.EOF {
		// Flush any remaining partial line in the buffer before finishing.
		si.flushRemaining()
		si.finish()
	}
	return n, err
}

func (si *streamInterceptor) Close() error {
	si.flushRemaining()
	si.finish()
	return si.inner.Close()
}

// processLines extracts complete lines from the buffer and parses SSE data lines.
func (si *streamInterceptor) processLines() {
	for {
		line, err := si.buf.ReadBytes('\n')
		if err != nil {
			// Incomplete line — put it back for next read.
			si.buf.Write(line)
			return
		}
		si.parseLine(line)
	}
}

// flushRemaining parses any data left in the buffer that doesn't end with a newline.
// Called on EOF/Close so the final message_delta event is never lost.
func (si *streamInterceptor) flushRemaining() {
	if si.buf.Len() > 0 {
		si.parseLine(si.buf.Bytes())
		si.buf.Reset()
	}
}

// parseLine handles a single SSE line: "data: {...}\n"
func (si *streamInterceptor) parseLine(line []byte) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data: ")) {
		return
	}
	data := bytes.TrimPrefix(line, []byte("data: "))
	if bytes.Equal(data, []byte("[DONE]")) {
		return
	}

	eventType, model, msgID, usage, hasUsage := ParseStreamEvent(data)
	if model != "" {
		si.model = model
	}
	if msgID != "" {
		si.msgID = msgID
	}

	// Track TTFT: the first content_block_delta carries the first output token.
	if !si.gotFirst && eventType == "content_block_delta" {
		si.gotFirst = true
		si.ttft = time.Since(si.startTime).Milliseconds()
	}

	if hasUsage {
		switch eventType {
		case "message_start":
			// Initial usage: input tokens, cache tokens.
			si.usage.InputTokens = usage.InputTokens
			si.usage.CacheCreationInputTokens = usage.CacheCreationInputTokens
			si.usage.CacheReadInputTokens = usage.CacheReadInputTokens
		case "message_delta":
			// Final delta: output tokens.
			si.usage.OutputTokens = usage.OutputTokens
		}
	}
}

func (si *streamInterceptor) finish() {
	if si.completed {
		return
	}
	si.completed = true
	if si.onComplete != nil && si.model != "" {
		si.onComplete(si.model, si.msgID, si.usage, si.ttft)
	}
}
