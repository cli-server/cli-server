package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentserver/agentserver/internal/auth"
	"github.com/agentserver/agentserver/internal/db"
	"github.com/agentserver/agentserver/internal/imbridge"
	"github.com/agentserver/agentserver/internal/imbridgesvc"
	"github.com/agentserver/agentserver/internal/sbxstore"
)

func main() {
	cfg := imbridgesvc.LoadConfigFromEnv()

	if cfg.DatabaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	database, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	defer database.Close()
	log.Println("connected to database")

	authSvc := auth.New(database)
	sandboxStore := sbxstore.NewStore(database)

	// Create K8s exec client for IPC group registration in sandbox pods.
	// Returns nil in non-K8s environments; bridge degrades gracefully.
	var execCmd imbridge.ExecCommander
	if k8sExec := imbridgesvc.NewK8sExec(database); k8sExec != nil {
		execCmd = k8sExec
		log.Println("imbridge: K8s exec available for group registration")
	}

	bridge := imbridge.NewBridge(database, sandboxStore, execCmd, []imbridge.Provider{
		&imbridge.WeixinProvider{},
		&imbridge.TelegramProvider{},
		&imbridge.MatrixProvider{},
	})

	// Initialize providers that need server-level setup (Matrix E2EE crypto DB).
	for _, p := range bridge.Providers() {
		if ip, ok := p.(imbridge.InitializableProvider); ok {
			if err := ip.InitProvider(cfg.DatabaseURL); err != nil {
				log.Printf("imbridge: failed to initialize provider %s: %v", p.Name(), err)
			} else {
				log.Printf("imbridge: provider %s initialized", p.Name())
			}
		}
	}

	srv := imbridgesvc.NewServer(database, authSvc, sandboxStore, bridge)
	srv.RestorePollers()

	httpServer := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: srv.Routes(),
	}

	// Graceful shutdown on SIGTERM/SIGINT.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		log.Printf("received %v, shutting down...", sig)
		bridge.StopAllPollers()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
	}()

	log.Printf("starting imbridge on %s", cfg.ListenAddr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
