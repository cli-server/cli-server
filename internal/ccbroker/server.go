package ccbroker

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/agentserver/agentserver/internal/ccbroker/workspace"
)

type Server struct {
	config   Config
	store    *Store
	s3       *workspace.S3Store
	sse      *SSEBroker
	turnLock *TurnLock
	logger   *slog.Logger
}

func NewServer(cfg Config, store *Store) (*Server, error) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	s3, err := workspace.NewS3Store(workspace.S3Config{
		Endpoint:        cfg.S3Endpoint,
		Region:          cfg.S3Region,
		Bucket:          cfg.S3Bucket,
		AccessKeyID:     cfg.S3AccessKeyID,
		SecretAccessKey: cfg.S3SecretAccessKey,
		UseSSL:          cfg.S3UseSSL,
		PathStyle:       cfg.S3PathStyle,
	})
	if err != nil {
		return nil, fmt.Errorf("init s3 store: %w", err)
	}
	return &Server{
		config:   cfg,
		store:    store,
		s3:       s3,
		sse:      NewSSEBroker(),
		turnLock: NewTurnLock(),
		logger:   logger,
	}, nil
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	r.Post("/api/turns", s.handleProcessTurn)
	r.Post("/v1/sessions", s.handleCreateSession)

	return r
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
