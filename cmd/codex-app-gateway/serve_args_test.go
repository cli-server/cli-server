package main

import "testing"

func TestParseServeArgs_Defaults(t *testing.T) {
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
}

func TestParseServeArgs_Overrides(t *testing.T) {
	args, err := parseServeArgs([]string{
		"--listen-addr", ":9090",
		"--codex-bin", "/usr/local/bin/codex",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if args.ListenAddr != ":9090" || args.CodexBin != "/usr/local/bin/codex" {
		t.Errorf("got %+v", args)
	}
}
