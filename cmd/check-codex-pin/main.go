package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	pinPath := flag.String("pin", "codex-pin.json", "path to codex-pin.json (relative to repo root or absolute)")
	repoRoot := flag.String("repo-root", ".", "path to this repo's root")
	upstreamSource := flag.String("upstream-source", "", "local path to upstream codex checkout (if empty, the program clones the pinned tag to a temp dir)")
	flag.Parse()

	upstream := *upstreamSource
	if upstream == "" {
		var cleanup func()
		var err error
		upstream, cleanup, err = cloneUpstream(*pinPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "clone upstream: %v\n", err)
			os.Exit(2)
		}
		defer cleanup()
	}

	report, err := Verify(*pinPath, *repoRoot, upstream)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify: %v\n", err)
		os.Exit(2)
	}
	if len(report.Mismatches) == 0 {
		fmt.Println("codex-pin: OK")
		return
	}
	fmt.Fprintf(os.Stderr, "codex-pin: %d mismatch(es):\n", len(report.Mismatches))
	for _, m := range report.Mismatches {
		fmt.Fprintf(os.Stderr, "  %s [%s] want=%s got=%s\n", m.File, m.Reason, m.Want, m.Got)
	}
	os.Exit(1)
}

func cloneUpstream(pinPath string) (string, func(), error) {
	pinBytes, err := os.ReadFile(pinPath)
	if err != nil {
		return "", nil, fmt.Errorf("read pin: %w", err)
	}
	var pin struct {
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal(pinBytes, &pin); err != nil {
		return "", nil, fmt.Errorf("parse pin: %w", err)
	}
	if pin.Tag == "" {
		return "", nil, fmt.Errorf("pin has empty tag")
	}
	tmp, err := os.MkdirTemp("", "codex-pin-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	dst := filepath.Join(tmp, "codex")
	cmd := exec.Command("git", "clone", "--depth", "1", "--branch", pin.Tag, "https://github.com/openai/codex.git", dst)
	cmd.Stdout = os.Stderr // git progress to stderr, keep our stdout clean
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("git clone %s: %w", pin.Tag, err)
	}
	return dst, cleanup, nil
}
