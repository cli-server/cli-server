package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentserver/agentserver/internal/executorregistry"
)

func main() {
	cfg, err := executorregistry.LoadConfigFromEnv()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	store, err := executorregistry.NewStore(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer store.Close()

	srv := executorregistry.NewServer(cfg, store)
	httpServer := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: srv.Routes(),
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		httpServer.Shutdown(ctx)
	}()

	log.Printf("executor-registry listening on :%s", cfg.Port)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
