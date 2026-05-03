package main

import (
	"context"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentserver/agentserver/internal/agent"
	"github.com/agentserver/agentserver/internal/agent/tui"
)

func init() {
	agent.RunTUIFunc = runTUI
}

// runTUI is the real implementation of RunTUI. It wires AuthController
// (credential lifecycle), Bus (HTTP client), Bubble Tea program (UI), and
// — when authenticated — an ExecutorClient (yamux tunnel for cc-broker
// remote_* tool calls).
//
// Architecture: three goroutines share only OAuth credentials, no in-process
// control flow between them:
//  1. Bubble Tea program (UI thread, this function blocks on it via Run).
//  2. ExecutorClient (yamux to executor-registry; spawned only if logged in).
//  3. AuthController (callback from a polling goroutine).
func runTUI(ctx context.Context, opts agent.TUIOpts) error {
	// 1. Resolve server URL — flag wins, then saved creds.
	server := opts.Server
	creds, _ := agent.LoadCredentials(agent.DefaultCredentialsPath())
	if server == "" && creds != nil {
		server = creds.ServerURL
	}
	if opts.WorkspaceID == "" {
		return fmt.Errorf("--workspace-id is required")
	}
	workDir := opts.WorkDir
	if workDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
		workDir = cwd
	}

	// 2. Build AuthController.
	auth := tui.NewAuthController(tui.AuthConfig{
		ServerURL:       server,
		CredentialsPath: agent.DefaultCredentialsPath(),
		SkipOpenBrowser: opts.SkipOpenBrowser,
	})

	var executorID string

	// 3. If logged in, register executor + start tunnel BEFORE the Bubble Tea
	//    program starts. If not logged in, the executor goroutine is started
	//    after /login completes.
	if auth.State() == tui.AuthLoggedIn {
		sess, err := agent.LoadOrRegisterExecutor(agent.ExecutorOpts{
			ServerURL:   server,
			Name:        opts.Name,
			WorkspaceID: opts.WorkspaceID,
		})
		if err != nil {
			return fmt.Errorf("register executor: %w", err)
		}
		executorID = sess.ExecutorID
		ec := agent.NewExecutorClient(sess, workDir)
		go func() { _ = ec.Run(ctx) }()
	}

	// 4. Build Bus.
	bus := tui.NewBus(tui.BusConfig{
		ServerURL:   server,
		WorkspaceID: opts.WorkspaceID,
		ExecutorID:  executorID,
		Auth:        auth,
	})

	// 5. Build Model.
	model := tui.NewModel(tui.ModelConfig{
		ServerURL:    server,
		WorkspaceID:  opts.WorkspaceID,
		ExecutorID:   executorID,
		Bus:          bus,
		Auth:         auth,
		Yolo:         opts.Yolo,
		InitialModel: opts.Model,
		Resume:       opts.Resume,
		Continue:     opts.Continue,
	})

	// 6. Build the Bubble Tea program and wire AuthController.OnChange to
	//    pump AuthStateChangedMsg into the program.
	p := tea.NewProgram(model, tea.WithContext(ctx), tea.WithAltScreen())
	auth.SetOnChange(func(s tui.AuthState) {
		p.Send(tui.AuthStateChangedMsg{State: s})
	})

	// 7. If Resume is set, start SSE consumer and pump events into the program.
	//    TODO(T17): also start SSE when sessionID is known dynamically (after
	//    NewSessionReplyMsg, InboundAcceptedMsg, etc.).
	if opts.Resume != "" && auth.State() == tui.AuthLoggedIn {
		consumer := tui.NewSSEConsumer(bus, tui.SSEConfig{
			SessionID: opts.Resume,
		})
		go func() {
			evCh := consumer.Run(ctx)
			for ev := range evCh {
				p.Send(tui.EventArrivedMsg{Event: ev})
			}
		}()
	}

	_, err := p.Run()
	if err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}
