package agent

import (
	"context"
	"errors"
)

type TUIOpts struct {
	Server          string
	WorkspaceID     string
	Name            string
	WorkDir         string
	Resume          string
	Continue        bool
	Yolo            bool
	SkipOpenBrowser bool
	Model           string
	ResponderTTL    string
}

// RunTUI is the entry point for the `tui` subcommand. The actual wiring lives
// in cmd/agentserver-agent/tui_run.go to avoid the import cycle that would
// arise if this package imported internal/agent/tui (which already imports
// internal/agent).
//
// cmd/agentserver-agent sets RunTUIFunc at init() time.
var RunTUIFunc func(ctx context.Context, opts TUIOpts) error

// RunTUI delegates to RunTUIFunc, which is wired by the binary's init().
func RunTUI(ctx context.Context, opts TUIOpts) error {
	if RunTUIFunc == nil {
		return errors.New("tui: RunTUIFunc not wired (missing cmd/agentserver-agent/tui_run.go init)")
	}
	return RunTUIFunc(ctx, opts)
}
