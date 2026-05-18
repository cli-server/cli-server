package main

import "testing"

func TestParseServeArgs_Defaults(t *testing.T) {
	t.Setenv("CXG_OPLOG_URL", "")
	t.Setenv("CXG_OPLOG_SECRET", "")
	t.Setenv("CXG_OPLOG_CHAN", "")
	args, err := parseServeArgs([]string{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if args.ListenAddr != ":8086" {
		t.Errorf("ListenAddr = %q", args.ListenAddr)
	}
	if args.CodexBin != "codex" {
		t.Errorf("CodexBin = %q", args.CodexBin)
	}
	if args.OperationLogURL != "" {
		t.Errorf("OperationLogURL = %q, want empty", args.OperationLogURL)
	}
	if args.OperationLogSecret != "" {
		t.Errorf("OperationLogSecret = %q, want empty", args.OperationLogSecret)
	}
	if args.OperationLogChan != 1024 {
		t.Errorf("OperationLogChan = %d, want 1024", args.OperationLogChan)
	}
}

func TestParseServeArgs_Overrides(t *testing.T) {
	t.Setenv("CXG_OPLOG_URL", "")
	t.Setenv("CXG_OPLOG_SECRET", "")
	t.Setenv("CXG_OPLOG_CHAN", "")
	args, err := parseServeArgs([]string{
		"--listen-addr", ":9090",
		"--codex-bin", "/usr/local/bin/codex",
		"--oplog-url", "http://agentserver:8080/internal/operations",
		"--oplog-secret", "topsecret",
		"--oplog-chan", "4096",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if args.ListenAddr != ":9090" || args.CodexBin != "/usr/local/bin/codex" {
		t.Errorf("got %+v", args)
	}
	if args.OperationLogURL != "http://agentserver:8080/internal/operations" {
		t.Errorf("OperationLogURL = %q", args.OperationLogURL)
	}
	if args.OperationLogSecret != "topsecret" {
		t.Errorf("OperationLogSecret = %q", args.OperationLogSecret)
	}
	if args.OperationLogChan != 4096 {
		t.Errorf("OperationLogChan = %d", args.OperationLogChan)
	}
}

func TestParseServeArgs_EnvFallback(t *testing.T) {
	t.Setenv("CXG_OPLOG_URL", "http://env-url/ops")
	t.Setenv("CXG_OPLOG_SECRET", "env-secret")
	t.Setenv("CXG_OPLOG_CHAN", "2048")
	args, err := parseServeArgs([]string{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if args.OperationLogURL != "http://env-url/ops" {
		t.Errorf("OperationLogURL = %q", args.OperationLogURL)
	}
	if args.OperationLogSecret != "env-secret" {
		t.Errorf("OperationLogSecret = %q", args.OperationLogSecret)
	}
	if args.OperationLogChan != 2048 {
		t.Errorf("OperationLogChan = %d", args.OperationLogChan)
	}
}
