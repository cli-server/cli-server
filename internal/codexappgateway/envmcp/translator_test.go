package envmcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedBridge is a BridgeCaller whose responses are pre-scripted.
// It records every method call for assertion.
type scriptedBridge struct {
	startResult ProcessStartResult
	reads       []ProcessReadResult
	readIdx     atomic.Int32
	calls       []scriptedCall
}

type scriptedCall struct {
	method string
	params json.RawMessage
}

func (s *scriptedBridge) Call(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	s.calls = append(s.calls, scriptedCall{method: method, params: params})
	switch method {
	case ExecMethodProcessStart:
		out, _ := json.Marshal(s.startResult)
		return out, nil
	case ExecMethodProcessRead:
		i := int(s.readIdx.Add(1)) - 1
		if i >= len(s.reads) {
			return nil, errors.New("scriptedBridge: out of read responses")
		}
		out, _ := json.Marshal(s.reads[i])
		return out, nil
	default:
		return nil, errors.New("scriptedBridge: unknown method " + method)
	}
}

func (s *scriptedBridge) Notify(_ context.Context, _ string, _ json.RawMessage) error { return nil }

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestTranslator_RunShell_HappyPath(t *testing.T) {
	exit := 0
	b := &scriptedBridge{
		startResult: ProcessStartResult{ProcessID: "pid-1"},
		reads: []ProcessReadResult{
			{
				Chunks: []ProcessOutputChunk{
					{Seq: 1, Stream: "stdout", Chunk: b64("hello\n")},
				},
				NextSeq:  2,
				Exited:   true,
				ExitCode: &exit,
				Closed:   true,
			},
		},
	}
	tr := NewTranslator(b)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := tr.RunShell(ctx, []string{"echo", "hello"}, "/tmp")
	if err != nil {
		t.Fatalf("RunShell: %v", err)
	}
	if !strings.Contains(res.Text, "hello") {
		t.Errorf("Text = %q", res.Text)
	}
	if !strings.Contains(res.Text, "[exit_code=0]") {
		t.Errorf("Text missing exit code: %q", res.Text)
	}
	if res.IsError {
		t.Errorf("IsError = true on exit 0")
	}
	if len(b.calls) != 2 {
		t.Errorf("call count = %d, want 2", len(b.calls))
	}
	if b.calls[0].method != ExecMethodProcessStart {
		t.Errorf("first call method = %q", b.calls[0].method)
	}
}

func TestTranslator_RunShell_NonZeroExit_IsError(t *testing.T) {
	exit := 1
	b := &scriptedBridge{
		startResult: ProcessStartResult{ProcessID: "pid-1"},
		reads:       []ProcessReadResult{{NextSeq: 0, Exited: true, ExitCode: &exit, Closed: true}},
	}
	tr := NewTranslator(b)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := tr.RunShell(ctx, []string{"false"}, "/tmp")
	if err != nil {
		t.Fatalf("RunShell: %v", err)
	}
	if !res.IsError {
		t.Errorf("IsError = false on exit 1")
	}
	if !strings.Contains(res.Text, "[exit_code=1]") {
		t.Errorf("Text missing exit code: %q", res.Text)
	}
}

func TestTranslator_RunShell_StderrIncluded(t *testing.T) {
	exit := 0
	b := &scriptedBridge{
		startResult: ProcessStartResult{ProcessID: "pid-1"},
		reads: []ProcessReadResult{
			{
				Chunks: []ProcessOutputChunk{
					{Seq: 1, Stream: "stdout", Chunk: b64("ok\n")},
					{Seq: 2, Stream: "stderr", Chunk: b64("warn\n")},
				},
				NextSeq:  3,
				Exited:   true,
				ExitCode: &exit,
				Closed:   true,
			},
		},
	}
	tr := NewTranslator(b)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := tr.RunShell(ctx, []string{"sh", "-c", "echo ok; echo warn 1>&2"}, "/tmp")
	if err != nil {
		t.Fatalf("RunShell: %v", err)
	}
	if !strings.Contains(res.Text, "ok") || !strings.Contains(res.Text, "warn") {
		t.Errorf("Text missing stdout or stderr: %q", res.Text)
	}
	if !strings.Contains(res.Text, "--- stderr ---") {
		t.Errorf("Text missing stderr divider: %q", res.Text)
	}
}

func TestTranslator_RunShell_MultipleReadCycles(t *testing.T) {
	exit := 0
	b := &scriptedBridge{
		startResult: ProcessStartResult{ProcessID: "pid-1"},
		reads: []ProcessReadResult{
			{Chunks: []ProcessOutputChunk{{Seq: 1, Stream: "stdout", Chunk: b64("part1 ")}}, NextSeq: 2},
			{Chunks: []ProcessOutputChunk{{Seq: 2, Stream: "stdout", Chunk: b64("part2")}}, NextSeq: 3},
			{NextSeq: 3, Exited: true, ExitCode: &exit, Closed: true},
		},
	}
	tr := NewTranslator(b)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := tr.RunShell(ctx, []string{"echo", "x"}, "/tmp")
	if err != nil {
		t.Fatalf("RunShell: %v", err)
	}
	if !strings.Contains(res.Text, "part1 part2") {
		t.Errorf("Text = %q", res.Text)
	}
	if len(b.calls) != 4 {
		t.Errorf("expected 1 start + 3 reads = 4 calls, got %d", len(b.calls))
	}
	// Verify afterSeq advanced correctly between reads.
	var p1, p2 ProcessReadParams
	_ = json.Unmarshal(b.calls[1].params, &p1)
	_ = json.Unmarshal(b.calls[2].params, &p2)
	if p1.AfterSeq != 0 || p2.AfterSeq != 2 {
		t.Errorf("afterSeq drift: %d → %d", p1.AfterSeq, p2.AfterSeq)
	}
}
