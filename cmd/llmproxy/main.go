package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentserver/agentserver/internal/llmproxy"
)

func main() {
	cfg := llmproxy.LoadConfigFromEnv()

	if cfg.AnthropicAPIKey == "" && cfg.AnthropicAuthToken == "" {
		log.Fatal("either ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN is required")
	}
	if cfg.AgentserverURL == "" {
		log.Fatal("LLMPROXY_AGENTSERVER_URL is required")
	}

	logger := llmproxy.NewLogger(slog.LevelInfo)

	// Connect to proxy database (optional — proxy works without DB, just no persistence).
	var store *llmproxy.Store
	if cfg.DatabaseURL != "" {
		var err error
		store, err = llmproxy.NewStore(cfg.DatabaseURL)
		if err != nil {
			log.Fatalf("failed to connect to database: %v", err)
		}
		defer store.Close()
		logger.Info("connected to database")
	} else {
		logger.Warn("no database configured, usage tracking disabled")
	}

	srv := llmproxy.NewServer(cfg, store, logger)

	httpServer := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: srv.Routes(),
	}

	// Graceful shutdown on SIGTERM/SIGINT.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			logger.Error("shutdown error", "error", err)
		}
	}()

	logger.Info("starting llmproxy", "addr", cfg.ListenAddr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
