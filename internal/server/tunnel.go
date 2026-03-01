package server

import (
	"context"
	"io"
	"log"
	"net/http"
	"time"

	"encoding/base64"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/agentserver/agentserver/internal/sbxstore"
	"github.com/agentserver/agentserver/internal/tunnel"
	"nhooyr.io/websocket"
)

// handleTunnel upgrades the connection to a WebSocket and serves as the
// server-side endpoint for local agent tunnels.
func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	sandboxID := chi.URLParam(r, "sandboxId")
	token := r.URL.Query().Get("token")
	if sandboxID == "" || token == "" {
		http.Error(w, "missing parameters", http.StatusBadRequest)
		return
	}

	// Validate sandbox exists, is local, and tunnel token matches.
	sbx, err := s.DB.GetSandboxByTunnelToken(sandboxID, token)
	if err != nil {
		log.Printf("tunnel auth error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if sbx == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Accept WebSocket upgrade.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("tunnel websocket accept error: %v", err)
		return
	}

	// Register tunnel.
	t := s.TunnelRegistry.Register(sandboxID, conn)
	log.Printf("tunnel connected: sandbox %s", sandboxID)

	// Update sandbox status to running.
	s.Sandboxes.UpdateStatus(sandboxID, sbxstore.StatusRunning)
	s.DB.UpdateSandboxHeartbeat(sandboxID)

	// Start heartbeat ticker.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.Done():
				return
			case <-ticker.C:
				if err := conn.Ping(ctx); err != nil {
					log.Printf("tunnel %s: ping failed: %v", sandboxID, err)
					cancel()
					return
				}
				s.DB.UpdateSandboxHeartbeat(sandboxID)
			}
		}
	}()

	// Wait for tunnel to close.
	select {
	case <-ctx.Done():
	case <-t.Done():
	}

	// Cleanup.
	s.TunnelRegistry.Unregister(sandboxID, t)
	t.Close()

	// Set status to offline.
	s.Sandboxes.UpdateStatus(sandboxID, sbxstore.StatusOffline)
	log.Printf("tunnel disconnected: sandbox %s", sandboxID)
}

// proxyViaTunnel forwards an HTTP request through a WebSocket tunnel to the local agent.
// All responses are received as chunked binary StreamFrames.
func (s *Server) proxyViaTunnel(w http.ResponseWriter, r *http.Request, sbx *sbxstore.Sandbox, t *tunnel.Tunnel) {
	// Read request body.
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusInternalServerError)
			return
		}
	}

	// Build request headers.
	headers := make(map[string]string)
	for key, vals := range r.Header {
		if len(vals) > 0 {
			headers[key] = vals[0]
		}
	}

	// Inject opencode Basic Auth.
	if sbx.OpencodePassword != "" {
		cred := base64.StdEncoding.EncodeToString([]byte("opencode:" + sbx.OpencodePassword))
		headers["Authorization"] = "Basic " + cred
	}

	reqHeader := &tunnel.RequestHeader{
		Type:    tunnel.FrameTypeRequest,
		ID:      uuid.New().String(),
		Method:  r.Method,
		Path:    r.URL.RequestURI(),
		Headers: headers,
	}

	// Track activity.
	s.throttledActivity(sbx.ID)

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	streamCh, err := t.SendRequest(ctx, reqHeader, body)
	if err != nil {
		log.Printf("tunnel proxy error for %s: %v", t.SandboxID, err)
		http.Error(w, "tunnel proxy error", http.StatusBadGateway)
		return
	}
	defer t.CleanupRequest(reqHeader.ID)

	flusher, _ := w.(http.Flusher)

	headersSent := false
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-streamCh:
			if !ok {
				return
			}

			// Send headers on first frame.
			if !headersSent {
				for k, v := range msg.Header.Headers {
					w.Header().Set(k, v)
				}
				if msg.Header.Status > 0 {
					w.WriteHeader(msg.Header.Status)
				}
				headersSent = true
			}

			if msg.Header.Done {
				return
			}

			if len(msg.Payload) > 0 {
				w.Write(msg.Payload)
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}
}
