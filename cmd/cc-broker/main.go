package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentserver/agentserver/internal/ccbroker"
)

func main() {
	cfg, err := ccbroker.LoadConfigFromEnv()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	store, err := ccbroker.NewStore(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer store.Close()

	srv := ccbroker.NewServer(cfg, store)
	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		httpServer.Shutdown(ctx)
	}()

	log.Printf("cc-broker listening on :%s", cfg.Port)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
