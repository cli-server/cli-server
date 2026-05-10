package codexappgateway

import (
	"context"
	"log/slog"
)

type Server struct {
	cfg      ServeConfig
	codexBin string
	logger   *slog.Logger
}

func NewServer(cfg ServeConfig, codexBin string, logger *slog.Logger) (*Server, error) {
	return &Server{cfg: cfg, codexBin: codexBin, logger: logger}, nil
}

// Run is a stub; Task 8 replaces it.
func (s *Server) Run(ctx context.Context, listenAddr string) error {
	s.logger.Info("server stub Run; sleeping until ctx done", "listen_addr", listenAddr)
	<-ctx.Done()
	return nil
}
