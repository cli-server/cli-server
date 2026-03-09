package main

import (
	"context"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentserver/agentserver/internal/auth"
	"github.com/agentserver/agentserver/internal/db"
	"github.com/agentserver/agentserver/internal/sandboxproxy"
	"github.com/agentserver/agentserver/internal/sbxstore"
	"github.com/agentserver/agentserver/internal/tunnel"
	"github.com/agentserver/agentserver/opencodeweb"
)

func main() {
	cfg := sandboxproxy.LoadConfigFromEnv()

	if cfg.DatabaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	if cfg.BaseDomain == "" {
		log.Fatal("BASE_DOMAIN is required")
	}

	// Connect to PostgreSQL.
	database, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Database connection failed: %v", err)
	}
	defer database.Close()
	log.Println("Connected to PostgreSQL")

	// Load embedded opencode frontend.
	var opcodeStaticFS fs.FS
	ocDistFS, err := fs.Sub(opencodeweb.StaticFS, "dist")
	if err != nil {
		log.Printf("Warning: embedded opencode static files not available: %v", err)
	} else {
		opcodeStaticFS = ocDistFS
	}

	authSvc := auth.New(database)
	sandboxStore := sbxstore.NewStore(database)
	tunnelReg := tunnel.NewRegistry()

	srv := sandboxproxy.New(cfg, authSvc, database, sandboxStore, tunnelReg, opcodeStaticFS)

	httpServer := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: srv.Router(),
	}

	// Graceful shutdown on SIGTERM/SIGINT.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		log.Printf("Received %v, shutting down...", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("Shutdown error: %v", err)
		}
	}()

	log.Printf("Starting sandbox-proxy on %s (domain: %s)", cfg.ListenAddr, cfg.BaseDomain)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
