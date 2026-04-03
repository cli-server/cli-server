package llmproxy

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestGeminiStreamInterceptor(t *testing.T) {
	sseData := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"Hello"}],"role":"model"}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1},"modelVersion":"gemini-2.5-flash"}`,
		"",
		`data: {"candidates":[{"content":{"parts":[{"text":" world"}],"role":"model"}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5},"modelVersion":"gemini-2.5-flash"}`,
		"",
		`data: {"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":20,"totalTokenCount":30},"modelVersion":"gemini-2.5-flash"}`,
		"",
	}, "\n")

	var gotModel string
	var gotUsage GeminiUsageMetadata
	var gotTTFT int64
	called := false

	inner := io.NopCloser(strings.NewReader(sseData))
	startTime := time.Now().Add(-10 * time.Millisecond) // ensure measurable TTFT
	si := newGeminiStreamInterceptor(inner, startTime, func(model string, usage GeminiUsageMetadata, ttft int64) {
		gotModel = model
		gotUsage = usage
		gotTTFT = ttft
		called = true
	})

	// Read all data through the interceptor.
	out, err := io.ReadAll(si)
	if err != nil {
		t.Fatalf("read error: %v", err)
	}

	// Data must pass through unchanged.
	if string(out) != sseData {
		t.Errorf("data was modified during passthrough")
	}

	if !called {
		t.Fatal("onComplete was not called")
	}
	if gotModel != "gemini-2.5-flash" {
		t.Errorf("model = %q, want %q", gotModel, "gemini-2.5-flash")
	}
	// Last chunk's usage should win.
	if gotUsage.PromptTokenCount != 10 {
		t.Errorf("input = %d, want 10", gotUsage.PromptTokenCount)
	}
	if gotUsage.CandidatesTokenCount != 20 {
		t.Errorf("output = %d, want 20", gotUsage.CandidatesTokenCount)
	}
	if gotTTFT <= 0 {
		t.Errorf("ttft = %d, want > 0", gotTTFT)
	}
}

func TestGeminiStreamInterceptor_NoContent(t *testing.T) {
	// Stream with only usage metadata, no content parts — TTFT should be 0.
	sseData := "data: {\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":0},\"modelVersion\":\"gemini-2.5-flash\"}\n\n"

	var gotTTFT int64
	called := false

	inner := io.NopCloser(strings.NewReader(sseData))
	si := newGeminiStreamInterceptor(inner, time.Now(), func(model string, usage GeminiUsageMetadata, ttft int64) {
		gotTTFT = ttft
		called = true
	})

	io.ReadAll(si)

	if !called {
		t.Fatal("onComplete was not called")
	}
	if gotTTFT != 0 {
		t.Errorf("ttft = %d, want 0 (no content parts seen)", gotTTFT)
	}
}

func TestGeminiStreamInterceptor_Close(t *testing.T) {
	sseData := "data: {\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":3},\"modelVersion\":\"gemini-2.5-flash\"}\n\n"

	called := false
	inner := io.NopCloser(strings.NewReader(sseData))
	si := newGeminiStreamInterceptor(inner, time.Now(), func(model string, usage GeminiUsageMetadata, ttft int64) {
		called = true
	})

	// Read partial, then close.
	buf := make([]byte, 10)
	si.Read(buf)
	si.Close()

	if !called {
		t.Fatal("onComplete was not called on Close")
	}
}
