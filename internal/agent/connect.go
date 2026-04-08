package agent

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// ConnectOptions holds all flags for the connect command.
type ConnectOptions struct {
	Server          string
	Name            string
	SkipOpenBrowser bool
	Resume          string // sandbox ID to resume
	Continue        bool   // resume most recent session
	OpencodeURL     string
	OpencodeURLSet  bool // true if --opencode-url was explicitly provided
	OpencodeToken   string
	AutoStart       bool
	OpencodeBin     string
	OpencodePort    int  // 0 = auto-assign
	OpencodePortSet bool // true if --opencode-port was explicitly provided
}

// RunConnect executes the agent connect workflow.
// Each invocation registers a new sandbox (or resumes an existing one).
func RunConnect(opts ConnectOptions) {
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get working directory: %v", err)
	}
	if opts.Name == "" {
		hostname, _ := os.Hostname()
		if hostname != "" {
			opts.Name = hostname
		} else {
			opts.Name = "Local Agent"
		}
	}

	var session *Session

	// Resume mode.
	if opts.Resume != "" {
		s, err := LoadSession(opts.Resume)
		if err != nil {
			log.Fatalf("Failed to load session: %v", err)
		}
		if isProcessAlive(s.PID) {
			log.Fatalf("Session %s is still active (PID %d)", s.SandboxID, s.PID)
		}
		session = s
		opts.Server = s.ServerURL
		log.Printf("Resuming session (sandbox: %s)", session.SandboxID)
	} else if opts.Continue {
		s, err := FindLatestSession(cwd, "opencode")
		if err != nil {
			log.Fatalf("No session to continue: %v", err)
		}
		session = s
		opts.Server = s.ServerURL
		log.Printf("Continuing session (sandbox: %s)", session.SandboxID)
	}

	// New session.
	if session == nil {
		if opts.Server == "" {
			creds, _ := LoadCredentials(DefaultCredentialsPath())
			if creds != nil && creds.ServerURL != "" {
				opts.Server = creds.ServerURL
			} else {
				log.Fatal("--server is required (no saved credentials found)")
			}
		}

		accessToken, err := EnsureValidToken(opts.Server)
		if err != nil {
			log.Println("No valid credentials, starting login...")
			if err := RunLogin(LoginOptions{
				ServerURL:       opts.Server,
				SkipOpenBrowser: opts.SkipOpenBrowser,
			}); err != nil {
				log.Fatalf("Login failed: %v", err)
			}
			accessToken, err = EnsureValidToken(opts.Server)
			if err != nil {
				log.Fatalf("Failed to get access token after login: %v", err)
			}
		}

		regResp, err := registerAgentWithToken(opts.Server, accessToken, opts.Name, "opencode")
		if err != nil {
			log.Fatalf("Agent registration failed: %v", err)
		}
		log.Printf("Registered new sandbox: %s", regResp.SandboxID)

		session = &Session{
			SandboxID:   regResp.SandboxID,
			TunnelToken: regResp.TunnelToken,
			ProxyToken:  regResp.ProxyToken,
			WorkspaceID: regResp.WorkspaceID,
			Name:        opts.Name,
			Type:        "opencode",
			ServerURL:   opts.Server,
			Dir:         cwd,
			CreatedAt:   time.Now(),
		}
	}

	if err := SaveSession(session); err != nil {
		log.Printf("Warning: failed to save session: %v", err)
	}
	defer CleanupSession(session)

	// Determine opencode port.
	opencodePort := opts.OpencodePort
	if opencodePort == 0 {
		opencodePort = 4096
	}

	// Auto-start opencode if requested.
	var opencodeProc *OpencodeProcess
	if opts.AutoStart {
		opencodeURL := fmt.Sprintf("http://localhost:%d", opencodePort)

		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(opencodeURL + "/")
		if err == nil {
			resp.Body.Close()
			log.Printf("opencode already running on port %d, skipping auto-start", opencodePort)
		} else {
			log.Printf("Starting opencode on port %d...", opencodePort)
			opencodeProc, err = StartOpencode(opts.OpencodeBin, opencodePort, opts.OpencodeToken)
			if err != nil {
				log.Fatalf("Failed to start opencode: %v", err)
			}

			readyCtx, readyCancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := opencodeProc.WaitReady(readyCtx, 30*time.Second); err != nil {
				readyCancel()
				opencodeProc.Stop()
				log.Fatalf("opencode failed to become ready: %v", err)
			}
			readyCancel()
		}

		if !opts.OpencodeURLSet {
			opts.OpencodeURL = opencodeURL
		}
	}

	if opts.OpencodeURL == "" {
		opts.OpencodeURL = fmt.Sprintf("http://localhost:%d", opencodePort)
	}

	tunnelClient := NewClient(session.ServerURL, session.SandboxID, session.TunnelToken, opts.OpencodeURL, opts.OpencodeToken, cwd)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		log.Printf("Received %v, disconnecting...", sig)
		cancel()
		if opencodeProc != nil {
			opencodeProc.Stop()
		}
	}()

	log.Printf("Connecting to %s (forwarding to %s)...", session.ServerURL, opts.OpencodeURL)
	if err := tunnelClient.Run(ctx); err != nil && ctx.Err() == nil {
		if opencodeProc != nil {
			opencodeProc.Stop()
		}
		CleanupSession(session)
		log.Fatalf("Agent error: %v", err)
	}

	if opencodeProc != nil {
		opencodeProc.Stop()
	}

	fmt.Println("Agent disconnected.")
	fmt.Printf("To resume this session: agentserver connect --resume %s\n", session.SandboxID)
}
