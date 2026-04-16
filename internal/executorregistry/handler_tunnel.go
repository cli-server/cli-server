package executorregistry

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/hashicorp/yamux"
	"nhooyr.io/websocket"
)

// handleTunnel upgrades the connection to a WebSocket and establishes a yamux
// multiplexed tunnel to an executor. The tunnel remains open until the session
// closes, at which point the executor status is set to offline.
func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	executorID := chi.URLParam(r, "executor_id")
	token := r.URL.Query().Get("token")
	if executorID == "" || token == "" {
		http.Error(w, "missing parameters", http.StatusBadRequest)
		return
	}

	// Validate tunnel token via store.
	valid, err := s.store.ValidateTunnelToken(r.Context(), executorID, token)
	if err != nil || !valid {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Accept WebSocket upgrade.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.logger.Error("tunnel websocket accept failed", "executor_id", executorID, "error", err)
		return
	}

	// Wrap WebSocket as net.Conn and create yamux server session.
	conn := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
	session, err := yamux.Server(conn, yamux.DefaultConfig())
	if err != nil {
		s.logger.Error("yamux session creation failed", "executor_id", executorID, "error", err)
		ws.Close(websocket.StatusInternalError, "yamux init failed")
		return
	}

	// Register tunnel and update executor status to online.
	s.tunnels.Register(executorID, session)
	if err := s.store.UpdateExecutorStatus(r.Context(), executorID, "online"); err != nil {
		s.logger.Error("failed to mark executor online", "executor_id", executorID, "error", err)
	}
	s.logger.Info("tunnel connected", "executor_id", executorID)

	// Block until the tunnel session closes.
	<-session.CloseChan()

	// Cleanup: unregister tunnel and mark executor offline.
	// Use background context because r.Context() is cancelled on disconnect.
	bgCtx := context.Background()
	s.tunnels.Unregister(executorID)
	if err := s.store.UpdateExecutorStatus(bgCtx, executorID, "offline"); err != nil {
		s.logger.Error("failed to mark executor offline", "executor_id", executorID, "error", err)
	}
	s.logger.Info("tunnel disconnected", "executor_id", executorID)
}
