package server

import (
	"context"
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/imryao/cli-server/internal/sbxstore"
	"github.com/imryao/cli-server/internal/tunnel"
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
func (s *Server) proxyViaTunnel(w http.ResponseWriter, r *http.Request, sbx *sbxstore.Sandbox, t *tunnel.Tunnel) {
	// Read request body.
	var bodyB64 string
	if r.Body != nil {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusInternalServerError)
			return
		}
		if len(bodyBytes) > 0 {
			bodyB64 = base64.StdEncoding.EncodeToString(bodyBytes)
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

	reqFrame := &tunnel.RequestFrame{
		Type:    tunnel.FrameTypeRequest,
		ID:      uuid.New().String(),
		Method:  r.Method,
		Path:    r.URL.RequestURI(),
		Headers: headers,
		Body:    bodyB64,
	}

	// Track activity.
	s.throttledActivity(sbx.ID)

	// Check Accept header to decide if we expect streaming.
	acceptHeader := r.Header.Get("Accept")
	isSSE := strings.Contains(acceptHeader, "text/event-stream")

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	if isSSE {
		s.proxySSEViaTunnel(ctx, w, reqFrame, t)
	} else {
		s.proxyHTTPViaTunnel(ctx, w, reqFrame, t)
	}
}

// proxyHTTPViaTunnel handles a normal (non-streaming) request via tunnel.
func (s *Server) proxyHTTPViaTunnel(ctx context.Context, w http.ResponseWriter, req *tunnel.RequestFrame, t *tunnel.Tunnel) {
	resp, err := t.SendRequest(ctx, req)
	if err != nil {
		log.Printf("tunnel proxy error for %s: %v", t.SandboxID, err)
		http.Error(w, "tunnel proxy error", http.StatusBadGateway)
		return
	}

	// Write response headers.
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(resp.Status)

	// Write response body.
	if resp.Body != "" {
		body, err := base64.StdEncoding.DecodeString(resp.Body)
		if err != nil {
			log.Printf("tunnel proxy: failed to decode response body: %v", err)
			return
		}
		w.Write(body)
	}
}

// proxySSEViaTunnel handles a streaming (SSE) request via tunnel.
func (s *Server) proxySSEViaTunnel(ctx context.Context, w http.ResponseWriter, req *tunnel.RequestFrame, t *tunnel.Tunnel) {
	streamCh, err := t.SendStreamRequest(ctx, req)
	if err != nil {
		log.Printf("tunnel SSE proxy error for %s: %v", t.SandboxID, err)
		http.Error(w, "tunnel proxy error", http.StatusBadGateway)
		return
	}
	defer t.CleanupStream(req.ID)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	headersSent := false
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-streamCh:
			if !ok {
				return
			}

			// Send headers on first frame.
			if !headersSent {
				for k, v := range frame.Headers {
					w.Header().Set(k, v)
				}
				if frame.Status > 0 {
					w.WriteHeader(frame.Status)
				}
				headersSent = true
			}

			if frame.Done {
				return
			}

			if frame.Chunk != "" {
				chunk, err := base64.StdEncoding.DecodeString(frame.Chunk)
				if err != nil {
					log.Printf("tunnel SSE proxy: failed to decode chunk: %v", err)
					return
				}
				w.Write(chunk)
				flusher.Flush()
			}
		}
	}
}
