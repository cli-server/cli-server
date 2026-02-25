package cmd

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/imryao/cli-server/internal/container"
	"github.com/imryao/cli-server/internal/process"
	"github.com/imryao/cli-server/internal/sandbox"
	"github.com/imryao/cli-server/internal/server"
	"github.com/imryao/cli-server/web"
	"github.com/spf13/cobra"
)

var (
	port       int
	password   string
	agentImage string
	backend    string
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the cli-server HTTP server",
	Long:  `Start the web server that provides a browser-based terminal to Claude Code CLI.`,
	Run: func(cmd *cobra.Command, args []string) {
		if password == "" {
			log.Fatal("--password is required")
		}

		var staticFS fs.FS
		distFS, err := fs.Sub(web.StaticFS, "dist")
		if err != nil {
			log.Printf("Warning: embedded static files not available: %v", err)
		} else {
			staticFS = distFS
		}

		var procMgr process.Manager

		switch backend {
		case "docker":
			cfg := container.DefaultConfig()
			if agentImage != "" {
				cfg.Image = agentImage
			}
			mgr, err := container.NewManager(cfg)
			if err != nil {
				log.Fatalf("Docker backend unavailable: %v", err)
			}
			log.Printf("Using Docker backend (image: %s)", cfg.Image)
			procMgr = mgr

		case "k8s":
			cfg := sandbox.DefaultConfig()
			if agentImage != "" {
				cfg.Image = agentImage
			}
			mgr, err := sandbox.NewManager(cfg)
			if err != nil {
				log.Fatalf("K8s backend unavailable: %v", err)
			}
			log.Printf("Using K8s sandbox backend (namespace: %s, image: %s)", cfg.Namespace, cfg.Image)
			procMgr = mgr

		default:
			log.Fatalf("Unknown backend: %s (supported: docker, k8s)", backend)
		}

		srv := server.New(password, procMgr, staticFS)
		addr := fmt.Sprintf(":%d", port)

		httpServer := &http.Server{Addr: addr, Handler: srv.Router()}

		// Graceful shutdown on SIGTERM/SIGINT
		go func() {
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
			sig := <-sigCh
			log.Printf("Received %v, shutting down...", sig)
			httpServer.Shutdown(context.Background())
			log.Println("Cleaning up sessions...")
			procMgr.Close()
		}()

		log.Printf("Starting cli-server on %s", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	},
}

func init() {
	rootCmd.AddCommand(serveCmd)
	serveCmd.Flags().IntVarP(&port, "port", "p", 8080, "Port to listen on")
	serveCmd.Flags().StringVar(&password, "password", "", "Login password (required)")
	serveCmd.Flags().StringVar(&agentImage, "agent-image", "", "Container image for agent sessions (default: from AGENT_IMAGE env or cli-server-agent:latest)")
	serveCmd.Flags().StringVar(&backend, "backend", "docker", "Session backend: docker or k8s")
}
