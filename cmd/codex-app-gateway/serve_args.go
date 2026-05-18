package main

import (
	"flag"
	"io"
	"os"
	"strconv"
)

type serveArgs struct {
	ListenAddr         string
	CodexBin           string
	OperationLogURL    string
	OperationLogSecret string
	OperationLogChan   int
}

func parseServeArgs(rawArgs []string) (serveArgs, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	listen := fs.String("listen-addr", ":8086", "HTTP listen address (env CXG_LISTEN_ADDR)")
	codexBin := fs.String("codex-bin", "codex", "path to codex binary used for `codex app-server` (env CXG_CODEX_BIN)")
	opLogURL := fs.String("oplog-url", "", "agentserver /internal/operations URL (env CXG_OPLOG_URL)")
	opLogSecret := fs.String("oplog-secret", "", "X-Internal-Secret header value (env CXG_OPLOG_SECRET)")
	opLogChan := fs.Int("oplog-chan", 1024, "bounded channel capacity (env CXG_OPLOG_CHAN)")
	if err := fs.Parse(rawArgs); err != nil {
		return serveArgs{}, err
	}
	if envListen := os.Getenv("CXG_LISTEN_ADDR"); envListen != "" && *listen == ":8086" {
		*listen = envListen
	}
	if envBin := os.Getenv("CXG_CODEX_BIN"); envBin != "" && *codexBin == "codex" {
		*codexBin = envBin
	}
	if envURL := os.Getenv("CXG_OPLOG_URL"); envURL != "" && *opLogURL == "" {
		*opLogURL = envURL
	}
	if envSec := os.Getenv("CXG_OPLOG_SECRET"); envSec != "" && *opLogSecret == "" {
		*opLogSecret = envSec
	}
	if envChan := os.Getenv("CXG_OPLOG_CHAN"); envChan != "" && *opLogChan == 1024 {
		n, err := strconv.Atoi(envChan)
		if err == nil && n > 0 {
			*opLogChan = n
		}
	}
	return serveArgs{
		ListenAddr:         *listen,
		CodexBin:           *codexBin,
		OperationLogURL:    *opLogURL,
		OperationLogSecret: *opLogSecret,
		OperationLogChan:   *opLogChan,
	}, nil
}
