package main

import (
	"flag"
	"io"
	"os"
)

type serveArgs struct {
	ListenAddr string
	CodexBin   string
}

func parseServeArgs(rawArgs []string) (serveArgs, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	listen := fs.String("listen-addr", ":8086", "HTTP listen address (env CXG_LISTEN_ADDR)")
	codexBin := fs.String("codex-bin", "codex", "path to codex binary used for `codex app-server` (env CXG_CODEX_BIN)")
	if err := fs.Parse(rawArgs); err != nil {
		return serveArgs{}, err
	}
	if envListen := os.Getenv("CXG_LISTEN_ADDR"); envListen != "" && *listen == ":8086" {
		*listen = envListen
	}
	if envBin := os.Getenv("CXG_CODEX_BIN"); envBin != "" && *codexBin == "codex" {
		*codexBin = envBin
	}
	return serveArgs{ListenAddr: *listen, CodexBin: *codexBin}, nil
}
